# internal/enrichment

## Purpose
Extrai dados estruturados do **texto livre** dos anúncios já coletados
(`listings.description_raw`, `listings.bedrooms_raw`): número de quartos e lista
de comodidades. É a primeira peça do pipeline de enriquecimento — alimenta
`listings.bedroom_count` / `listings.amenities` e, depois, o `properties`
canônico.

O pacote é **puro**: sem banco, sem rede, sem `os.Getenv`. O único I/O é a
leitura do vocabulário de comodidades no construtor. **Ainda não tem chamador**:
o wiring entra junto com a fila de enriquecimento (`enriched_at IS NULL`).

## Business logic
- **Quartos, primeira ocorrência vence.** Descrições brasileiras abrem com o
  número principal ("3 quartos, sendo 1 suíte"); o que vem depois costuma ser
  outra planta do empreendimento. Quatro padrões são avaliados e vence o que
  casar mais cedo no texto — a ordem da lista só desempata posição igual, e aí o
  mais específico (faixa) precisa vir antes.
- Reconhece `3 quartos`, `2 qtos`, `3 qts`, `4 dormitórios`, `2 dorm.`,
  `três quartos` (um…dez), a notação de mercado `3/4`, e faixa (`2 a 3 quartos`
  → 2, o piso).
- **`suíte` não é termo de quarto.** "3 quartos, sendo 1 suíte" são 3 quartos;
  contar a suíte inflaria a maioria dos anúncios de médio padrão.
- `studio`/`stúdio`/`kitnet`/`kitchenette`/`quitinete`/`conjugado` valem **0**,
  mas só quando nenhum padrão numérico casou — é assim que "studio de 1 quarto"
  devolve 1 sem regra extra de prioridade.
- **Comodidades saem na ordem do arquivo de vocabulário**, não na ordem do texto
  nem na de iteração de um mapa, e sem duplicatas (o laço para no primeiro
  sinônimo que casa em cada comodidade). Ordem estável é o que torna o resultado
  comparável entre passes e testável sem `sort`.
- Matching insensível a caixa e a acento **dos dois lados**, e tolerante a
  hífen vs espaço em termos compostos: "Ar-Condicionado", "ar condicionado" e
  "AR CONDICIONADO" são a mesma coisa.

## Key decisions
- **`BedroomCount` é `*int`, não `int`.** `nil` = o anúncio não informa; `0` =
  imóvel sem quarto separado. `migrations/004_enrich_listings.sql` deixa a coluna
  `NULL` exatamente por isso, e `db.Property.BedroomCount` já é `*int`. Com `int`,
  todo anúncio silencioso viraria studio.
- **`Extract` nunca devolve erro.** Texto de anúncio é sempre "válido"; o que
  varia é quanto dele é reconhecível. Não reconhecer nada é resultado legítimo
  (`ExtractedData{}`), não falha — quem processa milhares de anúncios em lote não
  tem o que fazer com um erro por anúncio ilegível.
- **Vocabulário carregado pelo chamador e injetado no construtor**, nunca em
  `init()` nem em global mutável. O extrator resultante é imutável e seguro para
  uso concorrente, e o teste consegue montar um vocabulário próprio.
- **O caminho do arquivo chega por parâmetro.** `internal/config` é a única
  fronteira do projeto com `os.Getenv` (`AMENITIES_FILE`, default
  `configs/amenities.yaml`).
- **Regex e termos, não IA.** Dezenas de milhares de anúncios por passe, com
  vocabulário imobiliário pequeno e fechado: uma chamada de rede por anúncio
  custaria tempo e dinheiro sem ganho perceptível. A interface `TextExtractor`
  existe para o dia em que um extrator por IA cobrir a cauda longa.
- **Tudo compilado uma vez**: os regexes de quartos são vars de pacote, os de
  comodidade nascem no construtor. Nada é compilado dentro de `Extract`.
- **`fold` é stateless** (NFD + descarte de `unicode.Mn` + NFC), e não
  `transform.Chain`/`runes.Remove`: um `transform.Transformer` guardado no
  extrator teria estado interno e não poderia ser reusado por várias goroutines.
- **Sinônimo ruim avisa, arquivo ruim aborta** — mas quase tudo aqui é arquivo
  ruim (lista vazia, canônico vazio ou duplicado, comodidade sem sinônimo
  utilizável). Diferente de `sources`, uma comodidade faltando não se manifesta
  como falha, e sim como milhares de anúncios enriquecidos pela metade.

## Dependencies
`golang.org/x/text/unicode/norm` (dobramento de acento) e `go.yaml.in/yaml/v4`
(parse do vocabulário) — ambas já estavam na árvore. Nada do projeto: nem
`config` (que só fornece a string do caminho) nem `db`. É folha do grafo.

## Gotchas
- **Não há tratamento de negação.** "sem piscina", "não possui elevador" casam
  com os termos e geram falso positivo. Fora de escopo por decisão da task; se um
  dia entrar, é aqui (janela de palavras antes do termo), não no chamador.
- **`\b` é ASCII no `regexp` do Go**, e o termo é ancorado nas duas pontas. Por
  isso um sinônimo precisa começar e terminar em letra/dígito ASCII **depois do
  fold** — "24h." seria um regex que nunca casa, e o loader o recusa com aviso.
- **`3/4` também é uma fração legítima** ("3/4 do terreno"). Aceito
  conscientemente: é notação corrente no mercado brasileiro e o denominador fixo
  em 4 já limita o estrago.
- **Mudar `canonical` no YAML muda o dado gravado.** Os valores viram conteúdo de
  `listings.amenities`; renomear exige reprocessar os anúncios já enriquecidos.
- **Inserir uma comodidade no meio do arquivo muda a ordem do resultado** — é o
  contrato de determinismo. Prefira acrescentar no fim quando a posição não
  importar.
- `fold` altera o comprimento em bytes: não use offsets dele contra o texto
  original.
