# internal/db

## Purpose
Cria e valida o pool de conexões com o PostgreSQL (`db.go`), aplica as
migrations SQL no startup (`migrate.go`) e concentra o acesso às tabelas
`site_selectors` (`selectors_repo.go`) e `listings` (`listings_repo.go`). Os
structs em `models.go` são o contrato compartilhado com `ai`, `selectors` e
`scraper` — nenhum outro pacote monta SQL.

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

## Repositórios (`models.go`, `selectors_repo.go`, `listings_repo.go`)
- **`CountListings` usa `COUNT(*)` exato, não a estimativa de
  `pg_class.reltuples`.** O número vai para o resumo do run, que é o indicador
  acompanhado entre execuções: uma estimativa que oscila sem nada ter mudado
  destruiria a confiança nele. É uma query por run, sobre uma tabela na ordem de
  dezenas de milhares de linhas.
- **Funções livres recebendo `*pgxpool.Pool`**, sem struct `Repository` nem
  interface. Não há o que injetar: um pacote, um banco, nenhum estado por
  repositório. A interface pode nascer no consumidor no dia em que houver dois
  backends — criá-la agora seria abstração sem segundo caso.
- **JSONB decodificado em Go** (`decodeSelectors`) e não via `pgtype`: um JSON
  malformado vira um erro citando o domínio, em vez de um erro de scan opaco.
  Chaves desconhecidas são ignoradas de propósito — a IA pode devolver campos
  extras e derrubar a coleta por isso seria pior.
- **As tags `json` de `SelectorFields` são o formato gravado no banco.**
  Renomear qualquer uma invalida todas as linhas já persistidas (o teste de
  round trip existe para lembrar disso).
- **`UpsertSelectors` grava sempre `status='valid'` + `last_validated_at=NOW()`**
  e ignora `config.Status`: ela registra um sucesso de validação, não é uma
  gravação neutra. O caminho de falha é `MarkSelectorsBroken`, que preserva
  `last_validated_at` (continua sendo "a última vez que funcionou").
- **`render_mode` é normalizado antes do INSERT** (`normalizeRenderMode`); sem
  isso o valor errado só falharia na CHECK constraint, com uma mensagem que não
  diz qual valor foi enviado. Vazio vira `static`.
- **`UpsertListings` roda numa transação única, em lotes de
  `upsertBatchSize` via `pgx.Batch`.** A transação não é preciosismo: um upsert
  parcial deixaria anúncios vivos com `last_seen_at` antigo e
  `DeleteStaleListings` os apagaria. O lote existe só para diluir round-trips;
  `CopyFrom` não serve porque não faz `ON CONFLICT`.
- `image_urls` e `extra_data` nunca gravam NULL (nil vira `[]`/`{}`): a coleta
  não distingue "sem imagens" de "não sei", e o leitor não deveria ter que
  distinguir.

## Business logic / invariantes
- `RunMigrations` é idempotente e roda **antes** do pipeline em `main`.
- `Connect` fecha o pool antes de retornar erro. Consequência para o chamador:
  **só chame `defer pool.Close()` quando `err == nil`** — o contrato é "ou você
  recebe um pool válido, ou nada foi deixado aberto".
- O `ctx` passado governa o ciclo de vida do pool (cancelá-lo encerra conexões
  ociosas), **não** apenas a criação. Em `main` passamos o contexto de sinais,
  para que SIGTERM encerre as conexões ordenadamente.
- **Domínio sem seletores não é erro:** `GetSelectorsByDomain` devolve
  `(nil, nil)`. É o caso normal de uma fonte nova, e o chamador reage acionando
  a descoberta pela IA. Confundir isso com erro trava a primeira coleta.
- **`DeleteStaleListings` só pode rodar depois de uma coleta bem-sucedida** do
  domínio, com o `runStartedAt` lido **antes** do primeiro upsert. Depois de uma
  coleta interrompida no meio, ela apaga o resto do catálogo; com "agora" no
  lugar do início do run, apaga tudo.
- `(source_domain, listing_url)` e `domain` são as identidades usadas nos
  `ON CONFLICT`; ambos precisam chegar já **normalizados** (host sem esquema e
  sem barra final). Este pacote só faz `TrimSpace` — normalizar host é
  responsabilidade de quem coleta.

## Gotchas
- **Nunca inclua a `DATABASE_URL` numa mensagem de erro** — ela contém a senha e
  vazaria para os logs. Por isso o erro de `Ping` cita apenas `Host` e
  `Database` extraídos do config parseado, e o erro de parsing não ecoa a URL.
- Parâmetros de pool (`MaxConns`, `MinConns`, `MaxConnLifetime`) ainda estão nos
  defaults do pgx. Quando houver medição de carga real, configure-os em
  `cfg` antes do `NewWithConfig` — a `DATABASE_URL` também aceita alguns deles
  como query params (`pool_max_conns`).

- Os testes deste pacote **não tocam no banco**: cobrem só as funções puras
  (montagem de argumentos, validação, JSON). O comportamento do SQL — upsert,
  `ON CONFLICT`, contagem do DELETE — depende de um PostgreSQL real e fica para
  o QA / testes de integração.
- `MarkSelectorsBroken` com domínio inexistente **não** é erro; apenas loga um
  warning. Na prática esse warning significa host não normalizado no chamador.

## Dependencies
`github.com/jackc/pgx/v5` e `pgxpool`. Importado por `cmd/scraper`; os pacotes
`selectors`, `ai` e `scraper` consomem `SelectorConfig`/`RawListing` daqui. Os
arquivos de schema ficam em `migrations/` na raiz — ver o CLAUDE.md de lá para
as regras das tabelas `site_selectors` e `listings`.
