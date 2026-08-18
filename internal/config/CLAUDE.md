# internal/config

## Purpose
Lê e valida toda a configuração de runtime a partir de variáveis de ambiente.
É a única fronteira do projeto com `os.Getenv` — nenhum outro pacote deve ler o
ambiente diretamente.

## Business logic
- **Obrigatórias:** `DATABASE_URL`, `ANTHROPIC_API_KEY`. Sem elas o processo não
  sobe.
- **Opcionais com default:** `SOURCES_FILE` (`sources.txt`),
  `SCRAPER_USER_AGENT` (`ImobHubBot/1.0`), `SCRAPER_RATE_LIMIT_MS` (`2000`).
- `SCRAPER_RATE_LIMIT_MS` é convertido para `time.Duration` aqui, não nos
  chamadores: quem consome a config recebe uma duração já tipada e não precisa
  saber que a unidade original era milissegundo. `0` é aceito e desativa o rate
  limiting; negativo é erro.

## Key decisions
- **Variável vazia == ausente.** `lookup` trata `""` como não definida e aplica
  o default. Motivo: `docker-compose` e runners de CI exportam variáveis vazias
  com frequência (`SOURCES_FILE=` numa lista de env), e sem esse tratamento uma
  variável vazia desativaria silenciosamente o default. O valor também sofre
  `TrimSpace` — arquivos `.env` costumam trazer espaços acidentais.
- **Erros de obrigatórias são acumulados**, não retornados no primeiro. Um
  operador que esqueceu duas variáveis descobre as duas de uma vez em vez de
  corrigir, redeployar e falhar de novo. Detectável com
  `errors.Is(err, ErrMissingRequired)`.
- **Nomes das variáveis são constantes não exportadas** (`envDatabaseURL` etc.),
  usadas tanto no código quanto nos testes. Evita divergência entre a string
  lida e a citada na mensagem de erro.

## Dependencies
Apenas a stdlib. Não importa nada do projeto — é folha do grafo de importação.
Importado por `cmd/scraper`.

## Gotchas
- Ao adicionar uma variável, atualize **três** lugares: a constante `env*`, a
  struct `Config` e o `.env.example` na raiz. O `.env.example` é a documentação
  de fato para quem sobe o projeto.
- `Config` não é validada além do parsing (ex.: não checamos se a
  `DATABASE_URL` é alcançável). Essa validação é do `internal/db`, que faz
  `Ping` na conexão.
