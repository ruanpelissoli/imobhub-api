package grouping

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/imobhub/api/internal/db"
)

// ErrMissingPool indica que NewPoolStore foi chamado sem pool de conexões.
var ErrMissingPool = errors.New("grouping: database pool is required")

// poolStore adapta as funções livres de internal/db à interface PropertyStore.
// É o único ponto deste pacote que conhece pgxpool — todo o resto do fluxo é
// testável sem PostgreSQL por causa dele.
type poolStore struct {
	pool *pgxpool.Pool
}

// NewPoolStore devolve o PropertyStore de produção.
func NewPoolStore(pool *pgxpool.Pool) (PropertyStore, error) {
	if pool == nil {
		return nil, ErrMissingPool
	}
	return &poolStore{pool: pool}, nil
}

func (s *poolStore) FindPropertiesByCoordinates(ctx context.Context, lat, lng float64, radiusMeters int) ([]db.Property, error) {
	return db.FindPropertiesByCoordinates(ctx, s.pool, lat, lng, radiusMeters)
}

func (s *poolStore) CreateProperty(ctx context.Context, property db.Property) (db.Property, error) {
	return db.CreateProperty(ctx, s.pool, property)
}

func (s *poolStore) LinkListingToProperty(ctx context.Context, propertyID string, listingID int64) error {
	return db.LinkListingToProperty(ctx, s.pool, propertyID, listingID)
}
