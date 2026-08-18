# internal/db

## Purpose
Cria e valida o pool de conexões com o PostgreSQL. Os demais pacotes recebem o
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

## Business logic / invariantes
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
`github.com/jackc/pgx/v5/pgxpool`. Importado por `cmd/scraper`; será importado
pelos pacotes de persistência conforme forem criados.
