# internal/api

## Purpose
Monta o servidor HTTP: roteador (`/api/v1` + `/health`), os middlewares base
(recovery, logging, CORS, envelope JSON de erro), o ciclo de vida do
`http.Server` (timeouts + shutdown gracioso) e os handlers de negócio — hoje
`GET /api/v1/properties` (busca paginada com cache Redis). `cmd/api` só
orquestra o boot.

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

## `GET /api/v1/properties` (`properties_handler.go`, `properties_cache.go`)
- **DTO próprio, em snake_case.** `db.Property` não tem tags `json`: serializá-la
  direto exporia `CanonicalAddress`/`BedroomCount` em PascalCase e amarraria o
  contrato público à forma interna do struct — renomear um campo no banco
  quebraria o front sem nada quebrar aqui. Ponteiros nulos saem como `null`;
  `amenities`/`photos` e `data` **nunca** saem `null` (o front não deveria ter
  que tratar dois formatos para "lista vazia").
- **Validação de formato aqui, normalização de faixa no repositório.** O handler
  rejeita com 400 o que não é número (`page=abc`) e o que é negativo num
  **filtro** (`min_bedrooms=-1`, `min_area=-2`). `page`/`page_size` fora da faixa
  **não** são 400: `db.EffectivePropertyPagination` normaliza
  (`page<=0`→1, `page_size<=0`→20, `>50`→50, `page>1e6`→1e6) e a resposta ecoa os
  valores **efetivos**. É decisão registrada em `internal/db/CLAUDE.md`:
  devolver a primeira página é mais útil que um erro para `?page=0`. Corolário:
  `page=-2` é normalizado, não rejeitado.
- **A paginação efetiva vem de `db.EffectivePropertyPagination`, nunca replicada
  aqui.** O envelope e a chave de cache precisam exatamente dos números que o
  `LIMIT/OFFSET` usou; copiar as constantes faria a API anunciar 20 no dia em que
  o teto mudasse no repositório, sem nada quebrar.
- **`min_price`/`max_price` presentes → 400**, não ignorados em silêncio.
  `properties` não tem coluna de preço; ignorá-los devolveria o catálogo inteiro
  **parecendo** filtrado por preço, que é o pior resultado possível para o front.
  Eles nascem junto da migration que criar a coluna normalizada.
- **Chave de cache: `properties:search:v1:<sha256 dos params normalizados>`.** O
  `v1` versiona o **formato do payload**, não a API: mudar o DTO sem trocá-lo
  serviria o shape antigo por até um TTL inteiro para clientes que já esperam o
  novo. Normalização antes do hash — trim nas strings, paginação efetiva, e
  amenities sem vazios, deduplicadas e **ordenadas** — é o que faz
  `?amenities=a,b` e `?amenities=b,a&amenities=b` compartilharem a entrada. O
  hash tem tamanho fixo, então nenhum valor do usuário vaza para dentro da chave.
- **TTL de 5 min é constante do pacote, não variável de ambiente**, porque
  `internal/api` não importa `internal/config` (ver Dependencies).
- **O que vai para o Redis é o corpo do 200 já serializado**, não `[]db.Property`:
  é o que garante que hit e miss respondam byte a byte igual. Por isso o caminho
  de sucesso usa `json.Marshal` + `writeJSONBytes`, e não `writeJSON` (o
  `json.Encoder` acrescenta `\n`).
- **Só 200 é cacheado.** Um 400 ou 500 gravado serviria o erro por um TTL inteiro.
- **Falha de Redis é transparente:** indisponibilidade, timeout ou valor ilegível
  (`json.Valid` falha) viram `logger.Warn` + miss, e a requisição responde 200
  pelo banco. Nunca 500. `deps.Redis == nil` é o mesmo caminho — é assim que os
  testes do pacote rodam sem Redis.
- **`propertyCacheOpTimeout` de 200ms, derivado de `r.Context()`.** Sem ele um
  Redis lento consumiria o `WriteTimeout` de 30s do servidor e o cache passaria a
  piorar exatamente a latência que existe para melhorar.
- **`X-Cache: HIT|MISS` em toda resposta 200**, e o `cors` emite
  `Access-Control-Expose-Headers: X-Cache` para origens permitidas — sem isso o
  browser esconde o header e o front não consegue nem medir o hit rate.
- **Seams declarados no consumidor** (`propertySearcher`, `cacheStore`), mesmo
  padrão de `grouping.PropertyStore`: o wiring de produção fecha sobre
  `deps.Pool`/`deps.Redis` em `registerV1Routes` e os testes injetam closures e um
  fake, sem Postgres nem Redis.

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
- **`newPropertyCache(nil)` devolve `nil` de propósito.** Guardar um
  `*redis.Client` nil dentro da interface `cacheStore` produziria uma interface
  **não-nil** e um panic na primeira operação. A checagem de nil mora no wiring,
  antes da atribuição; um `*propertyCache` nil é o caminho "sem cache".
- **Os testes de `NewRouter` montam `Deps` com `Pool` nil.** A rota de properties
  agora está registrada, mas nenhum deles a chama de verdade — um
  `GET /api/v1/properties` com pool nil entraria em panic e viraria 500 pelo
  `recovery`.
- **Mudou o shape do DTO de resposta? Troque o `v1` da chave de cache.** Sem
  isso, entradas gravadas com o formato antigo continuam sendo servidas por até
  5 minutos.
