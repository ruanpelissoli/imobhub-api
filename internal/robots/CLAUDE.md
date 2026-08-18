# internal/robots

## Purpose
Avalia o `robots.txt` das fontes antes de qualquer coleta. Respeitar essas
regras é um requisito do projeto (coleta responsável), não uma otimização — o
scraper não deve buscar uma URL sem consultar este pacote primeiro.

Duas camadas:
- `Rules` (`robots.go`) — parsing e avaliação de um robots.txt já em memória.
- `Checker` (`checker.go`) — busca o robots.txt por origem, cacheia e responde
  `IsAllowed(ctx, url)`. É a API que o scraper usa.

## Business logic / invariantes
- **Falha na busca ⇒ permitido (falha permissiva).** 404, 5xx, timeout, DNS
  quebrado, corpo ilegível: tudo vira `AllowAll()`. A ausência de robots.txt
  significa acesso liberado pela especificação, e bloquear um site inteiro por
  causa de um erro transitório de rede pararia a coleta do dia inteira. Essa é
  a decisão desta task (IMO-5) e substitui a política fail-closed anterior.
- **A decisão permissiva também é cacheada.** Se o robots.txt não respondeu uma
  vez, não tentamos de novo no mesmo processo — caso contrário cada URL do host
  pagaria um timeout de 10s.
- **Cache por origem (`scheme://host`), não por domínio nu.** `http` e `https`
  do mesmo host são origens distintas para o robots.txt (e a porta faz parte do
  host). O `Checker` busca o robots.txt no **mesmo scheme da URL avaliada**, não
  em `https` fixo, para não inventar um endpoint que pode não existir.
- **`Rules.Allowed` em receiver nulo retorna `false`.** O `Checker` nunca produz
  `*Rules` nulo; isso é uma rede de proteção para uso direto de `Rules`, onde um
  default permissivo esconderia um bug.
- A avaliação é por User-Agent: o mesmo path pode ser permitido para
  `ImobHubBot` e proibido para `*`. Passe sempre a string exata usada no header
  `User-Agent` das requisições (`cfg.ScraperUserAgent` /
  `httpclient.Client.UserAgent()`).
- `IsAllowed` só retorna erro para **URL inválida** (sem host, scheme fora de
  http/https, parsing impossível) — erro do chamador, validado na fronteira.
  Nesse caso o bool é `false`. Erro de rede nunca vira erro de retorno.

## Key decisions
- **`sync.Map` em vez de `map` + `RWMutex`.** O padrão é "escreve uma vez por
  host, lê muitas vezes por host", exatamente o caso em que `sync.Map` ganha.
- **Sem singleflight.** Duas goroutines podem buscar o mesmo robots.txt ao mesmo
  tempo; o custo máximo é uma requisição extra por host, contra a complexidade
  de coordenar buscas. `LoadOrStore` garante que só uma versão fica no cache.
- **Timeout próprio de 10s** (`FetchTimeout`), bem menor que o das páginas
  (`httpclient.DefaultTimeout`, 30s): robots.txt é um arquivo de texto pequeno.
- **`http.Client` próprio em vez de `httpclient.Client`.** Evita a dependência
  `robots → httpclient` (o grafo em `internal/CLAUDE.md` prevê o inverso) e
  permite o timeout mais curto. O User-Agent continua sendo enviado no header.
- **Tipo `Rules` próprio em vez de expor `*robotstxt.RobotsData`**, mantendo a
  troca da biblioteca como uma mudança local.
- Corpo lido com `io.LimitReader` de 512 KiB (mínimo da RFC 9309): um arquivo
  gigante não pode consumir memória do scraper.

## Gotchas
- `Crawl-delay` **não** é lido. O espaçamento vem de `SCRAPER_RATE_LIMIT_MS` via
  `internal/ratelimit`. Se passarmos a respeitá-lo, o valor deve ser lido aqui e
  alimentar o limiter (usando o maior entre os dois).
- O cache vive enquanto o processo vive — sem TTL. Suficiente para o MVP (um
  processo por run diária); uma alteração no robots.txt do site durante a run
  não é vista.
- O path avaliado inclui a query string (regras como `Disallow: /busca?*`).
- A busca do robots.txt **não passa pelo `ratelimit`**: é uma requisição por
  host, feita antes de tudo. Se isso mudar, cuidado com deadlock de ordem entre
  `Limiter.Wait` e o fetch.

## Dependencies
`github.com/temoto/robotstxt`, `net/http`, `log/slog`. Consumido por
`internal/scraper`; o User-Agent vem de `config.ScraperUserAgent`.
