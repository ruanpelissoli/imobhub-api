// Package ratelimit garante um intervalo mínimo entre requisições para o mesmo
// domínio, evitando sobrecarregar as fontes e reduzindo o risco de bloqueio.
package ratelimit

import (
	"context"
	"strings"
	"sync"
	"time"
)

// DomainLimiter espaça as requisições feitas a um mesmo domínio. Domínios
// diferentes não competem entre si: cada um tem seu próprio relógio.
//
// O zero value não é utilizável (o mapa interno seria nulo) — use
// NewDomainLimiter. Depois do primeiro uso o valor não pode ser copiado, porque
// contém um sync.Mutex; passe sempre o ponteiro.
type DomainLimiter struct {
	interval time.Duration

	// mu protege next. Um mutex simples basta: toda operação é uma leitura
	// seguida de uma escrita, então um RWMutex não traria ganho.
	mu   sync.Mutex
	next map[string]time.Time
}

// NewDomainLimiter cria um limiter com o intervalo mínimo, em milissegundos,
// entre requisições ao mesmo domínio. O valor vem de SCRAPER_RATE_LIMIT_MS
// (default 2000), exposto por config.Config.ScraperRateLimit.
//
// intervalMs menor ou igual a zero desativa o rate limiting: Wait passa a
// retornar imediatamente. É a configuração para testes locais, não para
// produção.
func NewDomainLimiter(intervalMs int) *DomainLimiter {
	return &DomainLimiter{
		interval: time.Duration(intervalMs) * time.Millisecond,
		next:     make(map[string]time.Time),
	}
}

// Interval retorna o intervalo mínimo configurado entre requisições ao mesmo
// domínio. Zero significa rate limiting desativado.
func (l *DomainLimiter) Interval() time.Duration {
	return l.interval
}

// Wait bloqueia até que seja permitido fazer a próxima requisição para domain,
// e então registra o horário desta chamada para espaçar a seguinte.
//
// Retorna ctx.Err() se o contexto for cancelado — inclusive durante a espera,
// para que um SIGTERM não fique preso atrás de um intervalo longo. Retorna nil
// em qualquer outro caso.
//
// Chame Wait imediatamente antes da requisição: o tempo gasto entre o Wait e o
// Get sai do intervalo efetivo.
func (l *DomainLimiter) Wait(ctx context.Context, domain string) error {
	if l.interval <= 0 {
		return ctx.Err()
	}

	key := normalize(domain)

	// O slot é reservado ainda sob o lock, antes de dormir. Se a reserva
	// acontecesse só depois da espera, N goroutines para o mesmo domínio leriam
	// o mesmo horário, dormiriam o mesmo tempo e disparariam juntas — o burst
	// que este pacote existe para evitar. Reservando antes, elas se enfileiram.
	l.mu.Lock()
	now := time.Now()
	slot := l.next[key]
	if slot.Before(now) {
		slot = now
	}
	l.next[key] = slot.Add(l.interval)
	l.mu.Unlock()

	delay := time.Until(slot)
	if delay <= 0 {
		// Primeira requisição ao domínio (ou o intervalo já decorreu): segue
		// direto, mas ainda respeita um contexto já cancelado.
		return ctx.Err()
	}

	// time.Sleep seria mais curto, porém não é interrompível: o contexto só
	// seria observado depois do intervalo inteiro. Um timer com select entrega
	// o mesmo espaçamento e continua respondendo ao cancelamento.
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// normalize deixa o mesmo host sempre na mesma chave. Nomes de host são
// case-insensitive; sem isso "Exemplo.com.br" e "exemplo.com.br" teriam
// relógios independentes e o servidor receberia o dobro das requisições.
func normalize(domain string) string {
	return strings.ToLower(strings.TrimSpace(domain))
}
