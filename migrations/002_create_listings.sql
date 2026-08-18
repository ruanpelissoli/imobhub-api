-- 002_create_listings.sql
--
-- Anúncios brutos coletados dos sites. Os campos "_raw" guardam exatamente o
-- texto extraído do HTML, sem normalização: a limpeza/parsing acontece depois,
-- e manter o original permite reprocessar sem raspar tudo de novo.

CREATE TABLE IF NOT EXISTS listings (
    id               BIGSERIAL PRIMARY KEY,
    -- Mesmo formato de site_selectors.domain (host normalizado).
    source_domain    TEXT NOT NULL,
    -- URL absoluta do anúncio; junto com source_domain forma a identidade.
    listing_url      TEXT NOT NULL,
    title_raw        TEXT,
    price_raw        TEXT,
    address_raw      TEXT,
    description_raw  TEXT,
    bedrooms_raw     TEXT,
    area_raw         TEXT,
    image_urls       TEXT[] DEFAULT '{}',
    -- Campos extras específicos do site que não cabem nas colunas acima.
    extra_data       JSONB DEFAULT '{}',
    -- Atualizado a cada vez que o anúncio é visto numa coleta. É a chave da
    -- remoção de anúncios sumidos: ao final de um run, apagam-se os listings do
    -- domínio cujo last_seen_at seja anterior ao início daquele run.
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Sem trigger: quem faz UPDATE é responsável por setar updated_at = NOW().
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Alvo do ON CONFLICT nos upserts da coleta.
    CONSTRAINT listings_source_domain_listing_url_key
        UNIQUE (source_domain, listing_url)
);

-- Consultas por fonte (relatórios e a limpeza pós-run são sempre por domínio).
CREATE INDEX IF NOT EXISTS idx_listings_source_domain ON listings (source_domain);
-- Suporta o DELETE ... WHERE last_seen_at < $inicio_do_run.
CREATE INDEX IF NOT EXISTS idx_listings_last_seen_at ON listings (last_seen_at);
