# internal/cache

## Purpose
Cria e valida o client do Redis (`New`). É o equivalente de `db.Connect` para o
Redis: entrega uma conexão já testada ou nada. As operações de cache em si
(set/get, TTL, desenho das chaves, invalidação) **não** moram aqui — ficam nos
pacotes que consumirem o client.

## Key decisions
- **Folha do grafo de importação**, como `config` e `db`: importa apenas
  `go-redis` e a stdlib. A URL chega por parâmetro, nunca de `os.Getenv` nem de
  `config` — quem lê o ambiente é `internal/config`, e só ele.
- **Contrato espelhado de `db.Connect`.** Em caso de erro, `New` fecha o client
  antes de retornar; o chamador só registra `defer client.Close()` quando
  `err == nil`. Manter os dois idênticos evita que o `main` tenha que lembrar de
  duas regras diferentes de ciclo de vida.
- **`pingTimeout` de 5s**, o mesmo de `internal/db` e pelo mesmo motivo:
  `redis.NewClient` não conecta (o pool é preguiçoso), então o `Ping` é a
  primeira ida à rede. Sem o timeout, um host inalcançável travaria o startup
  até o timeout do sistema operacional.
- **`Ping` no boot.** Sem ele, uma `REDIS_URL` errada só apareceria na primeira
  operação de cache — possivelmente horas depois, no meio de uma coleta.
- **`endpoint(*redis.Options)` é uma função pura e existe por segurança**, não
  por estética: a descrição do destino é derivada do `*redis.Options` já
  parseado (`Addr`, `DB`), então `Password` fica de fora **por construção**.
  Montar a mensagem a partir da URL crua seria um vazamento a uma interpolação
  de distância.
- **`parseError` redige a URL do erro de parsing.** `redis.ParseURL` repassa o
  `*url.Error` de `url.Parse`, cujo campo `URL` é a string original inteira —
  com senha. Nesse caminho o erro original é **deliberadamente não embrulhado
  com `%w`**: mantê-lo acessível por `errors.As` devolveria a URL a qualquer
  handler que a logasse.

## Business logic / invariantes
- `New` devolve `(nil, err)` ou `(client válido, nil)` — nunca um client aberto
  junto de um erro.
- Nenhuma mensagem de erro deste pacote pode conter a `REDIS_URL` crua nem a
  senha. Há teste dedicado para isso (`TestNewDoesNotLeakPassword`,
  `TestEndpointOmitsPassword`) — a asserção é explícita porque o vazamento é
  invisível até aparecer nos logs de produção.
- O `ctx` recebido governa a validação inicial: em `main` é o contexto de
  sinais, para que SIGTERM aborte um `Ping` pendurado.

## Dependencies
`github.com/redis/go-redis/v9` e stdlib. Importado por `cmd/scraper`, que passa
`cfg.RedisURL` (`REDIS_URL`, obrigatória, sem default).

## Gotchas
- **Um client por processo.** O scraper é batch (uma execução = uma coleta e
  sai) e o `*redis.Client` já é seguro para uso concorrente e tem pool próprio —
  não crie um por worker.
- **Os consumidores do client são `internal/api`** (busca de imóveis,
  `properties:search:v1:`) **e `internal/scraper`** (resumo da última coleta,
  `scraper:last_run`, TTL de 48h). Nenhum dos dois importa este pacote: os dois
  recebem o `*redis.Client` já validado, do `main`. Chave, TTL e política de
  fallback são documentados **neles**, não aqui.
- **Falha de Redis é erro de boot (exit 1)** nos dois binários. Se um dia a
  preferência for degradar e subir sem cache, isso é mudança de comportamento e
  precisa de decisão explícita, não de um `if err != nil { log.Warn }` aqui.
- **Fora de escopo hoje:** TLS/`rediss://`, Sentinel/Cluster, tuning de pool e
  retry/backoff. Os defaults do go-redis valem até haver medição real.
- Os testes **não sobem um Redis**, seguindo o padrão de `internal/db`: cobrem
  as funções puras (parsing, redação, `endpoint`) e o caminho de host
  inalcançável em loopback, que é pulado com `-short`.
