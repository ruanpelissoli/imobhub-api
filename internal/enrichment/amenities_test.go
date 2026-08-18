package enrichment

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoAmenitiesFile é o vocabulário versionado, visto a partir do diretório do
// pacote (o `go test` roda com o working directory no pacote). O caminho relativo
// é o mesmo espírito de TestRepoSourcesFileIsValid em internal/sources: garante
// que o arquivo que vai para produção continua carregável.
const repoAmenitiesFile = "../../configs/amenities.yaml"

// mentionsPath checa se o erro cita o caminho do arquivo. A comparação é contra
// a forma citada com %q porque é assim que o pacote formata o caminho — e no
// Windows %q escapa as barras invertidas, então comparar com a string crua daria
// falso negativo.
func mentionsPath(err error, path string) bool {
	return strings.Contains(err.Error(), fmt.Sprintf("%q", path))
}

// writeAmenities grava um vocabulário temporário e devolve o caminho.
func writeAmenities(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "amenities.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	return path
}

func TestNewTermExtractorLoadsFile(t *testing.T) {
	path := writeAmenities(t, `
amenities:
  - canonical: piscina
    synonyms: [piscina, piscinas]
  - canonical: ar-condicionado
    synonyms: ["ar condicionado", split]
`)

	extractor, err := NewTermExtractor(path)
	if err != nil {
		t.Fatalf("NewTermExtractor() error = %v, want nil", err)
	}

	got := extractor.Extract("Sala com SPLIT e piscinas aquecidas").Amenities
	want := []string{"piscina", "ar-condicionado"}
	if len(got) != len(want) {
		t.Fatalf("Amenities = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Amenities = %#v, want %#v", got, want)
		}
	}
}

// TestNewTermExtractorSkipsUnusableSynonym: sinônimo individual inutilizável
// avisa e é ignorado; a comodidade continua valendo pelos demais sinônimos.
func TestNewTermExtractorSkipsUnusableSynonym(t *testing.T) {
	path := writeAmenities(t, `
amenities:
  - canonical: sauna
    synonyms: ["", "   ", "-", sauna]
`)

	extractor, err := NewTermExtractor(path)
	if err != nil {
		t.Fatalf("NewTermExtractor() error = %v, want nil", err)
	}

	if got := extractor.Extract("Prédio com sauna").Amenities; len(got) != 1 || got[0] != "sauna" {
		t.Fatalf("Amenities = %#v, want [sauna]", got)
	}
}

func TestNewTermExtractorMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nao-existe.yaml")

	_, err := NewTermExtractor(path)
	if err == nil {
		t.Fatal("NewTermExtractor() error = nil, want error")
	}
	// O caminho na mensagem é o que permite ao operador corrigir a variável de
	// ambiente sem ler o código.
	if !mentionsPath(err, path) {
		t.Errorf("error %q does not mention path %q", err, path)
	}
	// O erro do os.ReadFile precisa continuar detectável pelo chamador.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("errors.Is(err, fs.ErrNotExist) = false, want true (err = %v)", err)
	}
}

func TestNewTermExtractorRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "yaml malformado",
			content: "amenities:\n  - canonical: piscina\n   synonyms: [piscina]\n\t- isto: nao é yaml\n",
		},
		{
			name:    "arquivo vazio",
			content: "",
		},
		{
			name:    "lista vazia",
			content: "amenities: []\n",
		},
		{
			name:    "chave de topo ausente",
			content: "outra_coisa:\n  - canonical: piscina\n",
		},
		{
			name:    "canonical vazio",
			content: "amenities:\n  - canonical: \"   \"\n    synonyms: [piscina]\n",
		},
		{
			name:    "sem sinonimos",
			content: "amenities:\n  - canonical: piscina\n    synonyms: []\n",
		},
		{
			name:    "sinonimos todos inutilizaveis",
			content: "amenities:\n  - canonical: piscina\n    synonyms: [\"\", \"-\", \"...\"]\n",
		},
		{
			name:    "canonical duplicado",
			content: "amenities:\n  - canonical: Ar-Condicionado\n    synonyms: [split]\n  - canonical: ar condicionado\n    synonyms: [climatizado]\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeAmenities(t, tt.content)

			_, err := NewTermExtractor(path)
			if err == nil {
				t.Fatal("NewTermExtractor() error = nil, want error")
			}
			if !mentionsPath(err, path) {
				t.Errorf("error %q does not mention path %q", err, path)
			}
			if !strings.HasPrefix(err.Error(), "enrichment:") {
				t.Errorf("error %q is not prefixed with the package name", err)
			}
		})
	}
}

// TestRepoAmenitiesFileLoads garante que o vocabulário versionado continua
// carregável e com o mínimo de comodidades acordado. Sem ele, um erro de
// indentação no YAML só apareceria no startup do pipeline de enriquecimento.
func TestRepoAmenitiesFileLoads(t *testing.T) {
	extractor, err := NewTermExtractor(repoAmenitiesFile)
	if err != nil {
		t.Fatalf("NewTermExtractor(%q) error = %v, want nil", repoAmenitiesFile, err)
	}

	const minAmenities = 12
	if got := len(extractor.amenities); got < minAmenities {
		t.Errorf("vocabulário tem %d comodidades, want >= %d", got, minAmenities)
	}

	for _, amenity := range extractor.amenities {
		if len(amenity.terms) == 0 {
			t.Errorf("comodidade %q ficou sem termos", amenity.canonical)
		}
	}
}
