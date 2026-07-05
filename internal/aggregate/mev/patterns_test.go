package mev

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// tOpt is the synthetic-trade spec for the pattern tests — the arb
// tests' fixed-tx `trade` helper can't express cross-tx patterns.
type tOpt struct {
	source string
	ledger uint32
	tx     string
	op     uint32
	maker  string
	taker  string
	base   string
	quote  string
	ts     time.Time
}

var baseTS = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

func mkTrade(t *testing.T, o tOpt) canonical.Trade {
	t.Helper()
	if o.source == "" {
		o.source = "sdex"
	}
	if o.ledger == 0 {
		o.ledger = 200
	}
	if o.ts.IsZero() {
		o.ts = baseTS
	}
	return canonical.Trade{
		Source:      o.source,
		Ledger:      o.ledger,
		TxHash:      o.tx,
		OpIndex:     o.op,
		Timestamp:   o.ts,
		Pair:        mustPair(t, o.base, o.quote),
		BaseAmount:  canonical.NewAmount(big.NewInt(1000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(2000)),
		Maker:       o.maker,
		Taker:       o.taker,
	}
}

const (
	txA = "aaa0000000000000000000000000000000000000000000000000000000000000"
	txB = "bbb0000000000000000000000000000000000000000000000000000000000000"
	txC = "ccc0000000000000000000000000000000000000000000000000000000000000"
	txD = "ddd0000000000000000000000000000000000000000000000000000000000000"
	txO = "eee0000000000000000000000000000000000000000000000000000000000000"
)

// ── sandwich ────────────────────────────────────────────────────────

// Attacker trades in two txs bracketing a victim's tx on the same
// pair in one ledger → detected, with the (front, victim, back) legs.
func TestDetectSandwiches_Bracket(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GVIC", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txC, taker: "GATK", base: "native", quote: usdc}),
	}
	idx := map[string]uint32{txA: 1, txB: 3, txC: 5}
	got := DetectSandwiches(trades, nil, idx)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != KindSandwich || c.Taker != "GATK" {
		t.Errorf("candidate = %+v", c)
	}
	if len(c.TxHashes) != 3 || c.TxHashes[0] != txA || c.TxHashes[2] != txC {
		t.Errorf("tx hashes = %v", c.TxHashes)
	}
	if len(c.Accounts) != 2 || c.Accounts[0] != "GATK" || c.Accounts[1] != "GVIC" {
		t.Errorf("accounts = %v", c.Accounts)
	}
	d, ok := c.Detail.(sandwichDetail)
	if !ok {
		t.Fatalf("detail type %T", c.Detail)
	}
	if len(d.Legs) != 3 || d.Legs[0].Role != "bracket" || d.Legs[1].Role != "victim" || d.Legs[2].Role != "bracket" {
		t.Errorf("legs = %+v", d.Legs)
	}
	if !strings.HasPrefix(c.DedupKey(), "sandwich:"+txA+":GATK:") {
		t.Errorf("dedup = %q", c.DedupKey())
	}
}

// The same pair written in the opposite orientation still groups —
// pair identity is orientation-independent.
func TestDetectSandwiches_OrientationIndependent(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GVIC", base: usdc, quote: "native"}), // flipped
		mkTrade(t, tOpt{tx: txC, taker: "GATK", base: "native", quote: usdc}),
	}
	idx := map[string]uint32{txA: 1, txB: 2, txC: 3}
	if got := DetectSandwiches(trades, nil, idx); len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
}

// No victim strictly between the attacker's txs → nothing.
func TestDetectSandwiches_NoVictimInBracket(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GVIC", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txC, taker: "GATK", base: "native", quote: usdc}),
	}
	idx := map[string]uint32{txA: 1, txB: 7, txC: 5} // victim AFTER the back leg
	if got := DetectSandwiches(trades, nil, idx); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0: %+v", len(got), got)
	}
}

// Both attacker trades in ONE tx is an atomic pattern (arbitrage
// territory), not a cross-tx sandwich.
func TestDetectSandwiches_SingleTxAttackerIgnored(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, op: 0, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GVIC", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txA, op: 1, taker: "GATK", base: "native", quote: usdc}),
	}
	idx := map[string]uint32{txA: 1, txB: 3}
	if got := DetectSandwiches(trades, nil, idx); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

// Unresolved hashes (not in the tx_index map) degrade to no
// detection — order is never guessed.
func TestDetectSandwiches_UnresolvedOrderSkips(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GVIC", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txC, taker: "GATK", base: "native", quote: usdc}),
	}
	idx := map[string]uint32{txA: 1, txC: 5} // victim's tx unresolved
	if got := DetectSandwiches(trades, nil, idx); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

// ── oracle sandwich ─────────────────────────────────────────────────

func oracleRef(asset string) OracleRef {
	return OracleRef{
		Source:     "reflector-dex",
		ContractID: "CORACLE",
		Ledger:     200,
		TxHash:     txO,
		OpIndex:    0,
		Asset:      asset,
		Quote:      "fiat:USD",
		Timestamp:  baseTS,
	}
}

// Trades on the oracle's asset in txs on both sides of the update →
// detected with before/after legs.
func TestDetectOracleSandwiches_Bracket(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GATK", base: "native", quote: usdc}),
	}
	idx := map[string]uint32{txA: 1, txO: 3, txB: 5}
	got := DetectOracleSandwiches(trades, nil, []OracleRef{oracleRef(usdc)}, idx)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != KindOracleSandwich || c.Taker != "GATK" || c.TxHash != txO {
		t.Errorf("candidate = %+v", c)
	}
	d, ok := c.Detail.(oracleSandwichDetail)
	if !ok {
		t.Fatalf("detail type %T", c.Detail)
	}
	if len(d.Legs) != 2 || d.Legs[0].Role != "before" || d.Legs[1].Role != "after" {
		t.Errorf("legs = %+v", d.Legs)
	}
	if d.OracleTxIndex != 3 || d.OracleSource != "reflector-dex" {
		t.Errorf("detail = %+v", d)
	}
}

// Trades only BEFORE the update → nothing (no bracket).
func TestDetectOracleSandwiches_OneSideOnly(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GATK", base: "native", quote: usdc}),
	}
	idx := map[string]uint32{txA: 1, txB: 2, txO: 5}
	if got := DetectOracleSandwiches(trades, nil, []OracleRef{oracleRef(usdc)}, idx); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

// The native-XLM SAC id and "native" are the same asset for oracle
// matching.
func TestDetectOracleSandwiches_XLMSACNormalised(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GATK", base: "native", quote: usdc}),
	}
	idx := map[string]uint32{txA: 1, txO: 3, txB: 5}
	if got := DetectOracleSandwiches(trades, nil, []OracleRef{oracleRef(xlmSAC)}, idx); len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
}

// A trade pair that doesn't touch the oracle's asset never matches.
func TestDetectOracleSandwiches_UnrelatedPair(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: aqua}),
		mkTrade(t, tOpt{tx: txB, taker: "GATK", base: "native", quote: aqua}),
	}
	idx := map[string]uint32{txA: 1, txO: 3, txB: 5}
	if got := DetectOracleSandwiches(trades, nil, []OracleRef{oracleRef(usdc)}, idx); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

// ── wash trading ────────────────────────────────────────────────────

// maker == taker → self-trade candidate with the default (tx, actor)
// dedup identity.
func TestDetectWashTrades_SelfTrade(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, maker: "GSELF", taker: "GSELF", base: "native", quote: usdc}),
	}
	got := DetectWashTrades(trades, []string{"12.34"})
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != KindWashTrade || c.DedupKey() != "wash_trade:"+txA+":GSELF" {
		t.Errorf("candidate = %+v (dedup %q)", c, c.DedupKey())
	}
	d, ok := c.Detail.(washDetail)
	if !ok || d.Variant != "self_trade" || d.NotionalUSD != "12.34" {
		t.Errorf("detail = %+v", c.Detail)
	}
}

// ≥2 fills each direction between two accounts on one pair in one UTC
// day → round-trip candidate keyed on (day, pair, account pair).
func TestDetectWashTrades_RoundTrip(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, ledger: 200, maker: "GYYY", taker: "GXXX", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, ledger: 201, maker: "GXXX", taker: "GYYY", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txC, ledger: 202, maker: "GYYY", taker: "GXXX", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txD, ledger: 203, maker: "GXXX", taker: "GYYY", base: "native", quote: usdc}),
	}
	got := DetectWashTrades(trades, nil)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	d, ok := c.Detail.(washDetail)
	if !ok || d.Variant != "round_trip" || len(d.Legs) != 4 {
		t.Fatalf("detail = %+v", c.Detail)
	}
	if len(c.Accounts) != 2 {
		t.Errorf("accounts = %v", c.Accounts)
	}
	wantDedup := "wash_trade:rt:2026-07-04:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN|native:GXXX|GYYY"
	if c.DedupKey() != wantDedup {
		t.Errorf("dedup = %q, want %q", c.DedupKey(), wantDedup)
	}
}

// One fill each direction is ordinary trading, not the repeated
// back-and-forth signature.
func TestDetectWashTrades_RoundTripBelowThreshold(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, maker: "GYYY", taker: "GXXX", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, maker: "GXXX", taker: "GYYY", base: "native", quote: usdc}),
	}
	if got := DetectWashTrades(trades, nil); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0: %+v", len(got), got)
	}
}

// Maker-less rows (AMM trades: pool identity absent/irrelevant) never
// wash-match.
func TestDetectWashTrades_NoMakerIgnored(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(t, tOpt{tx: txA, taker: "GXXX", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GYYY", base: "native", quote: usdc}),
	}
	if got := DetectWashTrades(trades, nil); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

// ── liquidation cascade ─────────────────────────────────────────────

func fill(pool, user, filler, tx string, ledger uint32) AuctionFill {
	return AuctionFill{
		Pool: pool, User: user, Filler: filler,
		AuctionType: 0, Ledger: ledger, TxHash: tx, OpIndex: 0,
		Timestamp: baseTS,
	}
}

// Two fills against different positions within the window with an
// oracle update in the bracket → one candidate anchored on the later
// fill.
func TestDetectLiquidationCascades_Cluster(t *testing.T) {
	fills := []AuctionFill{
		fill("CPOOLA", "GUSER1", "GFILL1", txA, 100),
		fill("CPOOLB", "GUSER2", "GFILL2", txB, 105),
	}
	oracles := []OracleRef{{Source: "reflector-dex", Asset: usdc, Ledger: 102, TxHash: txO}}
	got := DetectLiquidationCascades(fills, oracles)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != KindLiquidationCascade || c.TxHash != txB || c.Taker != "GFILL2" {
		t.Errorf("candidate = %+v", c)
	}
	d, ok := c.Detail.(cascadeDetail)
	if !ok || len(d.PriorFills) != 1 || len(d.OracleUpdates) != 1 {
		t.Fatalf("detail = %+v", c.Detail)
	}
	if d.PriorFills[0].TxHash != txA || d.OracleUpdates[0].Ledger != 102 {
		t.Errorf("detail = %+v", d)
	}
}

// Partial fills of the SAME position are one auction lifecycle, not a
// cascade.
func TestDetectLiquidationCascades_SamePositionIgnored(t *testing.T) {
	fills := []AuctionFill{
		fill("CPOOLA", "GUSER1", "GFILL1", txA, 100),
		fill("CPOOLA", "GUSER1", "GFILL2", txB, 103),
	}
	oracles := []OracleRef{{Source: "reflector-dex", Asset: usdc, Ledger: 101, TxHash: txO}}
	if got := DetectLiquidationCascades(fills, oracles); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

// Fills outside the ledger window don't cluster.
func TestDetectLiquidationCascades_OutsideWindow(t *testing.T) {
	fills := []AuctionFill{
		fill("CPOOLA", "GUSER1", "GFILL1", txA, 100),
		fill("CPOOLB", "GUSER2", "GFILL2", txB, 100+cascadeWindowLedgers+1),
	}
	oracles := []OracleRef{{Source: "reflector-dex", Asset: usdc, Ledger: 105, TxHash: txO}}
	if got := DetectLiquidationCascades(fills, oracles); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

// No oracle update in the bracket → no candidate (the correlation is
// the point).
func TestDetectLiquidationCascades_NoOracle(t *testing.T) {
	fills := []AuctionFill{
		fill("CPOOLA", "GUSER1", "GFILL1", txA, 100),
		fill("CPOOLB", "GUSER2", "GFILL2", txB, 105),
	}
	if got := DetectLiquidationCascades(fills, nil); len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
	oracles := []OracleRef{{Source: "reflector-dex", Asset: usdc, Ledger: 300, TxHash: txO}}
	if got := DetectLiquidationCascades(fills, oracles); len(got) != 0 {
		t.Fatalf("oracle outside bracket: got %d candidates, want 0", len(got))
	}
}

// ── ordering prefilter ──────────────────────────────────────────────

// OrderingTxHashes only asks the lake about hashes that could matter:
// a minimal sandwich-shaped group qualifies, an unrelated pair's
// trades don't.
func TestOrderingTxHashes_Prefilter(t *testing.T) {
	trades := []canonical.Trade{
		// Sandwich-shaped group (≥3 trades, ≥2 takers, one taker on 2 txs).
		mkTrade(t, tOpt{tx: txA, taker: "GATK", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txB, taker: "GVIC", base: "native", quote: usdc}),
		mkTrade(t, tOpt{tx: txC, taker: "GATK", base: "native", quote: usdc}),
		// Lone trade on another pair in a ledger with no oracle → excluded.
		mkTrade(t, tOpt{tx: txD, ledger: 999, taker: "GZZZ", base: "native", quote: aqua}),
	}
	got := OrderingTxHashes(trades, nil)
	want := map[string]bool{txA: true, txB: true, txC: true}
	if len(got) != len(want) {
		t.Fatalf("hashes = %v, want %v", got, want)
	}
	for _, h := range got {
		if !want[h] {
			t.Errorf("unexpected hash %s", h)
		}
	}

	// An oracle in the lone trade's ledger on its pair pulls that
	// trade (and the oracle tx) in.
	oracles := []OracleRef{{Source: "reflector-dex", Asset: aqua, Ledger: 999, TxHash: txO}}
	got = OrderingTxHashes(trades, oracles)
	if len(got) != 5 {
		t.Fatalf("with oracle: hashes = %v, want 5 entries", got)
	}
}
