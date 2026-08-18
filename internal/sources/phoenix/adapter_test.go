package phoenix

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── consumer.go ──────────────────────────────────────────────────

func TestTradeEvent_implementsConsumerEvent(t *testing.T) {
	te := TradeEvent{}
	if got := te.EventKind(); got != "phoenix.trade" {
		t.Errorf("EventKind() = %q, want \"phoenix.trade\"", got)
	}
	if got := te.Source(); got != SourceName {
		t.Errorf("Source() = %q, want %q", got, SourceName)
	}
	var _ consumer.Event = te
}

// ─── dispatcher_adapter.go ────────────────────────────────────────

func TestDecoder_Name(t *testing.T) {
	if got := newTestDecoder().Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestDecoder_Matches_swapTopic(t *testing.T) {
	d := newTestDecoder()
	good := events.Event{
		ContractID: "pool-A",
		Topic:      []string{TopicSymbolSwap, TopicSymbolSender},
	}
	if !d.Matches(good) {
		t.Error("Matches((swap, sender)) = false, want true")
	}
	bad := events.Event{
		ContractID: "pool-A",
		Topic:      []string{TopicSymbolSender, TopicSymbolSwap},
	}
	if d.Matches(bad) {
		t.Error("Matches((sender, swap)) = true, want false (wrong topic order)")
	}
	empty := events.Event{Topic: nil}
	if d.Matches(empty) {
		t.Error("Matches(empty topic) = true, want false")
	}
}

// makeFieldEvent builds one of the 8 swap-field events under a
// shared (ledger, txHash, opIndex) so the buffer groups them as
// one swap.
func makeFieldEvent(t *testing.T, fieldTopic, body string) events.Event {
	t.Helper()
	return events.Event{
		Topic:          []string{TopicSymbolSwap, fieldTopic},
		Value:          body,
		Ledger:         1_500_000,
		TxHash:         "phoenixtx0",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
		ContractID:     "C-pool-strkey",
	}
}

func TestDecoder_Decode_completesAfterEighthField(t *testing.T) {
	// Feed 7 distinct field events; each should return (nil, nil)
	// (still buffering). The 8th completes the swap and emits one
	// TradeEvent.
	d := newTestDecoder()

	sellToken := makeC(t, 0x20)
	buyToken := makeC(t, 0x30)
	sender := makeC(t, 0x10)
	offer := big.NewInt(1_000_000)          // base sold (input)
	returnAmt := big.NewInt(2_000_000)      // buy_token received (output → QuoteAmount)
	actualReceived := big.NewInt(1_000_000) // == offer: the INPUT the pool received (Q3)

	// Map: fieldTopic → body. SpreadAmount/ReferralFee/ActualReceived
	// must be valid i128 even though decodeSwap doesn't read them for
	// the amounts — the buffer's Complete() check requires all 8 slots.
	zeroI128 := i128Body(t, big.NewInt(0))
	fields := []struct{ topic, body string }{
		{TopicSymbolSender, addrBody(t, sender)},
		{TopicSymbolSellToken, addrBody(t, sellToken)},
		{TopicSymbolOfferAmount, i128Body(t, offer)},
		{TopicSymbolActualReceived, i128Body(t, actualReceived)},
		{TopicSymbolBuyToken, addrBody(t, buyToken)},
		{TopicSymbolReturnAmount, i128Body(t, returnAmt)},
		{TopicSymbolSpreadAmount, zeroI128},
		{TopicSymbolReferralFee, zeroI128},
	}

	for i, f := range fields {
		out, err := d.Decode(makeFieldEvent(t, f.topic, f.body))
		if err != nil {
			t.Fatalf("field %d (%s): unexpected error: %v", i, f.topic, err)
		}
		if i < 7 {
			if len(out) != 0 {
				t.Errorf("field %d (%s): got %d events, want 0 (still buffering)", i, f.topic, len(out))
			}
		} else {
			if len(out) != 1 {
				t.Fatalf("field 7 (%s): got %d events, want 1", f.topic, len(out))
			}
			te, ok := out[0].(TradeEvent)
			if !ok {
				t.Fatalf("expected TradeEvent, got %T", out[0])
			}
			if te.Trade.Source != SourceName {
				t.Errorf("Trade.Source = %q, want %q", te.Trade.Source, SourceName)
			}
			if te.Trade.BaseAmount.BigInt().Cmp(offer) != 0 {
				t.Errorf("BaseAmount = %s, want %s", te.Trade.BaseAmount, offer)
			}
			// QuoteAmount is return_amount (output), NOT actual_received
			// (== offer). base==quote was the Phoenix pricing bug.
			if te.Trade.QuoteAmount.BigInt().Cmp(returnAmt) != 0 {
				t.Errorf("QuoteAmount = %s, want return_amount %s", te.Trade.QuoteAmount, returnAmt)
			}
		}
	}
}

func TestDecoder_Decode_fieldDecodeErrorPropagates(t *testing.T) {
	// Send a known-good Sender event followed by a malformed
	// SellToken (non-base64 body). Decode of the malformed one must
	// fail — buffer absorption signals the error to the caller.
	d := newTestDecoder()
	d.Decode(makeFieldEvent(t, TopicSymbolSender, addrBody(t, makeC(t, 0x10))))
	// SellToken with a body the buffer's assign will accept structurally
	// but that decodeSwap can't parse — check error fan-out via decodeSwap.
	// Easier: send all 8 fields with the OfferAmount body NON-i128.
	d2 := newTestDecoder()
	bad := []struct{ topic, body string }{
		{TopicSymbolSender, addrBody(t, makeC(t, 0x10))},
		{TopicSymbolSellToken, addrBody(t, makeC(t, 0x20))},
		// OfferAmount carrying an Address body — decodeSwap will reject.
		{TopicSymbolOfferAmount, addrBody(t, makeC(t, 0x40))},
		{TopicSymbolActualReceived, i128Body(t, big.NewInt(1))},
		{TopicSymbolBuyToken, addrBody(t, makeC(t, 0x30))},
		{TopicSymbolReturnAmount, i128Body(t, big.NewInt(0))},
		{TopicSymbolSpreadAmount, i128Body(t, big.NewInt(0))},
		{TopicSymbolReferralFee, i128Body(t, big.NewInt(0))},
	}
	var lastErr error
	for _, f := range bad {
		_, err := d2.Decode(makeFieldEvent(t, f.topic, f.body))
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		t.Error("expected decodeSwap error on malformed OfferAmount, got none")
	}
}

func TestDecoder_EvictedOrphans_initiallyZero(t *testing.T) {
	d := newTestDecoder()
	if got := d.EvictedOrphans(); got != 0 {
		t.Errorf("EvictedOrphans() = %d on a fresh Decoder, want 0", got)
	}
}

func TestDecoder_EvictedOrphans_incrementsOnStaleEviction(t *testing.T) {
	// Drive the buffer's age-out by feeding two events whose
	// ClosedAt are >5 min apart. The first sits in the buffer
	// alone; the second's sweepStale should evict it.
	d := newTestDecoder()

	// Event 1: t0, only sender, partial buffer entry.
	evOld := events.Event{
		Topic:          []string{TopicSymbolSwap, TopicSymbolSender},
		Value:          addrBody(t, makeC(t, 0x10)),
		Ledger:         1_500_000,
		TxHash:         "old-tx",
		OperationIndex: 0,
		LedgerClosedAt: "2026-01-01T00:00:00Z",
		ContractID:     "pool-A",
	}
	if _, err := d.Decode(evOld); err != nil {
		t.Fatalf("Decode evOld: %v", err)
	}

	// Event 2: t0 + 10min, distinct group_key — sweepStale runs and
	// evicts evOld since it's now > 5 min stale.
	evNew := events.Event{
		Topic:          []string{TopicSymbolSwap, TopicSymbolSender},
		Value:          addrBody(t, makeC(t, 0x10)),
		Ledger:         1_500_001, // different ledger ⇒ different groupKey
		TxHash:         "new-tx",
		OperationIndex: 0,
		LedgerClosedAt: "2026-01-01T00:10:00Z",
		ContractID:     "pool-A",
	}
	if _, err := d.Decode(evNew); err != nil {
		t.Fatalf("Decode evNew: %v", err)
	}

	if got := d.EvictedOrphans(); got != 1 {
		t.Errorf("EvictedOrphans() = %d, want 1 (the t0 entry should have aged out)", got)
	}
}

// newTestDecoder returns a Decoder whose gate is seeded with the
// suite's synthetic fixture contract ids (the production curated set
// is real mainnet ids). Gating behavior itself is pinned by
// TestDecoder_GateRejectsForeignContract.
func newTestDecoder() *Decoder {
	return NewDecoder(contractid.WithSeed([]string{
		"C-pool-strkey", "pool-A",
		usdcContract, plPool, wlPool,
	}))
}

// TestDecoder_GateRejectsForeignContract pins ADR-0035/0040 (CS-026):
// phoenix topics are plain string tuples ANY pubnet contract can
// emit — a perfect topic shape from an unregistered contract must
// NOT be attributed to phoenix, while the same event from a curated
// mainnet pool must.
func TestDecoder_GateRejectsForeignContract(t *testing.T) {
	d := NewDecoder() // production gate: curated mainnet set only
	topics := []string{TopicSymbolSwap, TopicSymbolSender}

	foreign := events.Event{
		ContractID: "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000",
		Topic:      topics,
	}
	if d.Matches(foreign) {
		t.Fatal("foreign contract with phoenix-shaped topics matched — the CS-026 injection vector is open")
	}

	genuine := events.Event{ContractID: MainnetPools[0], Topic: topics}
	if !d.Matches(genuine) {
		t.Fatal("curated mainnet pool failed to match — gate is over-closed")
	}
	stake := events.Event{ContractID: MainnetStakeContracts[0], Topic: []string{TopicSymbolBond, TopicSymbolStakeUser}}
	if !d.Matches(stake) {
		t.Fatal("curated stake contract failed to match")
	}
}

// TestGatedSet_SeedsVerifiedCompletenessContracts pins the 2026-08-18
// phoenix projection-completeness seed additions: the legacy XYK pool
// CAZ6W4WH… and the 13 per-pool stake contracts VERIFIED genuine against
// the r1 lake (factory pool-create co-occurrence for the pool + 11
// stakes; the phoenix reward keeper CBZ7M5B3… + phoenix-stake v1.1
// migration events for the two pre-lake stakes — evidence in events.go).
// Each must be in MainnetGatedSet() AND a bare PRODUCTION Decoder must
// attribute a genuine phoenix event from it — otherwise the gated
// re-derive scores its served rows expected=0 again (the 455
// phoenix_liquidity + 2,513 phoenix_stake_events rows the gate dropped,
// plus still-live emissions to ledger ~64M). Addresses are spelled
// literally so the test cannot agree with a reverted or mistyped seed.
func TestGatedSet_SeedsVerifiedCompletenessContracts(t *testing.T) {
	const newPool = "CAZ6W4WHVGQBGURYTUOLCUOOHW6VQGAAPSPCD72VEDZMBBPY7H43AYEC"
	newStakes := []string{
		"CABWEFVXUB3XWYPTWFETEGJR2WRGE2ZKYYLZDLV3EBUVFMOU4ENK4DJC",
		"CAIR3UPW2PEP27QZWX4XGMO65W6LJ3XCRA3F5G7Z3D52MNOVF5K5YZ56",
		"CDP6DT2YU75ZMOPTTCQ563H2XZDDWHPWKRQ6N2W5LNVE5HHRSB4MMRNQ",
		"CB2S5X4H6ZMMCDQV4DNKEO2SBSW7T2YXVN5A7G2BBSN3VM73CQYIIZ3C",
		"CCP653KENMYCAYQ3PHJDT6PITMG4XYKVWV3OEDDCOAOS6Z4GOMXGYH3Z",
		"CCIWIW6ESCCCFMEI5QOSUHDKTMBEMRJ22F7GPYNRKM2UI2FH6WYUKOUU",
		"CBULEXIMZ5C4CSUPZ4E5LXATWDZNS6MDM2A57DAUD5GXSUG4IWKLOSOC",
		"CD2YKNPX3JPTGDANJRPEJS42MPQLEVUVVRZKJYLLUSPJKQJA7LUANBO4",
		"CDBMVFP7KJXW3YEFSLOU5GYUQHHJJI7QPZJPCSPDK6HHBCBZAMCHS2QY",
		"CDH6JILIADIC5SKE6OZJAYV3GM62RTR4O54OMVNP4ZOK4HH4J2JWJPVW",
		"CBDCTYZSZIOWCK5IGCQZNFUOJ53KMPYG2MG7GMVGE3A2LEYCFTDYYZ3S",
		"CDOXQONPND365K6MHR3QBSVVTC3MKR44ORK6TI2GQXUXGGAS5SNDAYRI",
		"CDEQYRWFU3IHPRR6H6VOQRUU3JFS6DTUYUL4YAQSD3ALB5IPBTEOZUFM",
	}
	if len(newStakes) != 13 {
		t.Fatalf("expected 13 newly-seeded stake contracts, listed %d", len(newStakes))
	}

	gated := make(map[string]bool)
	for _, c := range MainnetGatedSet() {
		gated[c] = true
	}
	if !gated[newPool] {
		t.Errorf("new pool %s absent from MainnetGatedSet() — its 455 phoenix_liquidity rows re-derive as expected=0", newPool)
	}
	for _, s := range newStakes {
		if !gated[s] {
			t.Errorf("new stake contract %s absent from MainnetGatedSet()", s)
		}
	}

	// Membership in the slice is necessary but not sufficient — Matches()
	// on a bare PRODUCTION decoder (the exact gate the reconcile re-derive
	// and the live pipeline both build) is what actually attributes an
	// event to phoenix.
	d := NewDecoder()
	if !d.Matches(events.Event{ContractID: newPool, Topic: []string{TopicSymbolProvideLiquidity, TopicSymbolPLSender}}) {
		t.Errorf("production gate rejects provide_liquidity from seeded pool %s", newPool)
	}
	for _, s := range newStakes {
		if !d.Matches(events.Event{ContractID: s, Topic: []string{TopicSymbolBond, TopicSymbolStakeUser}}) {
			t.Errorf("production gate rejects bond from seeded stake contract %s", s)
		}
	}

	// Non-vacuity: the SAME phoenix-shaped topics from an UNSEEDED
	// contract must NOT match — proving the assertions above exercise the
	// gate, not a decoder that attributes every bond-shaped event.
	if d.Matches(events.Event{ContractID: "CFOREIGNNOTSEEDED000000000000000000000000000000000000000", Topic: []string{TopicSymbolBond, TopicSymbolStakeUser}}) {
		t.Fatal("gate matched an unseeded contract — the membership assertions would be vacuous")
	}
}

// makeFieldEventAt is makeFieldEvent with a controllable close time +
// tx hash, for tests that need two swap groups on different timelines.
func makeFieldEventAt(t *testing.T, fieldTopic, body, txHash, closedAt string) events.Event {
	t.Helper()
	ev := makeFieldEvent(t, fieldTopic, body)
	ev.TxHash = txHash
	ev.LedgerClosedAt = closedAt
	return ev
}

// TestDecoder_Decode_rescuesPreUpgradeSevenFieldSwapAtSweep is the
// sources-decode audit 2026-08-04 finding-1 regression: the
// PRE-UPGRADE pool WASM (ledgers 51,019,036 → 53,134,167) emitted 7
// field-events per swap — no "actual received amount" — so the group
// could never Complete() and was dropped as an orphan at sweep. ALL
// 5,161 pre-upgrade swaps emitted zero trades (r1-confirmed). An
// aged-out group whose decode-consumed slots are present must now be
// DECODED at sweep, not orphaned; a genuinely under-filled group must
// still count as an orphan.
func TestDecoder_Decode_rescuesPreUpgradeSevenFieldSwapAtSweep(t *testing.T) {
	d := newTestDecoder()

	sellToken := makeC(t, 0x20)
	buyToken := makeC(t, 0x30)
	sender := makeC(t, 0x10)
	offer := big.NewInt(1_000_000)
	returnAmt := big.NewInt(2_000_000)
	zeroI128 := i128Body(t, big.NewInt(0))

	// The 7-event pre-upgrade shape: every field EXCEPT ActualReceived.
	preUpgrade := []struct{ topic, body string }{
		{TopicSymbolSender, addrBody(t, sender)},
		{TopicSymbolSellToken, addrBody(t, sellToken)},
		{TopicSymbolOfferAmount, i128Body(t, offer)},
		{TopicSymbolBuyToken, addrBody(t, buyToken)},
		{TopicSymbolReturnAmount, i128Body(t, returnAmt)},
		{TopicSymbolSpreadAmount, zeroI128},
		{TopicSymbolReferralFee, zeroI128},
	}
	for i, f := range preUpgrade {
		out, err := d.Decode(makeFieldEventAt(t, f.topic, f.body, "pre-upgrade-tx", "2026-04-23T12:00:00Z"))
		if err != nil {
			t.Fatalf("field %d (%s): %v", i, f.topic, err)
		}
		if len(out) != 0 {
			t.Fatalf("field %d (%s): emitted %d events before sweep, want 0 — rescue must be sweep-time only", i, f.topic, len(out))
		}
	}

	// A second, genuinely-broken group (2 fields only) on the same
	// timeline — must remain an orphan.
	for _, f := range preUpgrade[:2] {
		if _, err := d.Decode(makeFieldEventAt(t, f.topic, f.body, "broken-tx", "2026-04-23T12:00:01Z")); err != nil {
			t.Fatalf("broken group: %v", err)
		}
	}

	// An unrelated event past maxAge triggers the sweep of both groups.
	out, err := d.Decode(makeFieldEventAt(t, TopicSymbolSender, addrBody(t, sender), "later-tx", "2026-04-23T12:10:00Z"))
	if err != nil {
		t.Fatalf("sweep-trigger event: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("sweep emitted %d events, want exactly 1 (the rescued 7-field swap; the 2-field group stays an orphan)", len(out))
	}
	te, ok := out[0].(TradeEvent)
	if !ok {
		t.Fatalf("swept emission is %T, want TradeEvent", out[0])
	}
	if te.Trade.Source != SourceName {
		t.Errorf("rescued trade source = %q, want %q", te.Trade.Source, SourceName)
	}
	if got := te.Trade.QuoteAmount.BigInt().Int64(); got != returnAmt.Int64() {
		t.Errorf("rescued trade quote amount = %d, want %d (ReturnAmount — decode must match the 8-field era's field mapping)", got, returnAmt.Int64())
	}
	if d.evictedOrphans != 1 {
		t.Errorf("evictedOrphans = %d, want 1 (only the 2-field group)", d.evictedOrphans)
	}
}

// makeFieldEventIdx is makeFieldEvent with an explicit EventIndex, so a
// test can put two swaps' field-events under ONE (ledger, tx, op, pool)
// groupKey while keeping each swap's field-events distinguishable.
func makeFieldEventIdx(t *testing.T, fieldTopic, body string, eventIndex int) events.Event {
	t.Helper()
	ev := makeFieldEvent(t, fieldTopic, body)
	ev.EventIndex = eventIndex
	return ev
}

// TestDecoder_Decode_samePoolTwiceInOpKeepsBothSwaps is the
// W1-protocol-tables-3 regression: a router multi-hop (or cyclic
// arbitrage) that routes through the SAME phoenix pool twice in one op
// emits two swaps whose field-events share ONE groupKey (ledger, tx,
// op, contract). In the pre-upgrade 7-field era a group never
// Complete()s, so absorb never emit-and-clears mid-op; without the
// re-assignment guard the second swap's field-events overwrite the
// first's slots in place and only ONE trade survives — the first swap
// is silently dropped. The guard must rotate the first (Decodable)
// group out so BOTH trades land, with distinct op_index.
func TestDecoder_Decode_samePoolTwiceInOpKeepsBothSwaps(t *testing.T) {
	d := newTestDecoder()

	sellToken := makeC(t, 0x20)
	buyToken := makeC(t, 0x30)
	sender := makeC(t, 0x10)
	zeroI128 := i128Body(t, big.NewInt(0))

	// Two distinct 7-field swaps through the SAME pool in the SAME op.
	// Swap 1 offers 1,000,000 / returns 2,000,000; swap 2 offers
	// 3,000,000 / returns 4,000,000. Field EventIndex ranges are
	// disjoint (swap 1: 0..6, swap 2: 7..13) so the guard can tell a
	// second-swap field from a redelivery.
	swap := func(base int, offer, ret *big.Int) []events.Event {
		fields := []struct{ topic, body string }{
			{TopicSymbolSender, addrBody(t, sender)},
			{TopicSymbolSellToken, addrBody(t, sellToken)},
			{TopicSymbolOfferAmount, i128Body(t, offer)},
			{TopicSymbolBuyToken, addrBody(t, buyToken)},
			{TopicSymbolReturnAmount, i128Body(t, ret)},
			{TopicSymbolSpreadAmount, zeroI128},
			{TopicSymbolReferralFee, zeroI128},
		}
		evs := make([]events.Event, len(fields))
		for i, f := range fields {
			evs[i] = makeFieldEventIdx(t, f.topic, f.body, base+i)
		}
		return evs
	}

	offer1, ret1 := big.NewInt(1_000_000), big.NewInt(2_000_000)
	offer2, ret2 := big.NewInt(3_000_000), big.NewInt(4_000_000)

	var emitted []TradeEvent
	feed := func(evs []events.Event) {
		for _, ev := range evs {
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			for _, o := range out {
				te, ok := o.(TradeEvent)
				if !ok {
					t.Fatalf("emitted %T, want TradeEvent", o)
				}
				emitted = append(emitted, te)
			}
		}
	}
	feed(swap(0, offer1, ret1))
	feed(swap(7, offer2, ret2))

	// Sweep to flush the second (still-buffered) swap.
	out, err := d.Decode(makeFieldEventAt(t, TopicSymbolSender, addrBody(t, sender), "later-tx", "2026-04-23T12:10:00Z"))
	if err != nil {
		t.Fatalf("sweep-trigger: %v", err)
	}
	for _, o := range out {
		if te, ok := o.(TradeEvent); ok {
			emitted = append(emitted, te)
		}
	}

	if len(emitted) != 2 {
		t.Fatalf("emitted %d trades, want 2 (both same-pool swaps must survive; the second must not overwrite the first)", len(emitted))
	}

	// Both swaps' amounts must be present — neither dropped nor merged.
	got := map[int64]int64{}
	ops := map[uint32]bool{}
	for _, te := range emitted {
		got[te.Trade.BaseAmount.BigInt().Int64()] = te.Trade.QuoteAmount.BigInt().Int64()
		ops[te.Trade.OpIndex] = true
	}
	if got[offer1.Int64()] != ret1.Int64() {
		t.Errorf("first swap missing: base=%d not paired with quote=%d (it was overwritten)", offer1.Int64(), ret1.Int64())
	}
	if got[offer2.Int64()] != ret2.Int64() {
		t.Errorf("second swap missing: base=%d not paired with quote=%d", offer2.Int64(), ret2.Int64())
	}
	if len(ops) != 2 {
		t.Errorf("the two trades collapsed onto %d op_index value(s), want 2 distinct (FanoutOpIndex must keep them apart on the trades PK)", len(ops))
	}
}
