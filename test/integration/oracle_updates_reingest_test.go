//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestOracleUpdates_ReingestIsIdempotent pins why ts in oracle_updates'
// primary key is safe on re-ingest: ts is a pure function of the event
// payload and the ledger header close time, never the wall clock, so a
// live pass and a replay of the same mainnet event produce the same key
// and the second pass lands no new row and no new entry tally.
func TestOracleUpdates_ReingestIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	raw, err := os.ReadFile(filepath.Join("..", "fixtures", "reflector", "v6-2026-04-23", "62251160_9322ba2f5c95.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		ContractID     string   `json:"contract_id"`
		Ledger         uint32   `json:"ledger"`
		TxHash         string   `json:"tx_hash"`
		LedgerClosedAt string   `json:"ledger_closed_at"`
		Topics         []string `json:"topics"`
		Value          string   `json:"value"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	closedAt, err := time.Parse(time.RFC3339, fx.LedgerClosedAt)
	if err != nil {
		t.Fatal(err)
	}

	dec := reflector.NewDecoder(reflector.VariantDEX, fx.ContractID)
	decode := func(ledgerClosedAt string) []c.OracleUpdate {
		t.Helper()
		out, err := dec.Decode(events.Event{
			Type: "contract", ContractID: fx.ContractID, Ledger: fx.Ledger, TxHash: fx.TxHash,
			LedgerClosedAt: ledgerClosedAt, Topic: fx.Topics, Value: fx.Value,
		})
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		us := make([]c.OracleUpdate, 0, len(out))
		for _, ev := range out {
			us = append(us, ev.(reflector.UpdateEvent).Update)
		}
		return us
	}

	// Live ingest stamps the close time from the ledger header; a replay
	// reads it back from the ClickHouse lake in a non-UTC zone at worst.
	live := decode(fx.LedgerClosedAt)
	replay := decode(closedAt.In(time.FixedZone("UTC+3", 3*3600)).Format(time.RFC3339))
	if len(live) == 0 || len(live) != len(replay) {
		t.Fatalf("decoded %d live rows and %d replay rows, want the same non-zero count", len(live), len(replay))
	}
	for i := range live {
		if !live[i].Timestamp.Equal(replay[i].Timestamp) {
			t.Fatalf("row %d ts: live %s, replay %s; ts must not depend on when the event is ingested",
				i, live[i].Timestamp, replay[i].Timestamp)
		}
		// The topic's publication time, distinct from the close time, proves
		// the payload-derived arm ran rather than the close-time fallback.
		if live[i].Timestamp.Equal(closedAt) {
			t.Fatalf("row %d ts = ledger close %s, want the oracle's publication time from topic[2]", i, closedAt)
		}
	}

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	counts := func() (rows, tally int64) {
		t.Helper()
		if err := store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM oracle_updates WHERE ledger = $1 AND tx_hash = $2`,
			fx.Ledger, fx.TxHash).Scan(&rows); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if err := store.DB().QueryRowContext(ctx,
			`SELECT COALESCE(sum(entry_count), 0) FROM source_entry_counts WHERE source = $1`,
			live[0].Source).Scan(&tally); err != nil {
			t.Fatalf("read tally: %v", err)
		}
		return rows, tally
	}

	for _, u := range live {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("live insert: %v", err)
		}
	}
	rows, tally := counts()
	if rows != int64(len(live)) || tally != int64(len(live)) {
		t.Fatalf("after live pass: rows=%d tally=%d, want %d each", rows, tally, len(live))
	}

	for _, u := range replay {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("replay insert: %v", err)
		}
	}
	if r2, t2 := counts(); r2 != rows || t2 != tally {
		t.Fatalf("after replay: rows=%d tally=%d, want unchanged %d/%d", r2, t2, rows, tally)
	}
}
