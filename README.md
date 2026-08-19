# ImobHub API

Dois binários sobre o mesmo banco:

- **`cmd/scraper`** — processo batch. Lê a lista de fontes (`sources.txt`),
  respeita o `robots.txt` e o rate limit de cada domínio, busca as páginas,
  persiste os anúncios no PostgreSQL e os enriquece. **É o dono do schema**: as
  migrations rodam no startup dele.
- **`cmd/api`** — servidor HTTP de vida longa que serve o catálogo consolidado.
  **Não aplica migrations** (uma API de leitura que migra no boot mudaria o
  schema a cada deploy dela) e só conecta ao Postgres e ao Redis.

## Requisitos

- **Go 1.25+**
- **PostgreSQL** acessível pela `DATABASE_URL` (as migrations rodam no startup).
- **Redis** acessível pela `REDIS_URL`. A conexão é validada com um `PING` no
  startup: sem Redis no ar o processo sai com erro em vez de subir.
- **Chrome ou Chromium no PATH** — necessário apenas para as fontes que só
  montam o conteúdo via JavaScript, atendidas por `httpclient.FetchHeadless`.
  Sem o binário, essas fontes falham na alocação do browser (não é erro de
  rede); as demais continuam funcionando normalmente.

O chromedp procura, nessa ordem: `headless_shell`, `headless-shell`,
`chromium`, `chromium-browser`, `google-chrome` (Linux); `chrome.exe` e os
caminhos padrão do Program Files (Windows); `Chromium.app`/`Google Chrome.app`
(macOS).

Instalação do browser:

```bash
# Debian/Ubuntu
apt-get install -y chromium

# Alpine (imagens de container)
apk add --no-cache chromium

# macOS
brew install --cask chromium
```

Em container, além do binário, a imagem precisa das libs do Chrome — é o motivo
mais comum de o build funcionar local e a renderização falhar em produção.

## Configuração

Copie `.env.example` para `.env` e preencha os valores (na prática, só a
`ANTHROPIC_API_KEY`: os demais defaults já apontam para os serviços do
`docker-compose.yml`). As variáveis da aplicação são lidas por
`internal/config`; nenhum outro pacote chama `os.Getenv`. As variáveis
`POSTGRES_*`, `*_PORT` e `API_PORT` existem apenas para o compose.

Cada binário lê o subconjunto de que precisa: `cmd/scraper` usa `config.Load()`
(exige `ANTHROPIC_API_KEY`) e `cmd/api` usa `config.LoadAPI()` (exige apenas
`DATABASE_URL` e `REDIS_URL`, mais `PORT` e `CORS_ORIGINS` opcionais).

## Ambiente local com Docker

O `docker-compose.yml` sobe a stack completa: `db` (PostgreSQL), `redis`,
`scraper` (o batch de coleta) e `api` (o servidor HTTP).

```bash
cp .env.example .env               # preencha ANTHROPIC_API_KEY
docker compose up -d db redis      # só a infraestrutura
docker compose run --rm scraper    # executa uma coleta dentro do container
docker compose up -d api           # sobe a API em http://localhost:8080
docker compose down                # para tudo (-v também apaga os volumes)
```

> O serviço batch **se chamava `api`** e passou a se chamar `scraper`. Quem
> automatizou `docker compose run --rm api` esperando uma coleta precisa
> atualizar o nome — hoje `api` é o servidor HTTP.

O `scraper` é um processo batch: uma execução coleta todas as fontes e sai — por
isso `restart: "no"` e o uso de `run` em vez de um serviço sempre ligado. A
`api` é de vida longa (`restart: unless-stopped`) e publica a porta no host via
`API_PORT` (default `8080`). Os dois só iniciam depois que os healthchecks de
`db` e `redis` passam.

O mesmo `.env` serve para os dois modos de execução: os valores apontam para
`localhost` (para `go run` no host) e os serviços do compose sobrescrevem
`DATABASE_URL`/`REDIS_URL` com os nomes de host da rede do compose.

## Executar

Com a infra do compose no ar (ou um Postgres próprio na `DATABASE_URL`):

```bash
go run ./cmd/scraper                 # coleta + enriquecimento (default)
go run ./cmd/scraper -only=scrape    # só a coleta
go run ./cmd/scraper -only=enrich    # só a fila de enriquecimento
```

### API HTTP

```bash
docker compose up -d db redis
go run ./cmd/api
```

Sobe em `PORT` (default `8080`). Exige apenas `DATABASE_URL` e `REDIS_URL` —
`ANTHROPIC_API_KEY` e as demais variáveis do scraper não são necessárias.
Postgres ou Redis fora do ar, ou `PORT` inválida/ocupada, são erro de boot com
exit 1, nunca fallback silencioso.

```bash
curl -i localhost:8080/health          # 200 {"status":"ok"}
curl -i localhost:8080/api/v1/nada     # 404 {"error":"not found"}
curl -i -X POST localhost:8080/health  # 405 {"error":"method not allowed"}

curl -i -X OPTIONS localhost:8080/api/v1/x \
  -H 'Origin: http://localhost:3000' \
  -H 'Access-Control-Request-Method: GET'   # 204 com os headers de CORS
```

O `/health` é **liveness**: responde 200 sem tocar em Postgres ou Redis, para
que uma oscilação do banco não faça o orquestrador reiniciar uma API viva. As
rotas de negócio ficam sob `/api/v1`; SIGINT/SIGTERM disparam shutdown gracioso
(as requisições em voo terminam antes de o processo sair).

Uma execução aplica as migrations pendentes e roda duas etapas em sequência:

1. **Coleta** — todas as fontes do `sources.txt`, uma de cada vez: robots.txt,
   rate limiting por domínio, seletores (reusados do banco ou descobertos via
   Claude), extração e sincronização com a tabela `listings`.
2. **Enriquecimento** — os anúncios pendentes (`enriched_at` nulo ou anterior à
   última alteração) passam por normalização de bairro, extração de quartos e
   comodidades, geocodificação, agrupamento no imóvel canônico e consolidação de
   `properties`. Roda num worker pool de `ENRICHMENT_WORKERS` (default 4) e só
   começa se a coleta tiver terminado com sucesso.

Ao final de cada etapa, o resumo sai no log. Uma fonte ou um anúncio que falha
não interrompe os demais: fica registrado no resumo e é retomado no próximo run.
O processo sai com 1 em erro fatal (config, banco, arquivo de fontes ilegível) —
e também quando a fila de enriquecimento falha, **sem** que isso signifique
perda da coleta, que já foi persistida.

> O enriquecimento faz uma chamada **paga** à Anthropic por anúncio que chega com
> coordenadas e algum imóvel já cadastrado por perto. As variáveis
> `GROUPING_RADIUS_METERS` e `GROUPING_MAX_CANDIDATES` são o controle de custo.

## Testes

```bash
go test ./...          # inclui o teste que sobe um Chrome headless de verdade
go test -short ./...   # pula browser e os testes que exigem PostgreSQL
```

Testes que exigem browser são **pulados** (não falham) quando não há Chrome
instalado. O teste de integração da fila de enriquecimento usa o Postgres do
`docker-compose.yml` e é pulado quando `DATABASE_URL` não está definida.

## Estrutura

Todo o código vive em `internal/` — este repositório é um binário, não uma
biblioteca. Cada diretório tem um `CLAUDE.md` com as decisões daquele pacote;
comece por `internal/CLAUDE.md` para o grafo de dependências.
