package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWaitSpacesRequestsForSameDomain(t *testing.T) {
	const intervalMs = 40
	interval := intervalMs * time.Millisecond

	limiter := NewDomainLimiter(intervalMs)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := limiter.Wait(ctx, "example.com"); err != nil {
			t.Fatalf("Wait() error = %v, want nil", err)
		}
	}

	// A primeira chamada é imediata; as duas seguintes esperam um intervalo cada.
	if elapsed := time.Since(start); elapsed < 2*interval {
		t.Errorf("elapsed = %v, want at least %v", elapsed, 2*interval)
	}
}

func TestWaitDoesNotBlockDifferentDomains(t *testing.T) {
	limiter := NewDomainLimiter(500)
	ctx := context.Background()

	start := time.Now()
	for _, domain := range []string{"a.com", "b.com", "c.com"} {
		if err := limiter.Wait(ctx, domain); err != nil {
			t.Fatalf("Wait(%q) error = %v, want nil", domain, err)
		}
	}

	// Domínios distintos têm relógios independentes: nenhuma espera deve ocorrer.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %v, want near zero", elapsed)
	}
}

func TestWaitRespectsCanceledContext(t *testing.T) {
	limiter := NewDomainLimiter(int(time.Hour / time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())

	// Consome o slot imediato para que a próxima chamada precise esperar.
	if err := limiter.Wait(ctx, "example.com"); err != nil {
		t.Fatalf("first Wait() error = %v, want nil", err)
	}

	cancel()

	start := time.Now()
	if err := limiter.Wait(ctx, "example.com"); err != context.Canceled {
		t.Fatalf("Wait() error = %v, want %v", err, context.Canceled)
	}
	// O cancelamento não pode ficar preso atrás do intervalo de uma hora.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("elapsed = %v, want near zero", elapsed)
	}
}

func TestWaitCanceledDuringWait(t *testing.T) {
	limiter := NewDomainLimiter(int(time.Hour / time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := limiter.Wait(ctx, "example.com"); err != nil {
		t.Fatalf("first Wait() error = %v, want nil", err)
	}

	// Cancela só depois que a segunda chamada já está dormindo.
	time.AfterFunc(30*time.Millisecond, cancel)

	if err := limiter.Wait(ctx, "example.com"); err != context.Canceled {
		t.Fatalf("Wait() error = %v, want %v", err, context.Canceled)
	}
}

func TestWaitWithZeroIntervalIsNoOp(t *testing.T) {
	limiter := NewDomainLimiter(0)

	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := limiter.Wait(context.Background(), "example.com"); err != nil {
			t.Fatalf("Wait() error = %v, want nil", err)
		}
	}

	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("elapsed = %v, want near zero", elapsed)
	}
}

func TestWaitWithNegativeIntervalIsNoOp(t *testing.T) {
	limiter := NewDomainLimiter(-1)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := limiter.Wait(context.Background(), "example.com"); err != nil {
			t.Fatalf("Wait() error = %v, want nil", err)
		}
	}

	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("elapsed = %v, want near zero", elapsed)
	}
}

func TestWaitNormalizesDomain(t *testing.T) {
	const intervalMs = 60
	limiter := NewDomainLimiter(intervalMs)
	ctx := context.Background()

	if err := limiter.Wait(ctx, "exemplo.com.br"); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	// Mesmo host escrito de outro jeito deve compartilhar o relógio.
	start := time.Now()
	if err := limiter.Wait(ctx, "  Exemplo.COM.BR "); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}

	if elapsed := time.Since(start); elapsed < intervalMs*time.Millisecond/2 {
		t.Errorf("elapsed = %v, want a espera do intervalo", elapsed)
	}
}

func TestWaitIsThreadSafeAndDoesNotBurst(t *testing.T) {
	const (
		intervalMs = 20
		callers    = 5
	)
	interval := intervalMs * time.Millisecond

	limiter := NewDomainLimiter(intervalMs)
	ctx := context.Background()

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := limiter.Wait(ctx, "example.com"); err != nil {
				t.Errorf("Wait() error = %v, want nil", err)
			}
		}()
	}
	wg.Wait()

	// Chamadas concorrentes ao mesmo domínio se enfileiram em vez de dispararem
	// juntas: a última só pode passar depois de (callers-1) intervalos.
	if elapsed := time.Since(start); elapsed < (callers-1)*interval {
		t.Errorf("elapsed = %v, want at least %v", elapsed, (callers-1)*interval)
	}
}

func TestInterval(t *testing.T) {
	if got, want := NewDomainLimiter(2000).Interval(), 2*time.Second; got != want {
		t.Errorf("Interval() = %v, want %v", got, want)
	}
	if got := NewDomainLimiter(0).Interval(); got != 0 {
		t.Errorf("Interval() = %v, want 0", got)
	}
}
