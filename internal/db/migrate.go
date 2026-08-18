package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsLockID identifica o advisory lock que serializa a aplicação das
// migrations. O valor é arbitrário, mas precisa ser estável entre releases: duas
// instâncias subindo ao mesmo tempo só se excluem se usarem o mesmo id.
const migrationsLockID int64 = 728_311_004

// migrationFilePattern impõe o formato NNN_descricao.sql. O prefixo numérico é a
// versão registrada em schema_migrations; a descrição é livre e pode ser
// renomeada sem que a migration seja reaplicada.
var migrationFilePattern = regexp.MustCompile(`^(\d+)_([A-Za-z0-9_.\-]+)\.sql$`)

// ErrNoMigrations indica que o diretório informado existe mas não contém nenhum
// arquivo .sql — quase sempre sintoma de caminho errado (working directory do
// container, por exemplo), e não de um projeto legitimamente sem schema.
var ErrNoMigrations = errors.New("db: no .sql migration files found")

// createMigrationsTableSQL registra o que já foi aplicado. A tabela é criada
// dentro da mesma transação das migrations, então um banco vazio e um banco já
// migrado seguem o mesmo caminho de código.
const createMigrationsTableSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    BIGINT PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

// migration é um arquivo de migration já validado.
type migration struct {
	version int64  // prefixo numérico do arquivo
	name    string // nome do arquivo, ex.: "001_create_site_selectors.sql"
	path    string
}

// RunMigrations aplica, em ordem crescente de versão, os arquivos .sql de
// migrationsDir que ainda não constam em schema_migrations. É idempotente:
// chamá-la com o schema em dia não executa nenhum statement.
//
// Todas as migrations pendentes rodam numa única transação protegida por um
// advisory lock, de modo que (a) várias instâncias subindo simultaneamente não
// aplicam o mesmo arquivo em paralelo e (b) uma falha no meio do caminho deixa o
// banco no estado anterior, em vez de meio migrado.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, migrationsDir string) error {
	migrations, err := collectMigrations(migrationsDir)
	if err != nil {
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: could not begin migration transaction: %w", err)
	}
	// Rollback após um Commit bem-sucedido é no-op; este defer só age nos
	// caminhos de erro. WithoutCancel garante que o rollback seja enviado mesmo
	// quando o motivo da saída foi o cancelamento do ctx (SIGTERM).
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Lock de transação: liberado automaticamente no commit/rollback, sem risco
	// de vazar o lock numa conexão devolvida ao pool.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationsLockID); err != nil {
		return fmt.Errorf("db: could not acquire migration lock: %w", err)
	}

	if _, err := tx.Exec(ctx, createMigrationsTableSQL); err != nil {
		return fmt.Errorf("db: could not create schema_migrations table: %w", err)
	}

	applied, err := appliedVersions(ctx, tx)
	if err != nil {
		return err
	}

	pending := 0
	for _, m := range migrations {
		if _, ok := applied[m.version]; ok {
			continue
		}

		statements, err := os.ReadFile(m.path)
		if err != nil {
			return fmt.Errorf("db: could not read migration %s: %w", m.name, err)
		}

		// Exec sem argumentos usa o protocolo simples do PostgreSQL, que aceita
		// vários statements num mesmo comando — é o que permite um arquivo com
		// CREATE TABLE + CREATE INDEX.
		if _, err := tx.Exec(ctx, string(statements)); err != nil {
			return fmt.Errorf("db: migration %s failed: %w", m.name, err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
			m.version, m.name,
		); err != nil {
			return fmt.Errorf("db: could not record migration %s: %w", m.name, err)
		}

		slog.Info("migration applied", "version", m.version, "name", m.name)
		pending++
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: could not commit migrations: %w", err)
	}

	slog.Info("database schema up to date", "applied", pending, "total", len(migrations))
	return nil
}

// appliedVersions lê as versões já registradas. Roda dentro da transação de
// migração para enxergar a tabela recém-criada.
func appliedVersions(ctx context.Context, tx pgx.Tx) (map[int64]struct{}, error) {
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("db: could not read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]struct{})
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("db: could not scan applied migration version: %w", err)
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: could not read applied migrations: %w", err)
	}

	return applied, nil
}

// collectMigrations lista os arquivos de migrationsDir, valida o formato do nome
// e devolve-os ordenados por versão. Nomes fora do padrão NNN_descricao.sql são
// erro, não itens ignorados em silêncio: um arquivo com nome errado nunca
// rodaria e o schema divergiria sem nenhum aviso.
func collectMigrations(migrationsDir string) ([]migration, error) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("db: could not read migrations directory %q: %w", migrationsDir, err)
	}

	var migrations []migration
	seen := make(map[int64]string)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".sql" {
			continue
		}

		match := migrationFilePattern.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("db: migration file %q does not match the required NNN_description.sql format", name)
		}

		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("db: migration file %q has an invalid version prefix: %w", name, err)
		}

		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("db: migrations %q and %q share the version prefix %d", other, name, version)
		}
		seen[version] = name

		migrations = append(migrations, migration{
			version: version,
			name:    name,
			path:    filepath.Join(migrationsDir, name),
		})
	}

	if len(migrations) == 0 {
		return nil, fmt.Errorf("%w in %q", ErrNoMigrations, migrationsDir)
	}

	// Ordem numérica, não lexicográfica: assim "10_x.sql" vem depois de
	// "9_x.sql" mesmo sem zero à esquerda.
	slices.SortFunc(migrations, func(a, b migration) int {
		switch {
		case a.version < b.version:
			return -1
		case a.version > b.version:
			return 1
		default:
			return 0
		}
	})

	return migrations, nil
}
