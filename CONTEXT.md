# dispatch — contexto da conversa de origem

Transcrição condensada da conversa que originou o projeto (iniciada no repo Polterware/Lore, 2026-10-14).
A seção "Estado atual" no fim descreve o que já está implementado. Itens marcados *(inferido)* ou *(não verificado)* não foram confirmados em fonte.

## O que é

Roteador de agentes de código via CLI. Um chamador (Claude Code, outro agente ou humano) pede uma tarefa com um **tier de inteligência**; o `dispatch` escolhe qual CLI/modelo instalado executa, com base em **capacidade** e **folga de quota (pace)**, e devolve um resultado **compacto**.

Não é outro agente de código. É infraestrutura entre agentes e os CLIs existentes.

## Decisões tomadas

| Tema | Decisão |
|---|---|
| Nome | `dispatch` |
| Linguagem | Go, binário único (Rust como alternativa; C e TypeScript descartados) |
| Local | Repo separado do Lore/Polterware (execução de agentes está fora do escopo do Lore) |
| Interface | **Somente CLI.** MCP descartado: custo fixo de contexto e runs longos brigam com timeouts; os clientes têm shell. Descoberta via `dispatch skill install` |
| Persistência | Arquivo JSONL simples; SQLite só se necessário |
| LLM extra | Nenhuma chamada de LLM para resumir no início |
| Relação com o Lore | Opcional: registrar delegações como AgentRun/Activity via MCP do Lore |

## Requisitos originais (resumo do briefing)

- Qualquer cliente MCP pode delegar a qualquer CLI suportado; também usável direto no terminal sem MCP.
- Core sem dependência do Claude Code.
- Orquestrador paga o mínimo de tokens: retorno compacto, detalhes sob demanda com paginação (`result(taskId, section, offset, limit)`).
- Adapters pluggáveis, com capabilities declaradas (`sessionResume`, `structuredOutput`, `readOnly`, `fileEditing`); recurso não suportado é recusado ou avisado, não ignorado.
- Segurança: respeitar sandbox e permissões nativas de cada CLI, nunca extrair ou usar credenciais indevidamente, evitar delegação recursiva, limpar árvore de processos, um escritor por diretório (worktrees para paralelo).
- Ciclo de vida mínimo: PENDING → RUNNING → COMPLETED | FAILED | CANCELLED | TIMED_OUT; síncrono por padrão.
- Fora do MVP: multi-agent councils, pipelines, UI, nuvem, execução distribuída.

## Ideia central (adicionada na conversa)

Parâmetro de **tier** (`easy|normal|hard`) em vez de nome de modelo. O gateway:

1. descobre os CLIs instalados e seus modelos;
2. filtra por tier numa tabela declarativa editável (a tabela envelhece; é manutenção contínua);
3. ordena por **folga de pace** (em reserva, não em deficit) e por "dura até o reset";
4. faz fallback quando um CLI estoura limite.

## Como o CodexBar obtém uso e pace (fonte: código lido)

Repo: https://github.com/steipete/CodexBar — arquivos: `Sources/CodexBarCore/UsagePace.swift`, `Sources/CodexBar/HistoricalUsagePace.swift`, `docs/providers.md`, `docs/codex.md`, `docs/claude.md`.

### Fontes de dados (endpoints não documentados publicamente)

| Provedor | Fonte |
|---|---|
| Codex | `GET chatgpt.com/backend-api/wham/usage` (Bearer de `~/.codex/auth.json`), ou JSON-RPC com `codex -s read-only -a never app-server` (`account/read`, `account/rateLimits/read`) |
| Claude | `GET api.anthropic.com/api/oauth/usage` com `anthropic-beta: oauth-2025-04-20` (campos `five_hour`, `seven_day`, `seven_day_opus`…); fallback: PTY `claude` + `/usage` parseado |
| Gemini | API de quota (`retrieveUserQuota`) com credenciais OAuth do Gemini CLI |
| Grok | `grok agent stdio` JSON-RPC `x.ai/billing` |
| Custo | varre JSONL locais de sessão, preço de lista (estimativa, não fatura) |

Caminho mais defensável para o dispatch: protocolos oficiais dos próprios CLIs (ex.: `codex app-server`). Ler `auth.json` e bater em backend por conta própria é frágil e pode violar ToS *(não verificado)*.

### Pace linear (`UsagePace.weekly`)

```
duração   = windowMinutes * 60            # padrão 10080 min
decorrido = duração - (resetsAt - agora)
esperado  = decorrido / duração * 100
real      = usedPercent
delta     = real - esperado               # >0 deficit, <0 reserva
taxa      = real / decorrido
eta       = (100 - real) / taxa
se eta >= (resetsAt - agora): dura até o reset; senão: "acaba em eta"
```

- Estágios por |delta|: ≤2 on track, ≤6 slightly, ≤12 ahead/behind, >12 far.
- `workDays` opcional: conta só dias úteis na semana.
- `speedMultiplier = capacidade restante / uso projetado restante`.

### Pace histórico (só Codex semanal; origem do "% risk")

- Grava snapshots (`usedPercent`, `resetsAt`, `windowMinutes`, `sampledAt`) em JSONL a cada 30 min ou delta ≥1 pt; retém 56 dias.
- Semana "completa" = ≥6 amostras com cobertura nas primeiras e últimas 24 h; reconstrói curva monotônica em grade de 169 pontos.
- Curva esperada = mistura (peso `lambda`) de mediana ponderada histórica e linha linear; peso por semana `exp(-idade/3 semanas)`; `lambda = clamp((nEff-2)/6, 0, 1)`; nunca acima do linear.
- Risco = fração ponderada de semanas passadas que, deslocadas para o uso atual, cruzam 100% antes do reset; suavizado `(massa+0.5)/(peso+1)`; só com ≥5 semanas.
- "Runs out in" = mediana ponderada dos instantes de cruzamento.

### Para o dispatch

- Duas peças: **coletor** por CLI (parte frágil; devolve `usedPercent`, `windowMinutes`, `resetsAt`) e **motor de pace** (estável, ~100 linhas).
- O pace linear já basta para rotear; o histórico é refinamento e exige só um JSONL de snapshots.

## Riscos abertos

1. Endpoints de uso são internos e podem mudar sem aviso.
2. Uso de tokens OAuth fora do CLI oficial: checar ToS de cada provedor antes de implementar.
3. Tabela de tiers por modelo é subjetiva e perecível.
4. Descoberta de modelos varia por CLI; alguns não listam.
5. Estimativa local (registrar chamadas + aprender com erros de rate limit) é o fallback seguro quando não há fonte oficial.

## Pontos não verificados

- Código/capacidades reais de Chorus, PAL MCP (`clink`) e cc-multi-cli-plugin não foram lidos. PAL MCP é o mais próximo e deve ser estudado primeiro.
- Colisão do nome `dispatch` em GitHub/npm/Homebrew não foi checada.
- Flags não interativas de cada CLI (resume, read-only, JSON streaming) precisam ser confirmadas na documentação oficial antes de escrever adapters.

## Estado atual (implementado)

`dispatch` sem argumentos abre o TUI ao vivo do repositório atual (cartões em pilha, saída verbosa do agente em tela cheia com `enter`; tela de estatísticas com `s`: visão geral, atividade por dia e quebras por agente, modelo, provider e tier, com `t` para trocar o período; tela de learning com `l`: classificador, o que aprendeu, calibração, curva semanal, evidência por modelo e cooldowns; tela de settings com `,`: abas Config/Tiers/Subagents/Agents/Integration/Verify (Tiers: `c` roda `tiers --check`; Agents: coluna de cooldown, `c`/`C` limpam; Integration: versão, `u`/`U` checam/instalam update; Integration: on/off, hook SessionStart com teste e detecção de binário quebrado, skills por chamador; Verify: `.dispatch/verify.json` do repo), tudo editável na própria tela (priority/discover, listas allow/deny, entradas de tier com speed/strength/ordem/agent/model/efforts, perfis de subagent com tier/readonly/timeout/instructions/prefer/avoid etc.; cada valor é validado antes de salvar) e `e` abre o arquivo inteiro no `$VISUAL`/`$EDITOR` sobre uma cópia validada (reabre com o texto se der erro); `p` alterna global/projeto; sem terminal, imprime o uso. O log verboso (`~/.dispatch/logs/<id>.agent.log`, redigido) é visualização do usuário: nunca entra no contexto do Claude. Comandos: `agents`, `doctor`, `tiers [--init|--check]`, `config`, `route`, `classify`, `run`, `wait`, `list`, `cancel`, `recover`, `verify`, `merge`, `clean`, `result`, `stats`, `subagents`, `on|off|status`, `hook`, `skill install|show|refresh`, `update`, `version`. Detalhes de cada um na seção "Evoluções" abaixo.

| Agente | Run | Quota | Resume |
|---|---|---|---|
| Codex | `codex exec` (`--json` p/ thread id) | `app-server` `account/rateLimits/read` | `codex exec resume <id>` |
| Antigravity | `agy -p --output-format json` | `agy -p /usage --output-format json` (grupos Gemini / Claude+GPT; semanal, ou 5h se ≥90%) | `--conversation <id>` |
| Cursor | `cursor-agent -p` | **indisponível**: só via cookies do navegador (extração de credencial, recusado) | `create-chat` + `--resume` |
| Grok | `grok -p` | **indisponível**: `x.ai/billing` no `agent stdio` responde -32601 (1.0.52); o resto exige token/cookie | `-s <uuid>` + `--resume` |

- Sem fonte de quota, o roteador aprende com erros de rate limit: o agente entra em cooldown de 1h e vai para o fim do ranking.
- Snapshots de quota em `~/.dispatch/snapshots.jsonl` (a cada 30 min ou ≥1 pt, retenção 56 dias) alimentam o pace histórico (risco ponderado, ≥5 semanas completas), que penaliza o ranking.
- Runs de escrita em repo git rodam num worktree `<repo>/.dispatch/worktrees/<id>` (branch `dispatch/<id>`), commitados ao fim; `merge` integra e limpa. `.dispatch/` entra em `.git/info/exclude`.
- Modelos Claude são filtrados/recusados nos CLIs de terceiros.
- Estado em `~/.dispatch` (`DISPATCH_HOME`): `runs.jsonl`, `tiers.json`, `snapshots.jsonl`, `cooldowns.json`, `logs/`.

### Verificado em 2026-10-15
- Skill: Codex e Grok leem `~/.agents/skills`; Antigravity lê `~/.gemini/config/skills` (confirmado nas strings do `agy`); `skill install` rodado na home real.
- Nome `dispatch`: livre no Homebrew; npm tem um pacote homônimo sem relação (connect dispatcher); no GitHub há `khlieng/dispatch` (cliente IRC, 641 estrelas), que também instala um binário `dispatch`. Conflito de PATH só se alguém tiver os dois.
- Quota Cursor/Grok: `cursor-agent about/status` e `grok usage <session>` não expõem quota do plano (só auth e tokens por sessão). Segue cooldown por rate limit.

### Ainda aberto
- Quota de Cursor e Grok só se a fonte oficial aparecer.
- ToS dos provedores: o dispatch só invoca os CLIs oficiais de forma não interativa e não lê credenciais, mas a leitura dos termos de cada provedor é decisão do dono.
- Pace histórico só vale para janelas semanais.

## Distribuição
- `scripts/release.sh` compila darwin/linux × arm64/amd64 em `dist/` com `checksums.txt`; publicar com `gh release create <versão> dist/*` em rckbrcls/dispatch.
- `scripts/install.sh` baixa o binário do último release, confere o SHA-256 e instala em `~/.local/bin` (`DISPATCH_INSTALL_DIR`, `DISPATCH_VERSION`, `DISPATCH_REPO`, `DISPATCH_BASE_URL`).
- Uso: `curl -fsSL https://rckbrcls.com/api/dispatch/install | sh` (servido pelo repo portfolio em `apps/portfolio/public/api/dispatch/install`; manter em sincronia com `scripts/install.sh`).
- Atualização: `dispatch update [--check]` baixa o release mais novo (SHA-256), troca o binário e roda `skill refresh` (só reescreve cópias já instaladas). `dispatch version` mostra a versão embutida via `-X main.version`.

## Subagents (perfis)
- `~/.dispatch/agents.json` + `<repo>/.dispatch/agents.json` (projeto vence): `{"agents": {nome: {tier, readonly, instructions, prefer, avoid, timeout}}}`.
- `dispatch run --as <nome>`: perfil preenche tier/readonly/timeout (flags explícitas vencem) e prefixa `instructions` ao prompt (não em `--resume`). `prefer`/`avoid` viram `Bias` no score do roteador (−50 / +1500): peso, não filtro. Gravados no Record para o `--background` re-executar igual.
- `dispatch subagents [--init|--check]`: lista, cria exemplo, valida (tier, timeout, agentes conhecidos/instalados).

## Evoluções (fases 0–6, todas entregues)
- **Segurança:** ambiente do agente por allowlist (`DISPATCH_ENV_ALLOW`), guarda de recursão (`DISPATCH_DEPTH`), redação de segredos em logs e no store; nunca lê credenciais de agentes.
- **`on/off` + hook:** `dispatch on` registra um hook SessionStart no `~/.claude/settings.json` que cita a skill `dispatch` (uma linha de contexto); `off` remove. Caminhos com espaço são citados no comando do hook.
- **Lifecycle:** grupos de processo + kill da árvore de descendentes, status `ORPHANED` (identidade PID + início do processo), `cancel` SIGTERM→SIGKILL, `recover`. O dispatch nunca limpa órfãos sozinho.
- **Verificador:** `.dispatch/verify.json` (`verify --init`) ou autodescoberta Go (`go vet`, `go test -json`); baseline lazy no `BaseSHA` com cache; checks baratos (diff --check, marcadores de conflito, testes removidos, workflow/lockfile). Status em `verification`: `verified|verification_failed|unverified|error`; `merge` recusa `verification_failed` sem `--force`.
- **RunSummary v1** (`dispatch.run-summary.v1`): `run`/`wait` imprimem JSON compacto; texto do agente só em `result --section agent`. Telemetria por run: provider, effort, queuedMs/runMs/verifyMs, arquivos/+/-, `exitClass`, duração por attempt; `runs.jsonl` rotaciona com gzip.
- **`stats`:** `--by model|agent|provider|tier|effort|day|week|requested|resolved`, `--since`, `--json`, `--capacity` (snapshots de quota); mínimo de amostras antes de rankear; desconhecido é explícito.
- **`doctor` e quota tipada:** `Probe()` por adapter (versão e flags via `--help`, nunca chama modelo) com `OK|DEGRADED|UNVERIFIED`; `DEGRADED` ainda roda com `note`. Quota com fonte e confiança (`official` para codex e agy; `empirical` para cursor e grok, sem percentual inventado).
- **Merge gate:** `git merge-tree --write-tree` antes de integrar (conflito recusa sem tocar working tree, `merge_conflict`), lock por repo+branch, recusa com mudanças locais nos mesmos arquivos, reverificação pós-merge (`post_merge_verification`, sugere `git revert -m 1`), `--no-verify`.
- **Filtros:** `--provider/--model/--effort/--agent/--relax`; filtro restringe, o roteador decide o resto; fallback respeita o filtro; `route` é o dry-run explicativo; `requested`/`resolved` no resumo.
- **Prioridade:** `--priority speed|balanced|quality` só muda pesos; priors `speed`/`strength` (1–5) no `tiers.json` padrão, substituídos por medição com ≥5 amostras por tier (mistura n/(n+5)).
- **OpenCode e binários:** adapter `opencode` (`run --standalone --format json --auto`, sem modo read-only, modelos `provider/model`; modelos Claude recusados). A CLI vive dentro do app (`/Applications/OpenCode.app/Contents/Resources/opencode-cli`), fora do PATH: `config.json` global aceita `"binaries": {"opencode": "/caminho"}` (ignorado em config de projeto, por segurança). Na aba Agents das settings: `b` define o caminho, `d` usa o local detectado, `x` volta ao padrão.
- **Política persistente:** `config.json` (global e `<repo>/.dispatch`) com `allow`/`deny` de providers, modelos e agents (deny vence; `--relax` não contorna), `discover` (padrão false), `priority`.
- **Classificador:** regressão logística própria (24 features, SGD + L2, por tier), prior por regras (reproduz "código sensível sobe um tier"), `classifier.json` versionado só com pesos, rótulo = verificador (exclui falhas não atribuíveis), sugestões aprendidas só viram `note`. Sem LLM e sem rede.

## Descartado / adiado (decisão do dono)
Não serão feitos por ora: cascata repair/escalate, scope leases, Thompson sampling, perfis de sandbox, `dispatch bench`, previsão de duração, LLM-juiz, export OTel, `ACPAdapter`, classificador via LLM (Jev). Reabrir só com uso real que justifique.
