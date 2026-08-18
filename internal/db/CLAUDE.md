# internal/db

## Purpose
Cria e valida o pool de conexões com o PostgreSQL (`db.go`) e aplica as
migrations SQL no startup (`migrate.go`). Os demais pacotes recebem o
`*pgxpool.Pool` já pronto e executam suas queries a partir dele.

## Key decisions
- **`pgxpool` em vez de `database/sql`.** O scraper vai coletar várias fontes em
  paralelo; `pgxpool` dá controle explícito sobre o pool e acesso aos tipos
  nativos do PostgreSQL (arrays, JSONB, `numeric`) que `database/sql` achata em
  `driver.Value`.
- **`Ping` na inicialização.** Sem ele, uma `DATABASE_URL` errada só apareceria
  na primeira query, possivelmente minutos depois de o processo ter subido
  "com sucesso". Falhar no boot é mais fácil de diagnosticar.
- **`pingTimeout` de 5s.** `pgxpool.NewWithConfig` não conecta de verdade (o
  pool é preguiçoso), então o `Ping` é a primeira ida à rede. Sem timeout, um
  host inalcançável travaria o startup até o timeout do sistema operacional.

## Migrations (`migrate.go`)
- **Runner próprio em vez de golang-migrate/goose.** O projeto já depende de
  `pgx` e precisa de exatamente uma coisa: aplicar `.sql` em ordem. Uma
  dependência a mais custaria mais do que as ~150 linhas aqui, e o runner
  externo traria seu próprio CLI, sua tabela e seu formato de arquivo.
- **Uma única transação para todas as pendentes**, com
  `pg_advisory_xact_lock`. Duas consequências deliberadas: várias instâncias
  subindo juntas não aplicam o mesmo arquivo em paralelo, e uma falha no meio
  deixa o banco no estado anterior em vez de meio migrado. O lock de
  *transação* (e não de sessão) evita vazar o lock numa conexão devolvida ao
  pool. **Custo:** `CREATE INDEX CONCURRENTLY` e afins não podem ser usados nas
  migrations.
- **Versão = prefixo numérico do arquivo**, gravada em
  `schema_migrations(version, name, applied_at)`. Ordenação é numérica, não
  lexicográfica (`9` antes de `10`), e renomear a descrição não reaplica nada.
- **Nome fora do padrão `NNN_descricao.sql` é erro, não arquivo ignorado.** Um
  arquivo ignorado em silêncio nunca rodaria e o schema divergiria sem aviso.
  Pelo mesmo motivo, diretório sem nenhum `.sql` retorna `ErrNoMigrations` —
  normalmente é working directory errado no container.
- `tx.Exec` **sem argumentos** usa o protocolo simples do PostgreSQL, que aceita
  vários statements num só comando. É o que permite `CREATE TABLE` +
  `CREATE INDEX` no mesmo arquivo — se algum dia um parâmetro `$1` for passado
  junto, o arquivo passa a ter que ter um statement só.
- O diretório vem de `config.MigrationsDir` (`MIGRATIONS_DIR`, default
  `migrations`), então o caminho é **relativo ao working directory** do
  processo: uma imagem Docker precisa copiar `migrations/` junto do binário.

## Business logic / invariantes
- `RunMigrations` é idempotente e roda **antes** do pipeline em `main`.
- `Connect` fecha o pool antes de retornar erro. Consequência para o chamador:
  **só chame `defer pool.Close()` quando `err == nil`** — o contrato é "ou você
  recebe um pool válido, ou nada foi deixado aberto".
- O `ctx` passado governa o ciclo de vida do pool (cancelá-lo encerra conexões
  ociosas), **não** apenas a criação. Em `main` passamos o contexto de sinais,
  para que SIGTERM encerre as conexões ordenadamente.

## Gotchas
- **Nunca inclua a `DATABASE_URL` numa mensagem de erro** — ela contém a senha e
  vazaria para os logs. Por isso o erro de `Ping` cita apenas `Host` e
  `Database` extraídos do config parseado, e o erro de parsing não ecoa a URL.
- Parâmetros de pool (`MaxConns`, `MinConns`, `MaxConnLifetime`) ainda estão nos
  defaults do pgx. Quando houver medição de carga real, configure-os em
  `cfg` antes do `NewWithConfig` — a `DATABASE_URL` também aceita alguns deles
  como query params (`pool_max_conns`).

## Dependencies
`github.com/jackc/pgx/v5` e `pgxpool`. Importado por `cmd/scraper`; será
importado pelos pacotes de persistência conforme forem criados. Os arquivos de
schema ficam em `migrations/` na raiz — ver o CLAUDE.md de lá para as regras das
tabelas `site_selectors` e `listings`.
