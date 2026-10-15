---
name: dispatch
description: Delega uma tarefa a outro agente de código (Codex, Cursor, Grok, Antigravity) escolhendo por dificuldade (tier easy|normal|hard) em vez de modelo, e devolve um resultado compacto. Use para economizar tokens do orquestrador ou paralelizar trabalho — implementação localizada, testes, revisão, leitura de código, investigação. Não use para tarefas triviais que você resolve em poucas chamadas.
---

# dispatch

CLI que roteia uma tarefa para um agente instalado, por **tier**, e devolve resultado compacto.
O `dispatch` escolhe pela tabela de tiers e pela folga de quota,
e faz fallback se um agente falhar. Você é o maestro: delega, lê resumos curtos, integra.

## Quando delegar (e quando não)

Delegue: implementação localizada, testes, revisão, leitura/investigação de código, trabalho
independente que pode rodar em paralelo. **Faça você mesmo**: mudança de uma linha, tarefa que
exige o contexto desta conversa inteira, ou algo que o brief custaria mais que executar.
Escolha o menor tier que dá conta; suba só se o resultado pedir.

| Tier | Para quê |
|---|---|
| `easy` | tarefa clara e localizada, leitura, testes simples, refactor mecânico |
| `normal` | feature em alguns arquivos, bug com causa a investigar |
| `hard` | arquitetura, código sensível (auth, dinheiro, permissões, dados), bug difícil |

Código sensível sobe um tier automaticamente.

## Receita

1. **Despache em background** (padrão recomendado: runs levam minutos e o Bash tem timeout):

```bash
dispatch run --tier normal --dir <repo> --background - <<'BRIEF'
<objetivo; o que pode/não pode tocar; comando de verificação exato; relatório final em ≤ 10 linhas>
BRIEF
```
   Imprime JSON com o `id` na hora. O brief leva **caminhos, nunca código colado**.
2. **Espere:** `dispatch wait <id> --timeout 9m`. Exit 3 = ainda rodando: chame `wait` de novo.
   Não encerre o turno com runs pendentes.
3. **Leia o RunSummary** (JSON compacto; o texto do agente fica em `dispatch result <id> --section agent`), confira `git -C <worktree> diff --stat` e rode os testes. Não leia o
   código escrito na íntegra.
4. **Integre:** `dispatch merge <id>` (só com `COMPLETED`). `verification_failed` recusa o merge até `--force` e lista as regressões. `unverified` e `error` avisam e ainda mesclam. Falhou o run? `dispatch clean <id>`.
   Conflito (`merge_conflict`): detectado via `git merge-tree` antes de tocar o working tree, sem bypass por `--force`.
   Recusa com JSON listando os arquivos e sugere `dispatch run --resume <id>` (pedindo rebase) ou resolver à mão.

Síncrono (`run` sem `--background`) só serve para tarefas curtas; ele bloqueia até o fim.

## Paralelismo

Delegações independentes saem na **mesma resposta**, cada uma com `--background` e seu próprio `id`;
depois um `wait` por id. Duas escritas no mesmo `--dir` com `--no-worktree` conflitam; com worktree
(padrão em repo git) cada run tem sua branch e não há conflito.

## Resultado

`run` e `wait` imprimem um RunSummary (`dispatch.run-summary.v1`): `status`, `agent`, `model`, `tier`,
`durationMs`, arquivos e +/-, `verification` (só o status), `attempts`, `branch`, `worktree` e ponteiros
em `details`. O texto livre do agente não vem nesse JSON. `--json=false` imprime 3 linhas; o padrão é o JSON.

- `attempts` > 1: houve **fallback**. Erros em `--section attempts`. Rate limit gera **cooldown de 1h**.
- `note`: checkout sujo, worktree perdido, commit falho, "no file changes".
- `verification`: `verified` (sem regressão nova), `verification_failed`, `unverified` (não é um passe) ou `error`.

```bash
dispatch result <id> --section summary
dispatch result <id> --section agent --offset 0 --limit 100
dispatch result <id> --section verification
dispatch result <id> --section files
dispatch result <id> --section attempts
dispatch result <id> --section meta
```

Exit code: 0 ok, 124 timeout, 1 demais falhas (`wait`: 3 se ainda rodando).

## Continuar, listar, cancelar

```bash
dispatch run --resume <id> --background - <<< "ajuste X"  # mesma sessão, agente, modelo e worktree
dispatch list [--status RUNNING] [--limit N]               # histórico
dispatch cancel <id>                                        # cancela run em andamento
dispatch recover <id>                                      # ORPHANED: commita trabalho preservado, sem verificação
dispatch verify <id>                                       # re-roda os checks (ex.: depois de recover)
dispatch clean <id> [--force]                               # descarta o worktree
```

Prefira `--resume` a um run novo quando o trabalho é continuação: o agente mantém o contexto.

## Subagents (perfis nomeados)

Perfis em `~/.dispatch/agents.json` (e `<repo>/.dispatch/agents.json`, que vence) fixam papel, tier,
`readonly`, `timeout` e instruções. Use por papel em vez de repetir o brief:

```bash
dispatch subagents                 # lista os perfis efetivos
dispatch run --as reviewer --dir <repo> --background - <<< "revise o branch X"
```

Flags explícitas (`--tier`, `--readonly`, `--timeout`) vencem o perfil. `prefer`/`avoid` são **pesos**
no ranking, não filtros: um agente evitado ainda roda se for o único com folga.

`run` e perfis aceitam `--provider`, `--model`, `--effort low|medium|high|xhigh`, `--agent` (flags vencem o perfil).
Cada filtro restringe o fallback; sem candidato, falha. `--relax` permite voltar ao tier e registra `note`.
Tier é opcional com provider/model; se informado, vale a interseção. Effort incompatível é recusado.
`dispatch route [mesmos filtros] [--json]` explica candidatos/eliminações/vencedor usando quota em cache, sem executar.
O resumo expõe `requested`/`resolved`; `stats --by requested|resolved` agrega esses campos.
`--priority speed|balanced|quality` (padrão `balanced`; a flag vence o `priority` do perfil e o de `~/.dispatch/config.json`) só muda pesos dentro do conjunto elegível.
`speed` pondera p50 e folga de pace; `quality` pondera sucesso `verification=verified` e `strength`. Não fura tier, filtro, capability, cooldown nem readonly.
`tiers.json` aceita `speed` e `strength` (1–5; ausente = neutro). Com ≥5 amostras no mesmo tier a medição mistura com o prior (`n/(n+5)`); `route` mostra os termos e se o vencedor mudou.

## Flags úteis

- `--readonly`: revisão/leitura; o agente não edita arquivos. Use para revisores.
- `--timeout 10m`: limite do run (estoura como `TIMED_OUT`, sem fallback).
- `--no-worktree`: escreve direto no `--dir` (use só se um único escritor).
- `--no-verify`: pula os checks determinísticos depois de um run de escrita.
- `--dir`: repositório/diretório de trabalho (padrão: o atual).

## Regras

- **Sem delegação recursiva:** dentro de um run do dispatch o comando é recusado (`DISPATCH_DEPTH`).
- **Modelos Claude não rodam em CLIs de terceiros:** o dispatch os recusa.
- **Não reenvie igual:** `FAILED` com candidatos esgotados significa melhorar o brief (1–2 frases) ou
  subir um tier. `TIMED_OUT`/`CANCELLED` não fazem fallback.
- **Um escritor por diretório** e sem `push`, deploy ou segredos no brief.
- Não há fonte de quota para Cursor e Grok: o roteamento deles só aprende depois de um rate limit. `dispatch agents` mostra `confidence=empirical` e o risco (cooldown ou rate limit recente), nunca um percentual inventado.
- CLI `DEGRADED` ainda roda, com um note. Capability exigida e ausente (readonly, por exemplo) é recusada com erro claro.
- Permissões do agente (sandbox/allow-rules) valem: se um agente negar escrita, o run cai para o próximo.

## Diagnóstico

```bash
dispatch status          # status do dispatch (on/off), hook e skill
dispatch on / off        # liga/desliga injeção de contexto em novas sessões
dispatch hook install / uninstall # registra/remove hook SessionStart do Claude
dispatch agents          # CLIs instalados, com fonte e confiança da quota
dispatch doctor [--json] # versão, flags e capabilities (OK, DEGRADED, UNVERIFIED); não chama modelo
dispatch tiers           # tabela efetiva (edite ~/.dispatch/tiers.json após `dispatch tiers --init`)
dispatch tiers --check   # valida a tabela contra agentes e modelos instalados
dispatch subagents --check   # valida os perfis (tier, timeout, agentes citados)
dispatch stats --by model --since 7d
dispatch stats --capacity   # janelas de quota; sem snapshots a capacidade é desconhecida
```
