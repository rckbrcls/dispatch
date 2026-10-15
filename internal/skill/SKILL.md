---
name: dispatch
description: Delega uma tarefa a outro agente de código (Codex, Cursor, Grok, Antigravity) escolhendo por dificuldade (tier easy|normal|hard) em vez de modelo, e devolve um resultado compacto. Use quando quiser economizar tokens do orquestrador ou paralelizar trabalho barato — implementação localizada, testes, revisão, leitura de código.
---

# dispatch

CLI que roteia uma tarefa para um agente instalado, por **tier**, e devolve resultado compacto.
Você não escolhe CLI nem modelo: o `dispatch` escolhe pela tabela de tiers e pela folga de quota,
e faz fallback se um agente falhar.

## Uso

```bash
dispatch run --tier easy|normal|hard [--dir <repo>] [--readonly] [--timeout 10m] - <<'BRIEF'
<brief: objetivo, o que pode/não pode tocar, comando de verificação, relatório final em ≤ 10 linhas>
BRIEF
```

- `easy`: tarefa clara e localizada, leitura, testes simples. `normal`: feature em alguns arquivos.
  `hard`: arquitetura, código sensível (auth, dinheiro, dados), bug difícil.
- `--readonly` para revisão/leitura: o agente não edita arquivos.
- Em repo git, runs de escrita rodam num **worktree** próprio (branch `dispatch/<id>`) e são commitados ao
  fim; o seu checkout não é tocado. `--no-worktree` escreve direto em `--dir`.
- Por padrão o run é **síncrono** e pode levar minutos. Para não bloquear: `--background` devolve o `id`
  na hora, e `dispatch wait <id>` espera o fim.
- Brief com **caminhos, nunca código colado**.

## Resultado

O `run` imprime JSON: `id`, `status` (`COMPLETED|FAILED|TIMED_OUT|CANCELLED`), `agent`, `model`,
`summary` (primeiras ~40 linhas), `truncated`, `totalLines`, `error`.
Se `truncated` for `true`, busque o resto sob demanda:

```bash
dispatch result <id> --offset 40 --limit 100
dispatch result <id> --section meta       # metadados sem o output
dispatch result <id> --section attempts   # fallbacks e erros
```

Exit code: 0 ok, 124 timeout, 1 demais falhas (`wait`: 3 se ainda rodando).

## Depois do run

```bash
dispatch run --resume <id> - <<< "ajuste X"   # continua a sessão (mesmo agente/modelo/worktree)
dispatch merge <id>                           # integra a branch do worktree e limpa
dispatch clean <id> [--force]                 # descarta o worktree
dispatch list [--status RUNNING]              # histórico
dispatch cancel <id>                          # cancela run em andamento
```

## Regras

- Não delegue recursivamente: dentro de um run do dispatch o comando é recusado (`DISPATCH_DEPTH`).
- Um escritor por diretório: com worktree isso é automático; com `--no-worktree` duas escritas no mesmo `--dir` conflitam.
- O worktree nasce do último commit: mudanças não commitadas no checkout não entram (o campo `note` avisa).
- Não leia o código que o agente escreveu na íntegra; confira `git diff --stat` e rode os testes.
- `FAILED` com todos os candidatos esgotados: melhore o brief ou suba um tier, não reenvie igual.

## Diagnóstico

```bash
dispatch agents          # CLIs instalados
dispatch tiers           # tabela efetiva (edite ~/.dispatch/tiers.json após `dispatch tiers --init`)
dispatch tiers --check   # valida a tabela contra agentes e modelos instalados
```
