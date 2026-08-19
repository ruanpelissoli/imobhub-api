# migrations/

## Purpose
Schema do PostgreSQL em SQL puro, aplicado no startup do binário por
`db.RunMigrations` (ver `internal/db/migrate.go`). Não há ORM no projeto: estas
tabelas são o contrato que todas as queries `pgx` assumem.

## Convenções (obrigatórias)
- Nome do arquivo: `NNN_descricao.sql`. O prefixo numérico é a **versão**
  gravada em `schema_migrations`; a descrição é livre. Um arquivo fora desse
  padrão faz o startup falhar — de propósito, para não divergir em silêncio.
- **Migrations são imutáveis depois de aplicadas.** Editar um arquivo já
  registrado não o reaplica (a versão já consta em `schema_migrations`); o banco
  de produção ficaria diferente de um banco novo. Corrija sempre com um arquivo
  novo.
- Renomear a *descrição* de um arquivo é seguro (a versão é só o prefixo);
  mudar o *prefixo* faz a migration rodar de novo.
- Escreva statements idempotentes (`IF NOT EXISTS`) — barato e torna a
  recuperação manual de um banco parcialmente migrado trivial.
- O runner aplica **qualquer versão ausente**, mesmo menor que a última já
  aplicada. Duas branches que criam `003_*.sql` em paralelo vão aplicar as duas
  em ordens diferentes conforme o banco: renumere no merge.

## Business logic
- `site_selectors`: uma linha por domínio, com os seletores CSS descobertos pela
  IA em `selectors` (JSONB, chaves `listing_container`, `title`, `price`,
  `address`, `description`, `image`, `listing_url`). JSONB e não colunas porque
  a IA pode devolver seletores compostos/com fallback que variam por site.
  `render_mode` decide entre HTTP simples (`static`) e navegador (`headless`);
  `status = 'broken'` sinaliza que os seletores precisam de redescoberta.
- `listings`: anúncios **brutos**. Os campos `_raw` guardam o texto extraído sem
  normalização, para permitir reprocessamento sem nova raspagem.
- `listings.last_seen_at` é o mecanismo de remoção de anúncios sumidos: cada run
  guarda seu timestamp de início e, ao final, apaga os listings **daquele
  domínio** com `last_seen_at` anterior a ele. Consequência: todo upsert de
  coleta **precisa** atualizar `last_seen_at`, senão anúncios vivos são
  apagados.
- `UNIQUE (source_domain, listing_url)` (`listings_source_domain_listing_url_key`)
  é a identidade do anúncio e o alvo do `ON CONFLICT` nos upserts.
- `properties`: **registro canônico do imóvel**. Relação 1:N com `listings` —
  o mesmo imóvel físico é anunciado em vários portais, cada anúncio é uma linha
  em `listings` e todos apontam para uma linha em `properties`. `listings`
  guarda o bruto de cada fonte; `properties`, a versão consolidada.
- `properties.transaction_type` e `property_type` são `TEXT` **livre de
  propósito**: o vocabulário válido ainda não foi definido. CHECK/enum entram
  quando os valores estiverem fechados — restringir antes disso derrubaria a
  coleta na primeira variação inesperada.
- **Não existe chave de deduplicação em `properties`** (nenhum UNIQUE além da
  PK), também de propósito: a regra de matching é decidida na task de
  deduplicação. Uma chave inventada agora rejeitaria imóveis legítimos.
- `properties.active_listing_count` é **denormalizado**: quem altera
  `listings.property_id` é responsável por mantê-lo coerente. Existe para a
  listagem não fazer um `COUNT` em `listings` a cada consulta.
- Colunas de enriquecimento em `listings` (`normalized_neighborhood`,
  `bedroom_count`, `amenities`, `lat`, `lng`, `property_id`, `enriched_at`) são
  derivadas dos campos `_raw` — que continuam intocados, para permitir
  reprocessar quando a normalização mudar. Todas nullable e sem backfill.

## Gotchas
- **`updated_at` não tem trigger.** É `NOT NULL DEFAULT NOW()` apenas no INSERT;
  todo `UPDATE` precisa setar `updated_at = NOW()` explicitamente. Escolha
  consciente: sem trigger, o comportamento fica visível na query.
- `idx_site_selectors_domain` é redundante com o índice implícito do `UNIQUE`
  em `domain`. Mantido por exigência explícita do schema acordado no milestone.
- Tudo roda numa **única transação**: `CREATE INDEX CONCURRENTLY`, `VACUUM` e
  afins não funcionam aqui. Se algum dia forem necessários, o runner precisará
  de um modo fora de transação.
- `image_urls TEXT[]` e `extra_data JSONB` têm default `'{}'` mas aceitam NULL —
  ao ler, trate NULL e vazio da mesma forma. Vale igual para
  `properties.amenities`/`photos` (default `'{}'`) e para `listings.amenities`
  (sem default, adicionada em tabela já populada).
- **Nunca crie arquivos `.down.sql`.** Não há conceito de rollback no runner, e
  `003_x.down.sql` casaria com o regex de nome com a **mesma versão** de
  `003_x.sql` — versão duplicada faz o startup falhar. Correção sempre em
  arquivo novo.
- `listings.property_id` usa `ON DELETE SET NULL` **intencionalmente**: apagar
  um `property` não pode apagar os anúncios brutos coletados, que são o dado de
  origem e não se recuperam sem nova raspagem.
- `idx_properties_lat_lng` é um **btree composto, não um índice geoespacial**
  (PostGIS não faz parte da stack). Serve para filtro por faixa de
  latitude/longitude; busca por raio exigirá outro índice.
- `listings.enriched_at IS NULL` = anúncio pendente de enriquecimento. É o
  filtro da fila; comparar com `updated_at` diz se o anúncio mudou desde o
  último passe. **Isso só funciona porque `upsertListingSQL` passou a bumpar
  `updated_at` condicionalmente** (`IS DISTINCT FROM` campo a campo): com o
  `NOW()` incondicional que existia antes, o catálogo inteiro voltava à fila
  todo dia. Ver `internal/db/CLAUDE.md`.
- **O índice da fila é `idx_listings_enrichment_queue` (`006`), não o
  `idx_listings_pending_enrichment` do `005`.** O do `005` era
  `(id) WHERE enriched_at IS NULL` e **nunca seria usado**: o PostgreSQL só
  aproveita um índice parcial quando consegue provar que o resultado da query
  satisfaz o predicado do índice, e o `OR` da fila
  (`enriched_at IS NULL OR updated_at > enriched_at`) torna essa prova
  impossível — o segundo ramo não tem índice, não há BitmapOr a montar e o
  planner cai num scan de `listings_pkey` com filtro. Era custo de manutenção na
  tabela mais quente do schema sem ganho de leitura. O `006` derruba aquele e
  cria um cujo predicado é **idêntico** ao da query, o que torna a implicação
  trivial. O comentário dentro do arquivo `005` está superado; ele não pode ser
  editado (migrations são imutáveis) e a correção vive no `006`.
- **O predicado de `idx_listings_enrichment_queue` precisa continuar idêntico ao
  WHERE de `selectPendingListingsSQL`.** Se um dos dois mudar sozinho, o índice
  deixa de ser usado **em silêncio**: a query continua correta e volta a varrer a
  tabela inteira. `TestEnrichmentQueueIndexPredicateMatchesTheQuery`
  (`internal/db`) compara os dois textos justamente para flagrar isso.
- **Medido** em PostgreSQL 17, com 50 mil anúncios e 200 pendentes: com o índice
  do `005`, o plano é `Index Scan using listings_pkey` + `Filter`, descartando
  49.792 linhas (12,4 ms) — ou seja, o parcial é ignorado, como previsto. Com o
  do `006`, `Index Only Scan using idx_listings_enrichment_queue` (0,08 ms). A
  diferença não é constante: o `005` custa proporcional ao **total** de anúncios,
  o `006` ao número de **pendentes**.
- Comparação de `timestamptz` é `IMMUTABLE`, então ela é aceita num predicado de
  índice parcial. O que **não** seria aceito é `now()` ou um cast dependente de
  fuso — se o predicado da fila algum dia virar "enriquecido nos últimos N dias",
  ele não pode ir para um índice.
