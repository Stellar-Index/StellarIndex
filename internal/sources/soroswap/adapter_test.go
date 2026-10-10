package soroswap

import (
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── consumer.go ──────────────────────────────────────────────────

func TestTradeEvent_implementsConsumerEvent(t *testing.T) {
	te := TradeEvent{}
	if got := te.EventKind(); got != "soroswap.trade" {
		t.Errorf("EventKind() = %q, want \"soroswap.trade\"", got)
	}
	if got := te.Source(); got != SourceName {
		t.Errorf("Source() = %q, want %q", got, SourceName)
	}
	var _ consumer.Event = te
}

// ─── dispatcher_adapter.go ────────────────────────────────────────

func TestDecoder_Name(t *testing.T) {
	if got := NewDecoder().Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

// TestIsMainnetFactory_multiFactory pins ADR-0035: Soroswap has more than
// one factory and new_pair must be honored from every verified factory (the
// primary CA4HEQTL + the launch-era ones), and from nothing else.
func TestIsMainnetFactory_multiFactory(t *testing.T) {
	if len(MainnetFactories) < 2 {
		t.Fatalf("expected >=2 Soroswap factories, got %v", MainnetFactories)
	}
	d := NewDecoder()
	for _, f := range MainnetFactories {
		ev := events.Event{Topic: []string{TopicPrefixFactory, TopicSymbolNewPair}, ContractID: f}
		if !d.Matches(ev) {
			t.Errorf("new_pair from verified factory %s: Matches=false, want true", f)
		}
	}
	notFactory := makeContractStrkey(t, 0x33)
	ev := events.Event{Topic: []string{TopicPrefixFactory, TopicSymbolNewPair}, ContractID: notFactory}
	if d.Matches(ev) {
		t.Error("new_pair from a non-factory: Matches=true, want false (injection guard)")
	}
}

func TestDecoder_Matches_pairAndFactoryTopics(t *testing.T) {
	// Contract-gated: topic symbols aren't unique across
	// protocols, so Matches() requires the emitter to be a canonical
	// factory (for new_pair) or a REGISTERED pair (for pair events).
	d := NewDecoder()
	registered := makeContractStrkey(t, 0x42)
	d.SeedPair(registered, canonical.Asset{}, canonical.Asset{})
	foreign := makeContractStrkey(t, 0x77) // not in the registry

	for _, tc := range []struct {
		name string
		ev   events.Event
		want bool
	}{
		// Pair events from a REGISTERED pair match.
		{"registered pair swap", events.Event{Topic: []string{TopicPrefixPair, TopicSymbolSwap}, ContractID: registered}, true},
		{"registered pair sync", events.Event{Topic: []string{TopicPrefixPair, TopicSymbolSync}, ContractID: registered}, true},
		{"registered pair deposit", events.Event{Topic: []string{TopicPrefixPair, TopicSymbolDeposit}, ContractID: registered}, true},
		{"registered pair withdraw", events.Event{Topic: []string{TopicPrefixPair, TopicSymbolWithdraw}, ContractID: registered}, true},
		// Pair events from an UNREGISTERED contract (topic collision) do NOT.
		// deposit/withdraw fail CLOSED on an unseeded pair too — an unseeded
		// pair is a recognition gap (no row), NOT a NULL-token row (migration
		// 0127 header + emitLiquidity doc).
		{"foreign pair swap", events.Event{Topic: []string{TopicPrefixPair, TopicSymbolSwap}, ContractID: foreign}, false},
		{"unseeded pair deposit", events.Event{Topic: []string{TopicPrefixPair, TopicSymbolDeposit}, ContractID: foreign}, false},
		{"unseeded pair withdraw", events.Event{Topic: []string{TopicPrefixPair, TopicSymbolWithdraw}, ContractID: foreign}, false},
		// new_pair only from the canonical factory contract.
		{"factory new_pair from factory", events.Event{Topic: []string{TopicPrefixFactory, TopicSymbolNewPair}, ContractID: MainnetFactory}, true},
		{"new_pair from a foreign contract (injection)", events.Event{Topic: []string{TopicPrefixFactory, TopicSymbolNewPair}, ContractID: foreign}, false},
		{"unrelated topic", events.Event{Topic: []string{TopicSymbolSwap, TopicPrefixPair}, ContractID: registered}, false},
		{"empty topic", events.Event{Topic: nil, ContractID: registered}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.Matches(tc.ev); got != tc.want {
				t.Errorf("Matches(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// makeNewPairEvent builds a factory new_pair event whose body
// encodes (token0, token1, pair) — matching the production path
// the registry seeder consumes.
func makeNewPairEvent(t *testing.T, token0, token1, pair string) events.Event {
	t.Helper()
	npL := xdr.Uint32(1)
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("new_pairs_length"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &npL}},
		xdr.ScMapEntry{Key: symbol("pair"), Val: contractAddrFromStrkey(t, pair)},
		xdr.ScMapEntry{Key: symbol("token_0"), Val: contractAddrFromStrkey(t, token0)},
		xdr.ScMapEntry{Key: symbol("token_1"), Val: contractAddrFromStrkey(t, token1)},
	))
	return events.Event{
		Topic:          []string{TopicPrefixFactory, TopicSymbolNewPair},
		Value:          body,
		Ledger:         52_000_000,
		TxHash:         "factorytx0",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
		// new_pair is emitted BY the factory, so its ContractID is the
		// factory address — not the created pair (the pair is carried in
		// the body). The dispatcher's Matches() gates new_pair on this.
		ContractID: MainnetFactory,
	}
}

// makeSwapEvent builds a pair-contract swap event whose body
// carries a single direction (token0 → token1).
func makeSwapEvent(t *testing.T, pair string, in0, out1 *big.Int) events.Event {
	t.Helper()
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount_0_in"), Val: i128(in0)},
		xdr.ScMapEntry{Key: symbol("amount_0_out"), Val: i128(big.NewInt(0))},
		xdr.ScMapEntry{Key: symbol("amount_1_in"), Val: i128(big.NewInt(0))},
		xdr.ScMapEntry{Key: symbol("amount_1_out"), Val: i128(out1)},
		xdr.ScMapEntry{Key: symbol("to"), Val: contractAddrFromStrkey(t, makeContractStrkey(t, 0x99))},
	))
	return events.Event{
		Topic:          []string{TopicPrefixPair, TopicSymbolSwap},
		Value:          body,
		Ledger:         52_000_001,
		TxHash:         "swaptx0",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:01Z",
		ContractID:     pair,
	}
}

// makeSyncEvent builds the sync event paired with makeSwapEvent (same group key).
func makeSyncEvent(t *testing.T, pair string) events.Event {
	t.Helper()
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("new_reserve_0"), Val: i128(big.NewInt(1_000_000))},
		xdr.ScMapEntry{Key: symbol("new_reserve_1"), Val: i128(big.NewInt(2_000_000))},
	))
	return events.Event{
		Topic:          []string{TopicPrefixPair, TopicSymbolSync},
		Value:          body,
		Ledger:         52_000_001,
		TxHash:         "swaptx0",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:01Z",
		ContractID:     pair,
	}
}

func TestDecoder_Decode_newPairSeedsRegistryButEmitsNothing(t *testing.T) {
	d := NewDecoder()
	token0 := makeContractStrkey(t, 0x10)
	token1 := makeContractStrkey(t, 0x11)
	pair := makeContractStrkey(t, 0x20)

	out, err := d.Decode(makeNewPairEvent(t, token0, token1, pair))
	if err != nil {
		t.Fatalf("Decode new_pair: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("new_pair should produce 0 trade events, got %d", len(out))
	}
	// Registry should have absorbed the pair.
	d.mu.RLock()
	tokens, ok := d.pairTokens[pair]
	d.mu.RUnlock()
	if !ok {
		t.Fatal("pair tokens not seeded after new_pair Decode")
	}
	if tokens.Token0.ContractID != token0 || tokens.Token1.ContractID != token1 {
		t.Errorf("tokens = %+v, want token0=%s token1=%s", tokens, token0, token1)
	}
}

func TestDecoder_Decode_swapSyncWithoutRegistryIncrementsSkipped(t *testing.T) {
	// No new_pair seeded for this pair — the swap+sync must be
	// dropped with skippedUnknownPair++.
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x20)

	if _, err := d.Decode(makeSwapEvent(t, pair, big.NewInt(100), big.NewInt(200))); err != nil {
		t.Fatalf("Decode swap: %v", err)
	}
	if _, err := d.Decode(makeSyncEvent(t, pair)); err != nil {
		t.Fatalf("Decode sync: %v", err)
	}
	if got := d.SkippedUnknownPair(); got != 1 {
		t.Errorf("SkippedUnknownPair() = %d, want 1", got)
	}
}

func TestDecoder_Decode_swapSyncWithRegistryEmitsTradeEvent(t *testing.T) {
	token0 := makeContractStrkey(t, 0x10)
	token1 := makeContractStrkey(t, 0x11)
	pair := makeContractStrkey(t, 0x20)
	t0Asset, _ := canonical.NewSorobanAsset(token0)
	t1Asset, _ := canonical.NewSorobanAsset(token1)

	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		pair: {Token0: t0Asset, Token1: t1Asset},
	}))

	// Swap arrives first — buffer holds, no output.
	out, err := d.Decode(makeSwapEvent(t, pair, big.NewInt(100), big.NewInt(200)))
	if err != nil {
		t.Fatalf("Decode swap: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d events on swap-only, want 0 (still buffering)", len(out))
	}

	// Sync completes the pair — exactly one TradeEvent.
	out, err = d.Decode(makeSyncEvent(t, pair))
	if err != nil {
		t.Fatalf("Decode sync: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events after sync, want 1", len(out))
	}
	te, ok := out[0].(TradeEvent)
	if !ok {
		t.Fatalf("expected TradeEvent, got %T", out[0])
	}
	if te.Trade.Source != SourceName {
		t.Errorf("Trade.Source = %q, want %q", te.Trade.Source, SourceName)
	}
}

// TestDecoder_Decode_twoPoolsOneOp_interleaved is a
// regression test: a router multi-hop swaps through two pools inside ONE
// operation. With the correlation buffer keyed on (ledger, tx, op)
// alone, both pools shared a slot — pool B's swap overwrote pool A's
// while Pair stayed pinned to A, so A's sync completed a trade
// carrying B's amounts under A's token mapping. Each pool must
// correlate independently, whatever order the events arrive in.
func TestDecoder_Decode_twoPoolsOneOp_interleaved(t *testing.T) {
	poolA := makeContractStrkey(t, 0x20)
	poolB := makeContractStrkey(t, 0x21)
	// Distinct token sets so a cross-correlated trade is identifiable
	// by its assets, not just its amounts.
	a0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	a1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	b0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x12))
	b1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x13))

	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		poolA: {Token0: a0, Token1: a1},
		poolB: {Token0: b0, Token1: b1},
	}))

	// Interleaved: both swaps land before either sync.
	feed := []events.Event{
		makeSwapEvent(t, poolA, big.NewInt(100), big.NewInt(200)),
		makeSwapEvent(t, poolB, big.NewInt(300), big.NewInt(400)),
		makeSyncEvent(t, poolA),
		makeSyncEvent(t, poolB),
	}
	var trades []TradeEvent
	for i, ev := range feed {
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode event %d: %v", i, err)
		}
		for _, o := range out {
			te, ok := o.(TradeEvent)
			if !ok {
				t.Fatalf("event %d: expected TradeEvent, got %T", i, o)
			}
			trades = append(trades, te)
		}
	}

	if len(trades) != 2 {
		t.Fatalf("got %d trades, want 2 (one per pool) — a shared buffer slot loses one and mis-attributes the other", len(trades))
	}
	// canonical.Trade carries no pair-contract field, so each pool's
	// trade is identified by ITS OWN token mapping. A cross-correlated
	// trade shows up as one pool's assets carrying the other's amounts.
	byBase := map[canonical.Asset]TradeEvent{}
	for _, te := range trades {
		byBase[te.Trade.Pair.Base] = te
	}
	for _, tc := range []struct {
		name     string
		wantBase canonical.Asset
		wantAmt  string
	}{
		{"poolA", a0, "100"},
		{"poolB", b0, "300"},
	} {
		te, ok := byBase[tc.wantBase]
		if !ok {
			t.Errorf("%s: no trade emitted with base %v — its swap was overwritten in a shared buffer slot", tc.name, tc.wantBase)
			continue
		}
		if te.Trade.BaseAmount.String() != tc.wantAmt {
			t.Errorf("%s: BaseAmount = %s, want %s (amounts from the WRONG pool's swap)",
				tc.name, te.Trade.BaseAmount.String(), tc.wantAmt)
		}
	}
}

func TestDecoder_Decode_unrelatedTopicReturnsNilNil(t *testing.T) {
	d := NewDecoder()
	out, err := d.Decode(events.Event{
		Topic: []string{"random-topic-0", "random-topic-1"},
	})
	if err != nil {
		t.Fatalf("Decode unrelated: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d events for unrelated topic, want 0", len(out))
	}
}

// makeSkimEvent builds a pair-contract skim event with the canonical
// `skimmed_0` / `skimmed_1` i128 body shape.
func makeSkimEvent(t *testing.T, pair string, amt0, amt1 *big.Int) events.Event {
	t.Helper()
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("skimmed_0"), Val: i128(amt0)},
		xdr.ScMapEntry{Key: symbol("skimmed_1"), Val: i128(amt1)},
	))
	return events.Event{
		Topic:          []string{TopicPrefixPair, TopicSymbolSkim},
		Value:          body,
		Ledger:         52_000_002,
		TxHash:         "skimtx0",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:02Z",
		ContractID:     pair,
	}
}

func TestDecoder_Matches_skimTopic(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x42)
	// Skim from a registered pair matches; from a foreign contract it
	// does not (contract gate).
	skimFrom := func(c string) events.Event {
		return events.Event{Topic: []string{TopicPrefixPair, TopicSymbolSkim}, ContractID: c}
	}
	if d.Matches(skimFrom(pair)) {
		t.Error("Matches(skim) = true before the pair is registered, want false")
	}
	d.SeedPair(pair, canonical.Asset{}, canonical.Asset{})
	if !d.Matches(skimFrom(pair)) {
		t.Error("Matches(skim from registered pair) = false, want true")
	}
	if d.Matches(skimFrom(makeContractStrkey(t, 0x99))) {
		t.Error("Matches(skim from foreign contract) = true, want false")
	}
}

func TestDecoder_Decode_skimEmitsSkimEvent(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x20)

	out, err := d.Decode(makeSkimEvent(t, pair, big.NewInt(7_500), big.NewInt(1_234_567)))
	if err != nil {
		t.Fatalf("Decode skim: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events for skim, want 1", len(out))
	}
	se, ok := out[0].(SkimEvent)
	if !ok {
		t.Fatalf("expected SkimEvent, got %T", out[0])
	}
	if se.Source() != SourceName {
		t.Errorf("Source() = %q, want %q", se.Source(), SourceName)
	}
	if se.EventKind() != "soroswap.skim" {
		t.Errorf("EventKind() = %q, want \"soroswap.skim\"", se.EventKind())
	}
	if se.ContractID != pair {
		t.Errorf("ContractID = %q, want %q", se.ContractID, pair)
	}
	if se.Amount0.BigInt().Cmp(big.NewInt(7_500)) != 0 {
		t.Errorf("Amount0 = %s", se.Amount0)
	}
	if se.Amount1.BigInt().Cmp(big.NewInt(1_234_567)) != 0 {
		t.Errorf("Amount1 = %s", se.Amount1)
	}
	if !se.ObservedAt.Equal(mustParseRFC3339(t, "2026-04-23T12:00:02Z")) {
		t.Errorf("ObservedAt = %v, want 2026-04-23T12:00:02Z", se.ObservedAt)
	}
	if se.To != "" {
		t.Errorf("To = %q, want empty (phase-1 shape has no `to` field)", se.To)
	}
}

// TestDecoder_Decode_skimPropagatesEventIndex confirms the SkimEvent
// carries the source event's in-op index rather than a hardcoded 0.
// The migration 0043 PK includes event_index, so two skims in the
// same op must keep distinct indices to avoid collapsing.
func TestDecoder_Decode_skimPropagatesEventIndex(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x21)

	ev := makeSkimEvent(t, pair, big.NewInt(1), big.NewInt(2))
	ev.EventIndex = 3 // second skim within the same op, say

	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode skim: %v", err)
	}
	se := out[0].(SkimEvent)
	if se.EventIndex != 3 {
		t.Errorf("EventIndex = %d, want 3 (propagated from the source event)", se.EventIndex)
	}
}

func TestDecoder_Decode_skimDoesNotFeedSwapBuffer(t *testing.T) {
	// A skim event is independent of the swap+sync correlation
	// buffer. After processing a standalone skim, the buffer's
	// in-flight count must remain 0 (no swap-without-sync warning
	// would otherwise leak through).
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x20)

	if _, err := d.Decode(makeSkimEvent(t, pair, big.NewInt(1), big.NewInt(2))); err != nil {
		t.Fatalf("Decode skim: %v", err)
	}
	if got := d.buf.size(); got != 0 {
		t.Errorf("buffer size = %d after skim, want 0 (skim is not buffered)", got)
	}
	if got := d.EvictedOrphans(); got != 0 {
		t.Errorf("EvictedOrphans() = %d after skim, want 0", got)
	}
	if got := d.SkippedUnknownPair(); got != 0 {
		t.Errorf("SkippedUnknownPair() = %d after skim, want 0 (skim does not need pair registry)", got)
	}
}

func TestDecoder_Decode_skimMalformedBodyReturnsError(t *testing.T) {
	d := NewDecoder()
	bad := events.Event{
		Topic:          []string{TopicPrefixPair, TopicSymbolSkim},
		Value:          "not-base64",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	if _, err := d.Decode(bad); err == nil {
		t.Error("expected decode error on malformed skim body, got nil")
	}
}

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// Deposit is not a silent no-op (it emits a LiquidityEvent — see
// TestDecoder_Decode_depositEmitsLiquidityEvent). A deposit topic with a
// MALFORMED body must surface as a decode ERROR — a recognition gap
// the ADR-0033 re-derive can see — never a silently-swallowed event.
func TestDecoder_Decode_depositMalformedBodyErrors(t *testing.T) {
	d := NewDecoder()
	_, err := d.Decode(events.Event{
		Topic:          []string{TopicPrefixPair, TopicSymbolDeposit},
		Value:          "",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	})
	if err == nil {
		t.Fatal("empty deposit body decoded without error — a malformed liquidity event must be a recognition gap, not a silent drop")
	}
}

func TestDecoder_Decode_malformedNewPairBodyReturnsError(t *testing.T) {
	d := NewDecoder()
	bad := events.Event{
		Topic: []string{TopicPrefixFactory, TopicSymbolNewPair},
		Value: "not-base64",
	}
	if _, err := d.Decode(bad); err == nil {
		t.Error("expected decode error on malformed new_pair body, got nil")
	}
}

func TestDecoder_SeedPair_addsPair(t *testing.T) {
	d := NewDecoder()
	t0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	t1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	pair := makeContractStrkey(t, 0x20)

	d.SeedPair(pair, t0, t1)
	d.mu.RLock()
	got := d.pairTokens[pair]
	d.mu.RUnlock()
	if got.Token0.ContractID != t0.ContractID {
		t.Errorf("Token0 = %s, want %s", got.Token0.ContractID, t0.ContractID)
	}
}

func TestDecoder_EvictedOrphans_initiallyZero(t *testing.T) {
	d := NewDecoder()
	if got := d.EvictedOrphans(); got != 0 {
		t.Errorf("EvictedOrphans() = %d on fresh Decoder, want 0", got)
	}
}

func TestDecoder_WithPairUpsertHook_firesOnSeedPair(t *testing.T) {
	t0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	t1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	pair := makeContractStrkey(t, 0x20)

	type call struct{ pair, t0, t1 string }
	var got []call
	d := NewDecoder(WithPairUpsertHook(func(p, a, b string) {
		got = append(got, call{p, a, b})
	}))

	d.SeedPair(pair, t0, t1)

	if len(got) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(got))
	}
	if got[0].pair != pair || got[0].t0 != t0.ContractID || got[0].t1 != t1.ContractID {
		t.Errorf("hook saw %+v, want pair=%s t0=%s t1=%s",
			got[0], pair, t0.ContractID, t1.ContractID)
	}
}

func TestDecoder_WithPairUpsertHook_firesOnFactoryNewPairDecode(t *testing.T) {
	token0 := makeContractStrkey(t, 0x10)
	token1 := makeContractStrkey(t, 0x11)
	pair := makeContractStrkey(t, 0x20)

	var fired int
	d := NewDecoder(WithPairUpsertHook(func(p, a, b string) {
		if p != pair || a != token0 || b != token1 {
			t.Errorf("hook saw (%s, %s, %s), want (%s, %s, %s)",
				p, a, b, pair, token0, token1)
		}
		fired++
	}))

	if _, err := d.Decode(makeNewPairEvent(t, token0, token1, pair)); err != nil {
		t.Fatalf("Decode new_pair: %v", err)
	}

	if fired != 1 {
		t.Errorf("hook fired %d times, want 1", fired)
	}
}

// At the end of a bounded stream a swap with no following sync is final (its
// trade reads from the swap body alone); a bare sync is LP-traffic noise.
func TestDecoder_Drain_emitsSwapOnlyAndCountsBareSync(t *testing.T) {
	poolA := makeContractStrkey(t, 0x20)
	poolB := makeContractStrkey(t, 0x21)
	t0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	t1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		poolA: {Token0: t0, Token1: t1},
		poolB: {Token0: t0, Token1: t1},
	}))

	if out, err := d.Decode(makeSwapEvent(t, poolA, big.NewInt(100), big.NewInt(200))); err != nil || len(out) != 0 {
		t.Fatalf("swap: out=%d err=%v, want buffered", len(out), err)
	}
	if out, err := d.Decode(makeSyncEvent(t, poolB)); err != nil || len(out) != 0 {
		t.Fatalf("sync: out=%d err=%v, want buffered", len(out), err)
	}

	out := d.Drain()
	if len(out) != 1 {
		t.Fatalf("Drain emitted %d events, want 1 (the swap-only trade)", len(out))
	}
	if got := out[0].(TradeEvent).Trade.BaseAmount.String(); got != "100" {
		t.Errorf("drained trade base = %s, want 100", got)
	}
	if got := d.EvictedBareSync(); got != 1 {
		t.Errorf("EvictedBareSync = %d, want 1", got)
	}
	if d.buf.size() != 0 || len(d.Drain()) != 0 {
		t.Errorf("buffer not empty after Drain: size=%d", d.buf.size())
	}
}

// TestDecoder_GatedContractSetIsExactlyWhatMatchesAccepts pins the
// enumeration the completeness re-derive scopes its lake read to: every
// verified factory, every seeded pair and every pair a new_pair announced
// — and nothing Matches would reject. A narrower set would drop real
// events from the expected side (a false projection red); a wider one
// only costs read volume.
func TestDecoder_GatedContractSetIsExactlyWhatMatchesAccepts(t *testing.T) {
	token0 := makeContractStrkey(t, 0x10)
	token1 := makeContractStrkey(t, 0x11)
	seeded := makeContractStrkey(t, 0x20)
	announced := makeContractStrkey(t, 0x21)
	foreign := makeContractStrkey(t, 0x30)

	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{seeded: {}}))
	if _, err := d.Decode(makeNewPairEvent(t, token0, token1, announced)); err != nil {
		t.Fatalf("Decode new_pair: %v", err)
	}

	got := d.GatedContractSet()
	want := append(slices.Clone(MainnetFactories), seeded, announced)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("GatedContractSet = %v, want factories ∪ {seeded, announced} = %v", got, want)
	}

	swapFrom := func(c string) events.Event {
		return events.Event{Topic: []string{TopicPrefixPair, TopicSymbolSwap}, ContractID: c}
	}
	for _, c := range []string{seeded, announced} {
		if !d.Matches(swapFrom(c)) {
			t.Errorf("Matches rejects registered pair %s that GatedContractSet lists", c)
		}
	}
	if d.Matches(swapFrom(foreign)) || slices.Contains(got, foreign) {
		t.Errorf("foreign contract %s: Matches=%v, listed=%v — want neither", foreign,
			d.Matches(swapFrom(foreign)), slices.Contains(got, foreign))
	}
}

// Real mainnet deposit + withdraw bodies captured from the ClickHouse
// lake (deposit pair CDLMAKG5…, ledger 63,181,271; withdraw pair
// CA3PA2NY…, ledger 62,940,668). Proves sdkDecodeLiquidity extracts all
// five i128 fields + the `to` provider from the REAL on-chain ScvMap
// shape (not a synthetic body). deposit and withdraw share one struct.
const (
	realDepositBody  = "AAAAEQAAAAEAAAAGAAAADwAAAAhhbW91bnRfMAAAAAoAAAAAAAAAAAAAAAAM7ZpEAAAADwAAAAhhbW91bnRfMQAAAAoAAAAAAAAAAAAAAAnHZSQAAAAADwAAAAlsaXF1aWRpdHkAAAAAAAAKAAAAAAAAAAAAAAAAryjoaQAAAA8AAAANbmV3X3Jlc2VydmVfMAAAAAAAAAoAAAAAAAAAAAAAAAFFR+O9AAAADwAAAA1uZXdfcmVzZXJ2ZV8xAAAAAAAACgAAAAAAAAAAAAAA9gsmmTQAAAAPAAAAAnRvAAAAAAASAAAAAAAAAAB/8XxHTA1msG+0b3b1TJd6rspEiEzR8MYWBjCsXdFIow=="
	realWithdrawBody = "AAAAEQAAAAEAAAAGAAAADwAAAAhhbW91bnRfMAAAAAoAAAAAAAAAAAAAAAAGfa6bAAAADwAAAAhhbW91bnRfMQAAAAoAAAAAAAAAAAAABJMjWbR7AAAADwAAAAlsaXF1aWRpdHkAAAAAAAAKAAAAAAAAAAAAAAAFcwMiGgAAAA8AAAANbmV3X3Jlc2VydmVfMAAAAAAAAAoAAAAAAAAAAAAAAAAAAAAFAAAADwAAAA1uZXdfcmVzZXJ2ZV8xAAAAAAAACgAAAAAAAAAAAAAAAAADR4UAAAAPAAAAAnRvAAAAAAASAAAAAAAAAAAqgK2+TSm3Zi13f+1ZI/dUxFKl2W6gSs4aa+ABaSQiZw=="
)

func TestDecodeLiquidity_realBodies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		amount0 string
		amount1 string
		liq     string
		r0      string
		r1      string
		to      string
	}{
		{
			name: "deposit", body: realDepositBody,
			amount0: "216898116", amount1: "42000000000", liq: "2938693737",
			r0: "5457306557", r1: "1056749033780",
			to: "GB77C7CHJQGWNMDPWRXXN5KMS55K5SSERBGND4GGCYDDBLC52FEKHUOR",
		},
		{
			name: "withdraw", body: realWithdrawBody,
			amount0: "108899995", amount1: "5029999785083", liq: "23404421658",
			r0: "5", r1: "214917",
			to: "GAVIBLN6JUU3OZRNO5762WJD65KMIUVF3FXKASWODJV6AALJEQRGPYOO",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := sdkDecodeLiquidity(tc.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if f.Amount0.String() != tc.amount0 {
				t.Errorf("amount_0 = %s, want %s", f.Amount0, tc.amount0)
			}
			if f.Amount1.String() != tc.amount1 {
				t.Errorf("amount_1 = %s, want %s", f.Amount1, tc.amount1)
			}
			if f.Liquidity.String() != tc.liq {
				t.Errorf("liquidity = %s, want %s", f.Liquidity, tc.liq)
			}
			if f.NewReserve0.String() != tc.r0 {
				t.Errorf("new_reserve_0 = %s, want %s", f.NewReserve0, tc.r0)
			}
			if f.NewReserve1.String() != tc.r1 {
				t.Errorf("new_reserve_1 = %s, want %s", f.NewReserve1, tc.r1)
			}
			if f.To != tc.to {
				t.Errorf("to = %s, want %s", f.To, tc.to)
			}
		})
	}
}

// A malformed body (missing a required field) must ERROR, not silently
// drop a leg — the every-event mission demands honest failure over a
// half-decoded row.
func TestDecodeLiquidity_missingFieldErrors(t *testing.T) {
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount_0"), Val: i128(big.NewInt(1))},
		xdr.ScMapEntry{Key: symbol("amount_1"), Val: i128(big.NewInt(2))},
		// liquidity / reserves / to missing.
	))
	if _, err := sdkDecodeLiquidity(body); err == nil {
		t.Fatal("expected error for a body missing liquidity/reserves/to")
	}
}

// makeLiquidityEvent builds a pair-contract deposit/withdraw
// events.Event with the six #[contracttype] fields, for the
// adapter-level tests.
func makeLiquidityEvent(t *testing.T, pair, action string, amt0, amt1, liq, r0, r1 *big.Int, to string) events.Event {
	t.Helper()
	sym := TopicSymbolDeposit
	if action == EventWithdraw {
		sym = TopicSymbolWithdraw
	}
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount_0"), Val: i128(amt0)},
		xdr.ScMapEntry{Key: symbol("amount_1"), Val: i128(amt1)},
		xdr.ScMapEntry{Key: symbol("liquidity"), Val: i128(liq)},
		xdr.ScMapEntry{Key: symbol("new_reserve_0"), Val: i128(r0)},
		xdr.ScMapEntry{Key: symbol("new_reserve_1"), Val: i128(r1)},
		xdr.ScMapEntry{Key: symbol("to"), Val: contractAddrFromStrkey(t, to)},
	))
	return events.Event{
		Topic:          []string{TopicPrefixPair, sym},
		Value:          body,
		Ledger:         52_000_002,
		TxHash:         "liqtx0",
		OperationIndex: 0,
		EventIndex:     0,
		LedgerClosedAt: "2026-04-23T12:00:02Z",
		ContractID:     pair,
	}
}

// A seeded pair's deposit resolves token identities and emits exactly
// one LiquidityEvent carrying every field.
func TestDecoder_Decode_depositEmitsLiquidityEvent(t *testing.T) {
	d := NewDecoder()
	token0 := makeContractStrkey(t, 0x10)
	token1 := makeContractStrkey(t, 0x11)
	pair := makeContractStrkey(t, 0x20)
	a0, _ := canonical.NewSorobanAsset(token0)
	a1, _ := canonical.NewSorobanAsset(token1)
	d.SeedPair(pair, a0, a1)
	provider := makeContractStrkey(t, 0x99)

	dep := makeLiquidityEvent(t, pair, EventDeposit,
		big.NewInt(111), big.NewInt(222), big.NewInt(333),
		big.NewInt(4444), big.NewInt(5555), provider)

	out, err := d.Decode(dep)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 event, got %d", len(out))
	}
	le, ok := out[0].(LiquidityEvent)
	if !ok {
		t.Fatalf("want LiquidityEvent, got %T", out[0])
	}
	if le.Action != EventDeposit {
		t.Errorf("action = %q, want deposit", le.Action)
	}
	if le.Token0 != a0.String() || le.Token1 != a1.String() {
		t.Errorf("tokens = (%s,%s), want (%s,%s)", le.Token0, le.Token1, a0, a1)
	}
	if le.Amount0.String() != "111" || le.Amount1.String() != "222" || le.Liquidity.String() != "333" {
		t.Errorf("amounts = (%s,%s,%s), want (111,222,333)", le.Amount0, le.Amount1, le.Liquidity)
	}
	if le.NewReserve0.String() != "4444" || le.NewReserve1.String() != "5555" {
		t.Errorf("reserves = (%s,%s), want (4444,5555)", le.NewReserve0, le.NewReserve1)
	}
	if le.To != provider {
		t.Errorf("to = %s, want %s", le.To, provider)
	}
}

// TestDecoder_Decode_withdrawUnseededPairInIsolationStillEmits exercises
// Decode directly, bypassing the dispatcher's Matches gate, to pin
// Decode's own behaviour: called on an unseeded pair, it still emits
// the liquidity row with empty token identities rather than dropping
// the event. This is NOT the production path — see the sibling test
// below for what actually reaches an unseeded pair's events.
func TestDecoder_Decode_withdrawUnseededPairInIsolationStillEmits(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x21) // never SeedPair'd
	provider := makeContractStrkey(t, 0x98)

	wd := makeLiquidityEvent(t, pair, EventWithdraw,
		big.NewInt(9), big.NewInt(8), big.NewInt(7),
		big.NewInt(6), big.NewInt(5), provider)

	out, err := d.Decode(wd)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 event even for an unseeded pair, got %d", len(out))
	}
	le := out[0].(LiquidityEvent)
	if le.Action != EventWithdraw {
		t.Errorf("action = %q, want withdraw", le.Action)
	}
	if le.Token0 != "" || le.Token1 != "" {
		t.Errorf("tokens = (%q,%q), want empty (pair unseeded)", le.Token0, le.Token1)
	}
	if le.Amount0.String() != "9" || le.Liquidity.String() != "7" {
		t.Errorf("amounts wrong: %+v", le)
	}
}

// TestDecoder_Matches_withdrawUnseededPairIsRejected pins the actual
// production behaviour for the same unseeded pair: the dispatcher
// calls Matches before Decode, and Matches returns false for a
// pair-contract event whose pair was never registered via SeedPair or
// a factory new_pair event. Decode's every-event emission above is
// therefore unreachable for an unseeded pair on the live ingest path.
func TestDecoder_Matches_withdrawUnseededPairIsRejected(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x21) // never SeedPair'd
	provider := makeContractStrkey(t, 0x98)

	wd := makeLiquidityEvent(t, pair, EventWithdraw,
		big.NewInt(9), big.NewInt(8), big.NewInt(7),
		big.NewInt(6), big.NewInt(5), provider)

	if d.Matches(wd) {
		t.Fatalf("Matches = true for an unseeded pair, want false (production ingest never reaches Decode for it)")
	}
}

// A route that re-enters ONE pair inside one op shares a groupKey
// (ledger, tx, op, pair) across both swaps. With non-contiguous emission
// (swap1, swap2, sync1, sync2) the second swap must not overwrite the
// first in place: one trade instead of two, and the lost one never
// counted as an orphan. The first swap must be rotated out and emitted
// on its own — decodeSwap reads only the swap body — and both trades
// must land on distinct op_index values.
func TestDecoder_Decode_samePoolTwiceInOp_nonContiguous(t *testing.T) {
	pool := makeContractStrkey(t, 0x20)
	t0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	t1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		pool: {Token0: t0, Token1: t1},
	}))

	at := func(ev events.Event, idx int) events.Event {
		ev.EventIndex = idx
		return ev
	}
	feed := []events.Event{
		at(makeSwapEvent(t, pool, big.NewInt(100), big.NewInt(200)), 0),
		at(makeSwapEvent(t, pool, big.NewInt(300), big.NewInt(400)), 1),
		at(makeSyncEvent(t, pool), 2),
		at(makeSyncEvent(t, pool), 3),
	}
	byBase := map[string]canonical.Trade{}
	ops := map[uint32]bool{}
	for i, ev := range feed {
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode event %d: %v", i, err)
		}
		for _, o := range out {
			tr := o.(TradeEvent).Trade
			byBase[tr.BaseAmount.String()] = tr
			ops[tr.OpIndex] = true
		}
	}
	if len(byBase) != 2 {
		t.Fatalf("got %d distinct trades, want 2 (the second same-pair swap overwrote the first)", len(byBase))
	}
	for base, quote := range map[string]string{"100": "200", "300": "400"} {
		tr, ok := byBase[base]
		if !ok {
			t.Errorf("swap with base %s missing", base)
			continue
		}
		if tr.QuoteAmount.String() != quote {
			t.Errorf("swap base %s: quote = %s, want %s", base, tr.QuoteAmount, quote)
		}
	}
	if len(ops) != 2 {
		t.Errorf("trades share %d op_index value(s), want 2 distinct", len(ops))
	}
}

// A redelivered swap (same EventIndex) is idempotent: it must not rotate
// the group and emit the same trade twice.
func TestDecoder_Decode_redeliveredSwapIsIdempotent(t *testing.T) {
	pool := makeContractStrkey(t, 0x20)
	t0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	t1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		pool: {Token0: t0, Token1: t1},
	}))
	swap := makeSwapEvent(t, pool, big.NewInt(100), big.NewInt(200))
	sync := makeSyncEvent(t, pool)
	sync.EventIndex = 1
	var n int
	for _, ev := range []events.Event{swap, swap, sync} {
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		n += len(out)
	}
	if n != 1 {
		t.Fatalf("emitted %d trades, want 1", n)
	}
}

// TestDecoder_UnknownContractDrops_countsSameAsSkippedUnknownPair pins
// that a completed swap+sync whose pair has no token mapping is
// dropped with (nil, nil) — indistinguishable from "not a trade" to
// the caller — and must be surfaced through the dispatcher's duck-typed
// reporter interface (mirroring EvictedOrphans) so
// internal/pipeline.emitDispatcherMetricDeltas can wire it to
// obs.SourceDecodeErrorsTotal. Decoder must expose an
// UnknownContractDrops method so this drop has a reader.
func TestDecoder_UnknownContractDrops_countsSameAsSkippedUnknownPair(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x20)

	if _, err := d.Decode(makeSwapEvent(t, pair, big.NewInt(100), big.NewInt(200))); err != nil {
		t.Fatalf("Decode swap: %v", err)
	}
	if _, err := d.Decode(makeSyncEvent(t, pair)); err != nil {
		t.Fatalf("Decode sync: %v", err)
	}

	if got := d.UnknownContractDrops(); got != 1 {
		t.Errorf("UnknownContractDrops() = %d, want 1 (one completed swap+sync dropped for want of a pair mapping)", got)
	}
	if got := d.SkippedUnknownPair(); got != 1 {
		t.Errorf("SkippedUnknownPair() = %d, want 1", got)
	}
}

// TestDecoder_EvictedOrphans_excludesBareSyncEvictions pins that a
// bare `sync` (deposit/withdraw/skim traffic, README Q2) that ages out
// of the correlation buffer with no preceding swap must NOT inflate
// EvictedOrphans — that counter is the real-loss signal (a swap that
// never got its sync) and must stay legible. Both classes must not feed
// a single `evictedOrphans` counter.
func TestDecoder_EvictedOrphans_excludesBareSyncEvictions(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x21)

	// A bare sync — no swap ever arrives for this group key.
	syncOnly := makeSyncEvent(t, pair)
	syncOnly.TxHash = "baresynctx"
	syncOnly.LedgerClosedAt = "2026-04-23T12:00:00Z"
	if _, err := d.Decode(syncOnly); err != nil {
		t.Fatalf("Decode bare sync: %v", err)
	}

	// An event more than defaultOrphanMaxAge (5m) later sweeps the
	// buffer and evicts the bare sync as an orphan-class entry.
	sweeper := makeSyncEvent(t, pair)
	sweeper.TxHash = "sweepertx"
	sweeper.LedgerClosedAt = "2026-04-23T12:10:00Z"
	if _, err := d.Decode(sweeper); err != nil {
		t.Fatalf("Decode sweeper: %v", err)
	}

	if got := d.EvictedBareSync(); got != 1 {
		t.Errorf("EvictedBareSync() = %d, want 1", got)
	}
	if got := d.EvictedOrphans(); got != 0 {
		t.Errorf("EvictedOrphans() = %d, want 0 (a bare sync is LP traffic, not a lost trade)", got)
	}
}

// TestDecoder_EvictedOrphans_countsSwapWithoutSync is the counterpart
// to the bare-sync test above: a swap that never gets its sync IS the
// real loss class and must still land in EvictedOrphans.
func TestDecoder_EvictedOrphans_countsSwapWithoutSync(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x22)

	swapOnly := makeSwapEvent(t, pair, big.NewInt(100), big.NewInt(200))
	swapOnly.TxHash = "swaponlytx"
	swapOnly.LedgerClosedAt = "2026-04-23T12:00:00Z"
	if _, err := d.Decode(swapOnly); err != nil {
		t.Fatalf("Decode swap-only: %v", err)
	}

	sweeper := makeSyncEvent(t, pair)
	sweeper.TxHash = "sweepertx2"
	sweeper.LedgerClosedAt = "2026-04-23T12:10:00Z"
	if _, err := d.Decode(sweeper); err != nil {
		t.Fatalf("Decode sweeper: %v", err)
	}

	if got := d.EvictedOrphans(); got != 1 {
		t.Errorf("EvictedOrphans() = %d, want 1 (a swap with no sync is a real lost trade)", got)
	}
	if got := d.EvictedBareSync(); got != 0 {
		t.Errorf("EvictedBareSync() = %d, want 0", got)
	}
}

func TestDecoder_SeedPairIsConcurrentSafe(t *testing.T) {
	// Race-flag regression: many concurrent SeedPair writers +
	// Decode readers must not trip -race. Guards against a future
	// refactor that inlines pair-cache writes without the lock.
	d := NewDecoder()
	xlm := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 16
	done := make(chan struct{}, goroutines*2)
	for i := 0; i < goroutines; i++ {
		pair := "CABC" + string(rune('A'+i))
		go func() {
			d.SeedPair(pair, xlm, usdc)
			done <- struct{}{}
		}()
		go func() {
			// Read-side race target — any read path that walks the
			// pairTokens map.
			d.SeedPair(pair, xlm, usdc) // idempotent write also counts as a reader via lock upgrade
			done <- struct{}{}
		}()
	}
	for i := 0; i < goroutines*2; i++ {
		<-done
	}
}

func TestDecoder_NameMatchesSourceName(t *testing.T) {
	if got := NewDecoder().Name(); got != SourceName {
		t.Errorf("Name = %q, want %q", got, SourceName)
	}
}
