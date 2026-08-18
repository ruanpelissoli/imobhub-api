# ImobHub API

Coletor de anúncios de imóveis: lê a lista de fontes (`sources.txt`), respeita o
`robots.txt` e o rate limit de cada domínio, busca as páginas e persiste os
anúncios no PostgreSQL.

## Requisitos

- **Go 1.25+**
- **PostgreSQL** acessível pela `DATABASE_URL` (as migrations rodam no startup).
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
`POSTGRES_*` e `*_PORT` existem apenas para o compose.

## Ambiente local com Docker

O `docker-compose.yml` sobe a stack completa: `db` (PostgreSQL), `redis` e
`api` (a aplicação).

```bash
cp .env.example .env            # preencha ANTHROPIC_API_KEY
docker compose up -d db redis   # só a infraestrutura
docker compose run --rm api     # executa uma coleta dentro do container
docker compose down             # para tudo (-v também apaga os volumes)
```

A `api` é um processo batch: uma execução coleta todas as fontes e sai — por
isso `restart: "no"` e o uso de `run` em vez de um serviço sempre ligado. Ela só
inicia depois que os healthchecks de `db` e `redis` passam, porque as migrations
rodam no startup.

O mesmo `.env` serve para os dois modos de execução: os valores apontam para
`localhost` (para `go run` no host) e o serviço `api` sobrescreve
`DATABASE_URL`/`REDIS_URL` com os nomes de host da rede do compose.

> O Redis ainda não é consumido por nenhum pacote Go — está no compose como
> infraestrutura prevista para cache/fila.

## Executar

Com a infra do compose no ar (ou um Postgres próprio na `DATABASE_URL`):

```bash
go run ./cmd/scraper
```

Uma execução aplica as migrations pendentes e coleta **todas** as fontes do
`sources.txt`, uma de cada vez: robots.txt, rate limiting por domínio,
seletores (reusados do banco ou descobertos via Claude), extração e
sincronização com a tabela `listings`. Ao final, o resumo do run sai no log.
Uma fonte que falha não interrompe as demais e **não** muda o exit code — o
processo só sai com 1 em erro fatal (config, banco, arquivo de fontes ilegível).

## Testes

```bash
go test ./...          # inclui o teste que sobe um Chrome headless de verdade
go test -short ./...   # pula os testes que dependem de browser
```

Testes que exigem browser são **pulados** (não falham) quando não há Chrome
instalado.

## Estrutura

Todo o código vive em `internal/` — este repositório é um binário, não uma
biblioteca. Cada diretório tem um `CLAUDE.md` com as decisões daquele pacote;
comece por `internal/CLAUDE.md` para o grafo de dependências.
