// Package ratelimit garante um intervalo mínimo entre requisições para o mesmo
// host, evitando sobrecarregar as fontes e reduzindo o risco de bloqueio.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter espaça requisições por chave (tipicamente o host). Hosts diferentes
// não competem entre si: cada um tem seu próprio relógio.
//
// O zero value não é utilizável — use New.
type Limiter struct {
	interval time.Duration

	mu   sync.Mutex
	next map[string]time.Time
}

// New cria um Limiter com o intervalo mínimo entre requisições da mesma chave.
// interval <= 0 desativa o rate limiting (Wait retorna imediatamente).
func New(interval time.Duration) *Limiter {
	return &Limiter{
		interval: interval,
		next:     make(map[string]time.Time),
	}
}

// Wait bloqueia até que seja permitido fazer a próxima requisição para key, ou
// até o contexto ser cancelado. Retorna o erro do contexto nesse caso.
//
// O slot é reservado antes da espera (ainda sob o lock), então chamadas
// concorrentes para a mesma chave se enfileiram em vez de dispararem juntas.
func (l *Limiter) Wait(ctx context.Context, key string) error {
	if l.interval <= 0 {
		return ctx.Err()
	}

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
		return ctx.Err()
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
