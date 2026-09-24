package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SchemaMigrationVersion reads golang-migrate's single schema_migrations
// row: the highest applied migration and whether it was left dirty. No
// row means no migrations are applied (version 0). It satisfies
// v1.SchemaVersionReader, so the indexer and aggregator gate readiness on
// the same schema-head check the API's /v1/readyz uses.
func (s *Store) SchemaMigrationVersion(ctx context.Context) (uint, bool, error) {
	var version uint
	var dirty bool
	err := s.db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("timescale: SchemaMigrationVersion: %w", err)
	}
	return version, dirty, nil
}
