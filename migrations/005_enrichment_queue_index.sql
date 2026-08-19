-- 005_enrichment_queue_index.sql
--
-- Índice da fila de enriquecimento (internal/enrichqueue). A fila lê
-- `WHERE (enriched_at IS NULL OR updated_at > enriched_at) AND id > $1
--  ORDER BY id LIMIT $2` uma vez por lote, ao final de cada coleta diária.
--
-- O índice é **parcial** e cobre apenas o ramo `enriched_at IS NULL` — o caso
-- dominante (todo anúncio recém-coletado) e o único indexável dos dois. O ramo
-- `updated_at > enriched_at` compara duas colunas da mesma linha, e nenhum
-- btree o atende: só um índice de expressão sobre `(updated_at - enriched_at)`
-- ou uma coluna gerada resolveriam, e ambos custam manutenção em toda escrita
-- da tabela mais quente do schema.
--
-- É por isso que a correção do `ON CONFLICT` de `upsertListingSQL` (bumpar
-- `updated_at` só quando algum campo coletado mudou de fato) é obrigatória e não
-- opcional: sem ela, o segundo ramo casaria com o catálogo inteiro todo dia e
-- nenhum índice salvaria a query.
--
-- Sem CONCURRENTLY: o runner aplica todas as migrations pendentes numa única
-- transação (ver internal/db/CLAUDE.md).

CREATE INDEX IF NOT EXISTS idx_listings_pending_enrichment
    ON listings (id) WHERE enriched_at IS NULL;
