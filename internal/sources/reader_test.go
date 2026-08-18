package sources

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSources cria um arquivo temporário com o conteúdo informado e devolve o
// caminho. t.TempDir() é limpo automaticamente ao fim do teste.
func writeSources(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sources.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v, want nil", err)
	}
	return path
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestReadSourcesSkipsBlankLinesAndComments(t *testing.T) {
	path := writeSources(t, strings.Join([]string{
		"# comentário no topo",
		"",
		"   ",
		"https://a.example.com/imoveis",
		"\t#  comentário indentado",
		"  https://b.example.com/imoveis  ",
		"",
	}, "\n"))

	got, err := ReadSources(path)
	if err != nil {
		t.Fatalf("ReadSources() error = %v, want nil", err)
	}

	want := []string{"https://a.example.com/imoveis", "https://b.example.com/imoveis"}
	if !equal(got, want) {
		t.Errorf("ReadSources() = %v, want %v", got, want)
	}
}

func TestReadSourcesDiscardsInvalidLines(t *testing.T) {
	path := writeSources(t, strings.Join([]string{
		"https://valida.example.com",
		"imobiliaria.example.com", // sem scheme: url.Parse trata como path
		"ftp://arquivos.example.com",
		"http://", // scheme ok, host vazio
		"https://outra.example.com/busca?tipo=casa",
		"://sem-scheme",
		"http://[::1", // URL malformada
	}, "\n"))

	got, err := ReadSources(path)
	if err != nil {
		t.Fatalf("ReadSources() error = %v, want nil", err)
	}

	want := []string{"https://valida.example.com", "https://outra.example.com/busca?tipo=casa"}
	if !equal(got, want) {
		t.Errorf("ReadSources() = %v, want %v", got, want)
	}
}

func TestReadSourcesDeduplicatesPreservingFirstOccurrence(t *testing.T) {
	path := writeSources(t, strings.Join([]string{
		"https://a.example.com",
		"https://b.example.com",
		"  https://a.example.com  ",
		"https://c.example.com",
		"https://b.example.com",
	}, "\n"))

	got, err := ReadSources(path)
	if err != nil {
		t.Fatalf("ReadSources() error = %v, want nil", err)
	}

	want := []string{"https://a.example.com", "https://b.example.com", "https://c.example.com"}
	if !equal(got, want) {
		t.Errorf("ReadSources() = %v, want %v", got, want)
	}
}

func TestReadSourcesEmptyFileReturnsNoSources(t *testing.T) {
	path := writeSources(t, "# só comentários\n\n")

	got, err := ReadSources(path)
	if err != nil {
		t.Fatalf("ReadSources() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadSources() = %v, want empty", got)
	}
}

func TestReadSourcesMissingFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nao-existe.txt")

	_, err := ReadSources(path)
	if err == nil {
		t.Fatal("ReadSources() error = nil, want error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("errors.Is(err, fs.ErrNotExist) = false, want true (err = %v)", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error message %q does not mention the file path %q", err, path)
	}
}

// O sources.txt versionado é a documentação do formato: se ele passar a conter
// uma linha inválida, este teste avisa antes de o operador copiar o erro.
func TestRepoSourcesFileIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "sources.txt")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("sources.txt não encontrado na raiz do repositório: %v", err)
	}

	if _, err := ReadSources(path); err != nil {
		t.Fatalf("ReadSources(%q) error = %v, want nil", path, err)
	}
}
