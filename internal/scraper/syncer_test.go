package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/imobhub/api/internal/db"
)

// syncSpy substitui as costuras de banco de SyncListings e registra o que foi
// chamado. O *pgxpool.Pool nunca é usado — nenhum teste deste pacote toca no
// PostgreSQL.
type syncSpy struct {
	upsertCalls       [][]db.RawListing
	deleteCalls       []deleteCall
	deleted           int64
	deletedProperties int64
	upsertErr         error
	deleteErr         error
}

type deleteCall struct {
	domain       string
	runStartedAt time.Time
}

// install troca as variáveis de pacote pelo spy e as restaura no fim do teste.
func (s *syncSpy) install(t *testing.T) {
	t.Helper()

	previousUpsert, previousDelete := upsertListings, deleteStaleListings
	upsertListings = func(_ context.Context, _ *pgxpool.Pool, listings []db.RawListing) error {
		s.upsertCalls = append(s.upsertCalls, listings)
		return s.upsertErr
	}
	deleteStaleListings = func(_ context.Context, _ *pgxpool.Pool, domain string, runStartedAt time.Time) (int64, int64, error) {
		s.deleteCalls = append(s.deleteCalls, deleteCall{domain: domain, runStartedAt: runStartedAt})
		return s.deleted, s.deletedProperties, s.deleteErr
	}
	t.Cleanup(func() {
		upsertListings, deleteStaleListings = previousUpsert, previousDelete
	})
}

func sampleListings(n int) []db.RawListing {
	listings := make([]db.RawListing, 0, n)
	for i := range n {
		listings = append(listings, db.RawListing{
			SourceDomain: "www.exemplo.com.br",
			ListingURL:   fmt.Sprintf("https://www.exemplo.com.br/imoveis/%d", i+1),
			TitleRaw:     fmt.Sprintf("Imóvel %d", i+1),
		})
	}
	return listings
}

func TestSyncListingsUpsertsThenDeletesStale(t *testing.T) {
	spy := &syncSpy{deleted: 4}
	spy.install(t)

	runStartedAt := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	extracted := sampleListings(3)

	stats, err := SyncListings(context.Background(), nil, "www.exemplo.com.br", extracted, runStartedAt)
	if err != nil {
		t.Fatalf("SyncListings() error = %v, want nil", err)
	}

	want := SyncStats{Upserted: 3, Deleted: 4, PropertiesDeleted: 0}
	if stats != want {
		t.Errorf("SyncListings() = %+v, want %+v", stats, want)
	}
	if len(spy.upsertCalls) != 1 || len(spy.upsertCalls[0]) != 3 {
		t.Fatalf("upserts = %v, want uma chamada com 3 anúncios", spy.upsertCalls)
	}
	if len(spy.deleteCalls) != 1 {
		t.Fatalf("deletes = %v, want uma chamada", spy.deleteCalls)
	}
	// O corte precisa chegar intacto ao repositório: qualquer ajuste aqui
	// (arredondar, usar "agora") apagaria anúncios recém-gravados.
	if got := spy.deleteCalls[0]; got.domain != "www.exemplo.com.br" || !got.runStartedAt.Equal(runStartedAt) {
		t.Errorf("delete recebeu %+v, want domínio %q e runStartedAt %v", got, "www.exemplo.com.br", runStartedAt)
	}
}

func TestSyncListingsSkipsEverythingWhenNothingWasExtracted(t *testing.T) {
	// Guarda mais importante do módulo: uma coleta que devolve zero anúncios por
	// seletor quebrado não pode apagar o catálogo já acumulado.
	for _, extracted := range [][]db.RawListing{nil, {}} {
		spy := &syncSpy{deleted: 99, deletedProperties: 7}
		spy.install(t)

		stats, err := SyncListings(context.Background(), nil, "www.exemplo.com.br", extracted, time.Now())
		if err != nil {
			t.Fatalf("SyncListings() error = %v, want nil", err)
		}
		if stats != (SyncStats{}) {
			t.Errorf("SyncListings() = %+v, want estatísticas zeradas", stats)
		}
		if len(spy.upsertCalls) != 0 || len(spy.deleteCalls) != 0 {
			t.Errorf("banco foi tocado com zero anúncios: upserts=%v deletes=%v", spy.upsertCalls, spy.deleteCalls)
		}
	}
}

func TestSyncListingsRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name         string
		domain       string
		runStartedAt time.Time
		wantErr      error
	}{
		{name: "domínio vazio", domain: "   ", runStartedAt: time.Now(), wantErr: ErrMissingSyncDomain},
		{name: "sem início de run", domain: "www.exemplo.com.br", wantErr: ErrMissingRunStart},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &syncSpy{}
			spy.install(t)

			stats, err := SyncListings(context.Background(), nil, tt.domain, sampleListings(2), tt.runStartedAt)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SyncListings() error = %v, want %v", err, tt.wantErr)
			}
			if stats != (SyncStats{}) {
				t.Errorf("SyncListings() = %+v, want estatísticas zeradas", stats)
			}
			if len(spy.upsertCalls) != 0 || len(spy.deleteCalls) != 0 {
				t.Errorf("argumento inválido chegou ao banco: upserts=%v deletes=%v", spy.upsertCalls, spy.deleteCalls)
			}
		})
	}
}

func TestSyncListingsDoesNotDeleteWhenUpsertFails(t *testing.T) {
	// Se o upsert falhou, parte dos anúncios ficou com last_seen_at antigo — o
	// DELETE os interpretaria como sumidos do site.
	spy := &syncSpy{upsertErr: errors.New("boom")}
	spy.install(t)

	stats, err := SyncListings(context.Background(), nil, "www.exemplo.com.br", sampleListings(2), time.Now())
	if err == nil {
		t.Fatal("SyncListings() error = nil, want erro do upsert")
	}
	if stats != (SyncStats{}) {
		t.Errorf("SyncListings() = %+v, want estatísticas zeradas", stats)
	}
	if len(spy.deleteCalls) != 0 {
		t.Errorf("DELETE rodou após falha de upsert: %v", spy.deleteCalls)
	}
}

func TestSyncListingsPropagatesDeleteError(t *testing.T) {
	sentinel := errors.New("connection reset")
	spy := &syncSpy{deleteErr: sentinel}
	spy.install(t)

	stats, err := SyncListings(context.Background(), nil, "www.exemplo.com.br", sampleListings(2), time.Now())
	if !errors.Is(err, sentinel) {
		t.Fatalf("SyncListings() error = %v, want %v embrulhado", err, sentinel)
	}
	if stats != (SyncStats{}) {
		t.Errorf("SyncListings() = %+v, want estatísticas zeradas", stats)
	}
}

func TestSyncListingsLogsSummary(t *testing.T) {
	// A mensagem é critério de aceitação da task: é o resumo que o operador
	// acompanha durante a coleta.
	spy := &syncSpy{deleted: 2}
	spy.install(t)

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if _, err := SyncListings(context.Background(), nil, "www.exemplo.com.br", sampleListings(5), time.Now()); err != nil {
		t.Fatalf("SyncListings() error = %v, want nil", err)
	}

	var entry struct {
		Msg      string `json:"msg"`
		Domain   string `json:"domain"`
		Upserted int64  `json:"upserted"`
		Deleted  int64  `json:"deleted"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("decoding log line %q: %v", buf.String(), err)
	}

	want := fmt.Sprintf("[%s] sync concluído: %d upserted, %d deletados", "www.exemplo.com.br", 5, 2)
	if entry.Msg != want {
		t.Errorf("log msg = %q, want %q", entry.Msg, want)
	}
	if entry.Domain != "www.exemplo.com.br" || entry.Upserted != 5 || entry.Deleted != 2 {
		t.Errorf("log attrs = %+v, want domain/upserted/deleted preenchidos", entry)
	}
}

// O número de imóveis canônicos removidos vem de db.DeleteStaleListings (que os
// apaga na mesma transação do DELETE dos anúncios) e precisa chegar intacto ao
// resumo do run.
func TestSyncListingsReportsDeletedProperties(t *testing.T) {
	spy := &syncSpy{deleted: 6, deletedProperties: 2}
	spy.install(t)

	stats, err := SyncListings(context.Background(), nil, "www.exemplo.com.br", sampleListings(3), time.Now())
	if err != nil {
		t.Fatalf("SyncListings() error = %v, want nil", err)
	}

	want := SyncStats{Upserted: 3, Deleted: 6, PropertiesDeleted: 2}
	if stats != want {
		t.Errorf("SyncListings() = %+v, want %+v", stats, want)
	}
}

// Os dois logs contratuais não mudam; a linha das properties órfãs é separada e
// só aparece quando alguma foi de fato removida.
func TestSyncListingsLogsDeletedPropertiesOnASeparateLine(t *testing.T) {
	tests := []struct {
		name              string
		deletedProperties int64
		wantLines         int
		wantMsg           string
	}{
		{
			name:              "com órfãs removidas",
			deletedProperties: 3,
			wantLines:         2,
			wantMsg:           "[www.exemplo.com.br] 3 imóveis canônicos removidos por terem ficado sem anúncios",
		},
		{name: "sem órfãs removidas", deletedProperties: 0, wantLines: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &syncSpy{deleted: 4, deletedProperties: tt.deletedProperties}
			spy.install(t)

			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			if _, err := SyncListings(context.Background(), nil, "www.exemplo.com.br", sampleListings(5), time.Now()); err != nil {
				t.Fatalf("SyncListings() error = %v, want nil", err)
			}

			lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
			if len(lines) != tt.wantLines {
				t.Fatalf("linhas de log = %d (%s), want %d", len(lines), buf.String(), tt.wantLines)
			}

			// A primeira linha continua sendo o resumo contratual, byte a byte.
			var summary struct {
				Msg string `json:"msg"`
			}
			if err := json.Unmarshal(lines[0], &summary); err != nil {
				t.Fatalf("decoding log line %q: %v", lines[0], err)
			}
			if want := "[www.exemplo.com.br] sync concluído: 5 upserted, 4 deletados"; summary.Msg != want {
				t.Errorf("log contratual = %q, want %q", summary.Msg, want)
			}

			if tt.wantLines == 1 {
				return
			}

			var orphans struct {
				Msg               string `json:"msg"`
				Domain            string `json:"domain"`
				PropertiesDeleted int64  `json:"properties_deleted"`
			}
			if err := json.Unmarshal(lines[1], &orphans); err != nil {
				t.Fatalf("decoding log line %q: %v", lines[1], err)
			}
			if orphans.Msg != tt.wantMsg {
				t.Errorf("log msg = %q, want %q", orphans.Msg, tt.wantMsg)
			}
			if orphans.Domain != "www.exemplo.com.br" || orphans.PropertiesDeleted != tt.deletedProperties {
				t.Errorf("log attrs = %+v, want domain e properties_deleted preenchidos", orphans)
			}
		})
	}
}

func TestSyncListingsLogsWarningWhenSkipping(t *testing.T) {
	spy := &syncSpy{}
	spy.install(t)

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if _, err := SyncListings(context.Background(), nil, "www.exemplo.com.br", nil, time.Now()); err != nil {
		t.Fatalf("SyncListings() error = %v, want nil", err)
	}

	var entry struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("decoding log line %q: %v", buf.String(), err)
	}
	if entry.Level != "WARN" {
		t.Errorf("log level = %q, want WARN", entry.Level)
	}
	if want := "[www.exemplo.com.br] sync ignorado: nenhum anúncio extraído, nada foi deletado"; entry.Msg != want {
		t.Errorf("log msg = %q, want %q", entry.Msg, want)
	}
}
