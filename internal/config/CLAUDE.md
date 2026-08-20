# internal/config

## Purpose
Lê e valida toda a configuração de runtime a partir de variáveis de ambiente.
É a única fronteira do projeto com `os.Getenv` — nenhum outro pacote deve ler o
ambiente diretamente.

## Business logic
- **Dois loaders, um por binário:** `Load()` para `cmd/scraper` e `LoadAPI()`
  para `cmd/api`. Eles compartilham `lookup` e `ErrMissingRequired`, mas têm
  listas de obrigatórias diferentes.
- **Obrigatórias de `Load()` (scraper):** `DATABASE_URL`, `REDIS_URL`,
  `ANTHROPIC_API_KEY`. Sem elas o processo não sobe.
- **Obrigatórias de `LoadAPI()`:** apenas `DATABASE_URL` e `REDIS_URL`.
  Opcionais: `PORT` (default `8080`), `CORS_ORIGINS` (sem default — vazia
  desliga o CORS) e `RATE_LIMIT_RPM` (default `60`).
- **`RATE_LIMIT_RPM` é o teto de requisições por minuto por IP** aplicado pelo
  middleware inbound de `internal/api`. `0` **desliga** o limitador (é a forma
  documentada de desativá-lo em testes locais); negativo ou não numérico é erro
  de boot citando o valor recebido. A regra vive em `parseNonNegativeInt`, que
  existe em vez de reusar `parseRateLimitMS` só porque a mensagem daquele cita
  "milliseconds" — enganosa para um limite contado em requisições. Note o
  contraste com `parsePositiveInt`, que **rejeita** zero.
- **`LoadAPI()` existe em vez de afrouxar `Load()`.** Tornar
  `ANTHROPIC_API_KEY` opcional em `Load()` resolveria o boot da API ao custo de
  o scraper subir sem chave e quebrar só na primeira chamada de IA, no meio de
  uma coleta — regressão silenciosa no processo que de fato paga por ela.
- **`PORT` fora de `[1, 65535]` ou não numérica é erro de boot**, citando o
  valor recebido (`parsePort`). Nada de fallback silencioso: um `PORT=8O80`
  (letra no lugar do zero) faria o servidor subir na 8080 enquanto o front
  continuaria batendo na porta errada, sem uma linha de log explicando.
- **`CORS_ORIGINS` é uma lista separada por vírgula** (`parseCORSOrigins`, pura
  e testável): cada entrada sofre `TrimSpace` e as vazias são descartadas — uma
  vírgula sobrando viraria uma origem `""` que casaria com requisições sem
  header `Origin`. Ausente ou vazia devolve `nil`, e `internal/api` trata `nil`
  como "CORS desligado".
- **`REDIS_URL` não tem default**, apesar de `redis://localhost:6379/0` ser o
  valor óbvio em dev. Pela mesma regra das demais variáveis do projeto (nada de
  fallback silencioso): um localhost implícito faria o processo apontar para um
  Redis que não é o do ambiente — e cache no lugar errado é o tipo de erro que
  só aparece como "dado velho" muito depois. Quem consome a URL é
  `internal/cache`; `config` não valida formato nem conectividade.
- **Obrigatória condicional:** `GEOCODING_API_KEY`, exigida **apenas** quando
  `GEOCODING_PROVIDER=googlemaps` (o Nominatim é gratuito e não aceita chave).
  Por isso o provider é resolvido **antes** do relatório de faltantes — a falta
  da chave entra na mesma lista acumulada das demais obrigatórias.
- **Opcionais com default:** `SOURCES_FILE` (`sources.txt`),
  `SCRAPER_USER_AGENT` (`ImobHubBot/1.0`), `SCRAPER_RATE_LIMIT_MS` (`2000`),
  `MIGRATIONS_DIR` (`migrations`), `GEOCODING_PROVIDER` (`nominatim`),
  `GEOCODING_RATE_LIMIT_MS` (`1000`, limite de 1 req/s do Nominatim),
  `AMENITIES_FILE` (`configs/amenities.yaml` — vocabulário de comodidades de
  `internal/enrichment`), `GROUPING_CONFIDENCE_THRESHOLD` (`0.85`),
  `GROUPING_RADIUS_METERS` (`100`) e `GROUPING_MAX_CANDIDATES` (`5`), lidas por
  `internal/grouping`, e `ENRICHMENT_WORKERS` (`4`), lida por
  `internal/enrichqueue`.
- **`ENRICHMENT_WORKERS` também passa por `parsePositiveInt`, então zero é
  erro.** Uma fila com zero workers nunca processa nada e parece "funcionando"
  nos logs — o mesmo sintoma silencioso de raio/candidatos zerados. O default 4
  é conservador de propósito: a geocodificação serializa em 1 req/s global, e
  subir esse número só paraleliza as chamadas **pagas** de IA do agrupamento.
- **As três variáveis de agrupamento são validadas no boot, sem fallback
  silencioso.** Threshold fora de `[0,1]`, raio `<= 0` ou candidatos `<= 0` são
  erro, citando o valor recebido. Motivo: um threshold de `8.5` (vírgula no
  lugar do ponto) viraria "nunca agrupa" e raio/candidatos zerados desligariam
  o agrupamento — os três casos parecem "funcionando" nos logs enquanto criam um
  imóvel canônico duplicado por anúncio, pela coleta inteira. `parseUnitInterval`
  e `parsePositiveInt` carregam essas regras; note que `parsePositiveInt`
  **rejeita zero**, ao contrário de `parseRateLimitMS`, onde zero é a forma
  documentada de desativar o espaçamento.
- **`GEOCODING_PROVIDER` fora de `{nominatim, googlemaps}` é erro de boot**,
  nunca fallback silencioso: quem digitou "google" errado esperava as cotas e a
  precisão do Google e receberia, sem aviso, as coordenadas do Nominatim. O
  valor é normalizado (trim + lower) antes de validar.
- Os dois rate limits (`SCRAPER_RATE_LIMIT_MS` e `GEOCODING_RATE_LIMIT_MS`) são
  convertidos para `time.Duration` aqui, não nos chamadores: quem consome a
  config recebe uma duração já tipada e não precisa saber que a unidade original
  era milissegundo. `0` é aceito e desativa o rate limiting; negativo é erro. As
  regras de parsing vivem em `parseRateLimitMS`, compartilhado pelas duas
  variáveis para que não divirjam.

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
Importado por `cmd/scraper` e `cmd/api`.

## Gotchas
- Ao adicionar uma variável, atualize **três** lugares: a constante `env*`, a
  struct (`Config` ou `APIConfig`) e o `.env.example` na raiz. O `.env.example`
  é a documentação de fato para quem sobe o projeto.
- **Variável nova que o scraper exige não pode entrar em `LoadAPI()`** sem
  decisão explícita: cada obrigatória adicionada lá é mais uma coisa que impede
  a API de subir.
- `Config` não é validada além do parsing (ex.: não checamos se a
  `DATABASE_URL`/`REDIS_URL` são alcançáveis nem se `MIGRATIONS_DIR` existe).
  Essa validação é do `internal/db` (que faz `Ping` na conexão e lê o diretório
  de migrations) e do `internal/cache` (que faz `PING` no Redis).
- Caminhos (`SOURCES_FILE`, `MIGRATIONS_DIR`, `AMENITIES_FILE`) são usados como
  vieram: relativos ao **working directory do processo**, não à raiz do
  repositório. No container, o `Dockerfile` precisa copiar o arquivo/diretório
  correspondente para `/app` — senão o default aponta para algo inexistente.
- `ProviderNominatim`/`ProviderGoogleMaps` existem **também** em
  `internal/enrichment`. A duplicação é consciente: `config` é folha do grafo e
  não pode importar `enrichment`. `config` valida no boot, `enrichment.NewGeocoder`
  valida de novo — defesa em profundidade. Ao adicionar um provider, mexa nos dois.
- `GEOCODING_API_KEY` nunca aparece em log nem em mensagem de erro. `Config` é
  logada em lugar nenhum; se isso mudar, redija a chave (mesma regra da
  `DATABASE_URL` e da `REDIS_URL`, que carregam a senha).
