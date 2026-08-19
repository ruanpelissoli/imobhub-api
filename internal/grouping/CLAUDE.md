# internal/grouping

## Purpose
Três responsabilidades sobre o imóvel canônico, todas em `PropertyGrouper`:

1. **Agrupar** (`grouper.go`) — decide se um anúncio já normalizado e
   geocodificado se refere ao mesmo imóvel físico de algum registro em
   `properties`, vinculando-o ao existente ou criando um novo.
   `GroupListing` devolve `(propertyID string, isNew bool, err error)`.
2. **Consolidar** (`merger.go`) — `MergePropertyData(ctx, propertyID)`
   reconsolida o canônico a partir de **todos** os anúncios vinculados a ele:
   fotos, descrição, comodidades e quartos. Existe porque `propertyFrom` só
   enxerga o anúncio que criou o registro; sem esta passagem o canônico ficaria
   congelado nos dados do primeiro anúncio.
3. **Desagrupar** (`grouper.go`) — `HandleListingRemoval(ctx, listingID)` tira um
   anúncio do grupo e apaga o canônico se ele ficou sem nenhum. É a porta de
   saída, para desfazer um agrupamento errado; devolve se o imóvel foi apagado.

É o par de `internal/selectors` do lado do enriquecimento: orquestra
`internal/ai` (a decisão) e `internal/db` (a persistência).

## Key decisions
- **Pacote próprio, não `internal/enrichment`.** Aquele pacote não importa `db`
  nem `ai` por decisão registrada em `internal/CLAUDE.md`; os enrichers de lá
  são funções puras/HTTP. O precedente correto para "orquestra `ai` + `db`" é
  `internal/selectors`.
- **Interfaces declaradas no consumidor** (`PropertyStore` — 9 métodos: 3 para o
  agrupamento, 3 para a consolidação, 3 para o desagrupamento — e
  `PropertyMatcher`).
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
- **`HandleListingRemoval` é o caminho *unitário*, não o da coleta.** Sequência:
  `UnlinkListingFromProperty` → `GetActiveListingCount` → `DeleteProperty` se
  zero. Anúncio sem vínculo (nunca agrupado ou inexistente) é **no-op sem erro**
  — nem a contagem é feita. `db.ErrPropertyNotFound` na contagem e
  `ErrPropertyHasListings`/`ErrPropertyNotFound` no delete são `slog.Warn` e
  `false, nil`: nos três casos o estado final já é o desejado.
- **A limpeza do fim de cada coleta não passa por aqui.** Quem apaga os anúncios
  que sumiram do site é `db.DeleteStaleListings`, que já decrementa o contador e
  remove os órfãos na mesma transação. Duplicar essa lógica neste pacote traria
  de volta o contador dessincronizado.

### Consolidação (`MergePropertyData`)
- **`db.UpdateProperty` reescreve TODAS as colunas consolidadas.** Por isso o
  fluxo é **read-modify-write**: lê o canônico, sobrescreve só os quatro campos
  deste merge e regrava o struct inteiro. Montar um `db.Property` apenas com
  `Photos`/`Description` apagaria endereço, bairro, cidade, estado, lat/lng e
  área — e um canônico sem geo nunca mais casa com nada em
  `FindPropertiesByCoordinates`. `active_listing_count` e `created_at` são
  preservados pelo próprio SQL, e `updated_at = NOW()` também (não há trigger).
- **Zero anúncios é no-op sem erro** (o vínculo pode ter acabado de ser
  desfeito); property inexistente é `ErrPropertyNotFound`.
- **Ordem determinística é invariante, não estilo.** Os anúncios chegam de
  `db.ListListingsByPropertyID` ordenados por `listings.id`, e a ordem original
  de cada array é mantida. É isso que faz o corte em `maxMergedPhotos = 50`
  escolher sempre as **mesmas** 50 fotos e sustenta a idempotência.
- **Dedup de fotos por igualdade exata**, sem normalizar barra final ou query
  string: num CDN elas costumam distinguir recortes/tamanhos, e descartar o
  "duplicado" errado perderia a única versão utilizável.
- **Fotos e comodidades são substituídas**, não unidas com o que já estava no
  canônico: os anúncios são a fonte, e uma foto que sumiu de todos eles não deve
  sobreviver. **A descrição é a exceção**: um texto já consolidado só é trocado
  por outro, nunca apagado quando nenhum anúncio tem texto.
- **Descrição = a mais longa, contada em runes.** Heurística assumida (o texto
  maior costuma trazer planta, condomínio e proximidades) e sem gastar IA. Runes
  porque os textos são pt-BR: em bytes, o mesmo texto "cresceria" só por estar
  acentuado. A comparação é estritamente maior, então empate fica com o menor
  `listings.id`.
- **`BedroomCount` = voto de maioria** entre os anúncios que informam, empate
  pelo valor de menor `id`. Maioria (e não "o anúncio mais recente") porque o
  dado vem de parser sobre texto livre: um erro isolado não deve reescrever o
  que dois portais confirmam. `nil` quando **nenhum** informa — nunca `0`.
- `transaction_type` e `property_type` **não** entram no merge: essas colunas não
  existem em `listings` (ver "a v1 não distingue venda de aluguel" abaixo).

## Dependencies
`internal/ai` (`MatchProperty`, `MatchListing`, `PropertyMatch`), `internal/db`
(`Property`, `Listing`, as sentinelas
`ErrPropertyNotFound`/`ErrPropertyHasListings` e as nove funções adaptadas em
`store.go`) e `pgxpool` — este último só em `store.go`. Os parâmetros vêm de
`internal/config`
(`GroupingConfidenceThreshold`, `GroupingRadiusMeters`, `GroupingMaxCandidates`).

**Ainda não tem chamador** — nem `GroupListing`, nem `MergePropertyData`, nem
`HandleListingRemoval`. Como os enrichers de `internal/enrichment`, o serviço
nasce desconectado: a fila por `enriched_at IS NULL`, a leitura dos anúncios
pendentes e o wiring em `cmd/scraper` são a task de follow-up **compartilhada** —
não duplique esse plumbing aqui. Quando ela existir, `MergePropertyData` roda
**depois** de `GroupListing` (o merge precisa do vínculo já gravado).

## Gotchas
- **Property órfã.** `createAndLink` faz `CreateProperty` e depois
  `LinkListingToProperty`, em transações separadas (pacotes diferentes, INSERT
  já commitado). Se o link falhar, a property fica com
  `active_listing_count = 0` e é removível por `db.DeleteProperty` — o id vai no
  erro e no `slog.Error` justamente para isso. Não há compensação transacional
  entre pacotes, de propósito.
- **`HandleListingRemoval` também não é uma transação única**: cada função de
  `db` abre a sua, mesmo precedente de `createAndLink`. Isso é seguro porque a
  guarda `active_listing_count = 0` vive dentro do próprio `DELETE` — um anúncio
  vinculado entre a contagem e a remoção só faz o imóvel não ser apagado. A
  limpeza em lote (`db.DeleteStaleListings`) **é** atômica; não confunda as duas.
- **A v1 não distingue venda de aluguel.** `listings` não tem esse dado; o mesmo
  imóvel anunciado para venda e para locação vira um único canônico. Quando a
  coluna existir, ela precisa entrar no prompt e em `propertyFrom`.
- **Falso positivo é caro de desfazer** (dois imóveis distintos viram um).
  O default 0,85 e os `slog.Debug` de prompt/resposta em `internal/ai` existem
  para calibrar; a correção é `HandleListingRemoval` (que já encadeia unlink →
  contagem → delete), não chamar as funções de `db` na mão.
- **Janela de *lost update* no merge, aceita de propósito.** `MergePropertyData`
  faz `SELECT` + `UPDATE` sem `FOR UPDATE`: dois merges concorrentes do mesmo
  imóvel podem sobrescrever um ao outro. É tolerável porque a operação é
  idempotente e re-executável pela fila. Fechar a janela exigiria
  `SELECT ... FOR UPDATE` + `UPDATE` **dentro de uma função de `db`** (padrão de
  `LinkListingToProperty`) — não tente compor transações entre pacotes, o que
  `createAndLink` deliberadamente não faz.
- Os testes usam fakes: o comportamento real do SQL (`Find`/`Create`/`Link`/
  `List`/`Update`/`Unlink`/`Delete`) contra um PostgreSQL de verdade fica para o
  QA, como já registrado em `internal/db/CLAUDE.md`.
