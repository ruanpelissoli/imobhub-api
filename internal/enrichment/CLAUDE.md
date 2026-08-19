# enrichment/ — normalização de bairros e geocodificação

## Purpose
Deriva campos canônicos a partir do texto bruto do anúncio. Dois serviços:
`NeighborhoodNormalizer` (`neighborhood.go`) → `listings.normalized_neighborhood`;
`Geocoder` (`geocoder*.go`) → `listings.lat`/`lng`. Nenhum deles tem chamador
ainda — persistir os campos (fila por `enriched_at IS NULL`, UPDATE em
`internal/db`, wiring em `cmd/scraper`) é follow-up **compartilhado** entre os
enrichers. Não duplicar esse plumbing por enricher.

**O pacote deixou de ser puro** com o geocoder: ele faz rede (Nominatim) e
importa `internal/ratelimit`. Continua **não** tocando banco.

## Key decisions — geocoder
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

## Business logic — geocoder
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

## Key decisions — normalizador de bairros
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

Pipeline de `Normalize(raw, city)`: trim/colapso → remoção de `Bairro` → chave de
busca → alias da cidade → alias global → fallback title-case pt-BR. Entrada vazia
ou só pontuação ⇒ `""` (o chamador grava **NULL**). `city == ""` consulta só a
seção global — é o caso de hoje, `listings` não tem coluna `city`; o parâmetro
existe para a assinatura não mudar. Função pura e idempotente.

## Dependencies
Stdlib + `golang.org/x/text` (já dependência direta) + `internal/ratelimit`.
**Não importa `internal/db`.**

## Gotchas
- **`cases.Caser` e `transform.Transformer` guardam estado**, por isso são
  construídos **por chamada** — a fila futura é concorrente e o sintoma seria
  saída corrompida, não panic. `-race` não roda aqui (exige cgo/gcc); os testes
  `...IsSafeForConcurrentUse` existem para flagrar.
- Pontuação **interna** é preservada na chave (`"Jd. Botânico"` → `"jd.
  botanico"`) — é assim que a entrada vai escrita na tabela.
- `cases.Title` produziria `"D'água"`; há regra explícita para o prefixo `d'`.
  `"Sant'Ana"` e afins resolvem-se por alias.
- **O cache do geocoder não tem TTL nem limite de tamanho** e vive enquanto o
  processo vive (mesma escolha do cache de `robots.Checker`).
- **O erro de status não é tipado** (não há `StatusError` como em `httpclient`):
  o chamador não distingue 429 de 500 programaticamente. Quando a fila precisar
  de backoff, é aqui que o tipo entra.
