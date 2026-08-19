# cmd/scraper

## Purpose
Ponto de entrada do coletor. Responsável apenas por: configurar o logger, ler a
flag `-only`, ler a configuração, abrir o pool do banco, aplicar as migrations e
disparar as etapas (`scraper.RunPipeline` e `enrichqueue.RunEnrichment`).
Nenhuma regra de negócio mora aqui — ela vive em `internal/`.

## Key decisions
- **`main` delega para `run() error`.** `os.Exit` **não** executa os `defer`
  registrados; se o `pool.Close()` estivesse em `main` junto com um
  `os.Exit(1)`, o pool ficaria aberto em todo caminho de erro. Com o `run`
  separado, `main` só decide o exit code depois que todos os defers rodaram.
- **`slog` com handler JSON em stdout.** Logs estruturados desde o início, para
  serem consumíveis por qualquer agregador sem parsing por regex. Regra do
  projeto: nada de `fmt.Println` para log operacional.
- **Erro de inicialização vai para `slog.Error` + `os.Exit(1)`, não panic.** Um
  panic imprime stack trace em texto puro, quebrando o formato JSON dos logs
  justamente no evento que o operador mais precisa ler.
- **`signal.NotifyContext` com SIGINT/SIGTERM.** O contexto cancelado propaga
  para o pool do banco e, no futuro, para as coletas em andamento — encerramento
  ordenado em vez de kill abrupto. SIGTERM é o sinal que orquestradores de
  container enviam.

- **A montagem dos módulos de coleta saiu daqui.** O `ratelimit.DomainLimiter`,
  o `robots.Checker`, os clientes HTTP e o serviço de seletores nascem em
  `scraper.NewPipeline`: a assinatura exigida `RunPipeline(ctx, cfg, pool)` não
  tem por onde recebê-los prontos, e escolher esses colaboradores é regra de
  coleta, que este pacote não guarda. A invariante do limiter continua valendo —
  ele é **um por run**, compartilhado por todas as fontes, porque dois limiters
  teriam relógios independentes e dobrariam a carga na fonte.

- **A flag `-only` é dispatch de CLI, não regra de negócio.** Valores:
  `scrape` (só a coleta), `enrich` (só a fila de enriquecimento) e `all`
  (default: coleta e, **em caso de sucesso**, enriquecimento). Valor
  desconhecido é erro de boot, nunca fallback silencioso — quem digitou
  `-only=enrichment` esperava só o enriquecimento e receberia uma coleta
  completa sem aviso. O wiring de cada etapa vive no pacote correspondente
  (`scraper.NewPipeline`, `enrichqueue.NewPipeline`), não aqui.

## Business logic / invariantes
- Ordem de inicialização é obrigatória: **config → db → migrations → coleta**. A
  config valida as variáveis obrigatórias, o `db.Connect` valida a conectividade
  com um `Ping` e o `db.RunMigrations` garante que o schema esperado existe;
  todos falham no boot em vez de na primeira query.
- `db.RunMigrations` recebe `cfg.MigrationsDir` (`MIGRATIONS_DIR`, default
  `migrations`), resolvido a partir do **working directory** do processo — rode
  o binário da raiz do repositório, ou copie `migrations/` ao lado dele na
  imagem.
- `defer pool.Close()` só é registrado quando `db.Connect` retorna `err == nil`
  — o contrato de `Connect` é fechar o pool sozinho em caso de erro.

- **Em `-only=all`, o enriquecimento só roda se a coleta tiver dado certo.**
  Enriquecer sobre uma coleta interrompida no meio processaria um catálogo que
  ainda vai mudar no mesmo dia, gastando IA duas vezes.

## Gotchas
- **Exit code 0 não significa "coletou alguma coisa".** `RunPipeline` só devolve
  erro em falha fatal (config, banco, arquivo de fontes ilegível, SIGTERM); uma
  fonte que falhou vira log de erro e uma linha no resumo, sem mudar o exit code
  — foi decisão de projeto para que um portal fora do ar não marque o run
  inteiro como falho. Quem monitora deve ler o resumo (`failed`, `succeeded`),
  não só o código de saída. O mesmo vale para o enriquecimento: um anúncio que
  falha vira log e uma linha no resumo, e volta à fila no run seguinte.
- **Exit code 1 depois de uma coleta bem-sucedida não significa dado perdido.**
  Se o enriquecimento falhar em `-only=all`, a coleta já foi persistida — o log
  de erro diz isso explicitamente, e os anúncios sem `enriched_at` são
  reprocessados no próximo run.
- `go run ./cmd/scraper` executa a coleta completa: exige `DATABASE_URL`,
  `ANTHROPIC_API_KEY` e — para as fontes `headless` — Chrome/Chromium no PATH.
- Como cada execução aplica migrations, subir o binário **altera o schema**. Não
  aponte um processo de versão antiga para um banco já migrado por uma versão
  nova esperando que ele reverta: não há `down`.

## Dependencies
`internal/config`, `internal/db`, `internal/scraper` e `internal/enrichqueue`.
Deve permanecer fino — se lógica começar a se acumular aqui, mova para um pacote
em `internal/`.
