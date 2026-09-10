// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
)

// relationDataVolumePathSelect resolves the directory a relation's storage
// lives under: its tablespace location when it has one, the server's
// data_directory otherwise. A caller measures free space on the filesystem
// holding that path — which only means anything when the caller runs ON the
// database host, so every caller is expected to offer an operator override
// for the case where it does not.
//
// $1 is cast to regclass explicitly rather than interpolated, so the
// relation name is a bind parameter and never a SQL fragment.
const relationDataVolumePathSelect = `
	SELECT COALESCE(NULLIF(pg_tablespace_location(t.oid), ''), current_setting('data_directory'))
	  FROM pg_class c
	  LEFT JOIN pg_tablespace t ON t.oid = c.reltablespace
	 WHERE c.oid = $1::regclass
`

// RelationDataVolumePath returns the directory whose filesystem holds
// `relation`. Reading data_directory needs pg_read_all_settings (or
// superuser); the error is returned as-is so the caller can fall back to an
// operator-supplied figure rather than guessing.
func (s *Store) RelationDataVolumePath(ctx context.Context, relation string) (string, error) {
	var path string
	if err := s.db.QueryRowContext(ctx, relationDataVolumePathSelect, relation).Scan(&path); err != nil {
		return "", fmt.Errorf("timescale: resolve data volume for %s: %w", relation, err)
	}
	return path, nil
}
