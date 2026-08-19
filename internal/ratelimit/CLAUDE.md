# internal/ratelimit

## Purpose
Garante um intervalo mínimo entre requisições para o mesmo domínio. Protege as
fontes de sobrecarga e reduz o risco de o bot ser bloqueado. API única:
`DomainLimiter` (`limiter.go`), criado com `NewDomainLimiter(intervalMs)`.

## Business logic / invariantes
- **O espaçamento é por domínio** (na prática, o host da URL). Domínios
  diferentes têm relógios independentes e nunca esperam uns pelos outros — sem
  isso, coletar 10 fontes em paralelo seria tão lento quanto coletá-las em série.
- **O slot é reservado antes da espera, ainda sob o lock.** Essa é a parte
  sutil: `Wait` grava `next[domain]` e só então libera o mutex e dorme. Se a
  reserva acontecesse depois da espera, N goroutines para o mesmo domínio leriam
  o mesmo horário, dormiriam o mesmo tempo e disparariam **juntas** — exatamente
  o burst que o limiter existe para evitar. Do jeito atual elas se enfileiram.
  Para o uso sequencial do MVP o efeito é idêntico a "marcar o timestamp quando
  `Wait` retorna".
- **O domínio é normalizado** (`TrimSpace` + `ToLower`). Nomes de host são
  case-insensitive; sem isso `Exemplo.com.br` e `exemplo.com.br` teriam relógios
  separados e o servidor receberia o dobro das requisições.
- **`intervalMs <= 0` desativa o limiter**, e `Wait` retorna `ctx.Err()`
  imediatamente. Corresponde a `SCRAPER_RATE_LIMIT_MS=0`; é para testes locais,
  não para produção.
- `Wait` respeita o cancelamento do contexto e retorna o erro do contexto. Uma
  espera longa não impede o encerramento do processo em SIGTERM.

## Key decisions
- **Timer + `select` em vez de `time.Sleep`.** `time.Sleep` não é interrompível:
  com ele o contexto só seria observado depois do intervalo inteiro, e um
  SIGTERM ficaria preso atrás da espera. O `select` sobre `timer.C` e
  `ctx.Done()` entrega o mesmo espaçamento e continua cancelável.
- **`NewDomainLimiter` recebe milissegundos (`int`), não `time.Duration`.**
  Assinatura definida pela task (IMO-6), para espelhar a unidade de
  `SCRAPER_RATE_LIMIT_MS`. `config` continua expondo `ScraperRateLimit` como
  `time.Duration` (decisão anterior, ver `internal/config/CLAUDE.md`), então
  `main.go` converte com `.Milliseconds()`. Se um dia só houver um chamador de
  peso, vale unificar em `time.Duration`.
- **Implementação própria em vez de `golang.org/x/time/rate`.** O `rate.Limiter`
  modela token bucket com burst; o que precisamos é o oposto — espaçamento fixo,
  sem burst algum. Além disso precisaríamos de um mapa de limiters por domínio
  de qualquer forma, então a economia seria pequena.
- **`sync.Mutex` simples, não `RWMutex`.** Toda operação é leitura seguida de
  escrita no mapa; não há caminho somente-leitura para o `RWMutex` acelerar.
- `NewDomainLimiter` é obrigatório: o zero value não inicializa o mapa e
  causaria panic na escrita.

## Gotchas
- **O mapa `next` cresce indefinidamente.** Uma entrada por domínio visto, nunca
  removida. Aceitável enquanto a lista de fontes for da ordem de dezenas; se o
  scraper passar a seguir links para domínios arbitrários, adicione uma limpeza
  de entradas cujo horário já passou há muito.
- `Wait` bloqueia. Chame-o imediatamente antes da requisição, não no início de
  um pipeline — o tempo gasto entre o `Wait` e o `Get` sai do intervalo efetivo.
- **Contexto cancelado durante a espera não devolve o slot.** O horário já foi
  reservado, então a próxima requisição àquele domínio pode esperar um intervalo
  "à toa". Erra para o lado educado; não vale a complexidade de reverter.
- Os testes deste pacote são baseados em tempo real (dezenas de ms). Em uma
  máquina muito carregada as margens são folgadas de propósito; não as aperte.

## Dependencies
Apenas a stdlib. Dois consumidores, com limiters **independentes**:
- `internal/scraper` — instanciado uma única vez em `cmd/scraper` e
  compartilhado. Intervalo de `config.Config.ScraperRateLimit`
  (`SCRAPER_RATE_LIMIT_MS`, default 2000).
- `internal/enrichment` — o geocoder cria o seu próprio, com a chave sendo o
  host do Nominatim. Intervalo de `config.Config.GeocodingRateLimit`
  (`GEOCODING_RATE_LIMIT_MS`, default 1000, que é o teto de 1 req/s da política
  do Nominatim). Limiter separado de propósito: o host de geocodificação não
  compete com os portais raspados, e unificar faria a coleta esperar pela
  geocodificação sem motivo.
