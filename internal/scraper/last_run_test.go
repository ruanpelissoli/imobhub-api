package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeLastRunStore substitui o Redis sem rede: registra o que foi gravado e
// devolve o resultado programado pelo teste.
type fakeLastRunStore struct {
	sets      int
	lastKey   string
	lastTTL   time.Duration
	lastValue []byte
	// ctxErrDuringSet é o ctx.Err() visto dentro do Set. É o que pina o
	// context.WithoutCancel: um SIGTERM não pode impedir a gravação.
	ctxErrDuringSet error
	setErr          error
}

func (s *fakeLastRunStore) Set(ctx context.Context, key string, value any, ttl time.Duration) *redis.StatusCmd {
	s.sets++
	s.lastKey = key
	s.lastTTL = ttl
	s.ctxErrDuringSet = ctx.Err()
	if body, ok := value.([]byte); ok {
		s.lastValue = append([]byte(nil), body...)
	}
	if s.setErr != nil {
		return redis.NewStatusResult("", s.setErr)
	}
	return redis.NewStatusResult("OK", nil)
}

func (s *fakeLastRunStore) decode(t *testing.T) LastRun {
	t.Helper()

	var run LastRun
	if err := json.Unmarshal(s.lastValue, &run); err != nil {
		t.Fatalf("decodificando o payload %q: %v", s.lastValue, err)
	}
	return run
}

func (s *fakeLastRunStore) decodeRaw(t *testing.T) map[string]any {
	t.Helper()

	var raw map[string]any
	if err := json.Unmarshal(s.lastValue, &raw); err != nil {
		t.Fatalf("decodificando o payload %q: %v", s.lastValue, err)
	}
	return raw
}

func sampleLastRun() LastRun {
	return LastRun{
		RanAt:             "2026-08-19T03:00:00Z",
		Success:           true,
		SitesTotal:        3,
		SitesSucceeded:    2,
		SitesFailed:       0,
		SitesBlocked:      1,
		ListingsFound:     120,
		ListingsRemoved:   7,
		ListingsTotalInDB: 4200,
		DurationMS:        91_000,
	}
}

func TestLastRunPublisherWritesPayloadWithKeyAndTTL(t *testing.T) {
	store := &fakeLastRunStore{}
	publisher := &lastRunPublisher{store: store}

	publisher.publish(context.Background(), sampleLastRun())

	if store.sets != 1 {
		t.Fatalf("gravações = %d, want 1", store.sets)
	}
	if store.lastKey != LastRunCacheKey || LastRunCacheKey != "scraper:last_run" {
		t.Errorf("chave = %q, want %q", store.lastKey, "scraper:last_run")
	}
	if store.lastTTL != lastRunTTL || lastRunTTL != 48*time.Hour {
		t.Errorf("TTL = %v, want 48h", store.lastTTL)
	}

	if got, want := store.decode(t), sampleLastRun(); got != want {
		t.Errorf("payload = %+v, want %+v", got, want)
	}
}

func TestLastRunPayloadCarriesTheAgreedFields(t *testing.T) {
	store := &fakeLastRunStore{}
	(&lastRunPublisher{store: store}).publish(context.Background(), sampleLastRun())

	raw := store.decodeRaw(t)
	wantFields := []string{
		"ran_at", "success", "sites_total", "sites_succeeded", "sites_failed",
		"sites_blocked", "listings_found", "listings_removed",
		"listings_total_in_db", "duration_ms",
	}
	for _, field := range wantFields {
		if _, ok := raw[field]; !ok {
			t.Errorf("campo %q ausente do payload %s", field, store.lastValue)
		}
	}
	if len(raw) != len(wantFields) {
		t.Errorf("payload com %d campos, want %d: %s", len(raw), len(wantFields), store.lastValue)
	}

	ranAt, err := time.Parse(time.RFC3339, raw["ran_at"].(string))
	if err != nil {
		t.Fatalf("ran_at = %v, want RFC3339: %v", raw["ran_at"], err)
	}
	if _, offset := ranAt.Zone(); offset != 0 {
		t.Errorf("ran_at = %v, want UTC", ranAt)
	}
}

func TestLastRunPublisherWritesEvenWithACanceledContext(t *testing.T) {
	// É o caminho do SIGTERM: derivar do ctx cancelado faria a gravação falhar
	// exatamente no cenário em que o resumo mais importa.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &fakeLastRunStore{}
	(&lastRunPublisher{store: store}).publish(ctx, sampleLastRun())

	if store.sets != 1 {
		t.Fatalf("gravações = %d, want 1 mesmo com o contexto cancelado", store.sets)
	}
	if store.ctxErrDuringSet != nil {
		t.Errorf("ctx.Err() dentro do Set = %v, want nil", store.ctxErrDuringSet)
	}
}

func TestLastRunPublisherWarnsWhenRedisFails(t *testing.T) {
	buf := captureLogs(t)
	store := &fakeLastRunStore{setErr: errors.New("READONLY You can't write against a read only replica")}

	(&lastRunPublisher{store: store}).publish(context.Background(), sampleLastRun())

	requireLoggedEvent(t, buf, "não foi possível publicar o resumo da última coleta no Redis", eventLastRunPublishFailed)
}

func TestNewLastRunPublisherWithoutClientIsNil(t *testing.T) {
	// Guardar um *redis.Client nil dentro de lastRunStore produziria uma
	// interface não-nil e um panic na primeira gravação.
	publisher := newLastRunPublisher(nil)
	if publisher != nil {
		t.Fatalf("newLastRunPublisher(nil) = %v, want nil", publisher)
	}

	publisher.publish(context.Background(), sampleLastRun())
	(&lastRunPublisher{}).publish(context.Background(), sampleLastRun())
}
