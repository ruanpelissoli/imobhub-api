package db

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeMigrations cria um diretório temporário com os arquivos informados
// (conteúdo irrelevante: collectMigrations só olha os nomes).
func writeMigrations(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatalf("could not write %s: %v", name, err)
		}
	}
	return dir
}

func TestCollectMigrationsSortsByVersion(t *testing.T) {
	// "9" antes de "10" só sai certo com ordenação numérica; lexicográfica
	// devolveria 001, 10, 9.
	dir := writeMigrations(t, "10_third.sql", "001_first.sql", "9_second.sql")

	migrations, err := collectMigrations(dir)
	if err != nil {
		t.Fatalf("collectMigrations() error = %v, want nil", err)
	}

	want := []int64{1, 9, 10}
	if len(migrations) != len(want) {
		t.Fatalf("got %d migrations, want %d", len(migrations), len(want))
	}
	for i, version := range want {
		if migrations[i].version != version {
			t.Errorf("migrations[%d].version = %d, want %d", i, migrations[i].version, version)
		}
	}

	if migrations[0].name != "001_first.sql" {
		t.Errorf("migrations[0].name = %q, want %q", migrations[0].name, "001_first.sql")
	}
	if want := filepath.Join(dir, "001_first.sql"); migrations[0].path != want {
		t.Errorf("migrations[0].path = %q, want %q", migrations[0].path, want)
	}
}

func TestCollectMigrationsIgnoresNonSQLFiles(t *testing.T) {
	dir := writeMigrations(t, "001_first.sql", "README.md", "notes.txt")
	if err := os.Mkdir(filepath.Join(dir, "archive"), 0o750); err != nil {
		t.Fatalf("could not create subdirectory: %v", err)
	}

	migrations, err := collectMigrations(dir)
	if err != nil {
		t.Fatalf("collectMigrations() error = %v, want nil", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("got %d migrations, want 1", len(migrations))
	}
}

func TestCollectMigrationsRejectsMalformedName(t *testing.T) {
	// Ignorar em silêncio deixaria o schema divergir sem nenhum aviso.
	dir := writeMigrations(t, "001_first.sql", "create_listings.sql")

	if _, err := collectMigrations(dir); err == nil {
		t.Fatal("collectMigrations() error = nil, want error for malformed file name")
	}
}

func TestCollectMigrationsRejectsDuplicateVersion(t *testing.T) {
	dir := writeMigrations(t, "001_first.sql", "1_other.sql")

	if _, err := collectMigrations(dir); err == nil {
		t.Fatal("collectMigrations() error = nil, want error for duplicate version")
	}
}

func TestCollectMigrationsRejectsEmptyDirectory(t *testing.T) {
	if _, err := collectMigrations(t.TempDir()); !errors.Is(err, ErrNoMigrations) {
		t.Fatalf("collectMigrations() error = %v, want ErrNoMigrations", err)
	}
}

func TestCollectMigrationsReportsMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := collectMigrations(missing)
	if err == nil {
		t.Fatal("collectMigrations() error = nil, want error for missing directory")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("collectMigrations() error = %v, want it to wrap os.ErrNotExist", err)
	}
}

// TestRepositoryMigrationsAreValid roda contra os arquivos reais do projeto:
// garante que uma migration nova nomeada fora do padrão quebre o build do
// pacote, e não só o startup em produção.
func TestRepositoryMigrationsAreValid(t *testing.T) {
	migrations, err := collectMigrations(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("collectMigrations() on repository migrations error = %v, want nil", err)
	}

	want := []string{"001_create_site_selectors.sql", "002_create_listings.sql"}
	for i, name := range want {
		if i >= len(migrations) {
			t.Fatalf("migration %q is missing", name)
		}
		if migrations[i].name != name {
			t.Errorf("migrations[%d].name = %q, want %q", i, migrations[i].name, name)
		}
	}
}
