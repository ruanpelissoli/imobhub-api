-- 007_properties_search_indexes.sql
--
-- Índices de busca em `properties`, o registro canônico do imóvel e a fonte da
-- busca com filtros. Até aqui a tabela tinha um único índice
-- (`idx_properties_lat_lng`, o pré-filtro de bounding box de
-- `FindPropertiesByCoordinates`): qualquer filtro por tipo de transação, tipo de
-- imóvel, localização, atributos numéricos ou ordenação por data caía em
-- sequential scan.
--
-- ⚠️ Estes índices **antecipam** a busca: no momento em que este arquivo foi
-- escrito não existe nenhum handler HTTP nem função em
-- `internal/db/properties_repo.go` que use esses filtros. É exatamente o cenário
-- que produziu o bug do 005 — um índice mantido em toda escrita e nunca usado
-- pelo planner. Aqui o risco é aceitável porque `properties` é escrita só no
-- enriquecimento (uma linha por imóvel canônico), e não em toda coleta como
-- `listings`. **Quando a query de busca for implementada, revalide cada índice
-- abaixo com `EXPLAIN (ANALYZE, BUFFERS)` contra o WHERE real** e corrija o que
-- não for usado — em arquivo novo, como o 006 corrigiu o 005.
--
-- Sem CONCURRENTLY: o runner aplica todas as migrations pendentes numa única
-- transação protegida por advisory lock (ver internal/db/CLAUDE.md), e
-- `CREATE INDEX CONCURRENTLY` não roda dentro de transação — usá-lo derrubaria o
-- startup. Sem rollback também: não há `.down.sql` no runner (um `007_x.down.sql`
-- casaria com o regex de nome com a **mesma versão** deste arquivo e faria o
-- startup falhar por versão duplicada). Correção de índice é sempre arquivo novo.
--
-- **Índice de preço ficou de fora de propósito, não por esquecimento.**
-- `properties` não tem coluna de preço; o único dado de preço no schema é
-- `listings.price_raw TEXT`, texto bruto não normalizado ("R$ 450.000", "450
-- mil"), sobre o qual nenhum índice de faixa é possível. Criar a coluna
-- normalizada envolve decisões que não cabem numa migration de índices (tipo
-- numérico, moeda, venda x aluguel x condomínio, backfill a partir dos `_raw`).
-- Follow-up: o índice de preço nasce junto da migration que introduzir a coluna
-- normalizada em `properties`.

-- Filtros combinados da busca (venda/aluguel + tipo + cidade + bairro).
-- ⚠️ Btree composto: o PostgreSQL só o aproveita quando a **coluna líder**
-- (`transaction_type`) está no filtro. Uma busca que omita `transaction_type` e
-- filtre só por `city` volta a fazer sequential scan. A ordem é da coluna
-- presente em todo recorte (venda x aluguel é o primeiro corte de qualquer
-- busca) para a mais específica; quem reordenar as colunas, ou fizer a busca
-- deixar `transaction_type` opcional, precisa saber que isso invalida o índice.
CREATE INDEX IF NOT EXISTS idx_properties_search_filters
    ON properties (transaction_type, property_type, city, neighborhood);

-- Filtros numéricos da busca (quartos, banheiros, vagas, área).
-- ⚠️ Mesma regra de coluna líder: só é usado com `bedroom_count` no filtro.
-- As quatro colunas são nullable e NULL entra no btree — engorda o índice, sem
-- impacto de correção. Não é um índice **parcial** de propósito: o predicado
-- teria de ser provado equivalente ao WHERE da busca, que ainda não existe, e é
-- justamente essa prova que faltou no 005.
CREATE INDEX IF NOT EXISTS idx_properties_attributes
    ON properties (bedroom_count, bathroom_count, parking_spots, area_sqm);

-- Busca por comodidades. `amenities` é `TEXT[]`, então o opclass default do GIN
-- (`array_ops`) já atende `@>` e `&&` — não há ramo `jsonb`/`jsonb_path_ops` a
-- decidir aqui. O GIN não indexa arrays NULL: como a convenção do projeto é ler
-- NULL e `'{}'` da mesma forma, isso é comportamento esperado, não bug.
CREATE INDEX IF NOT EXISTS idx_properties_amenities
    ON properties USING GIN (amenities);

-- Ordenação por mais recentes (`ORDER BY created_at DESC LIMIT n`). Um btree de
-- coluna única já é percorrido nos dois sentidos, então o `DESC` é redundante —
-- mantido só para explicitar a intenção. Um só índice, sem gêmeo ASC.
CREATE INDEX IF NOT EXISTS idx_properties_created_at
    ON properties (created_at DESC);
