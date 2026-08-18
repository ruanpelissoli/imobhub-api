# Build estático: CGO_ENABLED=0 para que o binário rode no alpine final sem
# depender da libc do builder.
FROM golang:1.25-alpine AS build

WORKDIR /src

# go.mod/go.sum primeiro: o download de dependências só refaz quando eles mudam.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/scraper ./cmd/scraper

# Runtime. O chromium é obrigatório: as fontes que só montam o conteúdo via
# JavaScript passam por httpclient.FetchHeadless, que procura o browser no PATH.
# nss/freetype/harfbuzz/ttf-freefont são as libs sem as quais o Chrome sobe e
# morre ao renderizar — o sintoma clássico de "funciona local, quebra no container".
FROM alpine:3.21

RUN apk add --no-cache \
        chromium \
        nss \
        freetype \
        harfbuzz \
        ttf-freefont \
        ca-certificates \
        tzdata

WORKDIR /app

COPY --from=build /out/scraper /app/scraper
# Migrations, fontes e o vocabulário de comodidades são lidos em runtime a
# partir do working directory (MIGRATIONS_DIR, SOURCES_FILE e AMENITIES_FILE são
# relativos a ele).
COPY migrations /app/migrations
COPY sources.txt /app/sources.txt
COPY configs /app/configs

# Roda como root de propósito: o Chrome do container sobe com --no-sandbox
# (ver internal/httpclient/headless.go), configuração que pressupõe root.
ENTRYPOINT ["/app/scraper"]
