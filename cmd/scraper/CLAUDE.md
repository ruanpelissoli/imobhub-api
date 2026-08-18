# cmd/scraper

## Purpose
Ponto de entrada do coletor. Responsável apenas por: configurar o logger, ler a
configuração, abrir o pool do banco e (futuramente) disparar a coleta. Nenhuma
regra de negócio mora aqui — ela vive em `internal/`.

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

- **O `ratelimit.DomainLimiter` é criado aqui, uma vez só.** O espaçamento por
  domínio só existe se todas as requisições passarem pelo mesmo limiter: dois
  limiters teriam relógios independentes e dobrariam a carga na fonte. Por isso
  ele nasce em `run` e será injetado no pipeline, em vez de ser construído dentro
  de cada componente.

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

## Gotchas
- Este binário ainda não faz coleta: ele sobe, valida config e conexão, aplica
  as migrations, loga e encerra com sucesso. É o suficiente para
  `go build ./...` e para um healthcheck de deploy, mas não confunda "processo
  saiu com 0" com "coletou alguma coisa".
- Como cada execução aplica migrations, subir o binário **altera o schema**. Não
  aponte um processo de versão antiga para um banco já migrado por uma versão
  nova esperando que ele reverta: não há `down`.

## Dependencies
`internal/config`, `internal/db`, `internal/ratelimit`. Deve permanecer fino — se lógica começar a se
acumular aqui, mova para um pacote em `internal/`.
