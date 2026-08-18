# internal/scraper

## Purpose
Orquestrará a coleta das páginas das fontes: consulta ao robots.txt, rate
limiting, busca do HTML e extração dos dados. Nesta task de scaffolding contém
apenas `RenderHTML`, o renderizador headless para páginas que dependem de
JavaScript.

## Key decisions
- **Renderização headless é opt-in, não o caminho padrão.** Subir um Chrome é
  ordens de magnitude mais caro (memória, CPU, latência) que um GET simples. O
  fluxo correto é: tentar `httpclient.Get` + `httpclient.ParseHTML` primeiro, e
  só cair em `RenderHTML` quando o HTML inicial não contiver os dados.
- **`DefaultRenderTimeout` de 45s**, maior que o timeout HTTP (30s) porque a
  renderização inclui carregar a página *e* executar seus scripts. Sem esse
  teto, uma página que nunca termina de carregar (polling infinito, script
  travado) seguraria o worker para sempre.
- **User-Agent injetado via `chromedp.UserAgent`**, e não deixado no default do
  Chrome. Precisa ser a mesma string usada no `httpclient` e na avaliação do
  robots.txt — coerência de identificação do bot.

## Gotchas
- **`RenderHTML` exige Chrome/Chromium instalado na máquina.** A ausência
  aparece como erro na alocação do contexto, não como erro de rede. Em
  containers, a imagem precisa incluir o binário e as libs do Chrome — é o
  principal motivo de o build funcionar local e falhar em produção.
- Os três `cancel` (alocador, browser, timeout) precisam rodar em ordem inversa;
  os `defer` já garantem isso. Esquecer um deixa processos `chrome.exe` órfãos
  acumulando entre execuções.
- `chromedp.OuterHTML("html", &html)` captura o DOM **no momento da chamada**.
  Páginas que carregam conteúdo depois (scroll infinito, XHR tardio) exigem uma
  ação `chromedp.WaitVisible`/`WaitReady` antes — sem isso o HTML volta
  incompleto e a extração falha silenciosamente.

## Dependencies
`github.com/chromedp/chromedp`. Passará a importar `httpclient`, `robots`,
`ratelimit`, `sources` e `selectors` conforme a coleta for implementada.
