# internal/scraper

## Purpose
Orquestrará a coleta das páginas das fontes: consulta ao robots.txt, rate
limiting, busca do HTML e extração dos dados. Ainda é um placeholder
(`doc.go`) — nenhuma lógica implementada.

## Key decisions
- **`RenderHTML` saiu daqui.** A renderização headless virou
  `httpclient.FetchHeadless`, ao lado de `httpclient.FetchStatic`. Manter duas
  implementações de chromedp (uma neste pacote, outra em `httpclient`) era
  garantia de divergência de timeout, flags e User-Agent; e a decisão
  "estático ou headless" é uma escolha de *como buscar a página*, que é
  exatamente a responsabilidade de `httpclient`.
- **Renderização headless continua opt-in.** O fluxo previsto é: tentar o
  caminho estático primeiro e só cair no headless quando o HTML inicial não
  contiver os dados — subir um Chrome é ordens de magnitude mais caro
  (memória, CPU, latência) que um GET.

## Gotchas
- O `doc.go` existe porque o Go exige um arquivo `.go` para que o diretório
  seja um pacote (e para que o git versione a pasta). Apagar sem substituir
  quebra `go build ./...`.

## Dependencies
Nenhuma hoje. Passará a importar `httpclient`, `robots`, `ratelimit`,
`sources` e `selectors` conforme a coleta for implementada.
