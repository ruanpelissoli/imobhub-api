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

Copie `.env.example` para `.env` e preencha os valores. Todas as variáveis são
lidas por `internal/config`; nenhum outro pacote chama `os.Getenv`.

## Executar

```bash
go run ./cmd/scraper
```

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
