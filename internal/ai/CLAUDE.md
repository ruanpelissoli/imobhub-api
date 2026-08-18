# internal/ai

## Purpose
Encapsula a integração com a API da Anthropic. Hoje expõe um fluxo de negócio:
`AnalyzeSelectors`, que recebe o HTML de uma página de listagens e devolve os
seletores CSS de extração (`db.SelectorFields`) mais o `renderMode` que aquele
site exige. É usado pelo pacote `selectors` quando aparece um domínio sem
mapeamento conhecido.

## Key decisions
- **Wrapper em vez de uso direto do SDK.** Os pacotes de negócio dependem de
  `ai`, nunca de `github.com/anthropics/anthropic-sdk-go`. Trocar de modelo ou
  de provedor vira uma mudança local em vez de um sweep pelo repositório.
- **Dois modelos, dois papéis.** `DefaultModel` (Opus) é o padrão do `Client`;
  `SelectorModel` (`claude-haiku-4-5`) é usado só na análise de seletores.
  Identificar classes CSS num HTML é extração, não raciocínio, e roda uma vez
  por domínio novo — o Haiku dá o mesmo resultado por uma fração do custo. Por
  isso `analyzeSelectors` ignora `c.model` de propósito.
- **`tool_use` em vez de JSON em texto livre.** O modelo é obrigado a chamar
  `report_listing_selectors` via `ToolChoice`. Parsear JSON de prosa quebra
  quando o modelo enfeita a resposta com ```json ou um parágrafo introdutório;
  com a ferramenta, o input já chega como JSON válido e validado pelo schema.
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
  tarde" (falha de rede).

## Dependencies
`github.com/anthropics/anthropic-sdk-go` (+ `/option`) e `internal/db` (para
`SelectorFields` e as constantes de render mode). Importado por
`internal/selectors` (`SelectorService`), que é quem decide **quando** pagar uma
análise: só em fonte nova ou com seletores quebrados.

## Gotchas
- **Este pacote gasta dinheiro.** `AnalyzeSelectors` faz uma chamada real e
  paga. Chame uma vez por domínio novo e persista o resultado em
  `site_selectors`; não coloque no caminho de cada coleta.
- `anthropic.NewClient` devolve um **valor**, não um ponteiro. Por isso
  `Client.api` é campo por valor e `API()` retorna `&c.api`. Evite copiar o
  nosso `Client` por valor se ele um dia ganhar mutex ou cache.
- O timeout de 60s cobre a chamada **inteira**, incluindo os retries automáticos
  do SDK — não cada tentativa isolada. É o tempo máximo que um domínio novo
  segura o worker.
- Os testes usam `newClient` (não exportado) com `option.WithBaseURL` apontando
  para um `httptest.Server`. As opções não são exportadas de propósito: nenhum
  pacote de negócio deve importar tipos do SDK.
- Trocar `selectorToolName` exige trocar também o `ToolChoice` e o system
  prompt, que citam o nome literalmente.
- `API()` continua exposto, mas prefira adicionar um método com intenção de
  negócio (como `AnalyzeSelectors`) a montar requisições no call site.
