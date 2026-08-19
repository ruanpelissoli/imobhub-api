-- 006_fix_enrichment_queue_index.sql
--
-- Corrige o índice da fila de enriquecimento criado em 005. Migrations são
-- imutáveis, então a correção vem em arquivo novo.
--
-- O problema do 005: ele era `ON listings (id) WHERE enriched_at IS NULL`, mas a
-- query da fila filtra por
-- `(enriched_at IS NULL OR updated_at > enriched_at) AND id > $1`. O PostgreSQL
-- só usa um índice parcial quando consegue **provar** que todas as linhas do
-- resultado satisfazem o predicado do índice; com o `OR`, o segundo ramo não tem
-- índice algum, não há como montar um BitmapOr e o planner ignora o parcial —
-- caindo num scan de `listings_pkey` com filtro. Ou seja: custo de manutenção em
-- toda escrita da tabela mais quente do schema, sem nenhum ganho de leitura.
--
-- A correção é fazer o predicado do índice ser **exatamente** o predicado da
-- query. Aí a prova de implicação é trivial (as expressões são iguais) e o
-- índice passa a conter só as linhas pendentes — que, depois do primeiro run,
-- são uma fração pequena da tabela. O `ORDER BY id LIMIT n` é servido pela
-- própria chave indexada, sem sort.
--
-- ⚠️ O predicado abaixo precisa continuar idêntico ao WHERE de
-- `selectPendingListingsSQL` (internal/db/listings_repo.go). Se um dos dois
-- mudar sozinho, o índice para de ser usado **em silêncio** — a query continua
-- correta, só volta a varrer a tabela inteira. Há um teste de contrato em
-- internal/db que compara os dois textos.
--
-- Comparação de timestamptz é IMMUTABLE, então ela é aceita num predicado de
-- índice parcial (o que não seria aceito é `now()` ou um cast dependente de
-- fuso). Sem CONCURRENTLY: o runner aplica tudo numa única transação (ver
-- internal/db/CLAUDE.md).

DROP INDEX IF EXISTS idx_listings_pending_enrichment;

CREATE INDEX IF NOT EXISTS idx_listings_enrichment_queue
    ON listings (id)
    WHERE enriched_at IS NULL OR updated_at > enriched_at;
