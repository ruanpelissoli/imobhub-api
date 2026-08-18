# internal/httpclient

## Purpose
Centraliza as requisições HTTP do scraper para que todas carreguem o mesmo
User-Agent e o mesmo timeout, e para que o parsing de HTML use sempre a mesma
biblioteca (`goquery`).

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

## Business logic / invariantes
- **`Get` não fecha o corpo da resposta.** O chamador é obrigado a fazer
  `defer resp.Body.Close()`; sem isso a conexão não volta para o pool e o
  processo vaza file descriptors ao longo de uma coleta longa.
- `Get` não interpreta o status code — um 404 ou 403 retorna `err == nil` com a
  resposta. Checar `resp.StatusCode` é responsabilidade de quem chama, porque a
  reação correta depende do contexto (404 num robots.txt significa "acesso
  liberado"; 404 numa página de imóvel significa anúncio removido).

## Gotchas
- `UserAgent()` existe para o pacote `robots`: as regras do robots.txt precisam
  ser avaliadas com **exatamente** a mesma string enviada no header, ou o bot
  pode obedecer a um bloco de regras diferente do que o site pretendia aplicar.
- Este client **não executa JavaScript**. Páginas que só montam o conteúdo no
  browser precisam de `internal/scraper.RenderHTML` (chromedp), que é ordens de
  magnitude mais caro — use apenas quando o HTML inicial vier vazio.

## Dependencies
`github.com/PuerkitoBio/goquery`, stdlib `net/http`. Será importado por
`internal/scraper`.
