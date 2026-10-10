package phoenix

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

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

// TestDecoder_GateRejectsForeignContract pins ADR-0035/0040:
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

// TestGatedSet_SeedsVerifiedCompletenessContracts pins the
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

// TestGatedSet_ExcludesBondInstrumentContract: CBBUVHCE… emits
// ("bond", created|live|…) from a non-stake WASM. It must stay out of the
// curated set so a stake-shaped event from it is never attributed to phoenix.
func TestGatedSet_ExcludesBondInstrumentContract(t *testing.T) {
	const bondInstrument = "CBBUVHCEML7UE46XXZXLTMGKFMKX7KOC2XAKI3TW6WBQBKWMSARMU3YM"
	for _, c := range MainnetGatedSet() {
		if c == bondInstrument {
			t.Fatalf("%s is in MainnetGatedSet() but is not a phoenix stake contract", bondInstrument)
		}
	}

	d := NewDecoder()
	for _, c := range d.GatedContractSet() {
		if c == bondInstrument {
			t.Fatalf("%s is in the production decoder's gate", bondInstrument)
		}
	}
	if d.Matches(events.Event{ContractID: bondInstrument, Topic: []string{TopicSymbolBond, TopicSymbolStakeUser}}) {
		t.Errorf("production gate attributes a stake-shaped bond event from %s to phoenix", bondInstrument)
	}

	created := events.Event{ContractID: bondInstrument, Topic: []string{TopicSymbolBond, scval.MustEncodeString("created")}}
	// classifyAny maps any ("bond", *) to actionBond, so only the gate keeps
	// this event from reaching Decode as an ErrUnknownField decode error.
	if d.Matches(created) {
		t.Errorf(`production gate matched ("bond","created") from %s`, bondInstrument)
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

// TestDecoder_Decode_rescuesPreUpgradeSevenFieldSwapAtSweep is a
// sources-decode regression: the
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

// TestDecoder_Decode_samePoolTwiceInOpKeepsBothSwaps is a
// regression test: a router multi-hop (or cyclic
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

// A stale partial liquidity group swept by a later swap event is an orphan
// too; the count must reach EvictedOrphans, not be discarded.
func TestDecoder_EvictedOrphans_countsStaleLiquidityGroup(t *testing.T) {
	d := newTestDecoder()
	if _, err := d.Decode(events.Event{
		Topic:          []string{TopicSymbolProvideLiquidity, TopicSymbolPLSender},
		Value:          addrBody(t, makeC(t, 0x10)),
		Ledger:         1_500_000,
		TxHash:         "old-pl-tx",
		LedgerClosedAt: "2026-01-01T00:00:00Z",
		ContractID:     plPool,
	}); err != nil {
		t.Fatalf("Decode partial provide_liquidity: %v", err)
	}
	if _, err := d.Decode(events.Event{
		Topic:          []string{TopicSymbolSwap, TopicSymbolSender},
		Value:          addrBody(t, makeC(t, 0x10)),
		Ledger:         1_500_001,
		TxHash:         "new-tx",
		LedgerClosedAt: "2026-01-01T00:10:00Z",
		ContractID:     "pool-A",
	}); err != nil {
		t.Fatalf("Decode swap: %v", err)
	}
	if got := d.EvictedOrphans(); got != 1 {
		t.Errorf("EvictedOrphans() = %d, want 1 (the stale liquidity group)", got)
	}
}

// TestRawSwap_Decodable_requiresEveryConsumedSlot: an aged-out group missing
// any slot decodeSwap reads must stay an orphan (never reach decodeSwap and
// its nil dereference); a group missing only an unread slot is rescuable.
func TestRawSwap_Decodable_requiresEveryConsumedSlot(t *testing.T) {
	ev := &events.Event{}
	for _, tc := range []struct {
		topic     string
		decodable bool
	}{
		{TopicSymbolSender, false},
		{TopicSymbolSellToken, false},
		{TopicSymbolOfferAmount, false},
		{TopicSymbolBuyToken, false},
		{TopicSymbolReturnAmount, false},
		{TopicSymbolActualReceived, true},
		{TopicSymbolSpreadAmount, true},
		{TopicSymbolReferralFee, true},
	} {
		var r RawSwap
		for _, f := range stringSwapTopics {
			if f != tc.topic {
				if err := r.assign(ev, f); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := r.Decodable(); got != tc.decodable {
			t.Errorf("missing %q: Decodable() = %v, want %v", tc.topic, got, tc.decodable)
		}
		if !tc.decodable {
			if _, err := decodeSwap(&r); !errors.Is(err, ErrIncompleteSwap) {
				t.Errorf("missing %q: decodeSwap err = %v, want ErrIncompleteSwap", tc.topic, err)
			}
		}
	}
}

// TestDecoder_SweepOrphansGroupMissingOfferAmount drives the same property
// through the production path: a 7-field group lacking offer_amount ages out
// as an orphan, emitting no trade.
func TestDecoder_SweepOrphansGroupMissingOfferAmount(t *testing.T) {
	d := newTestDecoder()
	sell, buy := makeC(t, 0x51), makeC(t, 0x52)
	senderVal, _ := accountVal(t, 0x53)
	bodies := map[string]string{
		TopicSymbolSender:         b64Marshal(t, senderVal),
		TopicSymbolSellToken:      b64Marshal(t, contractVal(t, sell)),
		TopicSymbolActualReceived: b64Marshal(t, i128HiLo(0, 10)),
		TopicSymbolBuyToken:       b64Marshal(t, contractVal(t, buy)),
		TopicSymbolReturnAmount:   b64Marshal(t, i128HiLo(0, 20)),
		TopicSymbolSpreadAmount:   b64Marshal(t, i128HiLo(0, 1)),
		TopicSymbolReferralFee:    b64Marshal(t, i128HiLo(0, 0)),
	}
	for topic, body := range bodies {
		if out, err := d.Decode(makeFieldEventAt(t, topic, body, "old", "2026-04-23T12:00:00Z")); err != nil || len(out) != 0 {
			t.Fatalf("buffering %q: out=%v err=%v", topic, out, err)
		}
	}
	// Ten minutes later: the old group is past defaultOrphanMaxAge.
	out, err := d.Decode(makeFieldEventAt(t, TopicSymbolSender, bodies[TopicSymbolSender], "new", "2026-04-23T12:10:00Z"))
	if err != nil || len(out) != 0 {
		t.Fatalf("sweep: out=%v err=%v, want no trade", out, err)
	}
	if got := d.EvictedOrphans(); got != 1 {
		t.Fatalf("EvictedOrphans = %d, want 1", got)
	}
}

// realCreateBody is a real factory ("create","liquidity_pool") body from
// test/fixtures/phoenix/factory-create (ledger 51572026).
const realCreateBody = "AAAAEgAAAAFOKMq33nPyGnLDFPIU2W2jUUiShHdABJPjEW7pvCVp4A=="

func createEvent(emitter, topic1, body string) events.Event {
	return events.Event{
		Topic:          []string{TopicSymbolCreate, topic1},
		Value:          body,
		Ledger:         51_572_026,
		TxHash:         "02cea787b98e0b3d426ea36d9510e62b1d125a16162059d13a2895531f0887b9",
		EventIndex:     3,
		ContractID:     emitter,
		LedgerClosedAt: "2024-05-07T20:27:59Z",
	}
}

func TestDecodeAnnouncedPool_realBodyAndNonContractRejected(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(realCreateBody)
	if err != nil {
		t.Fatal(err)
	}
	want, err := strkey.Encode(strkey.VersionByteContract, raw[8:])
	if err != nil {
		t.Fatal(err)
	}
	ev := createEvent(MainnetFactory, TopicCreateLiquidityPool, realCreateBody)
	got, err := decodeAnnouncedPool(&ev)
	if err != nil || got != want {
		t.Fatalf("decodeAnnouncedPool(real) = %q, %v; want %q", got, err, want)
	}

	acct, _ := accountVal(t, 0x61)
	ev.Value = b64Marshal(t, acct)
	if got, err := decodeAnnouncedPool(&ev); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("decodeAnnouncedPool(account) = %q, %v; want ErrMalformedPayload", got, err)
	}
}

// TestDecoder_CreatePoolGate pins the three legs of pool admission: only the
// ("create","liquidity_pool") pair classifies, only a FACTORY emitter matches
// (a curated pool republishing the topics must not), and a matched
// announcement admits the pool into the gate.
func TestDecoder_CreatePoolGate(t *testing.T) {
	if a, _ := classifyAny(&events.Event{Topic: []string{TopicSymbolCreate, TopicSymbolSender}}); a != actionUnknown {
		t.Fatalf(`classifyAny(("create","sender")) = %v, want actionUnknown`, a)
	}

	d := NewDecoder()
	curated := MainnetPools[0]
	if d.Matches(createEvent(curated, TopicCreateLiquidityPool, realCreateBody)) {
		t.Fatal("Matches accepted a create announcement from a curated pool, not the factory")
	}
	if d.Matches(createEvent(MainnetFactory, TopicSymbolSender, realCreateBody)) {
		t.Fatal(`Matches accepted a factory ("create","sender") event`)
	}

	// A pool outside the curated seed, so admission is observable.
	ann := createEvent(MainnetFactory, TopicCreateLiquidityPool, b64Marshal(t, contractVal(t, makeC(t, 0x77))))
	if !d.Matches(ann) {
		t.Fatal("Matches rejected the factory's create announcement")
	}
	pool, err := decodeAnnouncedPool(&ann)
	if err != nil {
		t.Fatal(err)
	}
	swapFromPool := events.Event{Topic: []string{TopicSymbolSwap, TopicSymbolSender}, ContractID: pool}
	if d.Matches(swapFromPool) {
		t.Fatal("announced pool matched before its announcement was decoded")
	}
	if out, err := d.Decode(ann); err != nil || len(out) != 0 {
		t.Fatalf("Decode(create) = %v, %v; want no events, no error", out, err)
	}
	if !d.Matches(swapFromPool) {
		t.Fatal("announced pool not admitted after Decode(create)")
	}
}

func bufEvent(contract string, idx int) *events.Event {
	return &events.Event{ContractID: contract, TxHash: "tx", Ledger: 7, EventIndex: idx}
}

// A second action through the same contract in the same op must not
// overwrite the first action's filled slot; the open group rotates out.
func TestBuffer_RotatesOpenGroupOnSecondAction(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name  string
		first []string
		again string
		open  func(b *buffer) int
		step  func(b *buffer, e *events.Event, topic string) (int, error)
	}{
		{
			"provide",
			[]string{TopicSymbolPLSender, TopicSymbolPLTokenA},
			TopicSymbolPLSender,
			func(b *buffer) int { return len(b.pl) },
			func(b *buffer, e *events.Event, topic string) (int, error) {
				_, n, err := b.absorbProvideLiquidity(e, topic, now)
				return n, err
			},
		},
		{
			"withdraw",
			[]string{TopicSymbolWLSender, TopicSymbolWLSharesAmount},
			TopicSymbolWLSender,
			func(b *buffer) int { return len(b.wl) },
			func(b *buffer, e *events.Event, topic string) (int, error) {
				_, n, err := b.absorbWithdrawLiquidity(e, topic, now)
				return n, err
			},
		},
		{
			"bond",
			[]string{TopicSymbolStakeUser, TopicSymbolStakeToken},
			TopicSymbolStakeUser,
			func(b *buffer) int { return len(b.bond) },
			func(b *buffer, e *events.Event, topic string) (int, error) {
				_, n, err := b.absorbStake(e, topic, now, true)
				return n, err
			},
		},
		{
			"withdraw_rewards",
			[]string{TopicSymbolWRUser},
			TopicSymbolWRUser,
			func(b *buffer) int { return len(b.withdrawRewards) },
			func(b *buffer, e *events.Event, topic string) (int, error) {
				_, n, err := b.absorbWithdrawRewards(e, topic, now)
				return n, err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuffer()
			for i, topic := range tc.first {
				if n, err := tc.step(b, bufEvent("C", i), topic); err != nil || n != 0 {
					t.Fatalf("first action field %s: evicted=%d err=%v", topic, n, err)
				}
			}
			// Redelivery of an already-held field (same EventIndex) is not a rotation.
			if n, err := tc.step(b, bufEvent("C", 0), tc.first[0]); err != nil || n != 0 {
				t.Fatalf("redelivery: evicted=%d err=%v, want 0", n, err)
			}
			n, err := tc.step(b, bufEvent("C", 10), tc.again)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("evicted = %d, want 1 (first action's open group rotated out)", n)
			}
			if got := tc.open(b); got != 1 {
				t.Errorf("open groups = %d, want 1 (fresh generation)", got)
			}
		})
	}
}

// A late optional auto-unbonded event after the withdraw completed must not
// open an empty group that later ages out as a false orphan.
func TestBuffer_LateAutoUnbondedOpensNoGroup(t *testing.T) {
	b := newBuffer()
	now := time.Unix(1_700_000_000, 0)
	for i, topic := range []string{TopicSymbolWLSender, TopicSymbolWLSharesAmount, TopicSymbolWLReturnAmountA, TopicSymbolWLReturnAmountB} {
		done, _, err := b.absorbWithdrawLiquidity(bufEvent("C", i), topic, now)
		if err != nil {
			t.Fatal(err)
		}
		if (done != nil) != (i == 3) {
			t.Fatalf("field %d: completed=%v", i, done != nil)
		}
	}
	if _, _, err := b.absorbWithdrawLiquidity(bufEvent("C", 4), TopicSymbolWLAutoUnbonded, now); err != nil {
		t.Fatal(err)
	}
	if len(b.wl) != 0 {
		t.Errorf("open withdraw groups = %d, want 0", len(b.wl))
	}
}

// Drain at the end of a bounded stream emits the pre-upgrade 7-field swap
// that no later event is left to sweep, counts the rest as orphans, and
// leaves nothing buffered.
func TestDecoder_Drain_rescuesOpenSwapAndCountsOrphans(t *testing.T) {
	d := newTestDecoder()
	sender, sell, buy := makeC(t, 0x10), makeC(t, 0x20), makeC(t, 0x30)
	zero := i128Body(t, big.NewInt(0))
	fields := []struct{ topic, body string }{
		{TopicSymbolSender, addrBody(t, sender)},
		{TopicSymbolSellToken, addrBody(t, sell)},
		{TopicSymbolOfferAmount, i128Body(t, big.NewInt(1_000_000))},
		{TopicSymbolBuyToken, addrBody(t, buy)},
		{TopicSymbolReturnAmount, i128Body(t, big.NewInt(2_000_000))},
		{TopicSymbolSpreadAmount, zero},
		{TopicSymbolReferralFee, zero},
	}
	for _, f := range fields {
		if _, err := d.Decode(makeFieldEventAt(t, f.topic, f.body, "tx-7field", "2026-04-23T12:00:00Z")); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range fields[:2] {
		if _, err := d.Decode(makeFieldEventAt(t, f.topic, f.body, "tx-broken", "2026-04-23T12:00:01Z")); err != nil {
			t.Fatal(err)
		}
	}
	out := d.Drain()
	if len(out) != 1 {
		t.Fatalf("Drain emitted %d events, want 1 (the 7-field swap)", len(out))
	}
	if _, ok := out[0].(TradeEvent); !ok {
		t.Fatalf("drained event is %T, want TradeEvent", out[0])
	}
	if got := d.EvictedOrphans(); got != 1 {
		t.Errorf("EvictedOrphans = %d, want 1 (the 2-field group)", got)
	}
	if again := d.Drain(); len(again) != 0 || d.buf.size() != 0 {
		t.Errorf("second Drain emitted %d, buffered %d; want empty", len(again), d.buf.size())
	}
}

func TestDecoder_ProvideLiquidity_completesOnFifthField(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	fields := []struct{ topic, body string }{
		{TopicSymbolPLSender, "addr:" + plSender},
		{TopicSymbolPLTokenA, "addr:" + plTokenA},
		{TopicSymbolPLTokenAAmt, "i128:1000000000"}, // 100 token_a
		{TopicSymbolPLTokenB, "addr:" + plTokenB},
		{TopicSymbolPLTokenBAmt, "i128:50000000"}, // 5 token_b
	}

	var out []consumer.Event
	for i, f := range fields {
		emitted, err := d.Decode(plField(f.topic, f.body, plTxHash))
		if err != nil {
			t.Fatalf("field %d (%s): %v", i, f.topic, err)
		}
		if i < 4 && len(emitted) != 0 {
			t.Fatalf("field %d: got %d events, want 0 (still buffering)", i, len(emitted))
		}
		if i == 4 {
			out = emitted
		}
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	le, ok := out[0].(LiquidityEvent)
	if !ok {
		t.Fatalf("expected LiquidityEvent, got %T", out[0])
	}
	if le.Change.Action != EventActionProvideLiquidity {
		t.Errorf("Action = %q, want %q", le.Change.Action, EventActionProvideLiquidity)
	}
	if le.Change.Pool != plPool {
		t.Errorf("Pool = %q, want %q", le.Change.Pool, plPool)
	}
	if le.Change.Sender != plSender {
		t.Errorf("Sender = %q, want %q", le.Change.Sender, plSender)
	}
	if le.Change.TokenA != plTokenA || le.Change.TokenB != plTokenB {
		t.Errorf("tokens = (%q,%q), want (%q,%q)", le.Change.TokenA, le.Change.TokenB, plTokenA, plTokenB)
	}
	if le.Change.AmountA.BigInt().Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Errorf("AmountA = %s", le.Change.AmountA)
	}
	if le.Change.AmountB.BigInt().Cmp(big.NewInt(50_000_000)) != 0 {
		t.Errorf("AmountB = %s", le.Change.AmountB)
	}
}

func TestDecoder_ProvideLiquidity_outOfOrder(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	// Reverse contract emission order — the buffer is order-independent.
	fields := []struct{ topic, body string }{
		{TopicSymbolPLTokenBAmt, "i128:50000000"},
		{TopicSymbolPLTokenB, "addr:" + plTokenB},
		{TopicSymbolPLTokenAAmt, "i128:1000000000"},
		{TopicSymbolPLTokenA, "addr:" + plTokenA},
		{TopicSymbolPLSender, "addr:" + plSender},
	}
	var out []consumer.Event
	for i, f := range fields {
		emitted, err := d.Decode(plField(f.topic, f.body, plTxHash))
		if err != nil {
			t.Fatalf("field %d (%s): %v", i, f.topic, err)
		}
		if i == 4 {
			out = emitted
		}
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 after 5th field", len(out))
	}
}

// ─── withdraw_liquidity ─────────────────────────────────────────

func TestDecoder_WithdrawLiquidity_completesOnFourthField(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	fields := []struct{ topic, body string }{
		{TopicSymbolWLSender, "addr:" + plSender},
		{TopicSymbolWLSharesAmount, "i128:7000000"},
		{TopicSymbolWLReturnAmountA, "i128:99000000"},
		{TopicSymbolWLReturnAmountB, "i128:4900000"},
	}
	var out []consumer.Event
	for i, f := range fields {
		emitted, err := d.Decode(wlField(f.topic, f.body, wlTxHash))
		if err != nil {
			t.Fatalf("field %d (%s): %v", i, f.topic, err)
		}
		if i < 3 && len(emitted) != 0 {
			t.Fatalf("field %d: got %d events, want 0 (still buffering)", i, len(emitted))
		}
		if i == 3 {
			out = emitted
		}
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	le := out[0].(LiquidityEvent)
	if le.Change.Action != EventActionWithdrawLiquidity {
		t.Errorf("Action = %q", le.Change.Action)
	}
	if le.Change.Pool != wlPool {
		t.Errorf("Pool = %q, want %q", le.Change.Pool, wlPool)
	}
	if le.Change.SharesAmount.BigInt().Cmp(big.NewInt(7_000_000)) != 0 {
		t.Errorf("SharesAmount = %s", le.Change.SharesAmount)
	}
	// Withdraw rows must NOT carry token addresses (contract doesn't emit them).
	if le.Change.TokenA != "" || le.Change.TokenB != "" {
		t.Errorf("withdraw token addresses leaked: a=%q b=%q", le.Change.TokenA, le.Change.TokenB)
	}
}

// TestDecoder_WithdrawLiquidity_optionalAutoUnbondedIgnored proves
// the optional 5th event (auto unbonded) is recognised (no
// ErrUnknownField) but discarded — the withdraw record completes on
// the 4 required fields regardless of its arrival.
func TestDecoder_WithdrawLiquidity_optionalAutoUnbondedIgnored(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	fields := []struct{ topic, body string }{
		{TopicSymbolWLSender, "addr:" + plSender},
		{TopicSymbolWLAutoUnbonded, "ignored-tuple-body"}, // interleaved — must not break correlation
		{TopicSymbolWLSharesAmount, "i128:7000000"},
		{TopicSymbolWLReturnAmountA, "i128:99000000"},
		{TopicSymbolWLReturnAmountB, "i128:4900000"},
	}
	var out []consumer.Event
	for _, f := range fields {
		emitted, err := d.Decode(wlField(f.topic, f.body, wlTxHash))
		if err != nil {
			t.Fatalf("field %s: %v", f.topic, err)
		}
		if len(emitted) > 0 {
			out = emitted
		}
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 (auto unbonded should not block completion)", len(out))
	}
}

// ─── bond / unbond ──────────────────────────────────────────────

func TestDecoder_Bond_completesOnThirdField(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	fields := []struct{ topic, body string }{
		{TopicSymbolStakeUser, "addr:" + stakeUser},
		{TopicSymbolStakeToken, "addr:" + lpTokenC},
		{TopicSymbolStakeAmount, "i128:12345678"},
	}
	var out []consumer.Event
	for i, f := range fields {
		emitted, err := d.Decode(bondField(f.topic, f.body, bondTx))
		if err != nil {
			t.Fatalf("field %d: %v", i, err)
		}
		if i < 2 && len(emitted) != 0 {
			t.Fatalf("field %d: got %d events, want 0 (still buffering)", i, len(emitted))
		}
		if i == 2 {
			out = emitted
		}
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	se := out[0].(StakeEvent)
	if se.Change.Action != EventActionBond {
		t.Errorf("Action = %q, want %q", se.Change.Action, EventActionBond)
	}
	if se.Change.Contract != stakeC {
		t.Errorf("Contract = %q", se.Change.Contract)
	}
	if se.Change.User != stakeUser || se.Change.LPToken != lpTokenC {
		t.Errorf("user/token = (%q, %q)", se.Change.User, se.Change.LPToken)
	}
	if se.Change.Amount.BigInt().Cmp(big.NewInt(12_345_678)) != 0 {
		t.Errorf("Amount = %s", se.Change.Amount)
	}
}

// TestDecoder_BondAndUnbond_independentBuffers proves bond + unbond
// from the same (ledger, tx, op) do NOT collide — they use distinct
// per-action correlation maps. (Phoenix's stake contract uses the
// same field name `amount` for both, so a single shared map would
// merge them.)
func TestDecoder_BondAndUnbond_independentBuffers(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	// Same ledger / tx / op shared across bond + unbond — proves
	// per-action sharding of the buffer.
	const sharedTx = "shareeshareeshareeshareeshareeshareeshareeshareeshareesharee123"

	bondFields := []struct{ topic, body string }{
		{TopicSymbolStakeUser, "addr:" + stakeUser},
		{TopicSymbolStakeToken, "addr:" + lpTokenC},
		{TopicSymbolStakeAmount, "i128:1000"},
	}
	unbondFields := []struct{ topic, body string }{
		{TopicSymbolStakeUser, "addr:" + stakeUser},
		{TopicSymbolStakeToken, "addr:" + lpTokenC},
		{TopicSymbolStakeAmount, "i128:500"},
	}

	emit := func(ev events.Event) consumer.Event {
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) == 1 {
			return out[0]
		}
		return nil
	}

	// Interleave: bond[0], unbond[0], bond[1], unbond[1], bond[2], unbond[2]
	var bondOut, unbondOut consumer.Event
	for i := 0; i < 3; i++ {
		bondEv := bondField(bondFields[i].topic, bondFields[i].body, sharedTx)
		if v := emit(bondEv); v != nil {
			bondOut = v
		}
		// Force unbond into the SAME (ledger, tx, op) by overriding
		// — distinct only in the action topic[0]. This is the worst
		// case for buffer-key collision.
		unbondEv := unbondField(unbondFields[i].topic, unbondFields[i].body, sharedTx)
		unbondEv.Ledger = bondEv.Ledger
		unbondEv.OperationIndex = bondEv.OperationIndex
		if v := emit(unbondEv); v != nil {
			unbondOut = v
		}
	}
	if bondOut == nil || unbondOut == nil {
		t.Fatalf("both should complete: bond=%v unbond=%v", bondOut != nil, unbondOut != nil)
	}
	if bondOut.(StakeEvent).Change.Action != EventActionBond {
		t.Errorf("bondOut.Action = %q", bondOut.(StakeEvent).Change.Action)
	}
	if unbondOut.(StakeEvent).Change.Action != EventActionUnbond {
		t.Errorf("unbondOut.Action = %q", unbondOut.(StakeEvent).Change.Action)
	}
	if bondOut.(StakeEvent).Change.Amount.BigInt().Cmp(big.NewInt(1000)) != 0 {
		t.Errorf("bond amount = %s, want 1000", bondOut.(StakeEvent).Change.Amount)
	}
	if unbondOut.(StakeEvent).Change.Amount.BigInt().Cmp(big.NewInt(500)) != 0 {
		t.Errorf("unbond amount = %s, want 500", unbondOut.(StakeEvent).Change.Amount)
	}
}

// ─── Decoder.Matches ────────────────────────────────────────────

func TestDecoder_Matches_allFiveActions(t *testing.T) {
	d := newTestDecoder()
	cases := []struct {
		name  string
		topic []string
	}{
		{"swap", []string{TopicSymbolSwap, TopicSymbolSender}},
		{"provide_liquidity", []string{TopicSymbolProvideLiquidity, TopicSymbolPLSender}},
		{"withdraw_liquidity", []string{TopicSymbolWithdrawLiquidity, TopicSymbolWLSender}},
		{"bond", []string{TopicSymbolBond, TopicSymbolStakeUser}},
		{"unbond", []string{TopicSymbolUnbond, TopicSymbolStakeUser}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !d.Matches(events.Event{ContractID: plPool, Topic: tc.topic}) {
				t.Errorf("Matches((%s, …)) = false", tc.name)
			}
		})
	}
	if d.Matches(events.Event{ContractID: plPool, Topic: []string{"unrelated_action", TopicSymbolSender}}) {
		t.Error("Matches(unrelated topic[0]) = true")
	}
}

// ─── consumer.Event impls ───────────────────────────────────────

// TestBuffer_ProvideLiquidity_backfillOldEventsComplete proves the
// 5-event reassembly survives a 6-hour-old ClosedAt — the same
// regression guard the swap path has. Without using the event's own
// ClosedAt for eviction reference, replaying ancient events would
// evict the first-absorbed field when the 5th arrived.
func TestBuffer_ProvideLiquidity_backfillOldEventsComplete(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	old := time.Now().UTC().Add(-6 * time.Hour)
	fields := []struct{ topic, body string }{
		{TopicSymbolPLSender, "addr:" + plSender},
		{TopicSymbolPLTokenA, "addr:" + plTokenA},
		{TopicSymbolPLTokenAAmt, "i128:1000"},
		{TopicSymbolPLTokenB, "addr:" + plTokenB},
		{TopicSymbolPLTokenBAmt, "i128:2000"},
	}
	var out []consumer.Event
	for i, f := range fields {
		ev := plField(f.topic, f.body, plTxHash)
		ev.LedgerClosedAt = old.Format(time.RFC3339)
		emitted, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("field %d: %v", i, err)
		}
		if len(emitted) > 0 {
			out = emitted
		}
	}
	if len(out) != 1 {
		t.Fatal("backfilled 5-event provide failed to complete")
	}
}

// TestDecoder_Liquidity_PopulatesEventIndex pins that the completed
// provide_liquidity / withdraw_liquidity reassembly must carry the
// FIRST field-event's in-op EventIndex onto the LiquidityChange so two
// same-(op,action) liquidity actions don't collapse on the
// phoenix_liquidity PK (migration 0060) via ON CONFLICT.
func TestDecoder_Liquidity_PopulatesEventIndex(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	fields := []struct{ topic, body string }{
		{TopicSymbolPLSender, "addr:" + plSender},
		{TopicSymbolPLTokenA, "addr:" + plTokenA},
		{TopicSymbolPLTokenAAmt, "i128:1000000000"},
		{TopicSymbolPLTokenB, "addr:" + plTokenB},
		{TopicSymbolPLTokenBAmt, "i128:50000000"},
	}
	var out []consumer.Event
	for i, f := range fields {
		ev := plField(f.topic, f.body, plTxHash)
		// The buffer stamps EventIndex from the FIRST arriving field —
		// give the rest distinct indices to prove only the first wins.
		ev.EventIndex = 11 + i
		emitted, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("field %d (%s): %v", i, f.topic, err)
		}
		if i == 4 {
			out = emitted
		}
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	le := out[0].(LiquidityEvent)
	if le.Change.EventIndex != 11 {
		t.Errorf("EventIndex = %d, want 11 (first field-event's index, F-1324)", le.Change.EventIndex)
	}
}

// TestDecoder_Stake_PopulatesEventIndex pins the same for the stake path
// (phoenix_stake_events PK, migration 0060): the bond / unbond
// reassembly carries the first field-event's in-op EventIndex.
func TestDecoder_Stake_PopulatesEventIndex(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	fields := []struct{ topic, body string }{
		{TopicSymbolStakeUser, "addr:" + plSender},
		{TopicSymbolStakeToken, "addr:" + plTokenA},
		{TopicSymbolStakeAmount, "i128:7000000"},
	}
	var out []consumer.Event
	for i, f := range fields {
		ev := bondField(f.topic, f.body, bondTx)
		ev.EventIndex = 20 + i
		emitted, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("field %d (%s): %v", i, f.topic, err)
		}
		if i == 2 {
			out = emitted
		}
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	se := out[0].(StakeEvent)
	if se.Change.EventIndex != 20 {
		t.Errorf("EventIndex = %d, want 20 (first field-event's index, F-1324)", se.Change.EventIndex)
	}
}

// TestDecoder_WithdrawLiquidity_twoInOneOp mirrors ledger 63767534: one op
// withdraws twice from the same pool (field events 6-9 and 14-17). Both
// actions must emit, each keyed by its own first EventIndex.
func TestDecoder_WithdrawLiquidity_twoInOneOp(t *testing.T) {
	restore := installAddressI128Fakes(t)
	defer restore()
	d := newTestDecoder()

	fields := []struct{ topic, body string }{
		{TopicSymbolWLSender, "addr:" + plSender},
		{TopicSymbolWLSharesAmount, "i128:7000000"},
		{TopicSymbolWLReturnAmountA, "i128:99000000"},
		{TopicSymbolWLReturnAmountB, "i128:4900000"},
	}
	var got []int
	for _, start := range []int{6, 14} {
		for i, f := range fields {
			ev := wlField(f.topic, f.body, wlTxHash)
			ev.EventIndex = start + i
			emitted, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("event %d (%s): %v", ev.EventIndex, f.topic, err)
			}
			for _, e := range emitted {
				got = append(got, e.(LiquidityEvent).Change.EventIndex)
			}
		}
	}
	if len(got) != 2 || got[0] != 6 || got[1] != 14 {
		t.Fatalf("emitted EventIndex = %v, want [6 14]", got)
	}
}

// TestDecoder_MapSwap_gatedAndDecoded proves the Map-schema swap flows
// through the production Decode seam: a single event from the gated
// CBENABXP pool emits one TradeEvent immediately (no buffer), and the
// same event from an unregistered contract is NOT attributed
// (ADR-0035/0040 gating).
func TestDecoder_MapSwap_gatedAndDecoded(t *testing.T) {
	d := NewDecoder() // production gate: curated mainnet set incl. MainnetMapPools
	ev := mapSwapEvent()

	if !d.Matches(ev) {
		t.Fatal("gated Map-schema pool CBENABXP should Match")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 (Map swap is a single event)", len(out))
	}
	te, ok := out[0].(TradeEvent)
	if !ok {
		t.Fatalf("got %T, want TradeEvent", out[0])
	}
	if te.Trade.QuoteAmount.BigInt().Int64() != mapSwapReturn {
		t.Errorf("QuoteAmount = %s, want %d", te.Trade.QuoteAmount, mapSwapReturn)
	}

	// Same event shape from a foreign contract → not attributed.
	foreign := ev
	foreign.ContractID = "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000"
	if d.Matches(foreign) {
		t.Error("foreign contract emitting the Map swap shape must NOT match (CS-026 gating)")
	}
}

func TestDecoder_MapProvideLiquidity_realFixture(t *testing.T) {
	c := decodeLiquidityThroughDecoder(t, mapProvideEvent())
	if c.Action != EventActionProvideLiquidity || c.Pool != cbenabxpPool {
		t.Errorf("action/pool = %q/%q", c.Action, c.Pool)
	}
	if c.Ledger != 63295145 || c.OpIndex != 0 || c.EventIndex != 4 {
		t.Errorf("ledger/op/event = %d/%d/%d", c.Ledger, c.OpIndex, c.EventIndex)
	}
	if c.Sender != mapLiquiditySender {
		t.Errorf("Sender = %q, want %q", c.Sender, mapLiquiditySender)
	}
	if c.TokenA != mapSwapBuyToken || c.TokenB != mapSwapSellToken {
		t.Errorf("tokens = %s / %s, want %s / %s", c.TokenA, c.TokenB, mapSwapBuyToken, mapSwapSellToken)
	}
	if c.AmountA.String() != "27300000000" || c.AmountB.String() != "5439074397" {
		t.Errorf("amounts = %s / %s, want 27300000000 / 5439074397", c.AmountA, c.AmountB)
	}
	if !c.SharesAmount.IsZero() {
		t.Errorf("SharesAmount = %s, want zero on provide", c.SharesAmount)
	}
}

func TestDecoder_MapWithdrawLiquidity_realFixture(t *testing.T) {
	c := decodeLiquidityThroughDecoder(t, mapWithdrawEvent())
	if c.Action != EventActionWithdrawLiquidity || c.Pool != cbenabxpPool {
		t.Errorf("action/pool = %q/%q", c.Action, c.Pool)
	}
	if c.Ledger != 63295946 || c.OpIndex != 0 || c.EventIndex != 4 {
		t.Errorf("ledger/op/event = %d/%d/%d", c.Ledger, c.OpIndex, c.EventIndex)
	}
	if c.Sender != mapLiquiditySender {
		t.Errorf("Sender = %q, want %q", c.Sender, mapLiquiditySender)
	}
	if c.AmountA.String() != "1120182615" || c.AmountB.String() != "223177896" {
		t.Errorf("amounts = %s / %s, want 1120182615 / 223177896", c.AmountA, c.AmountB)
	}
	if c.SharesAmount.String() != "500000000" {
		t.Errorf("SharesAmount = %s, want 500000000", c.SharesAmount)
	}
	if c.TokenA != "" || c.TokenB != "" {
		t.Errorf("tokens = %q / %q, want empty (withdraw carries no token addresses)", c.TokenA, c.TokenB)
	}
}

// TestDecoder_WithdrawRewards_EndToEnd feeds the two real field-events
// through the production Decoder (Matches + Decode), confirming the
// gated stake contract set is honored and a StakeEvent is emitted only
// once both fields have arrived.
func TestDecoder_WithdrawRewards_EndToEnd(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	base := events.Event{
		ContractID:     goldenStakeContract,
		Ledger:         53_589_647,
		LedgerClosedAt: "2026-05-15T00:05:00Z",
		TxHash:         "0cfec2141ee42c35ae593169c09b8951f97b31079d4e394d8979b5cd3622dd6e",
		OperationIndex: 0,
	}

	userEv := base
	userEv.EventIndex = 0
	userEv.Topic = []string{"AAAADgAAABB3aXRoZHJhd19yZXdhcmRz", "AAAADgAAAAR1c2Vy"}
	userEv.Value = "AAAAEgAAAAAAAAAA71OhKQPURFdpcB0NxOSVKbWRaAv+2hgUeSdB9OMczcA="

	tokenEv := base
	tokenEv.EventIndex = 1
	tokenEv.Topic = []string{"AAAADgAAABB3aXRoZHJhd19yZXdhcmRz", "AAAADgAAAAxyZXdhcmRfdG9rZW4="}
	tokenEv.Value = "AAAAEgAAAAFz9nQ7xy1g57dXaZEAriZ1tUpZvggsX2i7PE0ly4C7oQ=="

	if !d.Matches(userEv) {
		t.Fatal("Matches(user field) = false, want true (gated stake contract)")
	}
	out, err := d.Decode(userEv)
	if err != nil {
		t.Fatalf("Decode(user field): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("Decode(user field) emitted %d events, want 0 (incomplete)", len(out))
	}

	out, err = d.Decode(tokenEv)
	if err != nil {
		t.Fatalf("Decode(reward_token field): %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode(reward_token field) emitted %d events, want 1", len(out))
	}
	se, ok := out[0].(StakeEvent)
	if !ok {
		t.Fatalf("out[0] is %T, want StakeEvent", out[0])
	}
	if se.Change.Action != EventActionWithdrawRewards {
		t.Errorf("Action=%q want %q", se.Change.Action, EventActionWithdrawRewards)
	}
}

func TestDecoder_NameMatchesSourceName(t *testing.T) {
	if got := newTestDecoder().Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}
