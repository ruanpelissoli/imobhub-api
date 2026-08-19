package enrichqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/enrichment"
	"github.com/imobhub/api/internal/grouping"
)

// stubDeps monta um pipeline sem rede, sem IA e sem PostgreSQL: cada
// dependência é um closure que registra o que foi chamado. Os testes
// sobrescrevem só o campo que interessa ao caso e chamam New(stub.Deps).
//
// O mutex existe porque o pool é concorrente por design — sem ele os próprios
// testes teriam corrida na hora de registrar as chamadas.
type stubDeps struct {
	Deps

	mu sync.Mutex
	// calls é a sequência de etapas executadas, na ordem em que aconteceram.
	calls []string
	// saved e marked guardam o que foi persistido e o que foi carimbado.
	saved  []db.ListingEnrichment
	marked []int64
	// grouped guarda os anúncios que chegaram ao agrupamento.
	grouped []grouping.Listing
	// merged guarda os ids de imóvel consolidados.
	merged []string
	// batches registra os pares (cursor, limit) pedidos à fila.
	batches []batchRequest
}

type batchRequest struct {
	afterID int64
	limit   int
}

// record anota uma etapa executada. A ordem só é significativa com um worker.
func (s *stubDeps) record(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, step)
}

func (s *stubDeps) snapshot(read func(*stubDeps)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	read(s)
}

// samplePending devolve n anúncios pendentes com ids sequenciais a partir de 1.
func samplePending(n int) []db.PendingListing {
	listings := make([]db.PendingListing, 0, n)
	for i := 1; i <= n; i++ {
		listings = append(listings, db.PendingListing{
			ID:             int64(i),
			SourceDomain:   "www.exemplo.com.br",
			ListingURL:     fmt.Sprintf("https://www.exemplo.com.br/imovel/%d", i),
			TitleRaw:       fmt.Sprintf("Apartamento %d", i),
			DescriptionRaw: "3 quartos com piscina",
			AddressRaw:     fmt.Sprintf("Rua das Acácias, %d", i),
			AreaRaw:        "72 m²",
			BedroomsRaw:    "3",
			ImageURLs:      []string{"https://cdn.exemplo.com.br/1.jpg"},
		})
	}
	return listings
}

// newStub devolve dependências que enriquecem batch inteiro com sucesso, num
// único lote (a segunda leitura devolve a fila vazia).
func newStub(batch []db.PendingListing) *stubDeps {
	s := &stubDeps{}

	three := 3
	s.Deps = Deps{
		ListPending: func(_ context.Context, afterID int64, limit int) ([]db.PendingListing, error) {
			s.mu.Lock()
			s.batches = append(s.batches, batchRequest{afterID: afterID, limit: limit})
			s.mu.Unlock()

			var pending []db.PendingListing
			for _, listing := range batch {
				if listing.ID > afterID {
					pending = append(pending, listing)
				}
				if len(pending) == limit {
					break
				}
			}
			return pending, nil
		},
		NormalizeNeighborhood: func(string, string) string {
			s.record("normalize")
			return "Jardim Botânico"
		},
		ExtractTerms: func(string) enrichment.ExtractedData {
			s.record("extract")
			return enrichment.ExtractedData{BedroomCount: &three, Amenities: []string{"piscina"}}
		},
		Geocode: func(context.Context, string, string, string) (float64, float64, error) {
			s.record("geocode")
			return -15.78, -47.93, nil
		},
		SaveEnrichment: func(_ context.Context, enriched db.ListingEnrichment) error {
			s.mu.Lock()
			s.calls = append(s.calls, "save")
			s.saved = append(s.saved, enriched)
			s.mu.Unlock()
			return nil
		},
		GroupListing: func(_ context.Context, listing grouping.Listing) (string, bool, error) {
			s.mu.Lock()
			s.calls = append(s.calls, "group")
			s.grouped = append(s.grouped, listing)
			s.mu.Unlock()
			return "11111111-1111-1111-1111-111111111111", true, nil
		},
		MergeProperty: func(_ context.Context, propertyID string) error {
			s.mu.Lock()
			s.calls = append(s.calls, "merge")
			s.merged = append(s.merged, propertyID)
			s.mu.Unlock()
			return nil
		},
		MarkEnriched: func(_ context.Context, listingID int64) error {
			s.mu.Lock()
			s.calls = append(s.calls, "mark")
			s.marked = append(s.marked, listingID)
			s.mu.Unlock()
			return nil
		},

		Workers:   1,
		BatchSize: 10,
	}

	return s
}

// run monta o pipeline com as dependências do stub e o executa.
func (s *stubDeps) run(t *testing.T, ctx context.Context) (Summary, error) {
	t.Helper()

	pipeline, err := New(s.Deps)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return pipeline.Run(ctx)
}

func TestRunEnrichesEveryPendingListing(t *testing.T) {
	stub := newStub(samplePending(3))

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got, want := summary.Queued, 3; got != want {
		t.Errorf("summary.Queued = %d, want %d", got, want)
	}
	if got, want := summary.Enriched, 3; got != want {
		t.Errorf("summary.Enriched = %d, want %d", got, want)
	}
	if summary.Skipped != 0 || summary.Failed != 0 {
		t.Errorf("summary.Skipped/Failed = %d/%d, want 0/0", summary.Skipped, summary.Failed)
	}

	stub.snapshot(func(s *stubDeps) {
		if got, want := len(s.marked), 3; got != want {
			t.Errorf("len(marked) = %d, want %d", got, want)
		}
		if got, want := len(s.merged), 3; got != want {
			t.Errorf("len(merged) = %d, want %d", got, want)
		}
	})
}

// A drenagem precisa avançar por cursor: o LIMIT sozinho devolveria o mesmo lote
// para sempre quando um anúncio falha e continua casando com o predicado.
func TestRunAdvancesTheCursorBetweenBatches(t *testing.T) {
	stub := newStub(samplePending(5))
	stub.Deps.BatchSize = 2

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got, want := summary.Queued, 5; got != want {
		t.Errorf("summary.Queued = %d, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		// 3 lotes com dados (2+2+1) e um quarto vazio que encerra a drenagem.
		want := []batchRequest{{0, 2}, {2, 2}, {4, 2}, {5, 2}}
		if len(s.batches) != len(want) {
			t.Fatalf("batches = %v, want %v", s.batches, want)
		}
		for i, request := range want {
			if s.batches[i] != request {
				t.Errorf("batches[%d] = %v, want %v", i, s.batches[i], request)
			}
		}
	})
}

// Um anúncio que falha não pode voltar no lote seguinte para sempre: o cursor é
// o maior id **lido**, não o maior id enriquecido.
func TestRunDoesNotLoopOnAFailingListing(t *testing.T) {
	stub := newStub(samplePending(3))
	stub.Deps.BatchSize = 1
	stub.Deps.Geocode = func(context.Context, string, string, string) (float64, float64, error) {
		return 0, 0, errors.New("connection reset by peer")
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got, want := summary.Failed, 3; got != want {
		t.Errorf("summary.Failed = %d, want %d", got, want)
	}
	stub.snapshot(func(s *stubDeps) {
		if got, want := len(s.batches), 4; got != want {
			t.Errorf("len(batches) = %d, want %d (3 com dados + 1 vazio)", got, want)
		}
	})
}

// Falha de um anúncio não pode interromper os demais: um endereço que o provider
// não resolve não pode parar o enriquecimento do catálogo inteiro.
func TestRunIsolatesFailuresBetweenListings(t *testing.T) {
	stub := newStub(samplePending(5))
	stub.Deps.GroupListing = func(_ context.Context, listing grouping.Listing) (string, bool, error) {
		if listing.ID == 3 {
			return "", false, errors.New("anthropic: 529 overloaded")
		}
		return "11111111-1111-1111-1111-111111111111", false, nil
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got, want := summary.Enriched, 4; got != want {
		t.Errorf("summary.Enriched = %d, want %d", got, want)
	}
	if got, want := summary.Failed, 1; got != want {
		t.Errorf("summary.Failed = %d, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		for _, id := range s.marked {
			if id == 3 {
				t.Error("o anúncio que falhou foi carimbado como enriquecido; ele precisa voltar à fila")
			}
		}
	})
}

// O pool precisa processar mais de um anúncio ao mesmo tempo — é o ponto todo
// dele. A barreira segura os workers até que Workers anúncios estejam em voo.
func TestRunProcessesListingsConcurrently(t *testing.T) {
	const workers = 4

	stub := newStub(samplePending(workers))
	stub.Deps.Workers = workers

	var (
		inFlight sync.WaitGroup
		released = make(chan struct{})
	)
	inFlight.Add(workers)

	stub.Deps.Geocode = func(context.Context, string, string, string) (float64, float64, error) {
		// Só destrava quando os `workers` anúncios chegarem aqui; com execução
		// serial o teste travaria e o timeout do `go test` o denunciaria.
		inFlight.Done()
		<-released
		return -15.78, -47.93, nil
	}

	go func() {
		inFlight.Wait()
		close(released)
	}()

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := summary.Enriched, workers; got != want {
		t.Errorf("summary.Enriched = %d, want %d", got, want)
	}
}

// Cancelamento encerra o pool ordenadamente: Run devolve erro e nenhuma
// goroutine fica bloqueada no canal (o teste travaria se ficasse).
func TestRunReturnsErrorWhenContextIsCanceled(t *testing.T) {
	stub := newStub(samplePending(50))
	stub.Deps.Workers = 2

	ctx, cancel := context.WithCancel(context.Background())
	stub.Deps.Geocode = func(ctx context.Context, _, _, _ string) (float64, float64, error) {
		cancel()
		return 0, 0, ctx.Err()
	}

	summary, err := stub.run(t, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if summary.Enriched != 0 {
		t.Errorf("summary.Enriched = %d, want 0", summary.Enriched)
	}
}

// Cancelamento entre lotes encerra a drenagem sem ler o lote seguinte.
func TestRunStopsBeforeReadingTheNextBatchWhenCanceled(t *testing.T) {
	stub := newStub(samplePending(4))
	stub.Deps.BatchSize = 2

	ctx, cancel := context.WithCancel(context.Background())
	stub.Deps.MarkEnriched = func(_ context.Context, listingID int64) error {
		if listingID == 2 {
			cancel()
		}
		return nil
	}

	if _, err := stub.run(t, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	stub.snapshot(func(s *stubDeps) {
		if got, want := len(s.batches), 1; got != want {
			t.Errorf("len(batches) = %d, want %d — a fila não pode ler outro lote depois do cancelamento", got, want)
		}
	})
}

// Erro de leitura da fila é fatal: sem ela não há o que processar, e insistir
// produziria uma cascata de erros idênticos.
func TestRunFailsWhenTheQueueCannotBeRead(t *testing.T) {
	stub := newStub(samplePending(1))
	stub.Deps.ListPending = func(context.Context, int64, int) ([]db.PendingListing, error) {
		return nil, errors.New("connection refused")
	}

	if _, err := stub.run(t, context.Background()); err == nil {
		t.Fatal("Run() error = nil, want error")
	}
}

// captureLogs redireciona o logger padrão para um buffer pelo tempo do teste.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buffer := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return buffer
}

// Num run cancelado, `queued` é somado na leitura do lote e os anúncios que
// nunca chegaram a um worker não entram em nenhum dos três contadores. Sem
// `unprocessed`, quem somasse as colunas do resumo acharia que sumiram anúncios.
func TestLogSummaryAccountsForUnprocessedListings(t *testing.T) {
	stub := newStub(nil)
	pipeline, err := New(stub.Deps)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	logs := captureLogs(t)
	summary := Summary{Queued: 200, Enriched: 37, Skipped: 0, Failed: 0}
	pipeline.logSummary(context.Background(), summary, true)

	var entry struct {
		Level       string `json:"level"`
		Message     string `json:"msg"`
		Queued      int    `json:"queued"`
		Unprocessed int    `json:"unprocessed"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log não é JSON: %v (%s)", err, logs.String())
	}

	if got, want := entry.Unprocessed, 163; got != want {
		t.Errorf("unprocessed = %d, want %d", got, want)
	}
	if entry.Queued != summary.Enriched+summary.Skipped+summary.Failed+entry.Unprocessed {
		t.Errorf("o resumo não fecha: queued = %d, soma das colunas = %d",
			entry.Queued, summary.Enriched+summary.Skipped+summary.Failed+entry.Unprocessed)
	}
	if got, want := entry.Level, "WARN"; got != want {
		t.Errorf("level = %q, want %q — run interrompido não é Info", got, want)
	}
	if !strings.Contains(entry.Message, "interrompido") || !strings.Contains(entry.Message, "163 não processados") {
		t.Errorf("mensagem = %q, want menção a interrompido e aos não processados", entry.Message)
	}
}

// Num run completo o número é sempre zero, e a menção a ele na mensagem humana
// só poluiria — o atributo estruturado continua saindo, para quem soma colunas.
func TestLogSummaryOmitsUnprocessedFromTheMessageOfACompleteRun(t *testing.T) {
	stub := newStub(nil)
	pipeline, err := New(stub.Deps)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	logs := captureLogs(t)
	pipeline.logSummary(context.Background(), Summary{Queued: 3, Enriched: 2, Skipped: 1}, false)

	var entry struct {
		Level       string `json:"level"`
		Message     string `json:"msg"`
		Unprocessed int    `json:"unprocessed"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log não é JSON: %v (%s)", err, logs.String())
	}

	if entry.Unprocessed != 0 {
		t.Errorf("unprocessed = %d, want 0", entry.Unprocessed)
	}
	if strings.Contains(entry.Message, "não processados") {
		t.Errorf("mensagem = %q, não deve citar não processados num run completo", entry.Message)
	}
	if got, want := entry.Level, "INFO"; got != want {
		t.Errorf("level = %q, want %q", got, want)
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	full := newStub(nil).Deps

	cases := map[string]func(*Deps){
		"ListPending":           func(d *Deps) { d.ListPending = nil },
		"NormalizeNeighborhood": func(d *Deps) { d.NormalizeNeighborhood = nil },
		"ExtractTerms":          func(d *Deps) { d.ExtractTerms = nil },
		"Geocode":               func(d *Deps) { d.Geocode = nil },
		"SaveEnrichment":        func(d *Deps) { d.SaveEnrichment = nil },
		"GroupListing":          func(d *Deps) { d.GroupListing = nil },
		"MergeProperty":         func(d *Deps) { d.MergeProperty = nil },
		"MarkEnriched":          func(d *Deps) { d.MarkEnriched = nil },
	}

	for name, remove := range cases {
		t.Run(name, func(t *testing.T) {
			deps := full
			remove(&deps)

			_, err := New(deps)
			if !errors.Is(err, ErrMissingDependency) {
				t.Fatalf("New() error = %v, want ErrMissingDependency", err)
			}
		})
	}
}

func TestNewRejectsNonPositiveWorkersAndBatchSize(t *testing.T) {
	t.Run("workers", func(t *testing.T) {
		for _, workers := range []int{0, -1} {
			deps := newStub(nil).Deps
			deps.Workers = workers

			if _, err := New(deps); !errors.Is(err, ErrInvalidWorkers) {
				t.Errorf("New() com Workers=%d error = %v, want ErrInvalidWorkers", workers, err)
			}
		}
	})

	t.Run("batch size", func(t *testing.T) {
		for _, size := range []int{0, -1} {
			deps := newStub(nil).Deps
			deps.BatchSize = size

			if _, err := New(deps); !errors.Is(err, ErrInvalidBatchSize) {
				t.Errorf("New() com BatchSize=%d error = %v, want ErrInvalidBatchSize", size, err)
			}
		}
	})
}

func TestNewPipelineRejectsMissingConfigAndPool(t *testing.T) {
	if _, err := NewPipeline(nil, nil); !errors.Is(err, ErrMissingConfig) {
		t.Errorf("NewPipeline(nil, nil) error = %v, want ErrMissingConfig", err)
	}
}
