# cmd/api

## Purpose
Ponto de entrada do servidor HTTP que serve o catálogo consolidado
(`properties`) para o front. Responsável apenas pelo boot: logger, configuração,
pool do Postgres, client do Redis, montagem do roteador e ciclo de vida do
`http.Server`. Nenhuma rota e nenhuma regra de negócio moram aqui — elas vivem
em `internal/api`.

## Key decisions
- **`main` delega para `run() error`.** Mesma razão de `cmd/scraper`: `os.Exit`
  não executa os `defer`, então `pool.Close()`/`redisClient.Close()` em `main`
  ficariam de fora de todo caminho de erro.
- **Erro de boot vai para `slog.Error` + `os.Exit(1)`, nunca panic.** Um panic
  imprime stack em texto puro e quebra o formato JSON dos logs justamente no
  evento que o operador precisa ler.
- **A API não aplica migrations.** Quem é dono do schema é o scraper. Uma API de
  leitura que migra no boot alteraria o schema a cada deploy dela — e um rollback
  da API passaria a significar rollback de schema. Ela só valida a conexão (o
  `Ping` de `db.Connect` já faz isso).
- **`config.LoadAPI()`, não `config.Load()`.** Subir a API não pode exigir
  `ANTHROPIC_API_KEY`, `SOURCES_FILE` nem as variáveis de scraping/grouping.
  Afrouxar `Load()` resolveria o boot da API ao custo de o scraper subir sem
  chave e quebrar só na primeira chamada de IA — regressão silenciosa.
- **O listener é aberto explicitamente (`api.Listen`) antes de servir.** Porta em
  uso vira erro de boot com exit 1, e não uma falha assíncrona depois de o
  processo já ter logado "started".

## Business logic / invariantes
- Ordem de inicialização obrigatória: **config → Postgres → Redis → roteador →
  listener → servidor**. Falha em qualquer etapa impede o servidor de subir.
  Postgres vem antes do Redis pelo mesmo motivo do scraper: quando os dois estão
  fora, o erro que o operador vê é o do banco, que é o recurso indispensável.
- `defer pool.Close()` e `defer redisClient.Close()` só são registrados quando
  `err == nil` — `db.Connect` e `cache.New` fecham sozinhos em caso de erro.
- **`api.Serve` bloqueia**, então os dois `Close` rodam **depois** do
  `Shutdown`: as requisições em voo terminam com o pool ainda aberto. Inverter
  isso (fechar o pool antes do shutdown) faria as requisições em andamento
  falharem justamente durante um deploy.
- SIGINT/SIGTERM cancelam o contexto → `Shutdown` com timeout de 10s → exit 0.
  Timeout estourado é log de erro e exit 1.
- As dependências chegam aos handlers por `api.Deps`, passada por parâmetro.
  Nada de variável global: é o que permite testar handlers com um pool próprio.

## Dependencies
`internal/config` (`LoadAPI`), `internal/db` (`Connect`), `internal/cache`
(`New`) e `internal/api` (`NewRouter`, `Listen`, `NewServer`, `Serve`).
Deve permanecer fino — lógica nova vai para `internal/api`.

## Gotchas
- **O binário exige um banco no ar mesmo sem servir dados ainda.** `db.Connect`
  e `cache.New` fazem `Ping`/`PING` no boot; `docker compose up -d db redis` é
  pré-requisito de qualquer `go run ./cmd/api`.
- **O schema precisa existir.** Como a API não migra, um banco virgem faz os
  handlers falharem em runtime, não no boot. Rode o scraper (ou aplique as
  migrations) pelo menos uma vez antes.
- No `docker-compose.yml`, este binário é o serviço **`api`**; o serviço do
  batch chama-se **`scraper`** (era `api` até esta task).
- O `PORT` do container é fixo em 8080; o que varia é a porta publicada no host
  (`API_PORT`).
