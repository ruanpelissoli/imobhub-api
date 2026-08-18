# internal/httpclient

## Purpose
Centraliza as requisições HTTP do scraper para que todas carreguem o mesmo
User-Agent e o mesmo timeout, e para que o parsing de HTML use sempre a mesma
biblioteca (`goquery`). Expõe os dois modos de busca de página: `Get`/`ParseHTML`
(estático) e `FetchHeadless` (Chrome headless, para páginas que só montam o
conteúdo via JavaScript).

Duas portas de entrada, com contratos diferentes:
- `Client.Get` — GET cru, devolve `*http.Response` (usado quando o chamador
  precisa de headers/status ou quer streamar o corpo).
- `FetchStatic` — busca a página inteira e devolve o HTML já em UTF-8 mais a URL
  final. É o caminho padrão para páginas que não dependem de JavaScript.

## Key decisions
- **Wrapper fino, não abstração.** `Get` devolve `*http.Response` cru em vez de
  um tipo próprio: o chamador precisa do status code e dos headers, e esconder
  isso só criaria trabalho de tradução.
- **User-Agent no client, não no call site.** Ele é obrigatório em todas as
  requisições (identificação do bot) e é o mesmo usado na avaliação do
  robots.txt. Deixá-lo no construtor torna impossível esquecer.
- **`DefaultTimeout` de 30s.** Portais imobiliários com muito JavaScript e
  imagens são lentos; um timeout curto geraria falsos negativos. Ainda assim é
  um teto rígido — sem ele um host pendurado seguraria o worker indefinidamente.
- **Estático e headless no mesmo pacote.** A renderização headless morava em
  `internal/scraper.RenderHTML`; foi trazida para cá para não existirem duas
  implementações de chromedp e para que a escolha "estático ou headless" seja
  uma decisão só, com o mesmo User-Agent nos dois caminhos.
- **Um browser por chamada em `FetchHeadless`.** Sem pool: nenhum estado
  (cookies, cache, service worker) vaza de um site para outro e não há processo
  de longa duração para vazar memória. O custo (~1s de subida) é aceitável
  porque headless é usado só na minoria das fontes que exige JS.
- **`FetchStatic` cria um `http.Client` por chamada**, sem cookie jar. O scraper
  visita muitos domínios; um cliente compartilhado carregaria estado de sessão de
  um site para outro. O custo é irrelevante: o `Transport` padrão é global, então
  o pool de conexões continua sendo reaproveitado entre chamadas.
- **`FetchStatic` decodifica o corpo com `x/net/html/charset`.** Portais
  brasileiros antigos ainda servem ISO-8859-1; ler esses bytes como UTF-8
  corromperia acentos exatamente nos campos extraídos (bairro, descrição).
- **`StatusError` em vez de erro opaco.** A reação a um 4xx depende do código
  (404 = anúncio removido, 403/429 = bloqueio), então o status viaja no erro e é
  recuperável com `errors.As` / `IsStatus(err, code)`.

## Business logic / invariantes
- **`Get` não fecha o corpo da resposta.** O chamador é obrigado a fazer
  `defer resp.Body.Close()`; sem isso a conexão não volta para o pool e o
  processo vaza file descriptors ao longo de uma coleta longa. `FetchStatic`,
  ao contrário, é self-contained: lê tudo e fecha.
- `Get` não interpreta o status code — um 404 ou 403 retorna `err == nil` com a
  resposta. Checar `resp.StatusCode` é responsabilidade de quem chama, porque a
  reação correta depende do contexto (404 num robots.txt significa "acesso
  liberado"; 404 numa página de imóvel significa anúncio removido).
  **`FetchStatic` faz o oposto**: status >= 400 vira `*StatusError` e o HTML sai
  vazio.
- `finalURL` é preenchido mesmo em erro de status — é a URL após redirecionamentos
  e serve para identificar de onde veio o 404.
- Corpo vazio (204 ou 200 sem conteúdo) **não** é erro: retorna `("", url, nil)`.
  Quem extrai decide se a página vazia invalida a coleta.
- **`FetchHeadless` espera pelo `networkIdle`, mas não o exige.** O orçamento
  total é `HeadlessTimeout` (20s); a espera pelo evento termina em
  `HeadlessTimeout - captureReserve` para sobrar tempo de capturar o DOM.
  Não alcançar o `networkIdle` **não** é erro: muitos portais mantêm websocket
  ou polling de analytics abertos e nunca ficam ociosos — exigir o evento
  falharia justamente nos sites que precisam de headless. O caso vira um
  `slog.Warn` e o DOM atual é devolvido. Falha de navegação e estouro do
  orçamento de 20s, esses sim, viram erro.

## Gotchas
- `UserAgent()` existe para o pacote `robots`: as regras do robots.txt precisam
  ser avaliadas com **exatamente** a mesma string enviada no header, ou o bot
  pode obedecer a um bloco de regras diferente do que o site pretendia aplicar.
- Este pacote **não executa JavaScript** via `FetchStatic`. Páginas que só montam
  o conteúdo no browser precisam de `FetchHeadless` (chromedp), que é ordens de
  magnitude mais caro — use apenas quando o HTML inicial vier vazio.
- **`FetchHeadless` exige Chrome/Chromium no PATH** (ver README). A ausência
  aparece como erro na alocação do browser, não como erro de rede — é o motivo
  clássico de "funciona local, quebra no container".
- **O listener de `networkIdle` é registrado antes do `Navigate`** e só aceita o
  evento depois de um `frameNavigated` do frame principal. Registrar depois do
  Navigate perde o evento em páginas rápidas (espera os 17s à toa); aceitar sem
  o gate pega o `networkIdle` do `about:blank` inicial e devolve página em
  branco. Mexer nessa ordem quebra um dos dois casos.
- Os `cancel` (alocador, browser, timeout) rodam em ordem inversa via `defer`.
  Esquecer um deixa processos `chrome` órfãos acumulando entre coletas.
- O teste que sobe browser de verdade (`TestFetchHeadlessRendersJavaScript`) é
  pulado quando não há Chrome instalado ou em `-short`. Os demais testes do
  headless não sobem browser — mantenha assim.
- **Não setar `Accept-Encoding` manualmente.** O `net/http` só descomprime gzip
  de forma transparente quando é ele quem anuncia o header; setá-lo à mão faz o
  corpo chegar comprimido e o parsing falhar silenciosamente.
- Nenhum header além do User-Agent é enviado, de propósito: `Accept-Language`,
  `DNT` e afins são bits de fingerprint sem ganho para a coleta.
- `FetchStatic` **não limita o tamanho do corpo**. Uma página patológica de
  centenas de MB é lida inteira na memória. Se isso aparecer em produção, o lugar
  de corrigir é aqui (com `io.LimitReader` + erro explícito, nunca truncando em
  silêncio: HTML cortado gera extração errada em vez de falha).

## Dependencies
`github.com/PuerkitoBio/goquery`, `github.com/chromedp/chromedp` (+ `cdproto` para
os eventos de ciclo de vida), `golang.org/x/net/html/charset` (que puxa
`golang.org/x/text` para as tabelas de encoding), stdlib `net/http`. Importado
por `internal/selectors` (que embrulha `FetchStatic`/`FetchHeadless` nos
adaptadores `StaticFetcher`/`HeadlessFetcher`) e, futuramente, por
`internal/scraper`.
