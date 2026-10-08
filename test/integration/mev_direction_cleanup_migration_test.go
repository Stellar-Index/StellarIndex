//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/mev"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	mev0185Ledger   = 60_000_000
	mev0185Attacker = "GATTACKER0185"
	mev0185Victim   = "GVICTIM0185"
	mev0185USDC     = "USDC-" + mevIntegrationAccount
	mev0185XLMSAC   = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
)

type mev0185Scanner struct{ trades []c.Trade }

func (s mev0185Scanner) TradesForArbScan(context.Context, time.Time, int) ([]c.Trade, []string, error) {
	return s.trades, make([]string, len(s.trades)), nil
}

type mev0185Oracles struct{ refs []mev.OracleRef }

func (o mev0185Oracles) OracleUpdatesForMEVScan(context.Context, time.Time, int) ([]mev.OracleRef, error) {
	return o.refs, nil
}

type mev0185Order map[string]uint32

func (o mev0185Order) TxIndexes(context.Context, []string) (map[string]uint32, error) {
	return o, nil
}

func mev0185Hash(n int) string {
	return strings.Repeat("0", 62) + string("0123456789abcdef"[n>>4]) + string("0123456789abcdef"[n&0xf])
}

func mev0185Trade(t *testing.T, n int, taker string, base, quote c.Asset, ts time.Time) c.Trade {
	t.Helper()
	pair, err := c.NewPair(base, quote)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return c.Trade{
		Source: "sdex", Ledger: mev0185Ledger, TxHash: mev0185Hash(n), Timestamp: ts,
		Pair: pair, BaseAmount: c.NewAmount(big.NewInt(10_000_000)), QuoteAmount: c.NewAmount(big.NewInt(2_000_000)),
		Taker: taker,
	}
}

// mev0185DetectCurrent runs the CURRENT detectors through the real worker
// into the real store — the writer whose output 0185 must never delete. One
// ledger yields both kinds: the attacker trades native->USDC at tx 1 and
// USDC->native at tx 4, around a victim at tx 3 and an oracle update at tx 2.
func mev0185DetectCurrent(t *testing.T, ctx context.Context, store *timescale.Store) {
	t.Helper()
	usdc, err := c.NewClassicAsset("USDC", mevIntegrationAccount)
	if err != nil {
		t.Fatalf("usdc: %v", err)
	}
	ts := time.Now().UTC().Truncate(time.Second)
	trades := []c.Trade{
		mev0185Trade(t, 1, mev0185Attacker, c.NativeAsset(), usdc, ts),
		mev0185Trade(t, 3, mev0185Victim, c.NativeAsset(), usdc, ts),
		mev0185Trade(t, 4, mev0185Attacker, usdc, c.NativeAsset(), ts),
	}
	oracle := mev.OracleRef{Source: "reflector-dex", Ledger: mev0185Ledger, TxHash: mev0185Hash(2), Asset: mev0185USDC, Timestamp: ts}
	order := mev0185Order{mev0185Hash(1): 1, mev0185Hash(2): 2, mev0185Hash(3): 3, mev0185Hash(4): 4}
	w := mev.NewWorker(mev0185Scanner{trades: trades}, store, mev.WorkerConfig{
		Oracles: mev0185Oracles{refs: []mev.OracleRef{oracle}},
		Order:   order,
	})
	if _, _, err := w.RunOnce(ctx, ts.Add(time.Minute)); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

func mev0185Leg(source, base, quote, role string) string {
	return `{"source":"` + source + `","tx_hash":"` + mev0185Hash(9) + `","tx_index":1,"base":"` + base +
		`","quote":"` + quote + `","base_amount":"1","quote_amount":"1","role":"` + role + `"}`
}

func mev0185Insert(t *testing.T, ctx context.Context, store *timescale.Store, kind, key, detail string) {
	t.Helper()
	ok, err := store.InsertMEVEvent(ctx, domain.MEVStoredEvent{
		Kind: kind, Ledger: mev0185Ledger, DetectedAtLedger: mev0185Ledger,
		Timestamp: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		TxHashes:  []string{mev0185Hash(9)}, Accounts: []string{mev0185Attacker},
		DedupKey: key, DetailJSON: []byte(detail),
	})
	if err != nil || !ok {
		t.Fatalf("insert %s: ok=%v err=%v", key, ok, err)
	}
}

// mev0185InsertLegacy writes rows in the shape the legacy detectors
// persisted and returns which of them 0185 must keep.
func mev0185InsertLegacy(t *testing.T, ctx context.Context, store *timescale.Store) map[string]bool {
	t.Helper()
	sandwich := func(a, b string) string {
		return `{"pair":"x","attacker":"` + mev0185Attacker + `","legs":[` + a + `,` +
			mev0185Leg("sdex", "native", mev0185USDC, "victim") + `,` + b + `],"note":"legacy"}`
	}
	oracleSw := func(a, b string) string {
		return `{"asset":"` + mev0185USDC + `","account":"` + mev0185Attacker + `","legs":[` + a + `,` + b + `],"note":"legacy"}`
	}
	rows := []struct {
		kind, key, detail string
		keep              bool
	}{
		{"sandwich", "legacy:same-direction", sandwich(
			mev0185Leg("sdex", "native", mev0185USDC, "bracket"),
			mev0185Leg("sdex", "native", mev0185USDC, "bracket")), false},
		{"sandwich", "legacy:unknown-source", sandwich(
			mev0185Leg("sdex", "native", mev0185USDC, "bracket"),
			mev0185Leg("blend", mev0185USDC, "native", "bracket")), false},
		// Opposite once the XLM SAC is read as native; unnormalised it
		// would compare CAS3... against native and look same-direction.
		{"sandwich", "legacy:opposite-via-sac", sandwich(
			mev0185Leg("soroswap", mev0185XLMSAC, mev0185USDC, "bracket"),
			mev0185Leg("soroswap", mev0185USDC, "native", "bracket")), true},
		{"oracle_sandwich", "legacy:oracle-same-direction", oracleSw(
			mev0185Leg("soroswap", mev0185USDC, "native", "before"),
			mev0185Leg("soroswap", mev0185USDC, "native", "after")), false},
		{"oracle_sandwich", "legacy:oracle-opposite", oracleSw(
			mev0185Leg("soroswap", mev0185USDC, "native", "before"),
			mev0185Leg("sdex", mev0185USDC, "native", "after")), true},
		{"wash_trade", "legacy:other-kind", `{"variant":"self_trade","legs":[],"note":"x"}`, true},
	}
	want := map[string]bool{}
	for _, r := range rows {
		mev0185Insert(t, ctx, store, r.kind, r.key, r.detail)
		want[r.key] = r.keep
	}
	return want
}

func mev0185Keys(t *testing.T, ctx context.Context, store *timescale.Store) map[string]string {
	t.Helper()
	rows, err := store.DB().QueryContext(ctx, `SELECT dedup_key, kind FROM mev_events`)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var key, kind string
		if err := rows.Scan(&key, &kind); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[key] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestMigration0185_DropsOnlyUnprovenDirectionSandwiches: 0185 deletes the
// sandwich / oracle_sandwich rows whose own legs do not prove opposite
// directions, keeps every row the current detectors write, and leaves other
// kinds alone.
func TestMigration0185_DropsOnlyUnprovenDirectionSandwiches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 184)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = store.Close() }()

	mev0185DetectCurrent(t, ctx, store)
	current := mev0185Keys(t, ctx, store)
	kinds := map[string]bool{}
	for _, kind := range current {
		kinds[kind] = true
	}
	if !kinds["sandwich"] || !kinds["oracle_sandwich"] {
		t.Fatalf("fixture: the current detectors wrote %v, want a sandwich and an oracle_sandwich", current)
	}
	want := mev0185InsertLegacy(t, ctx, store)
	for key := range current {
		want[key] = true
	}

	applyMigrations(t, dsn)

	got := mev0185Keys(t, ctx, store)
	for key, keep := range want {
		if _, present := got[key]; present != keep {
			t.Errorf("%s: present after 0185 = %v, want %v", key, present, keep)
		}
	}
}
