package db

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// Os testes deste arquivo, como os do resto do pacote, **não tocam no banco**:
// cobrem as funções puras (montagem de argumentos, validação e a geometria do
// pré-filtro). O comportamento transacional — idempotência do vínculo,
// GREATEST no contador, guarda do DELETE — exige um PostgreSQL real e fica para
// o QA / testes de integração.

func ptr[T any](value T) *T { return &value }

func TestPropertyInsertArgsOrderAndArrayNormalization(t *testing.T) {
	property := Property{
		ID:               "ignorado-no-insert",
		CanonicalAddress: ptr("Rua A, 100"),
		Neighborhood:     ptr("Centro"),
		City:             ptr("Curitiba"),
		State:            ptr("PR"),
		Lat:              ptr(-25.43),
		Lng:              ptr(-49.27),
		BedroomCount:     ptr(3),
		BathroomCount:    ptr(2),
		ParkingSpots:     ptr(1),
		AreaSqm:          ptr(72.5),
		Amenities:        []string{"piscina"},
		Description:      ptr("Perto do centro"),
		Photos:           nil,
		TransactionType:  ptr("venda"),
		PropertyType:     ptr("apartamento"),
		// Deve ser ignorado: o contador é mantido pelo par link/unlink.
		ActiveListingCount: 7,
	}

	args := propertyInsertArgs(property)

	// A ordem é o contrato com os placeholders de insertPropertySQL.
	if got, want := len(args), 15; got != want {
		t.Fatalf("len(args) = %d, want %d", got, want)
	}

	wantPointers := []any{
		property.CanonicalAddress, property.Neighborhood, property.City,
		property.State, property.Lat, property.Lng, property.BedroomCount,
		property.BathroomCount, property.ParkingSpots, property.AreaSqm,
	}
	for i, want := range wantPointers {
		if got := args[i]; got != want {
			t.Errorf("args[%d] = %v, want %v", i, got, want)
		}
	}

	if got, want := args[10], []string{"piscina"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args[10] (amenities) = %v, want %v", got, want)
	}
	if got := args[11]; got != property.Description {
		t.Errorf("args[11] (description) = %v, want the description pointer", got)
	}
	// nil precisa virar array vazio: a coluna nunca guarda NULL vindo daqui.
	if got, want := args[12], []string{}; !reflect.DeepEqual(got, want) {
		t.Errorf("args[12] (photos) = %v, want %v", got, want)
	}
	if got := args[13]; got != property.TransactionType {
		t.Errorf("args[13] (transaction_type) = %v, want the transaction type pointer", got)
	}
	if got := args[14]; got != property.PropertyType {
		t.Errorf("args[14] (property_type) = %v, want the property type pointer", got)
	}
}

func TestPropertyInsertArgsKeepsNilPointersAsNil(t *testing.T) {
	// Um imóvel recém-criado pela deduplicação pode não ter nenhum campo
	// consolidado ainda: tudo precisa chegar como NULL, não como zero value.
	args := propertyInsertArgs(Property{})

	for i, arg := range args {
		switch i {
		case 10, 12: // amenities e photos viram array vazio, não nil
			if arg == nil {
				t.Errorf("args[%d] = nil, want empty slice", i)
			}
			continue
		}
		if !reflect.ValueOf(arg).IsNil() {
			t.Errorf("args[%d] = %v, want a nil pointer (NULL)", i, arg)
		}
	}
}

func TestPropertyUpdateArgsAppendsIDLast(t *testing.T) {
	property := Property{
		ID:               "  0f1b2c3d-0000-4000-8000-000000000001  ",
		CanonicalAddress: ptr("Rua B, 200"),
	}

	args, err := propertyUpdateArgs(property)
	if err != nil {
		t.Fatalf("propertyUpdateArgs() error = %v, want nil", err)
	}

	// updatePropertySQL usa os mesmos 15 placeholders do INSERT e o id em $16.
	if got, want := len(args), 16; got != want {
		t.Fatalf("len(args) = %d, want %d", got, want)
	}
	if got, want := args[15], "0f1b2c3d-0000-4000-8000-000000000001"; got != want {
		t.Errorf("args[15] (id) = %v, want %q (trimmed)", got, want)
	}
	if got := args[0]; got != property.CanonicalAddress {
		t.Errorf("args[0] = %v, want the canonical address pointer", got)
	}
}

func TestPropertyUpdateArgsRequiresID(t *testing.T) {
	for _, id := range []string{"", "   "} {
		_, err := propertyUpdateArgs(Property{ID: id})
		if err == nil {
			t.Fatalf("propertyUpdateArgs(ID=%q) error = nil, want error about the id", id)
		}
		if !strings.Contains(err.Error(), "id") {
			t.Errorf("propertyUpdateArgs(ID=%q) error = %q, want it to mention the id", id, err)
		}
	}
}

func TestNormalizeTextArrayNeverReturnsNil(t *testing.T) {
	if got := normalizeTextArray(nil); got == nil {
		t.Fatal("normalizeTextArray(nil) = nil, want empty slice")
	} else if len(got) != 0 {
		t.Errorf("normalizeTextArray(nil) = %v, want empty slice", got)
	}

	values := []string{"piscina", "elevador"}
	if got := normalizeTextArray(values); !reflect.DeepEqual(got, values) {
		t.Errorf("normalizeTextArray(%v) = %v, want the same values", values, got)
	}
}

func TestValidateCoordinatesRejectsOutOfRange(t *testing.T) {
	tests := []struct {
		name   string
		lat    float64
		lng    float64
		wantIn string
	}{
		{name: "latitude acima de 90", lat: 91, lng: 0, wantIn: "lat"},
		{name: "latitude abaixo de -90", lat: -90.5, lng: 0, wantIn: "lat"},
		{name: "longitude acima de 180", lat: 0, lng: 181, wantIn: "lng"},
		{name: "longitude abaixo de -180", lat: 0, lng: -180.1, wantIn: "lng"},
		{name: "latitude NaN", lat: math.NaN(), lng: 0, wantIn: "lat"},
		{name: "longitude NaN", lat: 0, lng: math.NaN(), wantIn: "lng"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCoordinates(tt.lat, tt.lng)
			if err == nil {
				t.Fatalf("validateCoordinates(%v, %v) error = nil, want error mentioning %q", tt.lat, tt.lng, tt.wantIn)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("validateCoordinates(%v, %v) error = %q, want it to mention %q", tt.lat, tt.lng, err, tt.wantIn)
			}
		})
	}

	// As bordas são válidas: um imóvel exatamente no limite não é entrada
	// inválida.
	for _, valid := range [][2]float64{{-90, -180}, {90, 180}, {0, 0}} {
		if err := validateCoordinates(valid[0], valid[1]); err != nil {
			t.Errorf("validateCoordinates(%v, %v) error = %v, want nil", valid[0], valid[1], err)
		}
	}
}

func TestBoundingBoxContainsCircle(t *testing.T) {
	const (
		lat    = -25.4284 // Curitiba
		lng    = -49.2733
		radius = 1000
	)

	minLat, maxLat, minLng, maxLng := boundingBox(lat, lng, radius)

	if lat <= minLat || lat >= maxLat {
		t.Fatalf("boundingBox() lat range = [%v, %v], want it to contain %v", minLat, maxLat, lat)
	}
	if lng <= minLng || lng >= maxLng {
		t.Fatalf("boundingBox() lng range = [%v, %v], want it to contain %v", minLng, maxLng, lng)
	}

	// A borda do retângulo tem de estar **fora** do círculo em ambos os eixos:
	// um retângulo mais apertado que o raio perderia resultados válidos, e o
	// filtro fino não teria como recuperá-los.
	if got := haversineMeters(lat, lng, maxLat, lng); got < radius {
		t.Errorf("distância até a borda norte = %.1f m, want >= %d m", got, radius)
	}
	if got := haversineMeters(lat, lng, minLat, lng); got < radius {
		t.Errorf("distância até a borda sul = %.1f m, want >= %d m", got, radius)
	}
	if got := haversineMeters(lat, lng, lat, maxLng); got < radius {
		t.Errorf("distância até a borda leste = %.1f m, want >= %d m", got, radius)
	}
	if got := haversineMeters(lat, lng, lat, minLng); got < radius {
		t.Errorf("distância até a borda oeste = %.1f m, want >= %d m", got, radius)
	}
}

func TestBoundingBoxLongitudeWidensWithLatitude(t *testing.T) {
	const radius = 5000

	_, _, minLngEquator, maxLngEquator := boundingBox(0, 0, radius)
	_, _, minLngNorth, maxLngNorth := boundingBox(60, 0, radius)

	widthEquator := maxLngEquator - minLngEquator
	widthNorth := maxLngNorth - minLngNorth

	// Um grau de longitude encolhe com o cosseno da latitude, então cobrir o
	// mesmo raio em graus exige um retângulo mais largo longe do equador.
	if widthNorth <= widthEquator {
		t.Errorf("largura em lat 60 = %v, want > largura no equador = %v", widthNorth, widthEquator)
	}
}

func TestBoundingBoxOpensUpAtPolesAndAntimeridian(t *testing.T) {
	tests := []struct {
		name   string
		lat    float64
		lng    float64
		radius int
	}{
		{name: "perto do polo norte", lat: 89.999, lng: 10, radius: 1000},
		{name: "sobre o antimeridiano", lat: 0, lng: 179.99, radius: 5000},
		{name: "raio maior que o planeta", lat: 0, lng: 0, radius: 40_000_000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, minLng, maxLng := boundingBox(tt.lat, tt.lng, tt.radius)
			if minLng != -180 || maxLng != 180 {
				t.Errorf("boundingBox() lng range = [%v, %v], want [-180, 180]", minLng, maxLng)
			}
		})
	}
}

func TestBoundingBoxClampsLatitude(t *testing.T) {
	minLat, maxLat, _, _ := boundingBox(89.99, 0, 100_000)
	if maxLat > 90 {
		t.Errorf("boundingBox() maxLat = %v, want <= 90", maxLat)
	}

	minLat, _, _, _ = boundingBox(-89.99, 0, 100_000)
	if minLat < -90 {
		t.Errorf("boundingBox() minLat = %v, want >= -90", minLat)
	}
}

func TestHaversineMeters(t *testing.T) {
	// Distância a si mesmo é exatamente zero (e não um resíduo de ponto
	// flutuante): é o caso do imóvel na coordenada exata do centro da busca.
	if got := haversineMeters(-25.4284, -49.2733, -25.4284, -49.2733); got != 0 {
		t.Errorf("haversineMeters(mesmo ponto) = %v, want 0", got)
	}

	// Um grau de latitude no meridiano ≈ 111,2 km.
	if got := haversineMeters(0, 0, 1, 0); math.Abs(got-111195) > 500 {
		t.Errorf("haversineMeters(0,0 -> 1,0) = %.0f m, want ~111195 m", got)
	}

	// Curitiba -> São Paulo ≈ 338 km em linha reta.
	got := haversineMeters(-25.4284, -49.2733, -23.5505, -46.6333)
	if math.Abs(got-338_000) > 5_000 {
		t.Errorf("haversineMeters(Curitiba -> São Paulo) = %.0f m, want ~338000 m", got)
	}

	// Simetria: a ordem dos pontos não muda a distância.
	reverse := haversineMeters(-23.5505, -46.6333, -25.4284, -49.2733)
	if math.Abs(got-reverse) > 1e-6 {
		t.Errorf("haversineMeters não é simétrico: %v != %v", got, reverse)
	}
}

func TestFilterWithinRadiusDropsCornersAndSortsByDistance(t *testing.T) {
	const (
		lat    = -25.4284
		lng    = -49.2733
		radius = 1000
	)

	minLat, _, _, maxLng := boundingBox(lat, lng, radius)

	near := Property{ID: "near", Lat: ptr(lat + 0.001), Lng: ptr(lng)}
	far := Property{ID: "far", Lat: ptr(lat + 0.005), Lng: ptr(lng)}
	// O canto do retângulo está dentro do pré-filtro do banco, mas fora do
	// círculo — é exatamente o que o filtro fino existe para descartar.
	corner := Property{ID: "corner", Lat: ptr(minLat), Lng: ptr(maxLng)}
	// Sem geocodificação: não dá para afirmar que está no raio.
	unlocated := Property{ID: "unlocated"}

	got := filterWithinRadius([]Property{far, corner, unlocated, near}, lat, lng, radius)

	wantIDs := []string{"near", "far"}
	gotIDs := make([]string, 0, len(got))
	for _, property := range got {
		gotIDs = append(gotIDs, property.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("filterWithinRadius() = %v, want %v (mais próximo primeiro, canto e sem coordenada descartados)", gotIDs, wantIDs)
	}
}

func TestFilterWithinRadiusIsInclusiveOnTheEdge(t *testing.T) {
	// O raio é inclusivo: "imóveis a até 1 km" inclui o que está a 1 km — e
	// exclui o que está logo depois.
	edge := Property{ID: "edge", Lat: ptr(0.01), Lng: ptr(0.0)}
	distance := haversineMeters(0, 0, 0.01, 0)

	if got := filterWithinRadius([]Property{edge}, 0, 0, int(math.Ceil(distance))); len(got) != 1 {
		t.Errorf("filterWithinRadius(raio = %.0f m) = %v, want the property at %.1f m to be kept",
			math.Ceil(distance), got, distance)
	}
	if got := filterWithinRadius([]Property{edge}, 0, 0, int(distance)-1); len(got) != 0 {
		t.Errorf("filterWithinRadius(raio = %d m) = %v, want the property at %.1f m to be dropped",
			int(distance)-1, got, distance)
	}
}

func TestFilterWithinRadiusReturnsEmptyNotNil(t *testing.T) {
	// Nenhum resultado é uma slice vazia: o chamador itera sem checar nil.
	if got := filterWithinRadius(nil, 0, 0, 100); got == nil {
		t.Fatal("filterWithinRadius(nil) = nil, want empty slice")
	} else if len(got) != 0 {
		t.Errorf("filterWithinRadius(nil) = %v, want empty slice", got)
	}
}
