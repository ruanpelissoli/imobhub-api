# internal/ratelimit

## Purpose
Garante um intervalo mínimo entre requisições para o mesmo host. Protege as
fontes de sobrecarga e reduz o risco de o bot ser bloqueado.

## Business logic / invariantes
- **O espaçamento é por chave** (na prática, o host). Hosts diferentes têm
  relógios independentes e nunca esperam uns pelos outros — sem isso, coletar
  10 fontes em paralelo seria tão lento quanto coletá-las em série.
- **O slot é reservado antes da espera, ainda sob o lock.** Essa é a parte
  sutil: `Wait` grava `next[key]` e só então libera o mutex e dorme. Se a
  reserva acontecesse depois da espera, N goroutines para o mesmo host leriam o
  mesmo horário, dormiriam o mesmo tempo e disparariam **juntas** — exatamente o
  burst que o limiter existe para evitar. Do jeito atual elas se enfileiram.
- **`interval <= 0` desativa o limiter**, e `Wait` retorna `ctx.Err()`
  imediatamente. Corresponde a `SCRAPER_RATE_LIMIT_MS=0`; é para testes locais,
  não para produção.
- `Wait` respeita o cancelamento do contexto e retorna o erro do contexto. Uma
  espera longa não impede o encerramento do processo em SIGTERM.

## Key decisions
- **Implementação própria em vez de `golang.org/x/time/rate`.** O `rate.Limiter`
  modela token bucket com burst; o que precisamos é o oposto — espaçamento fixo,
  sem burst algum. Além disso precisaríamos de um mapa de limiters por host de
  qualquer forma, então a economia seria pequena.
- `New` é obrigatório: o zero value não inicializa o mapa e causaria panic na
  escrita.

## Gotchas
- **O mapa `next` cresce indefinidamente.** Uma entrada por host visto, nunca
  removida. Aceitável enquanto a lista de fontes for da ordem de dezenas; se o
  scraper passar a seguir links para hosts arbitrários, adicione uma limpeza de
  entradas cujo horário já passou há muito.
- `Wait` bloqueia. Chame-o imediatamente antes da requisição, não no início de
  um pipeline — o tempo gasto entre o `Wait` e o `Get` sai do intervalo
  efetivo.

## Dependencies
Apenas a stdlib. Será importado por `internal/scraper`.
