//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseMovementsByAsset pins the movements_by_asset table: the MV copies live
// account_movements rows, both participants of one movement survive FINAL,
// an Int128 amount above 2^64 stays exact, and the operator catch-up
// statement (partition-bounded INSERT..SELECT) is idempotent over MV rows and skips superseded source rows (FINAL).
func TestClickHouseMovementsByAsset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		asset  = "TESTA-GISSUERMOVEMENTSBYASSET"
		other  = "TESTB-GISSUERMOVEMENTSBYASSET"
		ledger = uint32(78_000_001)
		tx     = "mba-tx-1"
	)
	big128 := new(big.Int).Lsh(big.NewInt(1), 100)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.account_movements
		(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
		 movement_kind, provenance, asset, counterparty, amount)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	rows := [][]any{
		{"GSENDER", ledger, at, tx, uint32(0), uint32(0), "sent", "payment", "classic", asset, "GRECV", big128},
		{"GRECV", ledger, at, tx, uint32(0), uint32(0), "received", "payment", "classic", asset, "GSENDER", big128},
		{"GSENDER", ledger, at, "mba-tx-2", uint32(0), uint32(0), "sent", "payment", "classic", other, "GRECV", big.NewInt(5)},
	}
	for _, r := range rows {
		if err := b.Append(r...); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}

	count := func(t *testing.T) (n uint64, sum string) {
		t.Helper()
		if err := raw.QueryRow(ctx, `SELECT count(), toString(sum(amount)) FROM stellar.movements_by_asset FINAL
			WHERE asset = ? AND ledger = ?`, asset, ledger).Scan(&n, &sum); err != nil {
			t.Fatalf("query: %v", err)
		}
		return n, sum
	}
	want := new(big.Int).Mul(big128, big.NewInt(2)).String()
	if n, sum := count(t); n != 2 || sum != want {
		t.Fatalf("after MV: n=%d sum=%s, want 2 rows summing %s", n, sum, want)
	}

	const catchUp = `INSERT INTO stellar.movements_by_asset
		SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
		       movement_kind, provenance, asset, counterparty, amount, attributes, ingested_at
		FROM stellar.account_movements FINAL
		WHERE ledger >= 78000000 AND ledger < 79000000`
	if err := raw.Exec(ctx, catchUp); err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	if n, sum := count(t); n != 2 || sum != want {
		t.Fatalf("after catch-up: n=%d sum=%s, want 2 rows summing %s", n, sum, want)
	}

	// A superseded source row (same key, older ingested_at, different asset)
	// is still unmerged in account_movements; FINAL must keep it out.
	stale := "TESTSTALE-GISSUERMOVEMENTSBYASSET"
	if err := raw.Exec(ctx, `SYSTEM STOP MERGES stellar.account_movements`); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), `SYSTEM START MERGES stellar.account_movements`) })
	// Two inserts = two parts, so the pair stays unmerged (merges are stopped).
	for _, r := range []struct{ asset, ts string }{{stale, "2026-09-01 00:00:00"}, {asset, "2026-09-02 00:00:00"}} {
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_movements
			(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
			 movement_kind, provenance, asset, counterparty, amount, ingested_at)
			VALUES ('GSTALE', 78000002, ?, 'mba-tx-stale', 0, 0, 'sent', 'payment', 'classic', ?, '', 1, ?)`,
			at, r.asset, r.ts); err != nil {
			t.Fatalf("insert stale pair: %v", err)
		}
	}
	if err := raw.Exec(ctx, `DELETE FROM stellar.movements_by_asset WHERE ledger = 78000002 SETTINGS mutations_sync = 2`); err != nil {
		t.Fatalf("clear MV rows: %v", err)
	}
	if err := raw.Exec(ctx, catchUp); err != nil {
		t.Fatalf("catch-up 2: %v", err)
	}
	var n uint64
	if err := raw.QueryRow(ctx, `SELECT count() FROM stellar.movements_by_asset WHERE ledger = 78000002 AND asset = ?`, stale).Scan(&n); err != nil {
		t.Fatalf("stale query: %v", err)
	}
	if n != 0 {
		t.Fatalf("stale superseded row copied by catch-up: %d rows under asset %s", n, stale)
	}
}

// TestClickHouseAssetMovementsReader pins the A1 reader and its route: one
// entry per movement rebuilt from either participant row, newest re-derive
// wins, keyset paging, the ledger ceiling, and the backfill marker.
func TestClickHouseAssetMovementsReader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dialClickHouse(t, ctx, "stellar")
	const (
		issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		asset  = "MBAREAD-" + issuer
		l      = uint32(50_100_001) // below P23: admitted by the ceiling without a cap67 watermark
	)
	big128 := new(big.Int).Lsh(big.NewInt(1), 100)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	insert := func(address string, ledger uint32, tx, dir, cp string, amt *big.Int, ingested string) {
		t.Helper()
		if err := raw.Exec(ctx, `INSERT INTO stellar.account_movements
			(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
			 movement_kind, provenance, asset, counterparty, amount, ingested_at)
			VALUES (?, ?, ?, ?, 0, 0, ?, 'payment', 'classic_derived', ?, ?, ?, ?)`,
			address, ledger, at, tx, dir, asset, cp, amt, ingested); err != nil {
			t.Fatalf("insert %s/%s: %v", tx, address, err)
		}
	}
	// Movement A: a stale first derive under the wrong sender, then the re-derive.
	insert("GSTALE", l, "mba-a", "sent", "GB", big128, "2026-09-01 00:00:00")
	insert("GA", l, "mba-a", "sent", "GB", big128, "2026-09-02 00:00:00")
	insert("GB", l, "mba-a", "received", "GA", big128, "2026-09-02 00:00:00")
	insert("GSELF", l+1, "mba-b", "self", "", big.NewInt(7), "2026-09-02 00:00:00")
	insert("GCLAIM", l+2, "mba-c", "received", "", big.NewInt(9), "2026-09-02 00:00:00")
	insert("GTOP", l+3, "mba-d", "sent", "GB", big.NewInt(1), "2026-09-02 00:00:00")

	er, err := chstore.NewExplorerReader(ctx, clickhouseAddr(t))
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	page1, err := er.AssetMovements(ctx, asset, 2, chstore.AccountMovementCursor{}, l+2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 2 || page1[0].TxHash != "mba-c" || page1[1].TxHash != "mba-b" {
		t.Fatalf("page 1 = %+v, want mba-c, mba-b (mba-d is above the ceiling)", page1)
	}
	if page1[0].From != "" || page1[0].To != "GCLAIM" || page1[1].From != "GSELF" || page1[1].To != "GSELF" {
		t.Fatalf("page 1 sides = %+v", page1)
	}
	last := page1[1]
	page2, err := er.AssetMovements(ctx, asset, 2, chstore.AccountMovementCursor{
		Ledger: last.Ledger, TxHash: last.TxHash, OpIndex: last.OpIndex, LegIndex: last.LegIndex,
	}, l+2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 1 || page2[0].From != "GA" || page2[0].To != "GB" || page2[0].Amount.Cmp(big128) != 0 {
		t.Fatalf("page 2 = %+v, want one GA->GB movement of 2^100", page2)
	}

	if thru, err := er.AssetMovementsBackfilledThru(ctx); err != nil || thru != 0 {
		t.Fatalf("marker before write = %d, %v; want 0", thru, err)
	}
	if err := raw.Exec(ctx, `INSERT INTO stellar.cap67_movements_watermark (name, thru_ledger) VALUES (?, ?)`,
		chstore.MovementsByAssetBackfillMarker, uint32(64_000_000)); err != nil {
		t.Fatalf("marker insert: %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Exec(context.Background(), `DELETE FROM stellar.cap67_movements_watermark WHERE name = ? SETTINGS mutations_sync = 2`,
			chstore.MovementsByAssetBackfillMarker)
	})
	if thru, err := er.AssetMovementsBackfilledThru(ctx); err != nil || thru != 64_000_000 {
		t.Fatalf("marker after write = %d, %v; want 64000000", thru, err)
	}

	ts := httptest.NewServer(v1.New(v1.Options{Explorer: er}).Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/assets/MBAREAD:" + issuer + "/movements?limit=5")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Data struct {
			Asset      string `json:"asset"`
			LowerBound bool   `json:"lower_bound"`
			Movements  []struct {
				TxHash string `json:"tx_hash"`
				Amount string `json:"amount"`
			} `json:"movements"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d decode %v", resp.StatusCode, err)
	}
	if got.Data.Asset != asset || got.Data.LowerBound || len(got.Data.Movements) != 4 ||
		got.Data.Movements[3].Amount != big128.String() {
		t.Fatalf("HTTP view = %+v, want 4 movements of %s ending in the exact 2^100 amount, not a lower bound", got.Data, asset)
	}
}
