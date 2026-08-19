# internal/ai

## Purpose
Encapsula a integração com a API da Anthropic. Hoje expõe dois fluxos de
negócio:

- `AnalyzeSelectors` — recebe o HTML de uma página de listagens e devolve os
  seletores CSS de extração (`db.SelectorFields`) mais o `renderMode` que aquele
  site exige. Usado por `selectors` quando aparece um domínio sem mapeamento.
- `Client.MatchProperty` — recebe um anúncio (`MatchListing`) e uma lista de
  imóveis canônicos próximos (`[]db.Property`) e devolve o veredito de
  deduplicação (`PropertyMatch`: `same_property`, `confidence`, `property_id`,
  `reason`). Usado por `internal/grouping`.

## Key decisions
- **Wrapper em vez de uso direto do SDK.** Os pacotes de negócio dependem de
  `ai`, nunca de `github.com/anthropics/anthropic-sdk-go`. Trocar de modelo ou
  de provedor vira uma mudança local em vez de um sweep pelo repositório.
- **Três modelos, três papéis.** `DefaultModel` (Opus) é o padrão do `Client`;
  `SelectorModel` (`claude-haiku-4-5`) é usado só na análise de seletores —
  identificar classes CSS num HTML é extração, não raciocínio, e roda uma vez
  por domínio novo. `MatcherModel` (`claude-sonnet-4-5`) fica no meio: comparar
  fotos e endereços exige visão e julgamento, que o Haiku não entrega, mas o
  fluxo é pago **por anúncio** e o Opus sairia caro demais no volume. Por isso
  os dois fluxos ignoram `c.model` de propósito; cada troca é uma linha.
- **`tool_use` em vez de JSON em texto livre**, nos dois fluxos. O modelo é
  obrigado a chamar `report_listing_selectors` / `report_property_match` via
  `ToolChoice`. Parsear JSON de prosa quebra quando o modelo enfeita a resposta
  com ```json ou um parágrafo introdutório; com a ferramenta, o input já chega
  como JSON válido e validado pelo schema.
- **Fotos entram como blocos de imagem por URL**
  (`anthropic.NewImageBlock(anthropic.URLImageSourceParam{...})`), cada uma
  precedida de um bloco de texto que diz a que lado ela pertence — uma sequência
  de imagens soltas seria inútil para comparar dois imóveis. Os tetos (3 do
  anúncio, 2 por candidato, 12 no total) são de custo: cada foto vale centenas
  de tokens numa chamada feita por anúncio.
- **Fallback somente-texto.** Se a primeira requisição levava imagens e a API
  respondeu **400** (URL inacessível, formato recusado), há **um** retry só com
  o bloco de texto, com `slog.Warn`. Perder a comparação textual — que é a parte
  mais determinante da decisão — por causa de uma foto seria o pior resultado
  possível. 401/429/5xx **não** disparam o retry: repetir sem as fotos falharia
  de novo pelo mesmo motivo e gastaria dinheiro duas vezes.
- **As chaves do schema são as tags json de `db.SelectorFields`.** A resposta do
  modelo desserializa direto no struct que vai para o banco, sem camada de
  tradução que possa divergir.
- **Truncagem em `MaxHTMLChars` (80k) por runes, não por bytes.** Cortar por
  bytes partiria um caractere acentuado ao meio e mandaria UTF-8 inválido no
  prompt — e páginas brasileiras são cheias de acento. O corte é no fim porque o
  começo do documento carrega `<head>`, os primeiros cards e os sinais de SPA.
- **A API key é passada explicitamente para `New`**, em vez de deixar o SDK ler
  `ANTHROPIC_API_KEY`. Mantém a regra de que toda configuração passa por
  `internal/config` e torna o pacote testável sem mexer no ambiente.

## Business logic
- Só `listing_container` é obrigatório de fato. Os demais seletores são
  relativos a ele; sem container, nada é extraível — daí `ErrNoListingContainer`
  em vez de devolver um struct meio preenchido.
- `renderMode` é normalizado (`TrimSpace` + `ToLower`) e validado contra o enum.
  Um valor livre só estouraria na CHECK constraint de `site_selectors`, no
  INSERT, longe da causa.
- `render_mode` vazio cai no default `static`: falhar na extração é barato e
  visível, enquanto subir um Chrome à toa é caro e silencioso.
- HTML vazio retorna `ErrEmptyHTML` **antes** de chamar a API — não faz sentido
  gastar token analisando nada. Se a página estática veio vazia, o caminho certo
  é renderizar com headless e analisar o resultado.
- Todos os erros são sentinelas (`errors.Is`) para que o chamador diferencie
  "esta fonte não é suportada" (`ErrNoListingContainer`) de "tente de novo mais
  tarde" (falha de rede). No matcher: `ErrNoCandidates`,
  `ErrNoPropertyMatchToolUse` e `ErrInvalidConfidence`.
- **`MatchProperty` não aplica threshold.** O veredito volta cru; "quanta
  confiança basta" e "o `property_id` devolvido está mesmo na lista de
  candidatos" são regras de negócio de `internal/grouping`, que é quem tem a
  lista. Aqui só se valida que `confidence ∈ [0,1]` — fora disso a comparação
  com o threshold ficaria corrompida.
- **Lista de candidatos vazia é `ErrNoCandidates` antes de qualquer
  requisição.** Comparar um anúncio com uma lista vazia gastaria tokens para
  produzir sempre a mesma resposta. `grouping` já barra esse caso; a checagem
  aqui é defesa em profundidade.
- **Campo ausente sai como "não informado" no prompt, nunca inventado.** Vale
  para os dois lados da comparação. O system prompt instrui explicitamente que
  isso significa "desconhecido", e não "diferente" — sem essa regra o modelo
  trata a ausência como evidência de que são imóveis distintos.

## Dependencies
`github.com/anthropics/anthropic-sdk-go` (+ `/option`) e `internal/db` (para
`SelectorFields`, `Property` e as constantes de render mode). Importado por
`internal/selectors` (`SelectorService`), que decide **quando** pagar uma
análise — só em fonte nova ou com seletores quebrados —, e por
`internal/grouping` (`PropertyGrouper`), que decide quando pagar uma comparação.

## Gotchas
- **Este pacote gasta dinheiro, e o matcher gasta mais.** `AnalyzeSelectors`
  custa **uma chamada por domínio novo**; `MatchProperty` custa **uma chamada
  por anúncio** que chegue geocodificado e com algum imóvel por perto — ordens
  de magnitude mais requisições. Quem controla esse volume é `internal/grouping`
  (saídas curtas antes da chamada, raio e nº de candidatos). Não chame
  `MatchProperty` direto de um laço de coleta.
- `anthropic.NewClient` devolve um **valor**, não um ponteiro. Por isso
  `Client.api` é campo por valor e `API()` retorna `&c.api`. Evite copiar o
  nosso `Client` por valor se ele um dia ganhar mutex ou cache.
- O timeout de 60s cobre a chamada **inteira**, incluindo os retries automáticos
  do SDK — não cada tentativa isolada. É o tempo máximo que um domínio novo
  segura o worker.
- Os testes usam `newClient` (não exportado) com `option.WithBaseURL` apontando
  para um `httptest.Server`. As opções não são exportadas de propósito: nenhum
  pacote de negócio deve importar tipos do SDK.
- Trocar `selectorToolName` ou `propertyMatchToolName` exige trocar também o
  `ToolChoice` e o system prompt, que citam o nome literalmente.
- `slog.Debug` grava o prompt montado e a resposta bruta do matcher — é o
  material para calibrar `GROUPING_CONFIDENCE_THRESHOLD`. Em `debug` esses logs
  são volumosos; a API key nunca entra em log nem em mensagem de erro.
- `API()` continua exposto, mas prefira adicionar um método com intenção de
  negócio (como `AnalyzeSelectors`) a montar requisições no call site.
