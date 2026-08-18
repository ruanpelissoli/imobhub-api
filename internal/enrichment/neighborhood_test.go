package enrichment

import (
	"errors"
	"strings"
	"testing"
)

// testTable é a tabela usada pelos testes de comportamento. É escrita em forma
// "humana" de propósito (caixa e acentos livres) para exercitar a normalização
// das chaves feita na construção.
func testTable() AliasTable {
	return AliasTable{
		Global: AliasMap{
			"Centro":          "Centro",
			"Jd. Botânico":    "Jardim Botânico",
			"jardim botanico": "Jardim Botânico",
			"sudoeste":        "Sudoeste",
		},
		Cities: CityAliasMap{
			"Brasília": AliasMap{
				"asa nte":      "Asa Norte",
				"asa norte":    "Asa Norte",
				"sudoeste":     "Setor Sudoeste",
				"aguas claras": "Águas Claras",
			},
			"Goiânia": AliasMap{
				"st. bueno": "Setor Bueno",
			},
		},
	}
}

func newTestNormalizer(t *testing.T) NeighborhoodNormalizer {
	t.Helper()

	n, err := NewNeighborhoodNormalizerFromAliases(testTable())
	if err != nil {
		t.Fatalf("NewNeighborhoodNormalizerFromAliases() error = %v, want nil", err)
	}
	return n
}

func TestNormalize(t *testing.T) {
	n := newTestNormalizer(t)

	tests := []struct {
		name string
		raw  string
		city string
		want string
	}{
		// Caixa mista e espaços: as três grafias colapsam na mesma chave.
		{name: "uppercase hits alias", raw: "ASA NORTE", city: "Brasília", want: "Asa Norte"},
		{name: "lowercase hits alias", raw: "asa norte", city: "Brasília", want: "Asa Norte"},
		{name: "double spaces hit alias", raw: "asa  norte", city: "Brasília", want: "Asa Norte"},
		{name: "tabs and newlines collapse", raw: "\tasa\n\tnorte  ", city: "Brasília", want: "Asa Norte"},

		// Aliases: o valor canônico volta verbatim, com acento e caixa da tabela.
		{name: "typo alias from the brief", raw: "asa nte", city: "Brasília", want: "Asa Norte"},
		{name: "abbreviation alias", raw: "Jd. Botânico", city: "", want: "Jardim Botânico"},
		{name: "accentless key hits accented canonical", raw: "jardim botanico", city: "", want: "Jardim Botânico"},
		{name: "accented input hits accentless key", raw: "JARDIM BOTÂNICO", city: "", want: "Jardim Botânico"},
		{name: "canonical returned verbatim not re-title-cased", raw: "aguas claras", city: "Brasília", want: "Águas Claras"},

		// Fallback: title-case pt-BR com os acentos originais preservados.
		{name: "fallback title cases", raw: "VILA SÃO JOSÉ", city: "", want: "Vila São José"},
		{name: "fallback keeps connectives lowercase", raw: "JARDIM DAS ACACIAS", city: "", want: "Jardim das Acacias"},
		{name: "fallback does not invent accents", raw: "aguas lindas", city: "", want: "Aguas Lindas"},
		{name: "fallback keeps every connective lowercase", raw: "VILA DE SAO JORGE DO NORTE E DA SERRA", city: "", want: "Vila de Sao Jorge do Norte e da Serra"},
		{name: "fallback capitalizes leading connective", raw: "do carmo", city: "", want: "Do Carmo"},
		{name: "fallback handles apostrophe prefix", raw: "OLHO D'AGUA", city: "", want: "Olho d'Agua"},

		// "Bairro" redundante como prefixo e como sufixo.
		{name: "strips Bairro prefix", raw: "Bairro Jardim Botânico", city: "", want: "Jardim Botânico"},
		{name: "strips Bairro prefix with punctuation", raw: "Bairro: Jardim Botânico", city: "", want: "Jardim Botânico"},
		{name: "strips Bairro suffix", raw: "Jardim Botânico, Bairro", city: "", want: "Jardim Botânico"},
		{name: "strips Bairro case insensitively", raw: "BAIRRO asa norte", city: "Brasília", want: "Asa Norte"},
		{name: "keeps standalone Bairro", raw: "Bairro", city: "", want: "Bairro"},

		// Setor jamais é removido: é parte do nome real em Brasília e Goiânia.
		{name: "Setor survives untouched", raw: "Setor Sudoeste", city: "", want: "Setor Sudoeste"},
		{name: "Setor survives in mixed case", raw: "SETOR SUDOESTE", city: "Brasília", want: "Setor Sudoeste"},
		{name: "Setor survives in fallback", raw: "setor  oeste", city: "", want: "Setor Oeste"},
		{name: "Setor survives via alias", raw: "St. Bueno", city: "Goiânia", want: "Setor Bueno"},

		// Escopo por cidade x global.
		{name: "city alias wins over global", raw: "Sudoeste", city: "Brasília", want: "Setor Sudoeste"},
		{name: "no city falls back to global", raw: "Sudoeste", city: "", want: "Sudoeste"},
		{name: "unknown city falls back to global", raw: "Sudoeste", city: "Curitiba", want: "Sudoeste"},
		{name: "city name is normalized", raw: "sudoeste", city: "BRASILIA", want: "Setor Sudoeste"},
		{name: "city not covering the key falls back to global", raw: "Centro", city: "Brasília", want: "Centro"},
		{name: "city alias is invisible to other cities", raw: "asa nte", city: "Goiânia", want: "Asa Nte"},

		// Entrada vazia.
		{name: "empty string", raw: "", city: "Brasília", want: ""},
		{name: "only spaces", raw: "   ", city: "Brasília", want: ""},
		{name: "only whitespace characters", raw: "\t\n  \r", city: "", want: ""},
		{name: "only punctuation", raw: " -,. ", city: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := n.Normalize(tt.raw, tt.city); got != tt.want {
				t.Errorf("Normalize(%q, %q) = %q, want %q", tt.raw, tt.city, got, tt.want)
			}
		})
	}
}

// TestNormalizeIsIdempotent garante que reprocessar um valor já canônico não o
// muda — a fila de enriquecimento pode reprocessar o mesmo anúncio.
func TestNormalizeIsIdempotent(t *testing.T) {
	n := newTestNormalizer(t)

	inputs := []string{
		"ASA NORTE", "Jd. Botânico", "Bairro Jardim Botânico", "Setor Sudoeste",
		"VILA SÃO JOSÉ", "JARDIM DAS ACACIAS", "sudoeste", "",
	}

	for _, raw := range inputs {
		for _, city := range []string{"", "Brasília"} {
			once := n.Normalize(raw, city)
			twice := n.Normalize(once, city)
			if once != twice {
				t.Errorf("Normalize(Normalize(%q, %q), %q) = %q, want %q", raw, city, city, twice, once)
			}
		}
	}
}

// TestNormalizeIsSafeForConcurrentUse exercita o mesmo normalizer de várias
// goroutines. É o teste que flagra um cases.Caser ou transform.Transformer
// guardado como campo do struct (ambos guardam estado e corrompem a saída).
// Sem -race neste ambiente (exige cgo/gcc), o valor aqui é a checagem do
// resultado, não a detecção da corrida.
func TestNormalizeIsSafeForConcurrentUse(t *testing.T) {
	n := newTestNormalizer(t)

	cases := []struct{ raw, city, want string }{
		{"ASA NORTE", "Brasília", "Asa Norte"},
		{"VILA SÃO JOSÉ", "", "Vila São José"},
		{"Bairro Jardim Botânico", "", "Jardim Botânico"},
		{"Setor Sudoeste", "", "Setor Sudoeste"},
	}

	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			t.Parallel()
			for i := 0; i < 200; i++ {
				if got := n.Normalize(c.raw, c.city); got != c.want {
					t.Fatalf("Normalize(%q, %q) = %q, want %q", c.raw, c.city, got, c.want)
				}
			}
		})
	}
}

func TestNewNeighborhoodNormalizerUsesEmbeddedTable(t *testing.T) {
	n, err := NewNeighborhoodNormalizer()
	if err != nil {
		t.Fatalf("NewNeighborhoodNormalizer() error = %v, want nil", err)
	}

	impl, ok := n.(*neighborhoodNormalizer)
	if !ok {
		t.Fatalf("NewNeighborhoodNormalizer() returned %T, want *neighborhoodNormalizer", n)
	}

	total := len(impl.global)
	for _, aliases := range impl.cities {
		total += len(aliases)
	}
	if total < 10 || total > 20 {
		t.Errorf("embedded alias table has %d entries, want between 10 and 20", total)
	}

	// Casos citados no enunciado da task.
	if got := n.Normalize("asa nte", "Brasília"); got != "Asa Norte" {
		t.Errorf("Normalize(%q, %q) = %q, want %q", "asa nte", "Brasília", got, "Asa Norte")
	}
	if got := n.Normalize("Jd. Botânico", ""); got != "Jardim Botânico" {
		t.Errorf("Normalize(%q, %q) = %q, want %q", "Jd. Botânico", "", got, "Jardim Botânico")
	}
	if got := n.Normalize("Setor Sudoeste", "Goiânia"); got != "Setor Sudoeste" {
		t.Errorf("Normalize(%q, %q) = %q, want %q", "Setor Sudoeste", "Goiânia", got, "Setor Sudoeste")
	}
}

func TestParseAliasTableRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr error
	}{
		{
			name:    "malformed JSON",
			json:    `{"global": {"centro": "Centro"`,
			wantErr: nil, // erro de sintaxe do decoder, sem sentinela
		},
		{
			name:    "duplicate literal key",
			json:    `{"global": {"centro": "Centro", "centro": "Centro Histórico"}}`,
			wantErr: ErrDuplicateAlias,
		},
		{
			name:    "duplicate city section",
			json:    `{"cities": {"brasilia": {"asa norte": "Asa Norte"}, "brasilia": {"asa sul": "Asa Sul"}}}`,
			wantErr: ErrDuplicateAlias,
		},
		{
			name:    "canonical value is not a string",
			json:    `{"global": {"centro": 42}}`,
			wantErr: ErrMalformedAliasTable,
		},
		{
			name:    "alias block is not an object",
			json:    `{"global": ["centro"]}`,
			wantErr: ErrMalformedAliasTable,
		},
		{
			name:    "unknown top-level field",
			json:    `{"globals": {"centro": "Centro"}}`,
			wantErr: nil, // DisallowUnknownFields, sem sentinela
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseAliasTable([]byte(tt.json))
			if err == nil {
				t.Fatalf("parseAliasTable() error = nil, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("parseAliasTable() error = %v, want errors.Is(err, %v)", err, tt.wantErr)
			}
		})
	}
}

func TestNewNeighborhoodNormalizerFromAliasesRejectsBadTables(t *testing.T) {
	tests := []struct {
		name    string
		table   AliasTable
		wantErr error
	}{
		{
			name: "keys colliding after normalization",
			table: AliasTable{Global: AliasMap{
				"Asa Norte":  "Asa Norte",
				"asa  norte": "Asa Nte",
			}},
			wantErr: ErrDuplicateAlias,
		},
		{
			name: "city keys colliding after normalization",
			table: AliasTable{Cities: CityAliasMap{
				"Brasília": AliasMap{"asa norte": "Asa Norte"},
				"BRASILIA": AliasMap{"asa sul": "Asa Sul"},
			}},
			wantErr: ErrDuplicateAlias,
		},
		{
			name:    "empty alias key",
			table:   AliasTable{Global: AliasMap{"  ": "Centro"}},
			wantErr: ErrEmptyAliasKey,
		},
		{
			name:    "punctuation-only alias key",
			table:   AliasTable{Global: AliasMap{" -., ": "Centro"}},
			wantErr: ErrEmptyAliasKey,
		},
		{
			name:    "empty city name",
			table:   AliasTable{Cities: CityAliasMap{"  ": AliasMap{"centro": "Centro"}}},
			wantErr: ErrEmptyAliasKey,
		},
		{
			name:    "empty canonical value",
			table:   AliasTable{Global: AliasMap{"centro": "   "}},
			wantErr: ErrEmptyCanonicalValue,
		},
		{
			name:    "empty canonical value inside a city",
			table:   AliasTable{Cities: CityAliasMap{"brasilia": AliasMap{"asa norte": ""}}},
			wantErr: ErrEmptyCanonicalValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewNeighborhoodNormalizerFromAliases(tt.table)
			if err == nil {
				t.Fatalf("NewNeighborhoodNormalizerFromAliases() error = nil, want an error")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewNeighborhoodNormalizerFromAliases() error = %v, want errors.Is(err, %v)", err, tt.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "enrichment: ") {
				t.Errorf("error message = %q, want it to start with %q", err.Error(), "enrichment: ")
			}
		})
	}
}

func TestNewNeighborhoodNormalizerFromAliasesAcceptsEmptyTable(t *testing.T) {
	n, err := NewNeighborhoodNormalizerFromAliases(AliasTable{})
	if err != nil {
		t.Fatalf("NewNeighborhoodNormalizerFromAliases() error = %v, want nil", err)
	}

	// Sem tabela, tudo cai no fallback — o normalizador continua útil.
	if got := n.Normalize("ASA  NORTE", "Brasília"); got != "Asa Norte" {
		t.Errorf("Normalize() = %q, want %q", got, "Asa Norte")
	}
}

func TestNormalizeKey(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "ASA NORTE", want: "asa norte"},
		{in: "asa  norte", want: "asa norte"},
		{in: "Asa Norte", want: "asa norte"},
		{in: "Jardim Botânico", want: "jardim botanico"},
		{in: "AÇÃO ÕÁÀÂÉÊÍÓÔÚ", want: "acao oaaaeeioou"},
		{in: " ,Centro. ", want: "centro"},
		{in: "Jd. Botânico", want: "jd. botanico"},
		{in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := normalizeKey(tt.in); got != tt.want {
				t.Errorf("normalizeKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
