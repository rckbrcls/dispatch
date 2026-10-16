# dispatch — plano de entregas

Origem: relatório de deep research (2026-10-15) filtrado para o tamanho do projeto, mais o classificador de dificuldade discutido depois.
As fases mexem nos mesmos arquivos (`runner.go`, `main.go`, `store`), então vão em sequência, cada uma em seu próprio commit.

## Visão geral

| Fase | Entrega | Depende de | Tamanho |
|---|---|---|---|
| 0 | Segurança barata **(feita)** | n/d | P |
| 0b | `dispatch on/off` + hook de sessão **(feita)** | n/d (pode sair antes de tudo) | P |
| 1 | Lifecycle robusto **(feita)** | 0 | M |
| 2 | Verificador + baseline **(feita)** | 1 | G |
| 3 | `RunSummary` v1 + `stats` **(feita)** | 2 | P/M |
| 3b | Telemetria e `dispatch stats` (uso, latência, capacidade) **(feita)** | 3 | M |
| 4 | Adapters (`doctor`, capabilities) + quota tipada **(feita)** | n/d (pode ir em paralelo à 3) | M |
| 5c | Filtros explícitos: `--provider`, `--model`, `--effort`, `--agent` **(feita)** | 4 (capabilities) | M |
| 5d | Allow/deny persistente de providers, modelos e agents (+ `discover` opcional) **(feita)** | 5c | P/M |
| 5b | Prioridade `speed|balanced|quality` no roteador **(feita)** | 3b (dados de latência) | P/M |
| 5 | Merge gate **(feita)** | 2 | P |
| 6 | Classificador logístico | 2 (rótulos) | M |
| 7 | Adiados (com gatilho) | 2 a 6 | n/d |

## Fase 0 — Segurança barata (feita)
- Env saneada: `cmd.Env` com allowlist (`PATH`, `HOME`, temp, locale e o que o CLI oficial precisa), em vez de `os.Environ()`.
- Guard de recursão: `DISPATCH_RUN_ID`, `DISPATCH_PARENT_RUN_ID`, `DISPATCH_DEPTH`, `DISPATCH_NO_DELEGATE=1`. O `run` recusa se herdar `NO_DELEGATE`.
- Redação de segredos nos logs e no JSONL (`*_TOKEN`, `*_SECRET`, `Authorization`, `Bearer`, blocos de chave privada).
- Pronto quando: teste prova que uma variável secreta do pai não chega ao filho, e que `dispatch run` dentro de um run falha com mensagem clara.

## Fase 0b — `dispatch on/off` e hook de sessão (feita)
Objetivo: sessões novas do Claude já começam sabendo que o dispatch está ativo e citam a skill sozinhas, sem o usuário pedir. Liga e desliga sob demanda.

Mecanismo (conferido em code.claude.com/docs/en/hooks e /plugins):
- Hook `SessionStart`, `type: "command"`. Stdout em texto puro com exit 0 entra no contexto; alternativa é JSON com `hookSpecificOutput.additionalContext`. Limite de 10.000 caracteres por string.
- Matcher omitido = dispara em `startup`, `resume`, `clear`, `compact` (reinjeta depois de compactar).
- Registro em `~/.claude/settings.json` (escopo usuário). Sessões locais do app desktop e do CLI leem os mesmos arquivos; sessões em nuvem não.
- A doc recomenda frases factuais; texto em tom de comando de sistema pode disparar defesas contra prompt injection. Redigir como fato ("O dispatch está ativo... a skill `dispatch` descreve como delegar"), não como ordem.

Entrega:
- `dispatch on|off|status`: flag em `~/.dispatch` (sem editar o settings a cada toggle).
- `dispatch hook install|uninstall`: grava/remove uma vez a entrada `SessionStart` no settings do usuário, com `command: dispatch context --hook`. `on` chama `hook install` se faltar e garante que a skill está instalada (`skill install`).
- `dispatch context --hook`: se a flag está desligada, não imprime nada e sai 0. Se ligada, imprime o bloco curto citando a skill (~30 a 60 tokens). Nunca chama modelo nem consome quota.
- Bloco só com campos estruturados, nunca texto livre vindo de runs de agentes.
- Evolução (depois da fase 4): acrescentar ao bloco os agentes disponíveis e cooldowns, lidos de `cooldowns.json` e do `doctor`.
- Opcional depois: empacotar skill + hook como plugin para ter o toggle nativo do `/plugin` (`claude plugin disable`), semelhante aos connectors.

Riscos e decisões:
- O Claude precisa obedecer à citação e invocar a skill; se esquecer com frequência, plano B é incluir um resumo de 3 a 4 linhas no bloco.
- Quando desligado o hook ainda dispara um processo no início da sessão; custo desprezível, mas existe. Alternativa: `hook uninstall` no `off`, ao custo de editar o settings em cada toggle.
- Vale só para sessões novas; uma sessão já aberta não recebe o bloco.

Pronto quando: com `on`, uma sessão nova cita e usa a skill sem ser instruída; com `off`, nenhum contexto extra é injetado; `hook uninstall` deixa o settings como estava.

## Fase 1 — Lifecycle (feita)
- Estados: `queued → starting → running → verifying → ready_to_merge → merged|failed|canceled|orphaned` (`verifying` entra vazio, preenchido na fase 2).
- `Setpgid` + cancel por grupo (`SIGTERM`, grace, `SIGKILL`). Guardar `pid`, hora de início, base SHA, branch, worktree.
- Reconciliação no startup: reler `runs.jsonl`, cruzar com processos vivos e `git worktree list --porcelain`. Árvore dirty vira `orphaned` e é preservada, nunca apagada.
- `dispatch recover|clean` para decidir o destino dos órfãos.
- Pronto quando: teste mata o `dispatch` no meio de um run e o próximo comando reconcilia sem perder trabalho.

## Fase 2 — Verificador (feita)
- Config em `.dispatch/verify.json` (comandos, timeout, `required`) e autodescoberta de fallback (`go test`, `go vet`, Makefile).
- Execução via `exec.CommandContext`, sem shell.
- Baseline por `base_sha + config_hash`, em cache; regressão = falha que não existia no baseline.
- Checks baratos: `git diff --check`, marcadores de conflito, testes removidos, arquivos fora do escopo.
- Três status separados: `process_success`, `verification_success`, `verified`. Classificação de falha (qualidade vs rate limit vs crash vs conflito) gravada no evento.
- Gravar as features do prompt no `runs.jsonl` já aqui, para o classificador ter dados depois.
- Pronto quando: run de escrita termina em `verified` ou `verification_failed` com a lista de regressões, e o baseline não imputa ao agente falhas pré-existentes.

## Fase 3 — `RunSummary` v1 (feita)
- Contrato `dispatch.run-summary.v1`: status, agente, tier, duração, arquivos, +/-, verify, merge, retries, ponteiro para detalhes. Saída humana de 3 linhas e `--json`.
- `result` com seções: `summary`, `verification`, `files`, `diff`, `stderr`, `agent`, `routing`, `quota`.
- `dispatch stats`: agregações direto do JSONL (sucesso na primeira, p50/p90, rate limits, fallbacks, órfãos).
- Pronto quando: `run` imprime só o summary e o texto livre do agente fica atrás de `--section agent`.

## Fase 3b — Telemetria e `dispatch stats` (feita)
Hoje o `runs.jsonl` já tem `agent`, `model`, `tier`, `status`, `attempts`, `createdAt`, `finishedAt` (duração já é derivável) e `snapshots.jsonl` tem a quota por janela. Faltam campos para uma telemetria de verdade.

Novos campos por run (todos opcionais; ausente = desconhecido, nunca zero):
- `provider` (derivado do agente/modelo), `effort` (extraído do nome do modelo quando embutido, ex. `...-high`; um campo próprio quando o CLI tiver flag — a verificar por adapter), `startedAt` (separa fila de execução), `queuedMs`, `runMs`, `verifyMs`.
- `tokensIn/Out` e custo estimado apenas quando o adapter souber; sem estimar por conta própria.
- `exitClass` (ok, rate_limit, crash, timeout, verify_fail, merge_conflict), `filesChanged`, `insertions`, `deletions`.
- Em runs com fallback, cada `attempt` leva seu `durationMs`.

`dispatch stats` (lê só o JSONL, sem rede nem modelo):
- `--by model|agent|provider|tier|effort|day|week` e `--since 7d`; colunas: runs, % sucesso (final e na primeira tentativa), p50/p90 de duração, fallbacks, rate limits, runs órfãos.
- Rankings: modelo mais usado, provider mais usado, mais lento, mais falho.
- Histórico: série por dia/semana por modelo e provider (`--json` para plotar).
- `--capacity`: uso de cada agente contra a própria capacidade. Fonte = `snapshots.jsonl` (percentual da janela consumido, reset, duração). Métricas: % consumido por janela, quanto sobrou no reset (capacidade desperdiçada), quantas vezes encostou em 100%, e se o pace estava adiantado ou atrasado.

Limites assumidos (importante):
- "Conta" = um CLI = uma assinatura; o dispatch não enxerga multi-conta.
- A normalização é **em percentual da própria janela**, não em tokens absolutos: os provedores não expõem a capacidade em unidade comparável. Comparar "40% do Codex" com "40% do Antigravity" diz quanto de cada plano foi usado, não quanto trabalho cada um rendeu.
- Cursor e Grok não têm fonte de quota; aparecem só com contagem de runs e latência.
- Latência depende do tamanho da tarefa. Antes de dizer "o modelo X é mais lento", segmentar por tier e por tamanho do diff (ou classe de tarefa, depois do classificador), e exigir um mínimo de amostras.

Retenção: rotação do `runs.jsonl` por tamanho/idade com `gzip`; `stats` lê os arquivos rotacionados.

Pronto quando: `dispatch stats --by model --since 30d` e `dispatch stats --capacity` rodam sobre dados reais e deixam explícito o que é desconhecido.

## Fase 4 — Adapters, `doctor` e quota tipada (feita)
- `Probe()` e `Capabilities()` por adapter, sem chamar modelo; capabilities opcionais via interfaces pequenas (`UsageProvider`, `Resumer`).
- `dispatch doctor`: versão, flags e capabilities por CLI, com `OK / DEGRADED / UNVERIFIED`.
- Golden tests em `testdata/<adapter>/<versão>/`.
- Quota tipada: `QuotaSample{window, used, resetAt, duration, source, confidence}`; sem fonte oficial, expor só risco empírico, nunca um percentual inventado.
- Pronto quando: mudar uma flag fake de um CLI faz o `doctor` marcar `DEGRADED` em vez de quebrar o run.

## Fase 5 — Merge gate (feita)
- Antes do `merge`: `git merge-tree` contra o HEAD atual, usando o código de saída e não só a lista de arquivos.
- Merge serializado por branch alvo e reverificação depois de integrar.
- Pronto quando: dois runs que editam o mesmo arquivo são detectados antes de tocar o working tree.

## Fase 5c — Filtros explícitos no roteamento (feita)
Hoje o chamador só diz o tier. Passa a poder restringir o espaço de busca; o roteador continua escolhendo o que sobrar por pace/rank.

Vocabulário (a distinção importa): **provider** = quem serve o modelo (openai, google, xai...); **agent** = o CLI que o executa (codex, cursor, grok, antigravity); **model** e **effort** como já existem. Um mesmo modelo pode estar disponível por mais de um agent, então escolher o modelo não escolhe o agent.

Flags em `run` (e campos nos perfis de subagent): `--provider`, `--model`, `--effort low|medium|high|xhigh`, `--agent`.

Regra: cada filtro **restringe** os candidatos; o que não foi fixado o roteador decide.
| Chamador fixa | Roteador decide |
|---|---|
| nada (só tier) | modelo, agent, effort |
| `--provider` | melhor modelo daquele provider que caiba no tier/prioridade, agent, effort |
| `--model` | agent (CLI) com mais folga de pace entre os que servem o modelo, e effort |
| `--model` + `--effort` | só o agent (CLI); modelo e effort ficam exatamente como pedidos |
| `--agent` | modelo e effort dentro daquele CLI |

Detalhes:
- **Tier opcional** quando há `--model`/`--provider`. Se vierem juntos, vale a interseção; modelo explícito fora da tabela de tiers roda mesmo assim, com `note`.
- **Effort**: automático a partir do tier/classificador quando não informado. Como cada CLI expressa isso diferente (sufixo no nome do modelo no agy, flag/config no codex), o adapter declara em capabilities como aplica effort; effort não suportado por aquele modelo/agent é **recusado com mensagem**, nunca ignorado.
- **Fallback respeita o filtro**: com `--model` o fallback só troca de agent que serve o mesmo modelo; com `--provider`, só de modelo do mesmo provider. Sem candidato restante, falha com erro claro (`FAILED` + motivo) em vez de rebaixar em silêncio; `--relax` opcional para permitir cair para o roteamento por tier, registrado em `note`.
- **Provider/effort conhecidos por modelo** vêm de `tiers.json` (campos `provider`, `efforts`), validados por `tiers --check` e por `doctor`.
- Restrições existentes continuam valendo: modelos Claude recusados em CLIs de terceiros, cooldown, `readonly`, capabilities.
- `dispatch route` (dry-run, sem executar) mostra candidatos, o que cada filtro eliminou e por que o vencedor ganhou; os filtros aparecem no `RunSummary` (`requested` vs `resolved`) e nas agregações do `stats`.
- Pronto quando: um teste com 2 agents servindo o mesmo modelo escolhe o agent por pace; `--provider` troca de modelo conforme tier/prioridade; `--model` + `--effort` roda sem chamar o ranking de modelo; filtro impossível falha com mensagem e sem fallback silencioso.

## Fase 5d — Allow/deny persistente
Hoje a tabela `tiers.json` é uma lista de permissão implícita por tier e os filtros da 5c valem por run. Falta uma política persistente.
- `config.json` (global `~/.dispatch` e por projeto `<repo>/.dispatch`, projeto vence): `allow` e `deny` com `providers`, `models`, `agents`; `discover` (padrão **false**).
- `allow` vazio = tudo o que estiver instalado e na tabela; preenchido = só o que casar. `deny` sempre vence. Aplica antes do ranking, junto com tier e filtros da 5c.
- Flags por run só **estreitam** dentro da política; pedir algo fora dela recusa com mensagem (nunca contorna).
- `discover: true`: modelos que os CLIs listam e a tabela não tem entram em tier `normal` com `note`, ainda sujeitos a allow/deny.
- `dispatch config get|set|allow|deny`, `route` e `doctor` mostram o que a política eliminou.
- Pronto quando: com `allow.providers=[openai]` nenhum run sai para outro provider mesmo com fallback; `deny` vence `allow`; flag fora da política recusa; `discover` off por padrão.

## Fase 5b — Prioridade de roteamento (feita)
- `--priority speed|balanced|quality` (padrão `balanced`) em `run`, e campo `priority` nos perfis de subagent; pode ter um padrão global em `~/.dispatch`.
- Dentro do tier elegível, a prioridade só muda os **pesos** do ranking (não filtra): `speed` pondera p50 de duração e folga de pace; `quality` pondera a taxa de sucesso verificada; `balanced` mantém o comportamento atual.
- Antes de haver dados: atributos estáticos na tabela de tiers (`speed` e `strength` por modelo, editáveis em `tiers.json`). Com dados da fase 3b, passam a ser sobrescritos por medições, com prior no valor estático e mínimo de amostras.
- A prioridade nunca fura as restrições: tier, capabilities, cooldown e `readonly` continuam valendo.
- Pronto quando: o mesmo tier roteia para modelos diferentes com `speed` e `quality` num cenário de teste, e `route` explica por quê.

## Fase 6 — Classificador logístico (sem LLM)
- Features (15 a 25): tamanho/estrutura do prompt, arquivos e pacotes citados que existem no repo, tipo de mudança, risco, verificabilidade, contexto (readonly/escrita, tier pedido).
- Normalização, modelo logístico com SGD de um passo por run, regularização L2 leve.
- Pesos iniciais = regras à mão (prior), para funcionar no dia 1. Persistência em `~/.dispatch/classifier.json` com versão do esquema de features.
- Rótulo = resultado do verificador, excluindo falhas não atribuíveis (rate limit, crash, conflito de merge). Treino por tier para mitigar viés de seleção. Mínimo de amostras antes de atualizar.
- `dispatch classify "<prompt>"` (tier, score, motivos), aviso no `run`, `--explain`, `--reset`.
- Pronto quando: com o prior reproduz as regras; com dados sintéticos o treino converge.

## Fase 7 — Adiados, com gatilho
| Item | Gatilho para começar |
|---|---|
| Cascata repair/escalate | verificador estável por algumas semanas |
| Scope leases | uso rotineiro de 3+ escritores paralelos |
| Thompson sampling | ~100+ runs verificados por agente |
| Perfis de sandbox (`read-only`, `write-no-network`...) | decidir como traduzir para cada CLI |
| `dispatch bench` (benchmark local por modelo) | verificador estável + ~50 runs verificados; suíte pequena de tarefas fixas do próprio repo, rodada sob demanda (consome quota), resultados em arquivo separado |
| Previsão de duração (regressão simples, sem LLM) | latência por modelo com amostras suficientes; uso: timeout automático e aviso de "este run deve levar ~N min" |
| Modelo de IA para "validar capacidade" | descartado por ora: o verificador executável já é o juiz; um LLM-juiz só entra como opcional para casos semânticos |
| Export OTel | alguém precisar consumir |
| `ACPAdapter` | Codex ou Grok oferecerem ACP estável |
| Jev opcional | classificador local medido e com margem de erro |

## Decisões em aberto
1. Config do verificador: `.dispatch/verify.json` por projeto (sugerido) ou só autodescoberta?
2. Política de `orphaned`: preservar sempre e exigir `recover/clean` manual (sugerido) ou limpar sozinho o que não tem diff?
3. Fase 4 antes da 3? São independentes; sugerido 3 primeiro por valor visível no uso diário.

## Ressalvas do relatório
- Várias citações de 2026 (IDs de arXiv, spec MCP `2026-07-28`) não foram verificadas; conferir antes de se apoiar nelas.
- Parte do que o relatório lista como novo já existe (worktrees, cooldown, pace histórico, resume, perfis de subagent, update).
