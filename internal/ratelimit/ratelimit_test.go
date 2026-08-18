package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestWaitSpacesRequestsForSameKey(t *testing.T) {
	const interval = 40 * time.Millisecond
	limiter := New(interval)
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

func TestWaitDoesNotBlockDifferentKeys(t *testing.T) {
	limiter := New(500 * time.Millisecond)
	ctx := context.Background()

	start := time.Now()
	for _, host := range []string{"a.com", "b.com", "c.com"} {
		if err := limiter.Wait(ctx, host); err != nil {
			t.Fatalf("Wait(%q) error = %v, want nil", host, err)
		}
	}

	// Hosts distintos têm relógios independentes: nenhuma espera deve ocorrer.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %v, want near zero", elapsed)
	}
}

func TestWaitRespectsCanceledContext(t *testing.T) {
	limiter := New(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	// Consome o slot imediato para que a próxima chamada precise esperar.
	if err := limiter.Wait(ctx, "example.com"); err != nil {
		t.Fatalf("first Wait() error = %v, want nil", err)
	}

	cancel()
	if err := limiter.Wait(ctx, "example.com"); err == nil {
		t.Fatal("Wait() error = nil, want context error")
	}
}

func TestWaitWithZeroIntervalIsNoOp(t *testing.T) {
	limiter := New(0)

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
