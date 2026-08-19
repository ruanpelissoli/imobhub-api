# internal/db

## Purpose
Cria e valida o pool de conexões com o PostgreSQL (`db.go`), aplica as
migrations SQL no startup (`migrate.go`) e concentra o acesso às tabelas
`site_selectors` (`selectors_repo.go`), `listings` (`listings_repo.go`) e
`properties` (`properties_repo.go`). Os structs em `models.go` são o contrato
compartilhado com `ai`, `selectors` e `scraper` — nenhum outro pacote monta SQL.

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

## Repositórios (`models.go`, `selectors_repo.go`, `listings_repo.go`, `properties_repo.go`)
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
- **`ListListingsByPropertyID` tem `ORDER BY id` como parte do contrato**, não
  como conveniência: quem consolida o canônico (`grouping.MergePropertyData`)
  depende dessa ordem para deduplicar fotos e cortá-las em 50 sempre do mesmo
  jeito. Trocar ou remover o `ORDER BY` quebra a idempotência do merge sem
  quebrar nenhum teste deste pacote. O `WHERE property_id = $1::uuid` é atendido
  pelo índice `idx_listings_property_id` (criado em `migrations/004`). Imóvel sem
  anúncios devolve slice vazia, nunca `nil` nem erro.
- **A fila de enriquecimento vive em três funções**, consumidas por
  `internal/enrichqueue`: `ListPendingListings` (predicado
  `enriched_at IS NULL OR updated_at > enriched_at`, `ORDER BY id`, `LIMIT` e
  cursor `id > afterID`), `UpdateListingEnrichment` (as colunas derivadas) e
  `MarkListingEnriched` (o carimbo). **Nenhuma das duas de escrita toca
  `updated_at`**, e isso está comentado no SQL: `updated_at` é metade do
  predicado da fila, então setá-lo ali deixaria o anúncio pendente para sempre —
  um passe **pago** de IA por run, sobre o catálogo inteiro. São duas funções, e
  não uma com flag, porque os dois momentos são distintos no fluxo (persistir
  vem antes do agrupamento; carimbar, depois da consolidação) e um booleano
  permitiria carimbar cedo. Anúncio inexistente é no-op sem erro nas duas
  (`DeleteStaleListings` pode ter apagado a linha entre a leitura e a escrita).
- **O cursor de `ListPendingListings` não é paginação cosmética.** Um anúncio que
  falha não recebe `enriched_at` e continua casando com o predicado: com `LIMIT`
  sem cursor, o lote seguinte devolveria as mesmas linhas para sempre.
- **`upsertListingSQL` só bumpa `updated_at` quando algum campo coletado mudou**
  (`IS DISTINCT FROM` campo a campo, dentro de um `CASE`). Antes ele fazia
  `NOW()` incondicional em todo `ON CONFLICT`, o que devolvia à fila de
  enriquecimento todo anúncio visto em toda coleta. `last_seen_at = NOW()`
  continua **incondicional** e não pode ser condicionado: ele responde "o anúncio
  ainda está no site", não "o anúncio mudou", e é ele que protege a linha de
  `DeleteStaleListings`.
- **`db.PendingListing` é um modelo novo, não uma extensão de `Listing`.**
  `Listing` é o modelo de leitura do **merge**, amarrado à ordem de colunas de
  `selectListingsByPropertyIDSQL`; ampliá-lo obrigaria `scanListing` a preencher
  colunas que aquela query não seleciona. `PendingListing` carrega exatamente o
  que `grouping.Listing` exige. `db.ListingEnrichment` é o payload de escrita
  correspondente, com ponteiros pela mesma regra de `Property` (nil = NULL).
- **`db.Listing` é o modelo de leitura, separado de `RawListing`.** `RawListing`
  é o modelo de **escrita** da coleta: identidade `(SourceDomain, ListingURL)`,
  sem `id`, com `ExtraData`. O de leitura precisa do `id` (é a ordem estável do
  merge) e só carrega as colunas que a consolidação usa. Fundir os dois obrigaria
  a coleta a preencher campos que ela não tem. `description_raw` é nullable e
  chega como `""` (mesma convenção dos `_raw`); os `TEXT[]` passam por
  `normalizeTextArray`.
- `image_urls` e `extra_data` nunca gravam NULL (nil vira `[]`/`{}`): a coleta
  não distingue "sem imagens" de "não sei", e o leitor não deveria ter que
  distinguir. A mesma regra vale para `properties.amenities`/`photos`, e mora
  num lugar só (`normalizeTextArray`, de quem `normalizeImageURLs` é apelido).
- **`Property.ID` é `string`, não um tipo UUID.** O valor nasce no banco
  (`gen_random_uuid()`) e volta pelo `RETURNING`; uma dependência a mais só para
  transportá-lo não se paga. As queries usam cast explícito (`$1::uuid`), então
  id malformado vira erro do PostgreSQL — e não uma validação de formato em Go,
  que rejeitaria formas que o PG aceita (sem hífens, com chaves).
- **Campos consolidados de `Property` são ponteiros.** A ausência é informação:
  `nil` é "não consolidado ainda", `0` seria "zero quartos". `COALESCE` no
  SELECT apagaria a distinção — e `pgx` nem escaneia NULL para `string`/`int`.
- **`FindPropertiesByCoordinates` filtra em dois passos**: bounding box no SQL
  (é o que o btree composto `idx_properties_lat_lng` consegue usar) e distância
  real (haversine) em Go. Só o retângulo devolveria os cantos; só a distância
  inviabilizaria o índice. A conversão metros→graus é **aproximada** e o
  retângulo é calculado pela borda mais próxima do polo, com 0,1% de margem:
  sobrar é de graça, faltar perde imóvel. PostGIS não está na stack — busca
  geoespacial "de verdade" é outra task.

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
- **`DeleteStaleListings` fecha o ciclo de vida do vínculo, numa transação só.**
  Os três passos — `DELETE ... RETURNING property_id`, decremento do contador de
  cada imóvel afetado (pela quantidade real de anúncios que perdeu) e
  `DELETE FROM properties WHERE id = ANY(...) AND active_listing_count = 0` —
  são atômicos. Separá-los era o bug que a task IMO-22 fechou: o hard delete sem
  decremento deixava `active_listing_count` inflado para sempre, e o imóvel
  virava órfão **e indeletável** (a guarda de `DeleteProperty` exige zero, e ele
  nunca mais chegava lá). `property_id` NULL (anúncio nunca agrupado) é o caso
  normal e é apenas ignorado na agregação.
- **Os decrementos saem em ordem de id** (`staleCountsByProperty` ordena). Não é
  cosmético: dois runs concorrentes que travassem o mesmo par de imóveis em
  ordens opostas se bloqueariam mutuamente. Ordem fixa é ordem de aquisição de
  locks — a função é pura e é ela que os testes deste pacote cobrem.
- **`properties.active_listing_count` é denormalizado, e este pacote é quem o
  mantém** — por `LinkListingToProperty`/`UnlinkListingFromProperty` (caminho
  unitário) e por `DeleteStaleListings` (caminho em lote, o único que roda em
  toda coleta). Todos alteram `listings.property_id` e o contador **na mesma
  transação** — os dois separados produziriam uma inconsistência que nenhuma
  leitura posterior detecta. O caminho unitário começa com
  `SELECT ... FROM listings ... FOR UPDATE`: sem esse lock, dois links
  concorrentes do mesmo anúncio leriam ambos `property_id` NULL e contariam duas
  vezes. A aritmética (`count + 1`, `GREATEST(count - N, 0)`) acontece dentro do
  `UPDATE`, nunca em Go.
- **`UnlinkListingFromProperty` devolve o id do imóvel de que o anúncio saiu**
  (string vazia = não havia vínculo, inclusive anúncio inexistente). O valor sai
  do mesmo `FOR UPDATE` que a transação já faz: quem precisa decidir se o imóvel
  ficou órfão (`grouping.HandleListingRemoval`) faria um `SELECT` extra, e entre
  ele e o desvínculo caberia outro vínculo.
- **Ambas as operações são idempotentes.** Religar o anúncio ao mesmo imóvel não
  incrementa de novo (a comparação é feita pelo PostgreSQL, tipo `uuid`, para
  não errar em UUID maiúsculo/sem hífens); religar a **outro** imóvel decrementa
  o antigo e incrementa o novo na mesma transação; desvincular um anúncio já
  solto é no-op — é isso que impede o contador de ficar negativo.
- **A guarda `active_listing_count = 0` vive sempre dentro do `DELETE`** — tanto
  em `DeleteProperty` (unitário) quanto em `deleteOrphanPropertiesSQL` (lote).
  Um read-then-delete deixaria um vínculo criado no meio virar anúncio órfão (a
  FK é `ON DELETE SET NULL`). No caminho unitário, quando nada é apagado, uma
  segunda query distingue `ErrPropertyNotFound` de `ErrPropertyHasListings` — aí
  já não há corrida a perder, o resultado é só a mensagem. No lote, um imóvel da
  lista que ainda tem anúncios simplesmente não casa com o `WHERE`, e isso é o
  caso normal, não erro.
- `ErrPropertyNotFound`, `ErrPropertyHasListings` e `ErrListingNotFound` são
  comparáveis com `errors.Is`: quem corrige uma deduplicação errada reage a
  `ErrPropertyHasListings` desvinculando os anúncios, sem inspecionar texto.
- **`GetPropertyByID` devolve `(nil, nil)` para id inexistente** (mesmo contrato
  de `GetSelectorsByDomain`: um imóvel pode ter sido apagado). `UpdateProperty`,
  ao contrário, trata ausência como erro — ali significa atualização perdida em
  silêncio.
- **`CreateProperty`/`UpdateProperty` ignoram `ActiveListingCount`** (e o Update
  não toca em `created_at`): deixar o caller gravar o contador dessincronizaria
  a contagem sem aviso. Hoje toda linha em `listings` é "ativa" (as sumidas são
  apagadas por `DeleteStaleListings`, que decrementa o contador junto), então
  incrementar no vínculo é correto — **se `listings` ganhar coluna de status,
  esta regra e a limpeza em lote precisam ser revistas.**
- **Bases que rodaram antes de IMO-22 continuam com o contador inflado.** O
  backfill/reconciliação de `active_listing_count` é task à parte, de propósito:
  a correção acima só impede novas divergências.
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
  (montagem de argumentos, validação, JSON, bounding box/haversine, agregação de
  `staleCountsByProperty`) e três **contratos em string** — que o SQL de
  enriquecimento não menciona `updated_at`, que o upsert mantém a guarda
  `IS DISTINCT FROM` com `last_seen_at` incondicional, e que o predicado de
  `selectPendingListingsSQL` continua idêntico ao do índice parcial de
  `migrations/006` (divergir faz o índice deixar de ser usado em silêncio, e a
  fila volta a varrer a tabela inteira). Esses são os bugs
  mais caros e mais invisíveis do pacote: só se manifestariam meses depois, como
  "o catálogo inteiro é reprocessado (e repago) todo dia" ou "este anúncio nunca
  sai da fila". A verificação em PostgreSQL real de "anúncio inalterado não bumpa
  `updated_at`" fica para o QA. O comportamento do SQL — upsert, `ON CONFLICT`,
  contagem do DELETE, `RETURNING`, `ANY($1::uuid[])`, idempotência do vínculo,
  `GREATEST` no contador, guarda do `DeleteProperty` e a atomicidade da limpeza
  em lote — depende de um PostgreSQL real e fica para o QA / testes de
  integração. Não há testcontainers nem banco in-memory aqui, de propósito.
- `MarkSelectorsBroken` com domínio inexistente **não** é erro; apenas loga um
  warning. Na prática esse warning significa host não normalizado no chamador.

## Dependencies
`github.com/jackc/pgx/v5` e `pgxpool`. Importado por `cmd/scraper`; os pacotes
`selectors`, `ai` e `scraper` consomem `SelectorConfig`/`RawListing` daqui. Os
arquivos de schema ficam em `migrations/` na raiz — ver o CLAUDE.md de lá para
as regras das tabelas `site_selectors`, `listings` (incluindo as colunas de
enriquecimento) e `properties`. `grouping` consome `Property`, `Listing` e as
funções de busca/vínculo/consolidação/desvínculo. A regra de deduplicação (que combinação de
endereço/geo/atributos identifica o mesmo imóvel) **não** vive aqui: este pacote
só oferece o vínculo `listing → property`; quem decide o vínculo é o pipeline de
enriquecimento.
