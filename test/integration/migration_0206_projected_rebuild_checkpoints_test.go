//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// TestMigration0206_ClearsOnlyEmptiedSourcesRebuildCheckpoints applies 0206
// over checkpoints for the sources 0137/0164 emptied and 0203 extended, a neighbour whose
// name shares a prefix, another projected source and the live projector's own
// cursors. Only the projected-rebuild rows for comet, cctp, rozo and
// sushiswap_v3 may go.
func TestMigration0206_ClearsOnlyEmptiedSourcesRebuildCheckpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 205)

	seed := [][2]string{
		{"projected-rebuild", "comet:51499000-51548999"},
		{"projected-rebuild", "cctp:62146641-62196640"},
		{"projected-rebuild", "rozo:60829397-60879396"},
		{"projected-rebuild", "rozo:60879397-60929396"},
		{"projected-rebuild", "sushiswap_v3:61487379-61537378"},
		{"projected-rebuild", "comet_v2:51499000-51548999"},
		{"projected-rebuild", "blend_backstop:51499546-51549545"},
		{"projector", "comet"},
		{"projector", "cctp"},
		{"projector", "rozo"},
	}
	for _, r := range seed {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO ingestion_cursors (source, sub_source, last_ledger) VALUES ($1, $2, 62200000)`,
			r[0], r[1]); err != nil {
			t.Fatalf("seed %s/%s: %v", r[0], r[1], err)
		}
	}

	applyMigrationsUpTo(t, dsn, 206)

	rows, err := db.QueryContext(ctx,
		`SELECT source || '/' || sub_source FROM ingestion_cursors ORDER BY 1`)
	if err != nil {
		t.Fatalf("read cursors: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []string{
		"projected-rebuild/blend_backstop:51499546-51549545",
		"projected-rebuild/comet_v2:51499000-51548999",
		"projector/cctp",
		"projector/comet",
		"projector/rozo",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("cursors after 0206 = %q, want %q", got, want)
	}
}
