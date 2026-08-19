package enrichqueue

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/imobhub/api/internal/db"
	"github.com/imobhub/api/internal/enrichment"
	"github.com/imobhub/api/internal/grouping"
)

// A ordem das sete etapas é invariante de correção, não estilo: SaveEnrichment
// precisa vir antes de MergeProperty porque a consolidação relê bedroom_count,
// amenities, image_urls e description_raw **do banco**.
func TestProcessListingRunsTheSevenStepsInOrder(t *testing.T) {
	stub := newStub(samplePending(1))

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	want := []string{"normalize", "extract", "geocode", "save", "group", "merge", "mark"}
	stub.snapshot(func(s *stubDeps) {
		if !reflect.DeepEqual(s.calls, want) {
			t.Errorf("ordem das etapas = %v, want %v", s.calls, want)
		}
	})
}

// O agrupamento precisa receber os valores recém-calculados, não os da linha
// lida — e o property_id da linha, que é o que dá a saída barata sem IA.
func TestProcessListingFeedsTheGrouperWithFreshValues(t *testing.T) {
	pending := samplePending(1)
	linked := "22222222-2222-2222-2222-222222222222"
	pending[0].PropertyID = &linked
	pending[0].NormalizedNeighborhood = "valor antigo"

	stub := newStub(pending)

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	stub.snapshot(func(s *stubDeps) {
		if len(s.grouped) != 1 {
			t.Fatalf("len(grouped) = %d, want 1", len(s.grouped))
		}
		listing := s.grouped[0]

		if got, want := listing.NormalizedNeighborhood, "Jardim Botânico"; got != want {
			t.Errorf("NormalizedNeighborhood = %q, want %q (valor recém-calculado)", got, want)
		}
		if listing.Lat == nil || listing.Lng == nil {
			t.Fatal("Lat/Lng chegaram nil ao agrupamento")
		}
		if listing.BedroomCount == nil || *listing.BedroomCount != 3 {
			t.Errorf("BedroomCount = %v, want 3", listing.BedroomCount)
		}
		if listing.PropertyID == nil || *listing.PropertyID != linked {
			t.Errorf("PropertyID = %v, want %q — sem ele o agrupamento pagaria IA de novo", listing.PropertyID, linked)
		}
		if got, want := listing.ImageURLs, pending[0].ImageURLs; !reflect.DeepEqual(got, want) {
			t.Errorf("ImageURLs = %v, want %v", got, want)
		}
	})
}

// Bairro não reconhecido grava NULL, nunca string vazia: é o contrato do
// normalizador, e "" faria o agrupamento comparar um bairro em branco.
func TestProcessListingStoresNullNeighborhoodWhenNothingIsRecognized(t *testing.T) {
	for _, normalized := range []string{"", "   "} {
		t.Run(fmt.Sprintf("%q", normalized), func(t *testing.T) {
			stub := newStub(samplePending(1))
			stub.Deps.NormalizeNeighborhood = func(string, string) string { return normalized }

			if _, err := stub.run(t, context.Background()); err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}

			stub.snapshot(func(s *stubDeps) {
				if len(s.saved) != 1 {
					t.Fatalf("len(saved) = %d, want 1", len(s.saved))
				}
				if s.saved[0].NormalizedNeighborhood != nil {
					t.Errorf("NormalizedNeighborhood = %q, want nil (NULL)", *s.saved[0].NormalizedNeighborhood)
				}
			})
		})
	}
}

// O extrator recebe descrição **e** bedrooms_raw: alguns portais publicam o
// número de quartos só nesse campo.
func TestProcessListingExtractsFromDescriptionAndBedroomsRaw(t *testing.T) {
	stub := newStub(samplePending(1))

	var received string
	stub.Deps.ExtractTerms = func(text string) enrichment.ExtractedData {
		received = text
		return enrichment.ExtractedData{}
	}

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	if got, want := received, "3 quartos com piscina\n3"; got != want {
		t.Errorf("texto enviado ao extrator = %q, want %q", got, want)
	}
}

// Endereço irresolvível não é falha: grava lat/lng NULL, pula o agrupamento e
// **carimba** — o geocoder faz negative caching, repetir a pergunta não muda o
// resultado e só gasta quota.
func TestProcessListingSkipsAndStampsUnresolvableAddresses(t *testing.T) {
	cases := map[string]error{
		"address not found": enrichment.ErrAddressNotFound,
		"empty address":     enrichment.ErrEmptyAddress,
	}

	for name, sentinel := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newStub(samplePending(1))
			stub.Deps.Geocode = func(context.Context, string, string, string) (float64, float64, error) {
				return 0, 0, fmt.Errorf("enrichment: geocoding %q: %w", "Rua X", sentinel)
			}

			summary, err := stub.run(t, context.Background())
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			if got, want := summary.Skipped, 1; got != want {
				t.Errorf("summary.Skipped = %d, want %d", got, want)
			}
			if summary.Failed != 0 || summary.Enriched != 0 {
				t.Errorf("summary.Failed/Enriched = %d/%d, want 0/0", summary.Failed, summary.Enriched)
			}

			stub.snapshot(func(s *stubDeps) {
				if len(s.saved) != 1 {
					t.Fatalf("len(saved) = %d, want 1 — os demais campos derivados precisam ser persistidos", len(s.saved))
				}
				if s.saved[0].Lat != nil || s.saved[0].Lng != nil {
					t.Error("lat/lng deveriam ser NULL para endereço irresolvível")
				}
				if len(s.grouped) != 0 {
					t.Error("anúncio sem coordenadas não pode ser agrupado")
				}
				if got, want := s.marked, []int64{1}; !reflect.DeepEqual(got, want) {
					t.Errorf("marked = %v, want %v — sem o carimbo o anúncio volta à fila para sempre", got, want)
				}
			})
		})
	}
}

// ErrAddressNotFound pode ser espúrio: nasce de um 200 com zero resultados, que
// o provider devolve ocasionalmente para endereços que já resolveu. Apagar um
// geocode bom **e** carimbar deixaria o anúncio sem coordenadas e sem nova
// tentativa — por isso o par anterior é preservado, e o agrupamento continua.
func TestProcessListingPreservesPreviousCoordinatesWhenTheAddressStopsResolving(t *testing.T) {
	pending := samplePending(1)
	previousLat, previousLng := -15.7942, -47.8822
	pending[0].Lat = &previousLat
	pending[0].Lng = &previousLng

	stub := newStub(pending)
	stub.Deps.Geocode = func(context.Context, string, string, string) (float64, float64, error) {
		return 0, 0, fmt.Errorf("enrichment: geocoding: %w", enrichment.ErrAddressNotFound)
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	// Com coordenadas válidas o anúncio segue o caminho feliz: ele **tem** geo,
	// só não conseguiu confirmá-la agora.
	if got, want := summary.Enriched, 1; got != want {
		t.Errorf("summary.Enriched = %d, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		if len(s.saved) != 1 {
			t.Fatalf("len(saved) = %d, want 1", len(s.saved))
		}
		if s.saved[0].Lat == nil || *s.saved[0].Lat != previousLat {
			t.Errorf("lat gravada = %v, want %v — o geocode anterior não pode ser apagado", s.saved[0].Lat, previousLat)
		}
		if s.saved[0].Lng == nil || *s.saved[0].Lng != previousLng {
			t.Errorf("lng gravada = %v, want %v", s.saved[0].Lng, previousLng)
		}
		if len(s.grouped) != 1 {
			t.Fatalf("len(grouped) = %d, want 1 — o anúncio tem coordenadas e deve ser agrupado", len(s.grouped))
		}
		if got, want := s.marked, []int64{1}; !reflect.DeepEqual(got, want) {
			t.Errorf("marked = %v, want %v", got, want)
		}
	})
}

// O regeocode acontece mesmo com coordenadas já gravadas: `price_raw` também
// devolve o anúncio à fila, e `listings` não guarda o endereço do passe anterior
// para distinguir "só o preço mudou" de "o endereço foi corrigido". A decisão é
// deliberada (correção acima de custo) e está registrada no CLAUDE.md.
func TestProcessListingRegeocodesListingsThatAlreadyHaveCoordinates(t *testing.T) {
	pending := samplePending(1)
	stale := -1.0
	pending[0].Lat = &stale
	pending[0].Lng = &stale

	stub := newStub(pending)

	var geocoded int
	stub.Deps.Geocode = func(context.Context, string, string, string) (float64, float64, error) {
		geocoded++
		return -15.78, -47.93, nil
	}

	if _, err := stub.run(t, context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := geocoded, 1; got != want {
		t.Fatalf("Geocode chamado %d vezes, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		if s.saved[0].Lat == nil || *s.saved[0].Lat != -15.78 {
			t.Errorf("lat gravada = %v, want -15.78 (a coordenada nova vence a antiga)", s.saved[0].Lat)
		}
	})
}

// Erro transitório do geocoder (rede, timeout, status >= 400) não persiste nada
// e não carimba: gravar lat/lng NULL apagaria um geocode válido de um passe
// anterior, e o próximo run precisa tentar de novo.
func TestProcessListingKeepsTransientGeocodingFailuresPending(t *testing.T) {
	stub := newStub(samplePending(1))
	stub.Deps.Geocode = func(context.Context, string, string, string) (float64, float64, error) {
		return 0, 0, errors.New("enrichment: geocoding: unexpected status 503")
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := summary.Failed, 1; got != want {
		t.Errorf("summary.Failed = %d, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		if len(s.saved) != 0 {
			t.Error("erro transitório não pode persistir lat/lng NULL por cima de um geocode válido")
		}
		if len(s.marked) != 0 {
			t.Error("erro transitório não pode carimbar enriched_at")
		}
	})
}

// ErrListingNotGeocoded é defensivo (o fluxo já garante coordenadas): pula o
// agrupamento e o merge sem virar erro fatal, e o anúncio sai da fila.
func TestProcessListingSkipsWhenTheGrouperReportsMissingCoordinates(t *testing.T) {
	stub := newStub(samplePending(1))
	stub.Deps.GroupListing = func(_ context.Context, listing grouping.Listing) (string, bool, error) {
		return "", false, fmt.Errorf("grouping: listing %d: %w", listing.ID, grouping.ErrListingNotGeocoded)
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := summary.Skipped, 1; got != want {
		t.Errorf("summary.Skipped = %d, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		if len(s.merged) != 0 {
			t.Error("sem agrupamento não há canônico a consolidar")
		}
		if got, want := s.marked, []int64{1}; !reflect.DeepEqual(got, want) {
			t.Errorf("marked = %v, want %v", got, want)
		}
	})
}

// Erro da IA no agrupamento deixa o anúncio pendente: os campos derivados já
// foram gravados (são idempotentes), mas enriched_at não é carimbado.
func TestProcessListingKeepsListingPendingWhenGroupingFails(t *testing.T) {
	stub := newStub(samplePending(1))
	stub.Deps.GroupListing = func(context.Context, grouping.Listing) (string, bool, error) {
		return "", false, errors.New("ai: could not decode the model response")
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := summary.Failed, 1; got != want {
		t.Errorf("summary.Failed = %d, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		if len(s.saved) != 1 {
			t.Errorf("len(saved) = %d, want 1", len(s.saved))
		}
		if len(s.marked) != 0 {
			t.Error("anúncio com agrupamento falho não pode sair da fila")
		}
	})
}

// Falha no merge também deixa o anúncio pendente. A repetição é barata: o
// vínculo já está gravado e GroupListing sai cedo, sem IA.
func TestProcessListingKeepsListingPendingWhenMergeFails(t *testing.T) {
	stub := newStub(samplePending(1))
	stub.Deps.MergeProperty = func(context.Context, string) error {
		return errors.New("db: update property: connection reset")
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := summary.Failed, 1; got != want {
		t.Errorf("summary.Failed = %d, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		if len(s.marked) != 0 {
			t.Error("anúncio com merge falho não pode sair da fila")
		}
	})
}

// Falha ao persistir impede o agrupamento: consolidar sobre campos que nunca
// foram gravados produziria um canônico com NULL.
func TestProcessListingDoesNotGroupWhenPersistenceFails(t *testing.T) {
	stub := newStub(samplePending(1))
	stub.Deps.SaveEnrichment = func(context.Context, db.ListingEnrichment) error {
		return errors.New("db: update enrichment: deadlock detected")
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := summary.Failed, 1; got != want {
		t.Errorf("summary.Failed = %d, want %d", got, want)
	}

	stub.snapshot(func(s *stubDeps) {
		if len(s.grouped) != 0 {
			t.Error("o agrupamento não pode rodar sobre campos que não foram persistidos")
		}
		if len(s.marked) != 0 {
			t.Error("anúncio não persistido não pode sair da fila")
		}
	})
}

// Falha no carimbo mantém o anúncio na fila e o conta como falha — o trabalho já
// feito é idempotente e será repetido barato no próximo run.
func TestProcessListingCountsFailureWhenStampingFails(t *testing.T) {
	stub := newStub(samplePending(1))
	stub.Deps.MarkEnriched = func(context.Context, int64) error {
		return errors.New("db: mark listing enriched: connection reset")
	}

	summary, err := stub.run(t, context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got, want := summary.Failed, 1; got != want {
		t.Errorf("summary.Failed = %d, want %d", got, want)
	}
	if summary.Enriched != 0 {
		t.Errorf("summary.Enriched = %d, want 0", summary.Enriched)
	}
}
