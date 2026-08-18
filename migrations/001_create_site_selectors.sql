-- 001_create_site_selectors.sql
--
-- Seletores CSS por domínio, descobertos e mantidos pela IA. Uma linha por
-- domínio: é o "manual de leitura" que o scraper consulta antes de raspar um
-- site. Quando a extração falha, o registro é marcado como 'broken' e a IA é
-- acionada para redescobrir os seletores.

CREATE TABLE IF NOT EXISTS site_selectors (
    id                BIGSERIAL PRIMARY KEY,
    -- Host normalizado (sem esquema e sem barra final), ex.: "www.exemplo.com.br".
    domain            TEXT NOT NULL UNIQUE,
    -- Objeto com as chaves: listing_container, title, price, address,
    -- description, image, listing_url. JSONB (e não colunas) porque a IA pode
    -- devolver seletores compostos ou com fallbacks que variam por site.
    selectors         JSONB NOT NULL,
    -- 'static'   = basta o HTML devolvido pelo GET;
    -- 'headless' = a listagem só existe após execução de JavaScript.
    render_mode       TEXT NOT NULL DEFAULT 'static',
    -- 'valid'  = extraiu itens na última execução;
    -- 'broken' = falhou e precisa de redescoberta pela IA.
    status            TEXT NOT NULL DEFAULT 'valid',
    -- NULL enquanto os seletores nunca tiverem sido exercitados numa coleta.
    last_validated_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Sem trigger: quem faz UPDATE é responsável por setar updated_at = NOW().
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT site_selectors_render_mode_check
        CHECK (render_mode IN ('static', 'headless')),
    CONSTRAINT site_selectors_status_check
        CHECK (status IN ('valid', 'broken')),
    -- Impede que um retorno malformado da IA (array, string, número) seja
    -- gravado onde o código espera um objeto de seletores.
    CONSTRAINT site_selectors_selectors_is_object_check
        CHECK (jsonb_typeof(selectors) = 'object')
);

-- O UNIQUE em domain já cria um índice implícito; este índice nomeado existe
-- por ser exigido explicitamente pelo schema acordado para o milestone e para
-- que o nome usado em planos de query não dependa da convenção do PostgreSQL.
CREATE INDEX IF NOT EXISTS idx_site_selectors_domain ON site_selectors (domain);
