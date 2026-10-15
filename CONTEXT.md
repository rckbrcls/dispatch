# dispatch — contexto da conversa de origem

Transcrição condensada da conversa que originou o projeto (iniciada no repo Polterware/Lore, 2026-10-14).
Nada aqui foi implementado ainda. Itens marcados *(inferido)* ou *(não verificado)* não foram confirmados em fonte.

## O que é

Roteador de agentes de código via CLI. Um chamador (Claude Code, outro agente ou humano) pede uma tarefa com um **tier de inteligência**; o `dispatch` escolhe qual CLI/modelo instalado executa, com base em **capacidade** e **folga de quota (pace)**, e devolve um resultado **compacto**.

Não é outro agente de código. É infraestrutura entre agentes e os CLIs existentes.

## Decisões tomadas

| Tema | Decisão |
|---|---|
| Nome | `dispatch` |
| Linguagem | Go, binário único (Rust como alternativa; C e TypeScript descartados) |
| Local | Repo separado do Lore/Polterware (execução de agentes está fora do escopo do Lore) |
| Interface | CLI como núcleo; MCP fino por cima (1–2 tools: `delegate`, `result`) |
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

## Próximos passos sugeridos

1. Verificar nome e ToS.
2. Ler `clink` do PAL MCP e decidir se vale construir ou consumir o que existe.
3. `go mod init`, interface `Adapter` + `Capabilities`.
4. Primeiro coletor: Codex via `app-server` (`account/rateLimits/read`).
5. Motor de pace linear, depois snapshots JSONL.
6. `dispatch agents`, `dispatch run --tier`, `dispatch result`.
7. MCP stdio fino.
