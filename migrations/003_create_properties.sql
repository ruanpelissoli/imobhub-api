-- 003_create_properties.sql
--
-- Registro canônico do imóvel. Um mesmo imóvel físico costuma ser anunciado em
-- vários portais ao mesmo tempo: cada anúncio vira uma linha em `listings`, mas
-- todos apontam para uma única linha aqui. Enquanto `listings` guarda o dado
-- bruto de cada fonte, `properties` guarda a versão consolidada e já
-- normalizada (endereço, geolocalização, atributos), que é o que a busca e as
-- comparações de preço consultam.

CREATE TABLE IF NOT EXISTS properties (
    -- UUID (e não BIGSERIAL como em listings/site_selectors) porque este id é
    -- exposto e referenciado por registros vindos de fontes distintas: um id
    -- sequencial vazaria o volume do catálogo e dificultaria a geração do id
    -- antes do INSERT durante a deduplicação. gen_random_uuid() é nativo do
    -- PostgreSQL 13+, não precisa da extensão pgcrypto.
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Endereço consolidado a partir dos address_raw dos anúncios. TEXT livre:
    -- os portais brasileiros variam demais para um formato estruturado agora.
    canonical_address    TEXT,
    -- Bairro/cidade/UF já normalizados (o equivalente bruto fica em listings).
    neighborhood         TEXT,
    city                 TEXT,
    state                TEXT,
    -- Resultado da geocodificação. NULL enquanto o imóvel não foi geocodificado
    -- — nem todo endereço bruto é resolvível.
    lat                  DOUBLE PRECISION,
    lng                  DOUBLE PRECISION,

    -- Atributos físicos consolidados. Todos nullable: o anúncio de origem pode
    -- simplesmente não informar, e 0 significaria "sem quarto", não "não sei".
    bedroom_count        INTEGER,
    bathroom_count       INTEGER,
    parking_spots        INTEGER,
    -- Área em metros quadrados. DOUBLE PRECISION porque os portais publicam
    -- valores fracionários (ex.: 72,5 m²).
    area_sqm             DOUBLE PRECISION,
    -- União das comodidades citadas nos anúncios (piscina, elevador, etc.).
    -- Default '{}' mas aceita NULL: ao ler, trate NULL e vazio igual.
    amenities            TEXT[] DEFAULT '{}',
    description          TEXT,
    -- URLs das fotos, agregadas dos anúncios. Mesma regra de NULL/vazio.
    photos               TEXT[] DEFAULT '{}',

    -- 'venda'/'aluguel' e 'apartamento'/'casa'/... — deixados como TEXT **livre
    -- de propósito**: o vocabulário válido ainda não foi definido. CHECK ou
    -- enum entram numa migration futura, quando os valores estiverem fechados;
    -- restringir agora bloquearia a coleta na primeira variação inesperada.
    transaction_type     TEXT,
    property_type        TEXT,

    -- Contador denormalizado de anúncios ativos apontando para este imóvel.
    -- Existe para a listagem ordenar/filtrar sem um COUNT em listings a cada
    -- consulta. NOT NULL DEFAULT 0 porque "sem anúncio ativo" é 0, nunca
    -- desconhecido. Consequência: quem altera listings.property_id é
    -- responsável por manter este número coerente.
    active_listing_count INTEGER NOT NULL DEFAULT 0,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Sem trigger: quem faz UPDATE é responsável por setar updated_at = NOW().
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()

    -- Nenhuma constraint UNIQUE de deduplicação aqui de propósito: a regra de
    -- matching (que combinação de endereço/geo/atributos identifica o mesmo
    -- imóvel) é decidida na task de deduplicação. Inventar uma chave agora
    -- rejeitaria imóveis legítimos e teria de ser desfeita com outra migration.
);

-- Btree composto, **não** geoespacial: PostGIS não faz parte da stack. Serve
-- para filtro por faixa (lat BETWEEN ... AND lng BETWEEN ...), que é o recorte
-- por região usado hoje. Busca por raio real é assunto de outra task, e exigirá
-- PostGIS ou um índice diferente.
CREATE INDEX IF NOT EXISTS idx_properties_lat_lng ON properties (lat, lng);
