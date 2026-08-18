# internal/robots

## Purpose
Avalia o `robots.txt` das fontes antes de qualquer coleta. Respeitar essas
regras é um requisito do projeto (coleta responsável), não uma otimização — o
scraper não deve buscar uma URL sem consultar este pacote primeiro.

## Business logic / invariantes
- **`Allowed` em `*Rules` nulo retorna `false`.** Esse é o comportamento
  deliberado e o mais importante do pacote: se não conseguimos avaliar o
  robots.txt (erro de rede, timeout, parsing impossível), o padrão é **não
  raspar**. Um default permissivo transformaria uma falha transitória de rede em
  violação das regras do site.
- **`AllowAll()` é para HTTP 404 e só para isso.** Pela especificação do
  robots.txt, ausência do arquivo significa acesso liberado. Um erro de rede
  **não** é ausência — nesse caso o chamador deve tratar como bloqueio (passar
  `nil` ou abortar), nunca converter em `AllowAll()`.
- A avaliação é por User-Agent: o mesmo path pode ser permitido para
  `ImobHubBot` e proibido para `*`. Sempre passe a string exata usada no header
  `User-Agent` das requisições (`httpclient.Client.UserAgent()`).

## Key decisions
- **Tipo `Rules` próprio em vez de expor `*robotstxt.RobotsData`.** Permite o
  comportamento seguro no nil receiver acima e mantém a troca da biblioteca como
  uma mudança local.
- `robotstxt.FromBytes` é tolerante a arquivos malformados (sites publicam
  robots.txt quebrado com frequência), então `Parse` raramente falha. Não trate
  o erro como "improvável, pode ignorar" — ele ainda precisa ir para o log.

## Gotchas
- O robots.txt é **por host** (`https://host/robots.txt`). Um `Rules` de um host
  não vale para outro — o cache futuro deve ser chaveado por scheme+host.
- `Crawl-delay` do robots.txt **não** é lido por este pacote. O espaçamento
  entre requisições vem de `SCRAPER_RATE_LIMIT_MS` via `internal/ratelimit`. Se
  passarmos a respeitar `Crawl-delay`, o valor deve ser lido aqui e alimentar o
  limiter (usando o maior entre os dois).

## Dependencies
`github.com/temoto/robotstxt`. Será importado por `internal/scraper`.
