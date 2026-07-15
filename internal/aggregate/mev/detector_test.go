package mev

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func mustPair(t *testing.T, base, quote string) canonical.Pair {
	t.Helper()
	b, err := canonical.ParseAsset(base)
	if err != nil {
		t.Fatalf("parse base %q: %v", base, err)
	}
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		t.Fatalf("parse quote %q: %v", quote, err)
	}
	p, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("pair %s/%s: %v", base, quote, err)
	}
	return p
}

func trade(t *testing.T, source string, op uint32, taker, base, quote string) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Ledger:      100,
		TxHash:      "abc1230000000000000000000000000000000000000000000000000000000000",
		OpIndex:     op,
		Timestamp:   time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC),
		Pair:        mustPair(t, base, quote),
		BaseAmount:  canonical.NewAmount(big.NewInt(1000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(2000)),
		Taker:       taker,
	}
}

const (
	usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aqua = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
)

// 2-pool, 2-asset arb across two venues → detected.
func TestDetectArbitrage_TwoPoolCycle(t *testing.T) {
	trades := []canonical.Trade{
		trade(t, "soroswap", 1, "GARB", "native", usdc),
		trade(t, "phoenix", 2, "GARB", usdc, "native"),
	}
	got := DetectArbitrage(trades, nil)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != KindArbitrage || c.Taker != "GARB" || len(c.Legs) != 2 {
		t.Errorf("candidate = %+v", c)
	}
	if c.DedupKey() != "arbitrage:abc1230000000000000000000000000000000000000000000000000000000000:GARB" {
		t.Errorf("dedup key = %q", c.DedupKey())
	}
}

// Triangular 3-asset arb on a single venue → detected.
func TestDetectArbitrage_Triangular(t *testing.T) {
	trades := []canonical.Trade{
		trade(t, "soroswap", 1, "GARB", "native", usdc),
		trade(t, "soroswap", 2, "GARB", usdc, aqua),
		trade(t, "soroswap", 3, "GARB", aqua, "native"),
	}
	got := DetectArbitrage(trades, nil)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if n := len(got[0].Assets); n != 3 {
		t.Errorf("assets = %d, want 3", n)
	}
}

// A 2-hop swap (path A→B→C, not a cycle) is NOT arbitrage.
func TestDetectArbitrage_TwoHopPathNotCycle(t *testing.T) {
	trades := []canonical.Trade{
		trade(t, "soroswap", 1, "GUSER", "native", usdc),
		trade(t, "soroswap", 2, "GUSER", usdc, aqua),
	}
	if got := DetectArbitrage(trades, nil); len(got) != 0 {
		t.Errorf("2-hop path flagged as arb: %+v", got)
	}
}

// 2-asset round-trip on a SINGLE venue is degenerate, not arb.
func TestDetectArbitrage_SingleVenueRoundTripRejected(t *testing.T) {
	trades := []canonical.Trade{
		trade(t, "soroswap", 1, "GUSER", "native", usdc),
		trade(t, "soroswap", 2, "GUSER", usdc, "native"),
	}
	if got := DetectArbitrage(trades, nil); len(got) != 0 {
		t.Errorf("single-venue round-trip flagged: %+v", got)
	}
}

// A single trade can't be a cycle; off-chain (ledger 0) is excluded.
func TestDetectArbitrage_SingleAndOffChainExcluded(t *testing.T) {
	single := []canonical.Trade{trade(t, "soroswap", 1, "GARB", "native", usdc)}
	if got := DetectArbitrage(single, nil); len(got) != 0 {
		t.Errorf("single trade flagged: %+v", got)
	}
	off := trade(t, "binance", 1, "GARB", "native", usdc)
	off.Ledger = 0
	off2 := trade(t, "kraken", 2, "GARB", usdc, "native")
	off2.Ledger = 0
	if got := DetectArbitrage([]canonical.Trade{off, off2}, nil); len(got) != 0 {
		t.Errorf("off-chain trades flagged: %+v", got)
	}
}

// Different takers in the same tx are grouped (and judged) separately.
func TestDetectArbitrage_PerTakerGrouping(t *testing.T) {
	trades := []canonical.Trade{
		trade(t, "soroswap", 1, "GARB", "native", usdc),
		trade(t, "phoenix", 2, "GARB", usdc, "native"),
		trade(t, "soroswap", 3, "GUSER", "native", usdc), // lone leg, no cycle
	}
	got := DetectArbitrage(trades, nil)
	if len(got) != 1 || got[0].Taker != "GARB" {
		t.Fatalf("want 1 candidate for GARB, got %+v", got)
	}
}

// NotionalUSD sums the parallel usd-volume slice across the cycle legs.
func TestDetectArbitrage_NotionalSum(t *testing.T) {
	trades := []canonical.Trade{
		trade(t, "soroswap", 1, "GARB", "native", usdc),
		trade(t, "phoenix", 2, "GARB", usdc, "native"),
	}
	got := DetectArbitrage(trades, []string{"10.50", "9.25"})
	if len(got) != 1 || got[0].NotionalUSD != "19.75" {
		t.Fatalf("notional = %q (want 19.75): %+v", got[0].NotionalUSD, got)
	}
}
