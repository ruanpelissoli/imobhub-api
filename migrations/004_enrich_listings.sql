-- 004_enrich_listings.sql
--
-- Colunas de enriquecimento em `listings`. São o resultado do processamento
-- (normalização, geocodificação e deduplicação) aplicado sobre os campos "_raw"
-- já coletados. Os "_raw" permanecem intocados: eles são o dado de origem e
-- permitem reprocessar tudo quando a regra de normalização mudar, sem raspar os
-- sites de novo.
--
-- Todas nullable e sem backfill: os anúncios já coletados continuam válidos e
-- simplesmente ainda não passaram pelo enriquecimento.

ALTER TABLE listings
    -- Bairro normalizado a partir de address_raw (o bruto varia por portal:
    -- "Jd. Botânico", "JARDIM BOTANICO", ...). É por ele que se agrupa região.
    ADD COLUMN IF NOT EXISTS normalized_neighborhood TEXT,
    -- bedrooms_raw é texto livre ("3 quartos", "3"); aqui vira número. NULL
    -- quando o anúncio não informa — 0 significaria "sem quarto".
    ADD COLUMN IF NOT EXISTS bedroom_count           INTEGER,
    -- Comodidades extraídas da descrição. Sem default: coluna adicionada em
    -- tabela já populada, e NULL aqui é honesto ("não enriquecido ainda").
    -- Ao ler, trate NULL e vazio da mesma forma, como em image_urls.
    ADD COLUMN IF NOT EXISTS amenities               TEXT[],
    -- Geocodificação do endereço deste anúncio. Fica no listing (e não só em
    -- properties) porque é a entrada da deduplicação: dois anúncios só viram o
    -- mesmo imóvel depois de comparadas as coordenadas.
    ADD COLUMN IF NOT EXISTS lat                     DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS lng                     DOUBLE PRECISION,
    -- Imóvel canônico ao qual este anúncio foi associado. NULL = ainda não
    -- deduplicado. ON DELETE SET NULL é proposital: apagar um `property`
    -- (correção de deduplicação errada, por exemplo) **não pode** apagar os
    -- anúncios brutos coletados, que são o dado de origem e não se recuperam
    -- sem nova raspagem. A constraint recebe o nome automático
    -- listings_property_id_fkey.
    ADD COLUMN IF NOT EXISTS property_id             UUID REFERENCES properties(id) ON DELETE SET NULL,
    -- Marcador de quando o enriquecimento rodou. NULL = nunca enriquecido: é o
    -- filtro que a fila de enriquecimento usa para escolher o que processar, e
    -- comparar com updated_at diz se o anúncio mudou depois do último passe.
    ADD COLUMN IF NOT EXISTS enriched_at             TIMESTAMPTZ;

-- Suporta "todos os anúncios deste imóvel" (a consulta de comparação de preços
-- entre portais) e a recontagem de properties.active_listing_count.
CREATE INDEX IF NOT EXISTS idx_listings_property_id ON listings (property_id);
