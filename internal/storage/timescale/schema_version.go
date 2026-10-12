package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SchemaMigrationVersion reads golang-migrate's schema_migrations row (highest applied version, dirty flag; no row means 0), satisfying v1.SchemaVersionReader so services gate readiness on the same check.
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
