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

// TestClickHouseAssetEntryChanges pins the asset entry-history read: a
// re-derived row collapses to its newest version before RMT merges, keyset
// pages never repeat or skip a row, nothing above the derive watermark is
// served, and an Int128 balance above 2^64 reaches the wire exact.
func TestClickHouseAssetEntryChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		asset  = "AECTEST-" + issuer
		base   = uint32(170_000_000)
		wm     = base + 10
	)
	t.Cleanup(func() {
		bg := context.Background()
		_ = raw.Exec(bg, `DELETE FROM stellar.asset_entry_changes WHERE asset = ? SETTINGS mutations_sync = 2`, asset)
		_ = raw.Exec(bg, `DELETE FROM stellar.entry_history_watermark WHERE name IN ('entry_history', ?) SETTINGS mutations_sync = 2`,
			chstore.EntryHistoryBackfillMarker)
	})
	if err := raw.Exec(ctx, `SYSTEM STOP MERGES stellar.asset_entry_changes`); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), `SYSTEM START MERGES stellar.asset_entry_changes`) })

	big128 := new(big.Int).Lsh(big.NewInt(1), 100)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	old, newer := at, at.Add(time.Hour)
	insert := func(rows ...[]any) {
		t.Helper()
		b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.asset_entry_changes
			(asset, ledger, close_time, tx_hash, op_index, change_index, role, intra_ledger_seq,
			 entry_type, change_type, changed, account, balance, fields, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		for _, r := range rows {
			if err := b.Append(r...); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		if err := b.Send(); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	row := func(ledger uint32, op int32, role, entryType string, bal *big.Int, fields string, ing time.Time) []any {
		return []any{
			asset, ledger, at, "aec-tx", op, uint32(0), role, uint32(0),
			entryType, "updated",
			[]string{"balance"},
			"GHOLDER", bal, fields, ing,
		}
	}
	insert(
		row(base+1, -1, "holder", "trustline", big128, `{"balance":"big"}`, old),
		row(base+2, 0, "claimable", "claimable_balance", big.NewInt(7), `{"v":"old"}`, old),
		row(base+3, 1, "selling", "offer", big.NewInt(9), `{}`, old),
		row(base+20, 0, "holder", "trustline", big.NewInt(1), `{}`, old),
	)
	// A second derive of the claimable row, in its own part so it is not yet merged.
	insert(row(base+2, 0, "claimable", "claimable_balance", big.NewInt(8), `{"v":"new"}`, newer))

	er, err := chstore.NewExplorerReader(ctx, clickhouseAddr(t))
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	var seen []uint32
	var cur chstore.AssetEntryChangeCursor
	for page := 0; page < 5; page++ {
		got, err := er.AssetEntryChanges(ctx, asset, 1, cur, wm)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(got) == 0 {
			break
		}
		r := got[0]
		if r.Ledger == base+2 && (r.Balance.Int64() != 8 || r.Fields != `{"v":"new"}`) {
			t.Fatalf("claimable row = %+v, want the newest derive", r)
		}
		if r.Ledger == base+1 && r.Balance.Cmp(big128) != 0 {
			t.Fatalf("holder balance = %s, want 2^100", r.Balance)
		}
		seen = append(seen, r.Ledger)
		cur = chstore.AssetEntryChangeCursor{Ledger: r.Ledger, TxHash: r.TxHash, OpIndex: r.OpIndex, ChangeIndex: r.ChangeIndex, Role: r.Role}
	}
	if len(seen) != 3 || seen[0] != base+3 || seen[1] != base+2 || seen[2] != base+1 {
		t.Fatalf("paged ledgers = %v, want [%d %d %d] (deduped, ceilinged at %d)", seen, base+3, base+2, base+1, wm)
	}

	if err := raw.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES ('entry_history', ?), (?, ?)`,
		wm, chstore.EntryHistoryBackfillMarker, wm); err != nil {
		t.Fatalf("watermark insert: %v", err)
	}
	if got, thru, err := er.EntryHistoryCoverage(ctx); err != nil || got < wm || thru < wm {
		t.Fatalf("coverage = %d, %d, %v; want both at least %d", got, thru, err, wm)
	}

	ts := httptest.NewServer(v1.New(v1.Options{Explorer: er}).Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/assets/AECTEST:" + issuer + "/entry-changes?limit=5")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data struct {
			Asset         string `json:"asset"`
			ThroughLedger uint32 `json:"through_ledger"`
			LowerBound    bool   `json:"lower_bound"`
			Changes       []struct {
				Ledger  uint32          `json:"ledger"`
				OpIndex int32           `json:"op_index"`
				Amount  string          `json:"amount"`
				Entry   json.RawMessage `json:"entry"`
			} `json:"changes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d decode %v", resp.StatusCode, err)
	}
	d := body.Data
	if d.Asset != asset || d.LowerBound || d.ThroughLedger < wm || len(d.Changes) != 3 {
		t.Fatalf("HTTP view = %+v, want 3 changes of %s through %d, not a lower bound", d, asset, wm)
	}
	if last := d.Changes[2]; last.Amount != big128.String() || last.OpIndex != -1 || string(last.Entry) != `{"balance":"big"}` {
		t.Fatalf("oldest change = %+v, want the exact 2^100 tx-level holder row", last)
	}
}
