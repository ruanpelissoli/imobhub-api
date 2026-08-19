# enrichment/ — normalização de bairros, geocodificação e extração de texto

## Purpose
Pipeline de enriquecimento de anúncios já coletados. O pacote entrega três
enrichers independentes:

- **`NeighborhoodNormalizer`** — deriva a forma canônica do bairro a partir do
  texto bruto do anúncio. Portais diferentes escrevem o mesmo lugar de formas
  distintas (`"Jd. Botânico"`, `"JARDIM BOTANICO"`, `"jardim botanico"`); o
  agrupamento por região usa `listings.normalized_neighborhood`, então grafias
  divergentes quebram o agrupamento em silêncio.
- **`Geocoder`** — converte endereço em coordenadas geográficas
  (`listings.lat`/`lng`), via Nominatim (default) ou Google Maps (stub).
- **`TermExtractor`** — extrai dados estruturados do texto livre
  (`listings.description_raw`, `listings.bedrooms_raw`): número de quartos e
  lista de comodidades. Alimenta `listings.bedroom_count` / `listings.amenities`
  e, depois, o `properties` canônico.

**O pacote deixou de ser puro** com o geocoder: ele faz rede (Nominatim) e
importa `internal/ratelimit`. Continua **não** tocando banco.

**O chamador dos três enrichers é `internal/enrichqueue`**, a fila que drena
`listings` por `enriched_at`. Ela monta **uma única instância de cada um**,
compartilhada por todos os workers — obrigatório no caso do geocoder, que carrega
o `DomainLimiter` de 1 req/s e o cache com negative caching (N instâncias =
N req/s = bloqueio do User-Agent). Não duplique esse plumbing por enricher.

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

## Geocoder

### Key decisions
- **`ratelimit.DomainLimiter` reusado, não um `time.Ticker` novo.** Ele já dá
  espaçamento fixo por host, sem burst e cancelável por contexto — a política do
  Nominatim (máx. 1 req/s). Aresta nova `enrichment → ratelimit`: acíclica e
  rasa. O `Wait` fica **imediatamente antes** do `Do`; o tempo entre os dois sai
  do intervalo efetivo.
- **`countrycodes=br` é correção, não otimização:** sem ele `"Centro, Rio"` casa
  com um lugar em Portugal. Query montada com `net/url` — concatenação quebraria
  em endereços com `&` ou acento.
- **User-Agent vem do construtor** (`cfg.ScraperUserAgent`), nunca de
  `os.Getenv`. Construtor **erra** se vier vazio: sem o header o Nominatim
  responde 403, e falhar no boot é melhor que na primeira requisição da run.
- **`Doer` injetável** (satisfeito por `*http.Client`) para os testes não tocarem
  a rede; o construtor padrão cria client próprio sem cookie jar, mesmo
  raciocínio de `httpclient.FetchStatic`. O geocoder **não** usa `FetchStatic`:
  aquilo é feito para HTML e não limita o corpo.
- **Constantes de provider duplicadas em `config` e aqui.** `config` é folha do
  grafo e não pode importar `enrichment`; valida no boot (desconhecido é erro,
  não fallback) e `NewGeocoder` valida de novo. Defesa em profundidade.
- **Google Maps é stub deliberado** (`ErrProviderNotSupported`, zero HTTP, zero
  dependência nova): fixa a costura de troca de provider sem antecipar o SDK.

### Business logic
Sentinelas (todas `errors.Is`, prefixo `enrichment:`):
- **`ErrAddressNotFound`** — 200 com zero resultados. **Não é falha**: o chamador
  grava NULL e segue. Rede/timeout/status ≥ 400 dão erros **distintos**, para a
  fila não confundir "não existe" com "não deu para perguntar agora".
- **`ErrEmptyAddress`** — endereço vazio; retorna sem requisição alguma.
- **`ErrInvalidCoordinates`** — não parseável ou fora de `[-90,90]`/`[-180,180]`
  (inclui `NaN`/`Inf`, que `ParseFloat` aceita). Gravar ponto errado é pior que
  não gravar nada.
- **`ErrProviderNotSupported`** / **`ErrMissingUserAgent`** — erros de construção.

Cache em memória por chave normalizada (`normalizeKey`, o mesmo tratamento das
chaves de alias), com **negative caching**: reperguntar o mesmo endereço
irresolvível a cada anúncio do condomínio é o pior desperdício de quota. Erro de
rede/timeout/status **não** entra no cache — é transitório.

**Fronteira:** o geocoder **não consulta o banco**. O filtro "anúncio já
geocodificado" (`lat IS NULL`) é da fila de enriquecimento, não daqui.

### Gotchas
- **O cache do geocoder não tem TTL nem limite de tamanho** e vive enquanto o
  processo vive (mesma escolha do cache de `robots.Checker`).
- **O erro de status não é tipado** (não há `StatusError` como em `httpclient`):
  o chamador não distingue 429 de 500 programaticamente. Quando a fila precisar
  de backoff, é aqui que o tipo entra.

---

## NeighborhoodNormalizer

### Key decisions
- **Tabela de aliases embutida via `//go:embed`, em JSON, dentro do pacote.**
  `//go:embed` não alcança caminhos acima do pacote (`configs/*.yaml` está fora).
  Embutir evita mais uma variável de ambiente e o gotcha de `migrations/` e
  `sources.txt`, que precisam de `COPY` no Dockerfile. JSON e não YAML porque
  `go.yaml.in/yaml/v4` é dependência **indireta**.
- **`Setor` NÃO é removido.** `Setor Sudoeste`/`Bueno`/`Oeste` são nomes reais de
  bairro em Brasília e Goiânia. Só `Bairro` é redundante; o resto vai por alias.
  Substitui conscientemente o critério original da task.
- **Alias vence e volta verbatim** (a tabela manda na grafia final) e **acento é
  removido só na chave de comparação** — inventar acento exigiria dicionário.
- **Chave duplicada ⇒ erro**, inclusive colisão após normalizar (`"Asa Norte"`
  vs. `"asa  norte"`): `encoding/json` aceitaria a repetida em silêncio.

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
Stdlib + `golang.org/x/text` (normalização, fold e title-case) +
`go.yaml.in/yaml/v4` (parse do vocabulário de comodidades) +
`internal/ratelimit` (espaçamento fixo por host para o Nominatim).
**Não importa `internal/db`.**
