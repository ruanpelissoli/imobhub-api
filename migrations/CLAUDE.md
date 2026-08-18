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
  ao ler, trate NULL e vazio da mesma forma.
