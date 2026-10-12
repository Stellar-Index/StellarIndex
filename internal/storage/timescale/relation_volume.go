// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
)

// relationDataVolumePathSelect resolves a relation's tablespace location, else data_directory; free space there is only meaningful on the DB
// host. $1 is cast to regclass so the relation name is a bind parameter, never a SQL fragment.
const relationDataVolumePathSelect = `
	SELECT COALESCE(NULLIF(pg_tablespace_location(t.oid), ''), current_setting('data_directory'))
	  FROM pg_class c
	  LEFT JOIN pg_tablespace t ON t.oid = c.reltablespace
	 WHERE c.oid = $1::regclass
`

// RelationDataVolumePath returns the directory whose filesystem holds `relation`.
func (s *Store) RelationDataVolumePath(ctx context.Context, relation string) (string, error) {
	var path string
	if err := s.db.QueryRowContext(ctx, relationDataVolumePathSelect, relation).Scan(&path); err != nil {
		return "", fmt.Errorf("timescale: resolve data volume for %s: %w", relation, err)
	}
	return path, nil
}
