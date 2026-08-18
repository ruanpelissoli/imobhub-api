// Package db concentra o acesso ao PostgreSQL. Expõe a criação do pool de
// conexões; a partir dele os demais pacotes executam queries.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pingTimeout limita a validação inicial da conexão para que uma URL apontando
// para um host inalcançável falhe rápido, em vez de travar o startup.
const pingTimeout = 5 * time.Second

// Connect cria um pool de conexões pgx a partir da connection string e valida a
// conectividade com um Ping. Em caso de erro o pool é fechado antes do retorno,
// então o chamador só precisa chamar Close quando err == nil.
//
// O ctx recebido governa o ciclo de vida do pool: ao cancelá-lo as conexões
// ociosas são encerradas. Use context.Background() para um pool com o tempo de
// vida do processo.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// A URL pode conter a senha; nunca a inclua na mensagem de erro.
		return nil, fmt.Errorf("db: invalid DATABASE_URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: could not create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: could not reach database (host %q, database %q): %w",
			cfg.ConnConfig.Host, cfg.ConnConfig.Database, err)
	}

	return pool, nil
}
