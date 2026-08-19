# internal/ — organização dos pacotes

## Purpose
Todo o código do ImobHub API vive sob `internal/` para que nada seja importável
por outros módulos: este é um binário, não uma biblioteca. `cmd/scraper` só
orquestra; a lógica está aqui.

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
cmd/scraper → config, db, scraper
scraper     → config, db, ratelimit, robots, selectors, sources, pgxpool
selectors   → ai, db, httpclient
ai          → db          (SelectorFields e as constantes de render mode)
robots      → net/http (client próprio, timeout de 10s para o robots.txt)
enrichment  → ratelimit, net/http (client próprio, timeout de 10s), stdlib + x/text
```

`scraper` chega a `httpclient` **através** de `selectors.StaticFetcher`/
`HeadlessFetcher`: os adaptadores já fecham sobre o User-Agent, e reusá-los
garante que a página analisada pela IA e a página coletada sejam buscadas do
mesmo jeito.

`robots` **não** importa `httpclient`: precisa de um timeout mais curto e o
grafo prevê o sentido contrário. O User-Agent chega como string (de `config` ou
de `httpclient.Client.UserAgent()`).

`config` é a única folha do grafo — não importa nada do projeto.

`enrichment` **era** folha e deixou de ser com o geocoder: ele fala com o
Nominatim e reusa `ratelimit.DomainLimiter` (espaçamento fixo por host, sem
burst, cancelável — exatamente a política de 1 req/s do Nominatim) em vez de um
`time.Ticker` próprio. A aresta `enrichment → ratelimit` é acíclica e rasa. O
pacote continua **sem tocar banco e sem IA**: o normalizador de bairros segue
puro e carrega sua tabela de aliases embutida com `//go:embed`, para não virar
mais uma variável de ambiente nem mais um `COPY` no Dockerfile.

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
- `enrichment/` entrega hoje o normalizador de bairros
  (`NeighborhoodNormalizer.Normalize`) e o geocoder (`Geocoder.Geocode`, via
  Nominatim), e **nenhum dos dois tem chamador**. Persistir os campos —
  `normalized_neighborhood`, `lat`/`lng`, e depois `bedroom_count`/`amenities` —
  com fila por `enriched_at IS NULL`, função de UPDATE em `db` e wiring em
  `cmd/scraper` é task de follow-up **compartilhada**. Não duplicar esse
  plumbing por enricher. Em particular, o filtro "anúncio já geocodificado"
  (`lat IS NULL`) é da fila, não do geocoder: `enrichment` não importa `db`.
- Logs operacionais usam `log/slog` (handler JSON configurado em `main`). Não
  usar `fmt.Println`/`log.Printf`: quebra o parsing dos logs em produção.
- Erros são embrulhados com `fmt.Errorf("pacote: ... %w", err)` e sempre citam o
  pacote de origem. Nunca inclua a `DATABASE_URL` completa numa mensagem — ela
  carrega a senha (ver `db/db.go`).
