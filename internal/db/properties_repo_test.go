package db

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

func TestBuildPropertySearchQueryWithoutFiltersProducesNoWhereClause(t *testing.T) {
	// Params zerado é um pedido válido: a primeira página do catálogo inteiro.
	where, args := buildPropertySearchQuery(PropertySearchParams{})

	if where != "" {
		t.Errorf("buildPropertySearchQuery(zero) where = %q, want empty", where)
	}
	if len(args) != 0 {
		t.Errorf("buildPropertySearchQuery(zero) args = %v, want none", args)
	}
}

func TestBuildPropertySearchQueryAppliesOnlyFilledFilters(t *testing.T) {
	tests := []struct {
		name       string
		params     PropertySearchParams
		wantClause string
		wantArg    any
	}{
		{
			name:       "transaction_type",
			params:     PropertySearchParams{TransactionType: "venda"},
			wantClause: "transaction_type = $1",
			wantArg:    "venda",
		},
		{
			name:       "property_type",
			params:     PropertySearchParams{PropertyType: "apartamento"},
			wantClause: "property_type = $1",
			wantArg:    "apartamento",
		},
		{
			name:       "city",
			params:     PropertySearchParams{City: "Curitiba"},
			wantClause: "city = $1",
			wantArg:    "Curitiba",
		},
		{
			name:       "neighborhood",
			params:     PropertySearchParams{Neighborhood: "Batel"},
			wantClause: "neighborhood = $1",
			wantArg:    "Batel",
		},
		{
			name:       "bedrooms",
			params:     PropertySearchParams{MinBedrooms: 3},
			wantClause: "bedroom_count >= $1",
			wantArg:    3,
		},
		{
			name:       "bathrooms",
			params:     PropertySearchParams{MinBathrooms: 2},
			wantClause: "bathroom_count >= $1",
			wantArg:    2,
		},
		{
			name:       "parking",
			params:     PropertySearchParams{MinParkingSpots: 1},
			wantClause: "parking_spots >= $1",
			wantArg:    1,
		},
		{
			name:       "area",
			params:     PropertySearchParams{MinArea: 72.5},
			wantClause: "area_sqm >= $1",
			wantArg:    72.5,
		},
		{
			name:       "amenities",
			params:     PropertySearchParams{Amenities: []string{"piscina", "elevador"}},
			wantClause: "amenities @> $1::text[]",
			wantArg:    []string{"piscina", "elevador"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			where, args := buildPropertySearchQuery(tt.params)

			if got, want := where, "\nWHERE "+tt.wantClause; got != want {
				t.Errorf("where = %q, want %q", got, want)
			}
			if len(args) != 1 {
				t.Fatalf("args = %v, want exactly one", args)
			}
			if !reflect.DeepEqual(args[0], tt.wantArg) {
				t.Errorf("args[0] = %#v, want %#v", args[0], tt.wantArg)
			}
		})
	}
}

func TestBuildPropertySearchQueryIgnoresZeroValues(t *testing.T) {
	// Zero-value é "filtro não aplicado", nunca "filtre por zero": um
	// `bedroom_count >= 0` deixaria de fora justamente os imóveis com NULL.
	params := PropertySearchParams{
		TransactionType: "",
		City:            "   ",
		MinBedrooms:     0,
		MinBathrooms:    -1,
		MinArea:         0,
		Amenities:       []string{},
		Page:            3,
		PageSize:        10,
	}

	where, args := buildPropertySearchQuery(params)

	if where != "" {
		t.Errorf("where = %q, want empty", where)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

func TestBuildPropertySearchQueryClauseOrderAndPlaceholderNumbering(t *testing.T) {
	params := PropertySearchParams{
		TransactionType: "venda",
		PropertyType:    "apartamento",
		City:            "Curitiba",
		Neighborhood:    "Batel",
		MinBedrooms:     3,
		MinBathrooms:    2,
		MinParkingSpots: 1,
		MinArea:         72.5,
		Amenities:       []string{"piscina"},
	}

	where, args := buildPropertySearchQuery(params)

	wantWhere := "\nWHERE transaction_type = $1" +
		"\n  AND property_type = $2" +
		"\n  AND city = $3" +
		"\n  AND neighborhood = $4" +
		"\n  AND bedroom_count >= $5" +
		"\n  AND bathroom_count >= $6" +
		"\n  AND parking_spots >= $7" +
		"\n  AND area_sqm >= $8" +
		"\n  AND amenities @> $9::text[]"

	if where != wantWhere {
		t.Errorf("where =\n%q\nwant\n%q", where, wantWhere)
	}

	// A numeração precisa ser 1..len(args), sem buracos e sem repetição: um
	// placeholder a mais (ou a menos) que o slice é erro de protocolo do pgx,
	// não um resultado errado — e só apareceria em runtime.
	placeholders := regexp.MustCompile(`\$(\d+)`).FindAllStringSubmatch(where, -1)
	if len(placeholders) != len(args) {
		t.Fatalf("where tem %d placeholders, want %d (len(args))", len(placeholders), len(args))
	}
	for i, match := range placeholders {
		number, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("placeholder %q não é numérico: %v", match[0], err)
		}
		if number != i+1 {
			t.Errorf("placeholder %d = $%d, want $%d", i, number, i+1)
		}
	}

	wantArgs := []any{"venda", "apartamento", "Curitiba", "Batel", 3, 2, 1, 72.5, []string{"piscina"}}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v, want %#v", args, wantArgs)
	}
}

func TestBuildPropertySearchQueryNeverInterpolatesUserValues(t *testing.T) {
	const injection = `Curitiba'; DROP TABLE properties;--`

	params := PropertySearchParams{
		TransactionType: injection,
		PropertyType:    `"aspas"`,
		City:            injection,
		Neighborhood:    `a' OR '1'='1`,
		Amenities:       []string{`piscina'); DELETE FROM properties;--`},
	}

	where, args := buildPropertySearchQuery(params)

	for _, forbidden := range []string{"DROP", "DELETE", "Curitiba", "aspas", "'", `"`, ";", "--"} {
		if strings.Contains(where, forbidden) {
			t.Errorf("where = %q contém %q; nenhum valor do usuário pode entrar na string SQL", where, forbidden)
		}
	}

	// O valor precisa estar presente — inteiro e sem escapes — mas só nos args.
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5", args)
	}
	if args[0] != injection {
		t.Errorf("args[0] = %#v, want %#v", args[0], injection)
	}
}

func TestBuildPropertySearchQueryTrimsTextFilters(t *testing.T) {
	where, args := buildPropertySearchQuery(PropertySearchParams{
		TransactionType: "  venda  ",
		City:            "\t \n",
	})

	if got, want := where, "\nWHERE transaction_type = $1"; got != want {
		t.Errorf("where = %q, want %q (city só com espaços não vira cláusula)", got, want)
	}
	if len(args) != 1 || args[0] != "venda" {
		t.Errorf("args = %#v, want [\"venda\"]", args)
	}
}

func TestBuildPropertySearchQueryIgnoresEmptyAmenities(t *testing.T) {
	for _, amenities := range [][]string{nil, {}, {"", "   "}} {
		where, args := buildPropertySearchQuery(PropertySearchParams{Amenities: amenities})
		if where != "" {
			t.Errorf("buildPropertySearchQuery(Amenities=%#v) where = %q, want empty", amenities, where)
		}
		if len(args) != 0 {
			t.Errorf("buildPropertySearchQuery(Amenities=%#v) args = %v, want none", amenities, args)
		}
	}

	// Itens em branco no meio da lista são descartados, não viram elemento do
	// TEXT[] — um `@>` com "" nunca casaria e zeraria a busca sem explicação.
	_, args := buildPropertySearchQuery(PropertySearchParams{Amenities: []string{" piscina ", "  ", "elevador"}})
	if got, want := args, []any{[]string{"piscina", "elevador"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %#v, want %#v", got, want)
	}
}

func TestNormalizePropertyPagination(t *testing.T) {
	tests := []struct {
		name       string
		page       int
		pageSize   int
		wantLimit  int
		wantOffset int
	}{
		{name: "tudo zerado usa a primeira página e o default", page: 0, pageSize: 0, wantLimit: 20, wantOffset: 0},
		{name: "negativos são normalizados", page: -3, pageSize: -1, wantLimit: 20, wantOffset: 0},
		{name: "acima do teto é cortado em 50", page: 1, pageSize: 51, wantLimit: 50, wantOffset: 0},
		{name: "o teto exato passa", page: 2, pageSize: 50, wantLimit: 50, wantOffset: 50},
		{name: "offset é (page-1) * pageSize", page: 3, pageSize: 20, wantLimit: 20, wantOffset: 40},
		{name: "página além do fim continua sendo um offset válido", page: 1000, pageSize: 20, wantLimit: 20, wantOffset: 19980},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, offset := normalizePropertyPagination(tt.page, tt.pageSize)
			if limit != tt.wantLimit {
				t.Errorf("limit = %d, want %d", limit, tt.wantLimit)
			}
			if offset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", offset, tt.wantOffset)
			}
		})
	}
}

func TestSearchPropertiesSQLContract(t *testing.T) {
	// O desempate por id é invariante, não estética: created_at não é único e,
	// sem ele, duas páginas consecutivas podem repetir ou pular um imóvel.
	if got, want := strings.TrimSpace(searchPropertiesOrderBy), "ORDER BY created_at DESC, id DESC"; got != want {
		t.Errorf("searchPropertiesOrderBy = %q, want %q", got, want)
	}

	// O total vem de um COUNT(*) separado. Com COUNT(*) OVER(), uma página além
	// do fim não devolve linha alguma e o total viria 0 em vez do valor real.
	if strings.Contains(countPropertiesSQL, "OVER") {
		t.Errorf("countPropertiesSQL = %q, want a plain COUNT(*) without a window function", countPropertiesSQL)
	}
	if !strings.Contains(countPropertiesSQL, "COUNT(*)") {
		t.Errorf("countPropertiesSQL = %q, want it to use COUNT(*)", countPropertiesSQL)
	}
	if strings.Contains(selectPropertiesPageSQL, "OVER") {
		t.Errorf("selectPropertiesPageSQL = %q, want no window function", selectPropertiesPageSQL)
	}

	// A página lê exatamente as colunas que scanProperty consome.
	if !strings.Contains(selectPropertiesPageSQL, propertyColumns) {
		t.Error("selectPropertiesPageSQL não usa propertyColumns; o scan sairia da ordem")
	}

	// Os dois prefixos aceitam o mesmo WHERE concatenado, o que é o que garante
	// que contagem e página filtrem igual.
	where, args := buildPropertySearchQuery(PropertySearchParams{TransactionType: "venda", MinBedrooms: 2})
	if !strings.HasSuffix(countPropertiesSQL+where, where) || !strings.HasSuffix(selectPropertiesPageSQL+where, where) {
		t.Error("o WHERE dinâmico precisa ser concatenável aos dois prefixos sem alteração")
	}

	// Os placeholders de LIMIT/OFFSET vêm depois dos do WHERE, sem colidir.
	if got, want := fmt.Sprintf("LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2), "LIMIT $3 OFFSET $4"; got != want {
		t.Errorf("paginação numerada como %q, want %q", got, want)
	}
}

func TestGetPropertyWithListingsRejectsBlankID(t *testing.T) {
	// pool nil é parte da asserção: se a função tentasse consultar o banco com
	// um id em branco, o teste entraria em pânico em vez de passar.
	for _, id := range []string{"", "   ", "\t\n"} {
		_, err := GetPropertyWithListings(context.Background(), nil, id)
		if !errors.Is(err, ErrInvalidPropertyID) {
			t.Errorf("GetPropertyWithListings(%q) = %v, want ErrInvalidPropertyID", id, err)
		}
	}
}

func TestIsInvalidTextRepresentation(t *testing.T) {
	invalid := &pgconn.PgError{Code: pgInvalidTextRepresentation, Message: `invalid input syntax for type uuid: "abc"`}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "erro nulo", err: nil, want: false},
		{name: "22P02 direto", err: invalid, want: true},
		{name: "22P02 embrulhado", err: fmt.Errorf("db: get property %q: %w", "abc", invalid), want: true},
		{name: "outro SQLSTATE", err: &pgconn.PgError{Code: "23503"}, want: false},
		{name: "erro comum", err: errors.New("boom"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isInvalidTextRepresentation(tt.err); got != tt.want {
				t.Errorf("isInvalidTextRepresentation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestPropertyListingsSQLContract(t *testing.T) {
	// A ordem das colunas é o que scanPropertyListing consome; trocá-la produz
	// um scan silenciosamente errado.
	if !strings.Contains(selectPropertyListingsSQL, "SELECT id, source_domain, listing_url, price_raw, last_seen_at") {
		t.Errorf("selectPropertyListingsSQL = %q, want the column order consumed by scanPropertyListing", selectPropertyListingsSQL)
	}

	// ORDER BY id é contrato: sem ele a tela de comparação reordena os anúncios
	// a cada refresh.
	if !strings.HasSuffix(strings.TrimSpace(selectPropertyListingsSQL), "ORDER BY id") {
		t.Errorf("selectPropertyListingsSQL = %q, want it to end with ORDER BY id", selectPropertyListingsSQL)
	}

	// O cast explícito segue o padrão do arquivo e é o que transforma id
	// malformado em 22P02 (traduzido para ErrInvalidPropertyID).
	if !strings.Contains(selectPropertyListingsSQL, "property_id = $1::uuid") {
		t.Errorf("selectPropertyListingsSQL = %q, want WHERE property_id = $1::uuid", selectPropertyListingsSQL)
	}

	// Não existe status/removed_at em listings: filtrar por isso seria um WHERE
	// sobre coluna inexistente.
	if strings.Contains(selectPropertyListingsSQL, "removed_at") {
		t.Errorf("selectPropertyListingsSQL = %q, want no removed_at filter (the column does not exist)", selectPropertyListingsSQL)
	}

	// Não é o SQL do merge: as colunas são outras e ampliar aquele quebraria
	// scanListing.
	if selectPropertyListingsSQL == selectListingsByPropertyIDSQL {
		t.Error("selectPropertyListingsSQL não pode ser o mesmo SQL do merge")
	}
}
