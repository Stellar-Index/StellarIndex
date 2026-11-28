//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// cometEntries is the real BLNT/USDC pool's instance, AllRecordData and TotalShares entries, by kind.
func cometEntries(t *testing.T) map[string]struct{ Key, Entry string } {
	t.Helper()
	raw, err := os.ReadFile("../fixtures/comet_storage/pool_entries.json")
	if err != nil {
		t.Fatal(err)
	}
	var all []struct {
		Pool     string `json:"pool"`
		Kind     string `json:"kind"`
		KeyXDR   string `json:"key_xdr"`
		EntryXDR string `json:"entry_xdr"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	out := map[string]struct{ Key, Entry string }{}
	for _, e := range all {
		if e.Pool == comet.MainnetBLNTUSDCPool && e.Kind != "Balance" {
			out[e.Kind] = struct{ Key, Entry string }{e.KeyXDR, e.EntryXDR}
		}
	}
	return out
}

// withQuoteBalance re-encodes a real AllRecordData entry with its USDC (index 1) record balance set to stroops.
func withQuoteBalance(t *testing.T, entry string, stroops int64) string {
	t.Helper()
	var le xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entry, &le); err != nil {
		t.Fatal(err)
	}
	recs := *le.Data.ContractData.Val.MustMap()
	fields := *recs[1].Val.MustMap()
	for i := range fields {
		if *fields[i].Key.Sym == "balance" {
			fields[i].Val = xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(stroops)}} //nolint:gosec // small positive
		}
	}
	out, err := xdr.MarshalBase64(le)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// lendingSnapshot is every watermark row (the lending_state cursor and the interest redo state) and every physical row
// of the other contracts' lending history: a rewrite adds rows even where FINAL hides it.
func lendingSnapshot(t *testing.T, ctx context.Context, conn driver.Conn, pool string) string {
	t.Helper()
	// Raw rows, not aggregates: a re-written duplicate must show.
	var out []string
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT name, toString(thru_ledger) FROM stellar.entry_history_watermark ORDER BY name, thru_ledger`, nil},
		{`SELECT key_xdr, toString(ledger_seq) FROM stellar.lending_state_hourly WHERE contract_id != ? ORDER BY key_xdr, ledger_seq`, []any{pool}},
	} {
		rows, err := conn.Query(ctx, q.sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, a+"="+b)
		}
		_ = rows.Close()
	}
	return strings.Join(out, " ")
}

// cometScopedWalk: on a host whose history lacks the pool, the reader refuses and the proof names the scoped walk;
// the scoped walk restores the pool's rows and touches no watermark, interest redo state, heartbeat or other row.
func cometScopedWalk(t *testing.T, ctx context.Context, conn driver.Conn, reader *chstore.ExplorerReader, addr, pool string, base uint32, want []chstore.CometPoolHour) {
	t.Helper()
	// An interest watermark through the range: a -from walk of an interest pool would arm the redo here.
	if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES ('lending_interest', ?), ('lending_interest_floor', ?)`,
		base+3, base); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE contract_id = ? SETTINGS mutations_sync = 2`, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.CometPoolStates(ctx, pool, want[0].Hour, want[len(want)-1].Hour.Add(time.Hour)); !errors.Is(err, chstore.ErrCometPoolNotWalked) {
		t.Errorf("unwalked pool: err = %v, want ErrCometPoolNotWalked", err)
	}
	from := strconv.FormatUint(uint64(base), 10)
	err := chops.Run([]string{"ch-entry-history", "-lending-only", "-prove", "-ch-addr", addr, "-from", from})
	if err == nil || !strings.Contains(err.Error(), "not walked yet") {
		t.Errorf("proof of an unwalked pool: %v, want not walked yet", err)
	}
	before := lendingSnapshot(t, ctx, conn, pool)
	hb := filepath.Join(t.TempDir(), "hb.prom")
	scoped := func(extra ...string) error {
		return chops.Run(append([]string{
			"ch-entry-history", "-lending-only", "-write", "-ch-addr", addr, "-from", from,
			"-lending-contract", pool, "-heartbeat", hb, "-lock-file", filepath.Join(t.TempDir(), "lending.lock"),
		}, extra...))
	}
	if err := scoped("-to", strconv.FormatUint(uint64(base+4), 10)); err == nil {
		t.Error("scoped walk past the watermark accepted")
	}
	if err := scoped(); err != nil {
		t.Fatalf("scoped walk: %v", err)
	}
	if after := lendingSnapshot(t, ctx, conn, pool); after != before {
		t.Errorf("scoped walk moved shared state:\n before %s\n after  %s", before, after)
	}
	if _, err := os.Stat(hb); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scoped walk wrote the heartbeat (%v)", err)
	}
	got, err := reader.CometPoolStates(ctx, pool, want[0].Hour, want[len(want)-1].Hour.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i].State == nil || got[i].Ledger != want[i].Ledger || got[i].State.Records[1].Balance.Cmp(want[i].State.Records[1].Balance) != 0 {
			t.Errorf("after the scoped walk hour %s = %+v, want %+v", want[i].Hour, got[i], want[i])
		}
	}
}

// The lending derive captures a Comet pool's records, LP supply and instance per key and hour, including a record
// change no contract event accompanies (v1.0 gulp, an archive restore); the proof covers them; the history reader
// carries each key to every hour's close and refuses an hour past the watermark.
func TestChEntryHistoryLending_CometPoolRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, q := range []string{`TRUNCATE TABLE stellar.entry_history_watermark`, `TRUNCATE TABLE stellar.lending_state_hourly`} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const base = uint32(171_600_000)
	pool := comet.MainnetBLNTUSDCPool
	seedLendingDeploy(t, ctx, addr, base)
	es := cometEntries(t)
	rec, shares, inst := es["AllRecordData"], es["TotalShares"], es["Instance"]
	t.Cleanup(func() {
		// The pool's real keys would otherwise stay live in the shared lake for every later lending proof.
		for _, tbl := range []string{"ledger_entry_changes", "ledger_entries_current"} {
			if err := conn.Exec(context.Background(), `ALTER TABLE stellar.`+tbl+` DELETE WHERE key_xdr IN (?, ?, ?) SETTINGS mutations_sync = 2`,
				rec.Key, shares.Key, inst.Key); err != nil {
				t.Errorf("clean %s: %v", tbl, err)
			}
		}
	})

	entryHistorySeedLedgers(t, ctx, addr, []uint32{base, base + 1, base + 2, base + 3}, 1, false)
	at := func(h, m int) time.Time { return time.Date(2027, 3, 1, h, m, 0, 0, time.UTC) }
	closes := []time.Time{at(10, 10), at(10, 50), at(11, 5), at(13, 20)}
	for i, ts := range closes {
		if err := conn.Exec(ctx, `ALTER TABLE stellar.ledgers UPDATE close_time = ? WHERE ledger_seq = ? SETTINGS mutations_sync = 2`,
			ts, base+uint32(i)); err != nil { //nolint:gosec // i < 4
			t.Fatalf("align ledger close time: %v", err)
		}
	}
	change := func(i uint32, ct, k, e string) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: base + i, CloseTime: closes[i], TxHash: "c0" + strconv.FormatUint(uint64(base+i), 10),
			ChangeType: ct, EntryType: "contract_data", KeyXDR: k, EntryXDR: e, IntraLedgerSeq: 1,
		}
	}
	// No contract event is seeded for any change: the derive reads entry changes only, so a record rewritten with no
	// POOL event (the base+2 change) is captured like any other.
	insertEntryChanges(t, ctx,
		change(0, "created", inst.Key, inst.Entry),
		change(0, "created", shares.Key, shares.Entry),
		change(0, "created", rec.Key, withQuoteBalance(t, rec.Entry, 100)),
		change(1, "updated", rec.Key, withQuoteBalance(t, rec.Entry, 200)),
		change(2, "updated", rec.Key, withQuoteBalance(t, rec.Entry, 300)),
		change(3, "updated", rec.Key, rec.Entry),
	)
	if err := runLendingWrite(t, addr, base, base+3); err != nil {
		t.Fatalf("derive [%d,%d]: %v", base, base+3, err)
	}

	rows, err := conn.Query(ctx, `SELECT protocol, toString(hour), ledger_seq FROM stellar.lending_state_hourly FINAL
		WHERE contract_id = ? AND key_xdr = ? ORDER BY hour`, pool, rec.Key)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var p, h string
		var seq uint32
		if err := rows.Scan(&p, &h, &seq); err != nil {
			t.Fatal(err)
		}
		got = append(got, p+" "+h+" "+strconv.FormatUint(uint64(seq-base), 10))
	}
	_ = rows.Close()
	want := []string{"comet 2027-03-01 10:00:00 1", "comet 2027-03-01 11:00:00 2", "comet 2027-03-01 13:00:00 3"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("AllRecordData history = %q, want %q", got, want)
	}

	prove := []string{"ch-entry-history", "-lending-only", "-prove", "-ch-addr", addr, "-from", strconv.FormatUint(uint64(base), 10)}
	if err := chops.Run(prove); err != nil {
		t.Fatalf("proof over the captured comet keys: %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	hours, err := reader.CometPoolStates(ctx, pool, at(10, 0), at(13, 0))
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range []struct {
		usdc   string
		ledger uint32
	}{{"200", base + 1}, {"300", base + 2}, {"300", base + 2}} {
		h := hours[i]
		if h.State == nil || h.State.Records[1].Balance.String() != w.usdc || h.Ledger != w.ledger || h.State.TotalShares.String() != "3796905613114" {
			t.Errorf("hour %s = %+v, want USDC %s at ledger %d", h.Hour, h, w.usdc, w.ledger)
		}
	}
	if _, err := reader.CometPoolStateAt(ctx, pool, at(13, 0)); !errors.Is(err, chstore.ErrCometHoursNotCovered) {
		t.Errorf("the watermark's hour: err = %v, want ErrCometHoursNotCovered", err)
	}
	// The fixture creates the pool at base, so 09:00 precedes it: no state, and Missing says which key.
	if before, err := reader.CometPoolStateAt(ctx, pool, at(9, 0)); err != nil || before.State != nil || before.Missing != "AllRecordData not recorded" {
		t.Errorf("hour before the pool's creation = %+v (%v), want no state, AllRecordData not recorded", before, err)
	}

	cometScopedWalk(t, ctx, conn, reader, addr, pool, base, hours)

	// A missing (key, hour) row of the comet pool fails the proof.
	if err := conn.Exec(ctx, `ALTER TABLE stellar.lending_state_hourly DELETE WHERE key_xdr = ? AND ledger_seq = ? SETTINGS mutations_sync = 2`,
		rec.Key, base+2); err != nil {
		t.Fatal(err)
	}
	if err := chops.Run(prove); err == nil {
		t.Fatal("proof passed with the comet record's 11:00 row deleted")
	}
}
