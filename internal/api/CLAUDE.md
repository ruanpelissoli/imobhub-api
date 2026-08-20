# internal/api

## Purpose
Monta o servidor HTTP: roteador (`/api/v1` + `/health`), os middlewares base
(recovery, request id, logging, CORS, envelope JSON de erro), o ciclo de vida do
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
- **Ordem da cadeia, de fora para dentro: `recovery → requestID → logging →
  cors → jsonErrors → mux`.** Cada posição é deliberada:
  - `recovery` é o mais externo para que um panic em qualquer outro middleware
    ainda vire 500 em vez de derrubar a conexão;
  - `requestID` fica **entre** os dois: dentro do `recovery` (para que um panic
    nele continue virando 500) e fora do `logging` (para que toda linha de log
    já nasça com o id). É middleware próprio, e não parte do `logging`, porque
    produzir identidade é **gerar estado**; o `logging` continua só observando;
  - `logging` vem abaixo para registrar o status que o `recovery` escreveu;
  - `cors` precisa responder o preflight **antes** do mux — `OPTIONS /api/v1/x`
    numa rota inexistente viraria 404 se descesse;
  - `jsonErrors` é o mais interno porque só existe para reescrever o que o
    próprio mux emite.
- **`request_id` é herdado-e-validado ou gerado.** O `X-Request-Id` da
  requisição só é aceito com **≤64 chars em `[A-Za-z0-9_-]`**; qualquer outra
  coisa (vazio, longo demais, espaço, `/`, `\n`) é descartada **em silêncio** e
  substituída por um UUID v4 de `crypto/rand` — sem dependência externa no
  `go.mod`. Nem 400 nem log de erro: é input não confiável que termina num
  header de resposta e numa linha de log, e responder erro só daria ao cliente
  um canal barato de flood. O valor é escrito no header de resposta **antes** do
  handler downstream, para existir mesmo se ele entrar em panic, e propagado por
  `context.Context` com chave de tipo não-exportado (`ctxKeyRequestID`), lida por
  `requestIDFromContext` (devolve `""` quando ausente).
- **`remote_addr` é o `r.RemoteAddr` cru.** Não interpretamos `X-Forwarded-For`
  nem `X-Real-IP`: confiar neles sem um proxy confiável configurado é spoofing de
  graça — o cliente escolhe o IP que vai para o log. Tratar proxy é outra task.
- **`quietPaths` rebaixa `/health` (e `/metrics`, quando existir) para `Debug`.**
  O handler de `cmd/api` nasce em `Info`, então o liveness do orquestrador
  simplesmente some do log de produção sem precisar de um `LOG_LEVEL` — que
  arrastaria `internal/config`, que este pacote não importa. `/metrics` já está
  no conjunto mesmo sem a rota existir, para não exigir edição quando nascer. O
  casamento é por path **exato**: prefixo pegaria um `/healthz` de terceiros por
  acidente.
- **O `request_id` do log de panic vem do header, não do contexto.** O `recovery`
  é externo ao `requestID`, então o `r` capturado pelo `defer` dele é o de fora —
  sem o valor no contexto. A fonte é `w.Header().Get(headerRequestID)`, que o
  `requestID` já setou.
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

## `GET /api/v1/properties` — busca (`properties_search_handler.go`, `properties_cache.go`)
- **A busca reusa `propertyResponse`, o DTO do imóvel**, embutindo-o em
  `propertyListItem` — mesma técnica de `propertyDetailResponse`. Um DTO próprio
  para a listagem entregaria `bedroom_count` numa rota e `bedrooms` na outra para
  a mesma entidade, que é exatamente o que a decisão do detalhe evita.
- **`active_listing_count` só aparece na busca.** O detalhe o omite de propósito
  (contador denormalizado ao lado de `listings` vira contradição visível); a
  listagem não tem lista de anúncios com que contradizer, e "3 anúncios" é o que
  faz o usuário abrir o imóvel. Por isso ele mora em `propertyListItem`, não em
  `propertyResponse`. `updated_at`, ao contrário, é campo da entidade e ficou no
  DTO compartilhado — pô-lo só na listagem seria a divergência que a regra do DTO
  único proíbe.
- **`data` nunca é `null`**, mesmo sem resultados (`[]`): o front não deveria ter
  que tratar dois formatos para "lista vazia". Vale o mesmo para
  `amenities`/`photos`, via `emptyIfNil`.
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
  `Access-Control-Expose-Headers: X-Cache, X-Request-Id` para origens permitidas
  — sem isso o browser esconde os dois headers: o front não consegue nem medir o
  hit rate, nem citar o `request_id` num relato de erro, que é a razão de
  devolvê-lo. Header novo que o front precise ler entra em `corsExposeHeaders`.
- **Seams declarados no consumidor** (`propertySearcher`, `cacheStore`), mesmo
  padrão de `grouping.PropertyStore`: o wiring de produção fecha sobre
  `deps.Pool`/`deps.Redis` em `registerV1Routes` e os testes injetam closures e um
  fake, sem Postgres nem Redis.

## `GET /api/v1/properties/{id}` — detalhe (`properties_handler.go`)
- **É a tela de comparação entre portais**: o imóvel canônico mais **todos** os
  anúncios vinculados, cada um com seu preço e a URL de origem.
- **`propertyResponse` é O DTO do imóvel.** Toda rota que devolver imóvel
  (detalhe, busca, geo) monta esse mesmo struct — dois mapeamentos JSON para a
  mesma entidade divergiriam no primeiro campo novo. `newPropertyDetailResponse`
  é pura de propósito: é ela que os testes cobrem sem banco. Campo novo do imóvel
  entra **nele**, não num struct por rota.
- **Nenhum acesso ao Redis neste endpoint**, nem leitura nem escrita, e é
  decisão, não esquecimento: servir preço obsoleto numa tela cujo propósito é
  comparar preços é o pior defeito possível dela. Duas queries por requisição é
  o preço aceito.
- **A resposta não tem `price`.** O schema não tem preço canônico em
  `properties`; o único dado de preço é `listings.price_raw`, **texto bruto**
  ("R$ 450.000", "450 mil"), exposto por anúncio e sem nenhum parsing aqui. A
  coluna normalizada (e um `price` no imóvel) nasce em outra task.
- **A fonte do anúncio é `source_domain`, não nome comercial.** Nome de fachada
  da imobiliária não existe em lugar nenhum do schema.
- **`active_listing_count` não é exposto.** O contador é denormalizado e pode
  divergir de `len(listings)` em bases anteriores à IMO-22; publicar os dois
  lado a lado colocaria uma contradição visível na tela. A verdade da resposta
  são as linhas realmente lidas.
- **Nullable vira `null`, array vazio vira `[]`.** Campos consolidados são
  ponteiros no DTO (NULL ≠ `0`/`""` — a ausência é informação); `amenities`,
  `photos` e `listings` passam por `emptyIfNil` e nunca saem como `null`. Imóvel
  existente mas nunca consolidado é `200` com tudo `null`, não erro; imóvel sem
  anúncios é `200` com `"listings": []`, não 404.
- **Timestamps saem em RFC 3339 UTC** (`formatTimestamp`): sem a conversão, o
  offset da timezone da conexão vazaria para o contrato.
- **`{id}` e `{$}` são registrados juntos.** `/api/v1/properties/` não casa com
  `{id}` (o wildcard exige segmento não-vazio) e cairia no 404 genérico do mux,
  quando o correto é `400` — o cliente mandou um id, ele é que está em branco.
  O `301` que o mux emitia em `GET /api/v1/properties` (sem barra) por causa
  desses padrões **acabou**: o padrão exato da busca tem precedência e ganha dele.
  `TestPropertiesWithoutTrailingSlashIsTheSearchRoute` é o teste que pina isso.
- **Id malformado é `400`, não `500`.** A validação não é feita em Go: o handler
  reage a `db.ErrInvalidPropertyID`, que é a tradução do SQLSTATE `22P02` do
  PostgreSQL (ver `internal/db/CLAUDE.md` para o porquê de não validar formato
  de UUID aqui).
- **`context.Canceled` (cliente desconectou) é logado em `Debug`**, não em
  `Error`: não é falha da aplicação e não deve poluir o alerta de erro.

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
- **Os testes deste pacote não abrem conexão.** Só os caminhos que não tocam o
  pool são exercitados de ponta a ponta (`400` de id em branco, `405`, `404` de
  path aninhado, o redirect); `Deps.Pool` é `*pgxpool.Pool` concreto, sem
  interface para fakear, então `200`/`404`/`500` reais ficam para o QA. O
  mapeamento do DTO é coberto pela função pura.
- **Um panic não produz linha do `logging`, só a do `recovery`.** O `logging`
  loga **depois** do `next.ServeHTTP` e não usa `defer`, então o unwinding passa
  através dele sem emitir nada — a requisição gera apenas o `Error` do
  `recovery`, que por isso carrega `method`, `path` e `request_id` próprios. Não
  procure o 500 de panic nas linhas `api: request`.
- **Teste que espera linha `Debug` precisa de handler com
  `Level: slog.LevelDebug`.** `capturingHandler` embute o `slog.Handler` e
  delega o `Enabled` a ele: com `slog.NewJSONHandler(io.Discard, nil)` (default
  `Info`) um registro `Debug` é descartado **antes** de chegar ao `Handle`, e o
  teste veria zero linhas em vez de falhar por nível errado. Use o helper
  `newCapture(slog.LevelDebug)`.
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
