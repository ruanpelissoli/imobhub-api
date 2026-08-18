# internal/enrichment

## Purpose
Pipeline de enriquecimento de anúncios já coletados. O pacote entrega dois
enrichers independentes:

- **`TermExtractor`** — extrai dados estruturados do texto livre
  (`listings.description_raw`, `listings.bedrooms_raw`): número de quartos e
  lista de comodidades. Alimenta `listings.bedroom_count` / `listings.amenities`
  e, depois, o `properties` canônico.
- **`NeighborhoodNormalizer`** — deriva a forma canônica do bairro a partir do
  texto bruto do anúncio. Portais diferentes escrevem o mesmo lugar de formas
  distintas (`"Jd. Botânico"`, `"JARDIM BOTANICO"`, `"jardim botanico"`); o
  agrupamento por região usa `listings.normalized_neighborhood`, então grafias
  divergentes quebram o agrupamento em silêncio.

O pacote é **puro**: sem banco, sem rede, sem `os.Getenv`. **Ainda não tem
chamador**: o wiring entra junto com a fila de enriquecimento
(`enriched_at IS NULL`).

---

## TermExtractor

### Business logic
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

### Key decisions
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

### Gotchas
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

---

## NeighborhoodNormalizer

### Key decisions
- **Tabela de aliases embutida via `//go:embed`, em JSON, dentro do pacote.**
  `//go:embed` não alcança caminhos acima do diretório do pacote, então
  `configs/*.yaml` está fora. Embutir evita uma variável de ambiente nova (o
  `internal/config` tem ritual de atualização em três lugares) e evita repetir o
  gotcha de `migrations/` e `sources.txt`, que precisam de `COPY` no Dockerfile
  e falham em runtime quando alguém esquece. JSON e não YAML porque
  `go.yaml.in/yaml/v4` é dependência **indireta** — `encoding/json` é stdlib e
  não mexe no `go.mod`.
- **`Setor` NÃO é removido.** Em Brasília e Goiânia, `Setor Sudoeste`,
  `Setor Bueno` e `Setor Oeste` são nomes reais de bairro; removê-la corromperia
  o dado sem deixar rastro. Só `Bairro` é tratada como palavra redundante. Casos
  em que `Setor` de fato sobra vão para a tabela de aliases — que é o mecanismo
  de escape por design. Isto substitui conscientemente o critério original da
  task ("remover sufixos redundantes: Bairro, Setor").
- **Alias vence e volta verbatim.** A tabela manda na grafia final, acentos e
  maiúsculas inclusive — nada de re-aplicar title-case por cima, senão
  `"Águas Claras"` viraria outra coisa a cada ajuste do caser.
- **Acento removido só na chave de comparação.** O fallback devolve a string
  limpa em title-case com os acentos **originais**. Inventar acento
  (`"Acacias"` → `"Acácias"`) exigiria dicionário; errar a grafia é pior que
  preservá-la. Acentuação correta vem por alias.
- **Chave duplicada ⇒ erro, não silêncio.** `encoding/json` aceita chave
  repetida (a última vence), então `AliasMap`/`CityAliasMap` têm `UnmarshalJSON`
  próprio que percorre tokens. Há uma segunda checagem depois de normalizar as
  chaves, que pega colisões lógicas (`"Asa Norte"` vs. `"asa  norte"`).

### Business logic
Pipeline de `Normalize(raw, city)`, nesta ordem: trim + colapso de espaços →
remoção de `Bairro` (prefixo ou sufixo, sem caixa e sem acento, tolerando
pontuação colada) → se sobrou vazio, `""` → chave de busca (minúsculas + sem
acento + espaços colapsados + pontuação de borda descartada) → alias da cidade →
alias global → fallback em title-case pt-BR.

Invariantes:
- Entrada vazia, só espaços ou só pontuação ⇒ `""`. O chamador grava **NULL**,
  nunca string vazia.
- `city == ""` consulta apenas a seção global. Hoje **nenhum chamador tem
  cidade**: `listings` não tem coluna `city` (só `properties` tem). O parâmetro
  existe porque aliases são conceitualmente por cidade ("Centro" existe em toda
  cidade) e para a assinatura não mudar quando o parsing de endereço evoluir.
- Cidade vence global: `"Sudoeste"` é `"Sudoeste"` em geral e `"Setor Sudoeste"`
  em Brasília.
- Função pura e determinística; `Normalize(Normalize(x)) == Normalize(x)`.

### Gotchas
- **`cases.Caser` e `transform.Transformer` guardam estado e não são seguros
  para uso concorrente.** Por isso são construídos **por chamada** dentro dos
  helpers, e não guardados como campo do struct — a fila de enriquecimento
  futura é concorrente e o sintoma seria saída corrompida, não panic. Se algum
  profiling mostrar que pesa, a correção é `sync.Pool`, nunca campo
  compartilhado. `TestNormalizeIsSafeForConcurrentUse` existe para flagrar isso;
  `-race` não roda neste ambiente (exige cgo/gcc).
- As chaves do JSON passam pela mesma normalização da entrada, então o arquivo
  pode ser escrito em forma legível (`"Jd. Botânico"`). Consequência: duas
  grafias que colapsam na mesma chave são erro de construção.
- `cases.Title` produziria `"D'água"` (trata o apóstrofo como fim de palavra);
  há uma regra explícita para o prefixo `d'`. Outros casos com apóstrofo ou
  hífen (`"Sant'Ana"`) resolvem-se por alias.
- Pontuação **interna** é preservada na chave: a de `"Jd. Botânico"` é
  `"jd. botanico"` — é assim que a entrada precisa estar escrita na tabela.

---

## Dependencies
`golang.org/x/text` (normalização, fold e title-case) e `go.yaml.in/yaml/v4`
(parse do vocabulário de comodidades) — ambas já estavam na árvore. Nada do
projeto: nem `config` (que só fornece strings de caminho) nem `db`. É folha do
grafo.
