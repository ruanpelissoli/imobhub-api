# internal/api

## Purpose
Monta o servidor HTTP: roteador (`/api/v1` + `/health`), os middlewares base
(recovery, logging, CORS, envelope JSON de erro) e o ciclo de vida do
`http.Server` (timeouts + shutdown gracioso). `cmd/api` só orquestra o boot.

## Key decisions
- **Roteador é o `net/http.ServeMux` da stdlib, não `chi`.** Desde o Go 1.22 o
  mux roteia por método e por path com wildcards (`GET /api/v1/properties/{id}`),
  que é tudo o que este projeto precisa. Vale a mesma regra do runner de
  migrations próprio (ver `internal/db/CLAUDE.md`): não adicionar dependência
  quando a stdlib resolve. Se algum dia faltar algo (sub-routers com middleware
  por grupo, por exemplo), a troca é local a este pacote — mas aí `chi` passa a
  ser o roteador **de todo o projeto**, não de um handler só.
- **Ordem da cadeia, de fora para dentro: `recovery → logging → cors →
  jsonErrors → mux`.** Cada posição é deliberada:
  - `recovery` é o mais externo para que um panic em qualquer outro middleware
    ainda vire 500 em vez de derrubar a conexão;
  - `logging` vem logo abaixo para registrar o status que o `recovery` escreveu
    (um 500 de panic aparece na linha de log como 500);
  - `cors` precisa responder o preflight **antes** do mux — `OPTIONS /api/v1/x`
    numa rota inexistente viraria 404 se descesse;
  - `jsonErrors` é o mais interno porque só existe para reescrever o que o
    próprio mux emite.
- **`wrapWriter` é idempotente.** `recovery` cria o `*responseWriter` e `logging`
  reaproveita o mesmo. Duas camadas de wrapper contariam bytes duas vezes e
  esconderiam o status real.
- **`jsonErrors` intercepta 404/405.** O `ServeMux` responde esses dois em
  `text/plain` ("404 page not found"), e o front receberia texto puro numa rota
  errada e JSON em todas as outras falhas. O discriminador é o `Content-Type`:
  handlers do projeto sempre setam `application/json` antes do `WriteHeader`, e
  só o erro cru do mux chega sem ele. O header `Allow` que o mux setou no 405 é
  preservado — só o corpo muda.
- **CORS ecoa a origem, não `*`.** A allowlist já é a política; `*` inviabiliza
  cookies/credenciais e trocar isso depois seria uma mudança silenciosa de
  segurança. `CORS_ORIGINS` vazia devolve o handler intacto: sem lista, sem CORS.
- **`/health` é liveness, não readiness.** Responde 200 sem tocar em Postgres ou
  Redis: um health que falha quando o banco pisca faz o orquestrador reiniciar
  uma API perfeitamente viva. Fica **fora** do `/api/v1` porque é infraestrutura,
  não contrato de produto, e não deve ser versionada com ele. Readiness com
  checagem de dependências é outra coisa e ainda não existe.
- **Timeouts explícitos.** O zero-value do `http.Server` é "sem limite": uma
  conexão que manda headers byte a byte seguraria um goroutine indefinidamente.
  Valores: `ReadHeaderTimeout 5s` (defesa contra slowloris), `ReadTimeout 15s`,
  `WriteTimeout 30s`, `IdleTimeout 60s`, `shutdownTimeout 10s`.
- **`Shutdown` usa `context.Background()`**, não o contexto já cancelado pelo
  sinal — passá-lo abortaria o shutdown na hora, o oposto de deixar as
  requisições em voo terminarem.

## Business logic / invariantes
- **Envelope de erro do projeto: `{"error":"mensagem"}` com
  `Content-Type: application/json`.** Todo handler novo responde falhas assim,
  via `writeError`.
- **Nada interno vaza para o corpo.** Erro do pgx, stack trace, `DATABASE_URL` e
  `REDIS_URL` (que carregam senha) ficam só no log. O handler escolhe a mensagem
  pública.
- `Deps` é passada por parâmetro até os handlers. **Nenhuma variável global.**
- `Deps.Logger` nulo cai em `slog.Default()` — é o que permite capturar as linhas
  de log nos testes injetando um handler próprio.

## Dependencies
Stdlib + `pgxpool` + `go-redis`. **Não importa `internal/config`**: recebe tudo
por `Deps`, como `internal/cache` recebe a `REDIS_URL` por parâmetro. Importado
por `cmd/api`.

## Gotchas
- **`registerV1Routes` é o ponto de extensão.** Rotas novas entram lá, com o
  padrão `"GET " + apiV1Prefix + "/properties"`. Registrar fora dela ignora a
  cadeia de middlewares.
- **Não escreva no `ResponseWriter` antes de decidir o status.** Se o handler já
  começou a responder, o `recovery` não consegue trocar a resposta por 500 —
  ele mantém o que foi enviado (trocar produziria uma resposta corrompida).
- **Um handler que setar `Content-Type` diferente de `application/json` num
  404/405 escapa do `jsonErrors`.** É intencional (permite servir outro formato
  no futuro), mas quebra o envelope se for acidental.
- `Vary: Origin` só é enviado para origens permitidas, conforme o critério de
  aceite. Se um proxy/CDN passar a cachear respostas da API, revise isso: sem
  `Vary` na resposta negada, o cache pode servir a resposta de uma origem para
  outra.
- `WriteTimeout` de 30s pode ficar curto para listagens grandes — revisar junto
  com a task de paginação.
