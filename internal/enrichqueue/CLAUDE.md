# internal/enrichqueue

## Purpose
O orquestrador assíncrono do enriquecimento: drena a fila de anúncios pendentes
(`listings.enriched_at`) e, por anúncio, encadeia normalização de bairro →
extração de termos → geocodificação → **persistência** → agrupamento canônico →
consolidação → carimbo. Roda ao final de cada coleta diária
(`cmd/scraper -only=all`) e pode rodar sozinho (`-only=enrich`).

É o chamador que faltava para `internal/enrichment` e `internal/grouping`: os
dois nasceram desconectados e este pacote é o plumbing **compartilhado** que os
liga — não duplique nada disso por enricher.

## Key decisions
- **Pacote próprio, não `internal/enrichment`.** Aquele pacote não importa `db`
  nem `ai` por decisão registrada em `internal/CLAUDE.md`; a fila precisa dos
  dois. O precedente é `internal/grouping`, criado pelo mesmo motivo.
- **Molde de `scraper.Pipeline`**: dependências como campos de função em `Deps`,
  `New` rejeitando campo nulo, `NewPipeline(cfg, pool)` com o wiring de produção
  e `RunEnrichment(ctx, cfg, pool)` como fachada para o binário. O ganho é o
  teste: um closure por campo, sem PostgreSQL, sem rede e sem gastar token.
- **Worker pool com canal + `sync.WaitGroup`, não `errgroup`.** `golang.org/x/sync`
  é dependência **indireta** e promovê-la se pagaria mal aqui: a semântica do
  `errgroup` é cancelar tudo no primeiro erro, exatamente o oposto do requisito
  ("falha de um anúncio não interrompe os demais").
- **Duas funções de escrita em `db`, não uma com flag.** `UpdateListingEnrichment`
  e `MarkListingEnriched` são momentos distintos do fluxo (etapas 4 e 7); um
  booleano permitiria carimbar cedo e o anúncio sairia da fila sem ser agrupado.
- **Drenagem por lotes com cursor keyset (`id > afterID`).** Um anúncio que falha
  não recebe `enriched_at` e continua casando com o predicado: sem o cursor, o
  `LIMIT` devolveria o mesmo lote para sempre.

## Business logic / invariantes
- **A ordem das sete etapas é invariante de correção.** `MergePropertyData` relê
  `bedroom_count`, `amenities`, `image_urls` e `description_raw` **do banco**
  (`db.ListListingsByPropertyID`), então persistir (4) **antes** de agrupar (5) e
  consolidar (6) é obrigatório — invertido, o canônico consolidaria NULL.
- **Uma única instância de cada colaborador**, compartilhada por todos os
  workers (ver `NewPipeline`). Obrigatório: o `Geocoder` carrega o
  `ratelimit.DomainLimiter` (1 req/s do Nominatim) e o cache com negative
  caching — N geocoders seriam N req/s e bloqueio do User-Agent. Mesma
  invariante do "um `DomainLimiter` por run" em `scraper`.
- **`city`/`state` vão sempre vazios**: `listings` não tem essas colunas (ver
  `migrations/004`). Os parâmetros existem para a assinatura não mudar quando o
  parsing de endereço evoluir.
- **Bairro não reconhecido grava NULL, nunca `""`** — contrato do normalizador.
- Tabela de decisão dos sentinelas (todos existem para isto):

  | Situação | Persiste campos | Agrupa/merge | Carimba `enriched_at` | Contador |
  |---|---|---|---|---|
  | Caminho feliz | sim | sim | **sim** | `Enriched` |
  | `enrichment.ErrAddressNotFound` / `ErrEmptyAddress` | sim, `lat`/`lng` NULL | não | **sim** | `Skipped` |
  | Rede/timeout/status do geocoder | **não** | não | **não** | `Failed` |
  | `grouping.ErrListingNotGeocoded` | sim | não | sim | `Skipped` |
  | Erro da IA em `GroupListing` | sim | — | **não** | `Failed` |
  | Erro em `MergeProperty` | sim | vínculo já gravado | **não** | `Failed` |
  | `ctx` cancelado | — | — | não | nenhum (`outcomeAborted`) |

  As duas linhas que não são óbvias: endereço irresolvível **é carimbado** porque
  o geocoder faz negative caching e reperguntar não muda o resultado; erro
  **transitório não persiste nada** porque um `UPDATE` gravaria `lat`/`lng` NULL
  por cima de um geocode válido de um passe anterior.
- **Erro só é fatal em dois casos**: leitura da fila impossível e contexto
  cancelado. Anúncio que falha vira `slog.Error` com `listing_id`/`source_domain`
  e a fila segue.
- **Cancelamento encerra o pool ordenadamente**: o canal é sempre fechado e o
  `Wait` sempre acontece antes do return, inclusive no caminho de cancelamento —
  nenhuma goroutine vaza.
- O resumo final segue o formato do `logSummary` do scraper: mensagem humana
  (`enriquecimento concluído: N anúncios enfileirados (...)`) mais os atributos
  estruturados `queued`/`enriched`/`skipped`/`failed`/`workers`.

## Dependencies
`internal/config` (`EnrichmentWorkers`, `AmenitiesFile`, as variáveis de
geocodificação e de agrupamento), `internal/db` (`PendingListing`,
`ListingEnrichment`, `ListPendingListings`, `UpdateListingEnrichment`,
`MarkListingEnriched`), `internal/enrichment` (normalizador, extrator,
geocoder e as sentinelas), `internal/grouping` (`GroupListing`,
`MergePropertyData`, `NewPoolStore`), `internal/ai` (o matcher) e `pgxpool`.
Consumido por `cmd/scraper`, que só chama `RunEnrichment`.

## Gotchas
- **O ganho da concorrência é assimétrico, e isso não é bug.** A geocodificação
  serializa em 1 req/s global pelo `DomainLimiter` compartilhado; quem realmente
  paraleliza são as chamadas de IA do agrupamento — que são **pagas por anúncio**.
  Subir `ENRICHMENT_WORKERS` aumenta o custo por unidade de tempo, não o total.
- **A correção do `updated_at` em `upsertListingSQL` é o que segura o custo
  desta fila.** Se ela for revertida, o predicado `updated_at > enriched_at`
  casa com o catálogo inteiro todo dia e o passe pago de IA se repete
  diariamente. Ver `internal/db/CLAUDE.md`.
- **Janela de *lost update* no merge** (já documentada em `grouping/CLAUDE.md`):
  dois workers podem consolidar o mesmo `property` concorrentemente. Tolerável
  porque o merge é idempotente e reexecutável pela fila; fechá-la exigiria
  `SELECT ... FOR UPDATE` dentro de uma função de `db`.
- **Dois workers podem criar dois canônicos para o mesmo imóvel** se ambos
  buscarem candidatos antes de qualquer property existir. É a mesma classe de
  corrida acima, aceita pelo mesmo motivo; a correção é
  `grouping.HandleListingRemoval`, não mexer aqui. Por isso o teste de
  integração roda com `Workers: 1`.
- O teste de integração é pulado com `-short` ou sem `DATABASE_URL`, e usa o
  Postgres do `docker-compose.yml` (serviço `db`). Não há testcontainers — a
  ausência é decisão registrada em `internal/db/CLAUDE.md`.
