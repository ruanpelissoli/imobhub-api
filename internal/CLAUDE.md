# internal/ — organização dos pacotes

## Purpose
Todo o código do ImobHub API vive sob `internal/` para que nada seja importável
por outros módulos: este é um repositório de binários, não uma biblioteca.
`cmd/scraper` (batch de coleta) e `cmd/api` (servidor HTTP) só orquestram; a
lógica está aqui.

## Key decisions
- **Um pacote por responsabilidade técnica**, não por camada. `robots`,
  `ratelimit` e `httpclient` são separados porque cada um tem uma política
  própria (respeito ao robots.txt, espaçamento entre requisições, User-Agent) e
  precisa ser testável isoladamente.
- **`config` é a única fronteira com o ambiente.** Nenhum outro pacote chama
  `os.Getenv`. Isso mantém a lista de variáveis auditável em um lugar só e
  permite testar os demais pacotes sem mexer no ambiente.
- **`ai` envolve o SDK da Anthropic.** Os pacotes de negócio dependem de `ai`,
  nunca de `github.com/anthropics/anthropic-sdk-go` diretamente, para que trocar
  de modelo ou provedor seja uma mudança local.
- **`pgxpool` em vez de `database/sql`.** O scraper vai rodar coletas
  concorrentes; `pgxpool` dá controle direto sobre o pool e acesso aos tipos
  nativos do PostgreSQL, que `database/sql` esconde atrás de `driver.Value`.

## Dependencies
Grafo de importação atual (mantê-lo acíclico e raso):

```
cmd/scraper → config, db, cache, scraper, enrichqueue
cmd/api     → config, db, cache, api
api         → pgxpool, go-redis, stdlib   (NÃO importa config)
cache       → go-redis    (folha, como config e db)
scraper     → config, db, ratelimit, robots, selectors, sources, pgxpool
enrichqueue → ai, config, db, enrichment, grouping, pgxpool
selectors   → ai, db, httpclient
grouping    → ai, db, pgxpool
ai          → db          (SelectorFields, Property e as constantes de render mode)
robots      → net/http (client próprio, timeout de 10s para o robots.txt)
enrichment  → ratelimit, net/http (client próprio, timeout de 10s), stdlib + x/text + yaml/v4 (vocabulário de comodidades)
```

`enrichqueue` é o pacote mais "alto" do grafo: ele existe justamente para ser o
único lugar que enxerga `enrichment` **e** `db`/`ai` ao mesmo tempo, mantendo
`enrichment` sem banco e sem IA.

`scraper` chega a `httpclient` **através** de `selectors.StaticFetcher`/
`HeadlessFetcher`: os adaptadores já fecham sobre o User-Agent, e reusá-los
garante que a página analisada pela IA e a página coletada sejam buscadas do
mesmo jeito.

`robots` **não** importa `httpclient`: precisa de um timeout mais curto e o
grafo prevê o sentido contrário. O User-Agent chega como string (de `config` ou
de `httpclient.Client.UserAgent()`).

`api` **não importa `config`** de propósito: recebe pool, client do Redis,
allowlist de CORS e logger por `api.Deps`, montada em `cmd/api`. É a mesma regra
de `cache`, que recebe a `REDIS_URL` por parâmetro — quem lê o ambiente é só
`config`, e quem o repassa é só o `main`. O roteador é o `net/http.ServeMux` da
stdlib (nenhum `chi`/`gorilla` no `go.mod`); o porquê está em
`internal/api/CLAUDE.md`.

`config` e `cache` são folhas do grafo — não importam nada do projeto. `cache`
recebe a `REDIS_URL` por parâmetro (de `cfg.RedisURL`, em `main`) justamente
para continuar assim, e entrega **só** o `*redis.Client` validado por `Ping`:
set/get, TTL e desenho de chaves ficam em quem consumir o cache. O primeiro
consumidor é `api`, na busca de imóveis — chave, TTL e política de fallback
estão documentados em `internal/api/CLAUDE.md`, não aqui.

`enrichment` **era** folha e deixou de ser com o geocoder: ele fala com o
Nominatim e reusa `ratelimit.DomainLimiter` (espaçamento fixo por host, sem
burst, cancelável — exatamente a política de 1 req/s do Nominatim) em vez de um
`time.Ticker` próprio. A aresta `enrichment → ratelimit` é acíclica e rasa. O
pacote continua **sem tocar banco e sem IA**: o normalizador de bairros segue
puro e carrega sua tabela de aliases embutida com `//go:embed`; o vocabulário de
comodidades do `TermExtractor` chega por parâmetro de construtor (de
`config.AmenitiesFile`) via `yaml/v4`.

## Gotchas
- `selectors/` já está implementado (`SelectorService`: reuso da linha
  de `site_selectors` e descoberta via `ai` quando ela falta ou está quebrada) —
  o `doc.go` de lá agora só carrega o doc do pacote.
  `scraper/` está completo: extração (`ExtractListings`), sincronização
  (`SyncListings` — grava o que foi visto e apaga o que sumiu do site, **nunca**
  deletando quando a coleta devolveu zero anúncios) e a orquestração
  (`RunPipeline`), que é o que `cmd/scraper` executa. O wiring dos módulos de
  coleta vive em `scraper.NewPipeline`, não no `main`: a assinatura
  `RunPipeline(ctx, cfg, pool)` não recebe módulos prontos e o `main` não guarda
  regra de negócio. O `scraper.RenderHTML` que existia no scaffolding virou
  `httpclient.FetchHeadless` — busca de página (estática ou headless) é
  responsabilidade de `httpclient`. `sources/` já está implementado (`ReadSources`) — o `doc.go`
  dele deu lugar a `reader.go`, que carrega o doc do pacote.
- `grouping/` decide se um anúncio geocodificado é o mesmo imóvel de algum
  `property` canônico (`PropertyGrouper.GroupListing`, com `ai.MatchProperty`) e
  **consolida o canônico** a partir de todos os anúncios vinculados
  (`PropertyGrouper.MergePropertyData`: fotos, descrição, comodidades e quartos).
  `PropertyGrouper.HandleListingRemoval` é a operação inversa, **unitária**, para
  desfazer um agrupamento errado — a limpeza de fim de coleta não passa por ela
  (quem apaga os anúncios sumidos e as properties órfãs, na mesma transação, é
  `db.DeleteStaleListings`).
  Segue o padrão de `selectors` — orquestra `ai` + `db` — e **não** vive em
  `enrichment` justamente porque aquele pacote não importa `db` nem `ai`. As
  interfaces de acesso a dados são declaradas nele (consumidor); `db` continua
  sem struct de repositório. Quem chama `GroupListing` e, **depois**,
  `MergePropertyData` é `enrichqueue`.
- `enrichment/` entrega o normalizador de bairros
  (`NeighborhoodNormalizer.Normalize`), o geocoder (`Geocoder.Geocode`, via
  Nominatim) e o extrator de termos (`TermExtractor.Extract`, quartos +
  comodidades). O chamador dos três é `enrichqueue`, que também persiste
  `normalized_neighborhood`, `lat`/`lng`, `bedroom_count` e `amenities`. O
  filtro "anúncio já enriquecido" é da fila, não do geocoder: `enrichment` não
  importa `db`. O vocabulário do `TermExtractor` é carregado **uma vez** em
  `enrichqueue.NewPipeline` (`enrichment.NewTermExtractor(cfg.AmenitiesFile)`) e
  o extrator é injetado — não chame o loader por anúncio.
- `enrichqueue/` é o orquestrador assíncrono do enriquecimento: drena
  `listings` por `enriched_at IS NULL OR updated_at > enriched_at`, num worker
  pool de `ENRICHMENT_WORKERS`, e roda ao final de cada coleta
  (`cmd/scraper -only=all`). **A ordem das etapas é invariante**: os campos
  derivados são persistidos **antes** do agrupamento e da consolidação, porque
  `MergePropertyData` os relê do banco. Uma **única instância** de geocoder,
  extrator, normalizador e agrupador é compartilhada por todos os workers — o
  geocoder carrega o `DomainLimiter` de 1 req/s do Nominatim.
- `api/` é o servidor HTTP: roteador com o grupo `/api/v1` (o ponto de extensão
  é `registerV1Routes` — registrar fora dele ignora a cadeia de middlewares),
  `/health` de liveness fora do grupo, middlewares de recovery/logging/CORS e a
  conversão dos 404/405 do `ServeMux` para o envelope `{"error":"..."}`. Esse
  envelope é convenção do projeto para toda resposta de erro. A primeira rota de
  negócio é `GET /api/v1/properties` (busca paginada de `db.SearchProperties`
  com DTO próprio em snake_case e cache Redis de 5 min). O acesso a dados e ao
  Redis entra por seams declarados **no próprio `api`** (`propertySearcher`,
  `cacheStore`), como `grouping` faz — é o que mantém os testes do pacote sem
  Postgres e sem Redis.
- Logs operacionais usam `log/slog` (handler JSON configurado em `main`). Não
  usar `fmt.Println`/`log.Printf`: quebra o parsing dos logs em produção.
- Erros são embrulhados com `fmt.Errorf("pacote: ... %w", err)` e sempre citam o
  pacote de origem. Nunca inclua a `DATABASE_URL` completa numa mensagem — ela
  carrega a senha (ver `db/db.go`).
