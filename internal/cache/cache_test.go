package cache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// password usada em todas as URLs de teste. O ponto dos testes abaixo é que ela
// nunca apareça em mensagem de erro — a REDIS_URL carrega credencial, como a
// DATABASE_URL.
const password = "s3cr3t-p4ss"

func TestNewRejectsInvalidURL(t *testing.T) {
	client, err := New(context.Background(), "not-a-redis-url")
	if err == nil {
		t.Fatalf("New() error = nil, want error")
	}
	if client != nil {
		t.Errorf("New() client = %v, want nil on error", client)
	}
	if !strings.Contains(err.Error(), "cache:") {
		t.Errorf("error %q does not mention the package", err)
	}
}

// Cada URL abaixo falha em um ponto diferente de redis.ParseURL: número de
// banco inválido, esquema desconhecido e caractere de controle (este último
// falha ainda em url.Parse, cujo *url.Error carrega a URL crua inteira).
func TestNewDoesNotLeakPassword(t *testing.T) {
	urls := map[string]string{
		"banco inválido":        "redis://user:" + password + "@localhost:6379/notanumber",
		"esquema desconhecido":  "amqp://user:" + password + "@localhost:6379/0",
		"caractere de controle": "redis://user:" + password + "@localhost:6379/0\n",
	}

	for name, raw := range urls {
		t.Run(name, func(t *testing.T) {
			_, err := New(context.Background(), raw)
			if err == nil {
				t.Fatalf("New(%q) error = nil, want error", raw)
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("error %q leaks the password", err)
			}
		})
	}
}

func TestEndpointOmitsPassword(t *testing.T) {
	opts, err := redis.ParseURL("redis://user:" + password + "@cache.internal:6380/3")
	if err != nil {
		t.Fatalf("ParseURL() error = %v, want nil", err)
	}

	got := endpoint(opts)
	if strings.Contains(got, password) {
		t.Fatalf("endpoint() = %q, leaks the password", got)
	}
	for _, want := range []string{"cache.internal:6380", "3"} {
		if !strings.Contains(got, want) {
			t.Errorf("endpoint() = %q, does not mention %q", got, want)
		}
	}
}

// Um host inalcançável precisa falhar em segundos, e não travar o startup até o
// timeout do sistema operacional. A porta 1 é privilegiada e não tem nada
// escutando em loopback.
func TestNewFailsFastOnUnreachableHost(t *testing.T) {
	if testing.Short() {
		t.Skip("depende de uma tentativa de conexão em loopback")
	}

	start := time.Now()
	client, err := New(context.Background(), "redis://user:"+password+"@127.0.0.1:1/0")
	elapsed := time.Since(start)

	if err == nil {
		_ = client.Close()
		t.Fatalf("New() error = nil, want error (nada escuta em 127.0.0.1:1)")
	}
	if client != nil {
		t.Errorf("New() client = %v, want nil on error", client)
	}
	if elapsed > 2*pingTimeout {
		t.Errorf("New() demorou %v, want <= %v", elapsed, 2*pingTimeout)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error %q does not mention the endpoint", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("error %q leaks the password", err)
	}
}

// O ctx cancelado precisa abortar o Ping antes do pingTimeout — é o que faz o
// SIGTERM encerrar um boot travado na validação do Redis.
func TestNewAbortsOnCanceledContext(t *testing.T) {
	if testing.Short() {
		t.Skip("depende de uma tentativa de conexão em loopback")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := New(ctx, "redis://127.0.0.1:1/0"); err == nil {
		t.Fatalf("New() error = nil, want error")
	}
}
