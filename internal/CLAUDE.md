# internal/ — organização dos pacotes

## Purpose
Todo o código do ImobHub API vive sob `internal/` para que nada seja importável
por outros módulos: este é um binário, não uma biblioteca. `cmd/scraper` só
orquestra; a lógica está aqui.

## Key decisions
- **Um pacote por responsabilidade técnica**, não por camada. `robots`,
  `ratelimit` e `httpclient` são separados porque cada um tem uma política
  própria (respeito ao robots.txt, espaçamento entre requisições, User-Agent) e
  precisa ser testável isoladamente.
- **`config` é a única fronteira com o ambiente.** Nenhum outro pacote chama
  `os.Getenv`. Isso mantém a lista de variáveis auditável em um lugar só e
  permite testar os demais pacotes sem mexer no ambiente.
- **`ai` envolve o SDK da Anthropic.** Os pacotes de negócio dependem de `ai`,
  nunca de `github.com/anthropics/anthropic-sdk-go` diretamente, para que trocar
  de modelo ou provedor seja uma mudança local.
- **`pgxpool` em vez de `database/sql`.** O scraper vai rodar coletas
  concorrentes; `pgxpool` dá controle direto sobre o pool e acesso aos tipos
  nativos do PostgreSQL, que `database/sql` esconde atrás de `driver.Value`.

## Dependencies
Grafo de importação atual (mantê-lo acíclico e raso):

```
cmd/scraper → config, db, ratelimit
scraper     → db (models)
scraper     → httpclient, robots, ratelimit, selectors, sources   (futuro)
selectors   → ai, db, httpclient
ai          → db          (SelectorFields e as constantes de render mode)
robots      → net/http (client próprio, timeout de 10s para o robots.txt)
```

`robots` **não** importa `httpclient`: precisa de um timeout mais curto e o
grafo prevê o sentido contrário. O User-Agent chega como string (de `config` ou
de `httpclient.Client.UserAgent()`).

`config` não importa nada do projeto — é a folha do grafo.

## Gotchas
- `selectors/` já está implementado (`SelectorService`: reuso da linha
  de `site_selectors` e descoberta via `ai` quando ela falta ou está quebrada) —
  o `doc.go` de lá agora só carrega o doc do pacote.
  `scraper/` já tem a extração (`ExtractListings`), mas ainda não a
  orquestração. O `scraper.RenderHTML` que existia no scaffolding virou
  `httpclient.FetchHeadless` — busca de página (estática ou headless) é
  responsabilidade de `httpclient`. `sources/` já está implementado (`ReadSources`) — o `doc.go`
  dele deu lugar a `reader.go`, que carrega o doc do pacote.
- Logs operacionais usam `log/slog` (handler JSON configurado em `main`). Não
  usar `fmt.Println`/`log.Printf`: quebra o parsing dos logs em produção.
- Erros são embrulhados com `fmt.Errorf("pacote: ... %w", err)` e sempre citam o
  pacote de origem. Nunca inclua a `DATABASE_URL` completa numa mensagem — ela
  carrega a senha (ver `db/db.go`).
