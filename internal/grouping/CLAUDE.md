# internal/grouping

## Purpose
Decide se um anúncio já normalizado e geocodificado se refere ao mesmo imóvel
físico de algum registro canônico em `properties` — vinculando-o ao existente
ou criando um novo. `PropertyGrouper.GroupListing` é a única porta de entrada e
devolve `(propertyID string, isNew bool, err error)`.

É o par de `internal/selectors` do lado do enriquecimento: orquestra
`internal/ai` (a decisão) e `internal/db` (a persistência).

## Key decisions
- **Pacote próprio, não `internal/enrichment`.** Aquele pacote não importa `db`
  nem `ai` por decisão registrada em `internal/CLAUDE.md`; os enrichers de lá
  são funções puras/HTTP. O precedente correto para "orquestra `ai` + `db`" é
  `internal/selectors`.
- **Interfaces declaradas no consumidor** (`PropertyStore`, `PropertyMatcher`).
  `internal/db` expõe funções livres sobre `*pgxpool.Pool` e **não** ganha
  struct nem interface de repositório (ver `internal/db/CLAUDE.md`);
  `store.go` é o adaptador fino e o único ponto do pacote que conhece `pgxpool`.
  É isso que permite testar o fluxo inteiro sem PostgreSQL e sem gastar token.
- **A ordem do fluxo é uma invariante de custo**, não estilo. A comparação por
  IA é paga **por anúncio**; as três saídas baratas vêm antes de qualquer
  requisição: (1) anúncio já vinculado — sem IA e sem banco; (2) sem `lat`/`lng`
  → `ErrListingNotGeocoded`; (3) zero candidatos no raio → cria direto, sem IA.
  Só depois vem **uma** (e apenas uma) chamada ao modelo.
- **Raio e `MaxCandidates` são o controle de custo.** A lista de
  `FindPropertiesByCoordinates` já vem do mais próximo ao mais distante, então
  o corte em `MaxCandidates` descarta os piores candidatos, não candidatos
  aleatórios.
- **O threshold mora aqui, não em `ai`.** `ai.MatchProperty` devolve o veredito
  cru; "quanta confiança basta" é regra de negócio e vem de
  `config.GroupingConfidenceThreshold`.
- **Config validada no construtor** (`NewPropertyGrouper`), como em
  `NewSelectorService`: wiring incompleto derruba o boot em vez de falhar no
  primeiro anúncio da fila.

## Business logic / invariantes
- **Anúncio sem coordenadas não é agrupado e não vira `property`.** Um canônico
  sem geo nunca mais aparece numa busca por raio e, portanto, nunca mais casa
  com nada. A fila deve geocodificar antes de agrupar.
- **`property_id` fora da lista de candidatos é tratado como "sem match"**, com
  `slog.Warn`. Ele nunca vai ao banco: derrubaria na FK
  `listings_property_id_fkey` ou, pior, vincularia o anúncio a um imóvel que o
  modelo nunca viu. A comparação usa `EqualFold` (o modelo pode ecoar o UUID em
  outra caixa) e o id **gravado é o do candidato**, na grafia do PostgreSQL.
- **Erro da IA não cria nada**: é embrulhado e propagado, para o anúncio
  continuar pendente e a fila tentar de novo.
- **Campo ausente vira `nil`, nunca `""` nem `0`** em `propertyFrom`. Em
  `db.Property` a ausência é informação, e um endereço em branco gravado
  contaminaria a comparação de todos os anúncios seguintes.
- `AreaSqm`, `City`, `State`, `TransactionType` e `PropertyType` nascem `nil`:
  `listings` não tem essas colunas e `AreaRaw` é texto livre que este pacote
  **não** parseia.

## Dependencies
`internal/ai` (`MatchProperty`, `MatchListing`, `PropertyMatch`), `internal/db`
(`Property` e as três funções adaptadas em `store.go`) e `pgxpool` — este
último só em `store.go`. Os parâmetros vêm de `internal/config`
(`GroupingConfidenceThreshold`, `GroupingRadiusMeters`, `GroupingMaxCandidates`).

**Ainda não tem chamador.** Como os enrichers de `internal/enrichment`, o
serviço nasce desconectado: a fila por `enriched_at IS NULL`, a leitura dos
anúncios pendentes e o wiring em `cmd/scraper` são a task de follow-up
**compartilhada** — não duplique esse plumbing aqui.

## Gotchas
- **Property órfã.** `createAndLink` faz `CreateProperty` e depois
  `LinkListingToProperty`, em transações separadas (pacotes diferentes, INSERT
  já commitado). Se o link falhar, a property fica com
  `active_listing_count = 0` e é removível por `db.DeleteProperty` — o id vai no
  erro e no `slog.Error` justamente para isso. Não há compensação transacional
  entre pacotes, de propósito.
- **A v1 não distingue venda de aluguel.** `listings` não tem esse dado; o mesmo
  imóvel anunciado para venda e para locação vira um único canônico. Quando a
  coluna existir, ela precisa entrar no prompt e em `propertyFrom`.
- **Falso positivo é caro de desfazer** (dois imóveis distintos viram um).
  O default 0,85 e os `slog.Debug` de prompt/resposta em `internal/ai` existem
  para calibrar; a correção é `db.UnlinkListingFromProperty` + `DeleteProperty`.
- Os testes usam fakes: o comportamento real do SQL (`Find`/`Create`/`Link`)
  contra um PostgreSQL de verdade fica para o QA, como já registrado em
  `internal/db/CLAUDE.md`.
