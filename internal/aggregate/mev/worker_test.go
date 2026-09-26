package mev

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type fakeScanner struct {
	trades []canonical.Trade
	usd    []string
	err    error
}

func (f *fakeScanner) TradesForArbScan(_ context.Context, _ time.Time, _ int) ([]canonical.Trade, []string, error) {
	return f.trades, f.usd, f.err
}

type fakeSink struct {
	seen   map[string]bool
	events []StoredEvent
	err    error
}

func (s *fakeSink) InsertMEVEvent(_ context.Context, e StoredEvent) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[e.DedupKey] {
		return false, nil
	}
	s.seen[e.DedupKey] = true
	s.events = append(s.events, e)
	return true, nil
}

func arbTrades(t *testing.T) []canonical.Trade {
	return []canonical.Trade{
		trade(t, "soroswap", 1, "GARB", "native", usdc),
		trade(t, "phoenix", 2, "GARB", usdc, "native"),
	}
}

// RunOnce detects + persists once, and a re-run over the same window
// inserts nothing (dedup via the key).
func TestWorker_RunOnce_DetectsThenDedups(t *testing.T) {
	scanner := &fakeScanner{trades: arbTrades(t), usd: []string{"5.00", "5.00"}}
	sink := &fakeSink{}
	w := NewWorker(scanner, sink, WorkerConfig{})

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	detected, inserted, err := w.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if detected != 1 || inserted != 1 {
		t.Fatalf("first run: detected=%d inserted=%d, want 1/1", detected, inserted)
	}
	if len(sink.events) != 1 {
		t.Fatalf("want 1 stored event, got %d", len(sink.events))
	}

	// Evidence: detail carries the legs + notional.
	var d arbDetail
	if err := json.Unmarshal(sink.events[0].DetailJSON, &d); err != nil {
		t.Fatalf("detail unmarshal: %v", err)
	}
	if len(d.Legs) != 2 || d.NotionalUSD != "10.00" {
		t.Errorf("detail = %+v (want 2 legs, notional 10.00)", d)
	}
	if sink.events[0].AssetID != "" || sink.events[0].QuoteID != "" {
		t.Errorf("arbitrage is cross-asset: asset/quote = %q/%q, want empty (NULL)",
			sink.events[0].AssetID, sink.events[0].QuoteID)
	}

	// Re-run: same window → dedup → nothing new.
	detected2, inserted2, err := w.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	if detected2 != 1 || inserted2 != 0 {
		t.Fatalf("second run: detected=%d inserted=%d, want 1/0 (dedup)", detected2, inserted2)
	}
}

// A scan error surfaces and writes nothing.
func TestWorker_RunOnce_ScanError(t *testing.T) {
	w := NewWorker(&fakeScanner{err: errors.New("boom")}, &fakeSink{}, WorkerConfig{})
	if _, _, err := w.RunOnce(context.Background(), time.Now()); err == nil {
		t.Fatal("want scan error, got nil")
	}
}

// Every pair-scoped kind persists its primary asset/quote (the per-asset
// history index is keyed on asset_id), and every kind's stored detail
// carries the candidate's assets + sources, not just arbitrage's.
func TestStoredFrom_AssetsReachEveryKind(t *testing.T) {
	idx := map[string]uint32{txA: 1, txB: 2, txC: 3, txO: 4, txD: 5}
	sandwich := DetectSandwiches([]canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GVIC", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txC, taker: "GATK", base: usdc, quote: "native"}),
	}, nil, idx)
	oracleSw := DetectOracleSandwiches([]canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: usdc, quote: "native"}),
		mkTrade(t, tOpt{tx: txD, taker: "GATK", base: "native", quote: usdc}),
	}, nil, []OracleRef{oracleRef(usdc)}, idx)
	cascade := DetectLiquidationCascades([]AuctionFill{
		fill("CPOOLA", "GUSER1", "GFILL1", txA, 100),
		fill("CPOOLB", "GUSER2", "GFILL2", txB, 105),
	}, []OracleRef{{Source: "reflector-dex", Asset: usdc, Ledger: 102, TxHash: txO}})
	if len(sandwich) != 1 || len(oracleSw) != 1 || len(cascade) != 1 {
		t.Fatalf("fixtures: sandwich=%d oracle=%d cascade=%d, want 1 each", len(sandwich), len(oracleSw), len(cascade))
	}

	cases := []struct {
		c            Candidate
		asset, quote string
	}{
		{sandwich[0], "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "native"},
		{oracleSw[0], usdc, "fiat:USD"},
		{cascade[0], "", ""},
	}
	for _, tc := range cases {
		ev, err := storedFrom(tc.c)
		if err != nil {
			t.Fatalf("%s: storedFrom: %v", tc.c.Kind, err)
		}
		if ev.AssetID != tc.asset || ev.QuoteID != tc.quote {
			t.Errorf("%s: asset/quote = %q/%q, want %q/%q", tc.c.Kind, ev.AssetID, ev.QuoteID, tc.asset, tc.quote)
		}
		var d struct {
			Assets  []string `json:"assets"`
			Sources []string `json:"sources"`
			Note    string   `json:"note"`
		}
		if err := json.Unmarshal(ev.DetailJSON, &d); err != nil {
			t.Fatalf("%s: detail: %v", tc.c.Kind, err)
		}
		if len(d.Assets) == 0 || len(d.Sources) == 0 || d.Note == "" {
			t.Errorf("%s: detail lost assets/sources/note: %s", tc.c.Kind, ev.DetailJSON)
		}
	}
}

type truncObserver struct {
	nopObserver
	truncated []string
}

func (o *truncObserver) Truncated(input string) { o.truncated = append(o.truncated, input) }

// A trade scan that fills ScanLimit is reported, and its oldest ledger —
// which the cap may have cut mid-transaction — is not detected over.
// Here that ledger holds a complete-looking arbitrage cycle; the newest
// ledger holds one unrelated trade.
func TestWorker_RunOnce_TruncatedScanReportedAndOldestLedgerDropped(t *testing.T) {
	trades := arbTrades(t) // ledger 100
	tip := trade(t, "sdex", 0, "GOTHER", "native", usdc)
	tip.Ledger = 101
	tip.TxHash = "fff0000000000000000000000000000000000000000000000000000000000000"
	trades = append(trades, tip)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	o := &truncObserver{}
	w := NewWorker(&fakeScanner{trades: trades, usd: []string{"1", "1", "1"}}, &fakeSink{},
		WorkerConfig{ScanLimit: len(trades), Observer: o})
	detected, _, err := w.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(o.truncated) != 1 || o.truncated[0] != ScanInputTrades {
		t.Errorf("truncation reports = %v, want [%s]", o.truncated, ScanInputTrades)
	}
	if detected != 0 {
		t.Errorf("detected = %d, want 0: the cap-cut oldest ledger must not be detected over", detected)
	}

	// Below the cap: nothing reported, the cycle is detected.
	o2 := &truncObserver{}
	w2 := NewWorker(&fakeScanner{trades: trades, usd: []string{"1", "1", "1"}}, &fakeSink{},
		WorkerConfig{ScanLimit: len(trades) + 1, Observer: o2})
	detected, _, err = w2.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(o2.truncated) != 0 || detected != 1 {
		t.Errorf("untruncated scan: reports=%v detected=%d, want none/1", o2.truncated, detected)
	}
}
