package aquarius

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
	if got := te.EventKind(); got != "aquarius.trade" {
		t.Errorf("EventKind() = %q, want \"aquarius.trade\"", got)
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

// testPool is a synthetic pool strkey the test decoder seeds so
// Matches() gate checks pass without depending on the curated
// mainnet list (which newTestDecoder also carries, being built on
// NewDecoder).
const testPool = "C-test-pool-strkey"

// newTestDecoder mirrors phoenix's helper: production seed + one
// synthetic test pool.
func newTestDecoder() *Decoder {
	return NewDecoder(contractid.WithSeed([]string{testPool}))
}

func TestDecoder_Matches(t *testing.T) {
	d := newTestDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)

	good := events.Event{
		ContractID: testPool,
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
	}
	if !d.Matches(good) {
		t.Error("Matches(trade event) = false, want true")
	}

	for _, tc := range []struct {
		name string
		ev   events.Event
	}{
		{"empty topic", events.Event{ContractID: testPool}},
		{"non-trade topic[0]", events.Event{ContractID: testPool, Topic: []string{
			// "deposit" (bare) is now a recognized+gated rewards-gauge
			// topic so it does not prove this case —
			// use a genuinely unclassified topic name instead.
			encodeSymbol(t, "totally_unrecognized_topic"),
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d.Matches(tc.ev) {
				t.Errorf("Matches(%s) = true, want false", tc.name)
			}
		})
	}
}

// TestDecoder_GateRejectsForeignContract pins ADR-0035/0040:
// the bare Symbol("trade") 4-topic shape is forgeable — the r1 lake
// contains a parallel non-registry router deployment and a
// foreign-WASM look-alike emitting the identical shape
// (docs/protocols/aquarius.md, flagged sets). A perfect trade shape
// from an unregistered contract must NOT be attributed to aquarius,
// while the same event from a curated registry pool must.
func TestDecoder_GateRejectsForeignContract(t *testing.T) {
	d := NewDecoder() // production gate: curated registry set only
	topics := []string{
		TopicSymbolTrade,
		encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x01)),
		encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x02)),
		encodeAccountAddrFromStrkey(t, makeAccountStrkey(t, 0x03)),
	}

	foreign := events.Event{
		ContractID: "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000",
		Topic:      topics,
	}
	if d.Matches(foreign) {
		t.Fatal("foreign contract with aquarius-shaped topics matched — the CS-026 injection vector is open")
	}

	genuine := events.Event{ContractID: MainnetPools[0], Topic: topics}
	if !d.Matches(genuine) {
		t.Fatal("curated registry pool failed to match — gate is over-closed")
	}
}

// TestDecoder_AddPoolRegistersNewPool pins the router fan-out
// (ADR-0035): a router add_pool announcement registers the new pool
// so its subsequent trades pass the gate; the same announcement from
// a non-router contract is rejected outright.
func TestDecoder_AddPoolRegistersNewPool(t *testing.T) {
	d := NewDecoder()
	newPool := makeContractStrkey(t, 0x7A)
	tradeTopics := []string{
		TopicSymbolTrade,
		encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x01)),
		encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x02)),
		encodeAccountAddrFromStrkey(t, makeAccountStrkey(t, 0x03)),
	}

	preGate := events.Event{ContractID: newPool, Topic: tradeTopics}
	if d.Matches(preGate) {
		t.Fatal("unannounced pool matched before the router announced it")
	}

	announce := events.Event{
		ContractID: MainnetRouter,
		Ledger:     63_000_000,
		Topic:      []string{TopicSymbolAddPool},
		Value:      encodeAddPoolBody(t, newPool),
	}
	if !d.Matches(announce) {
		t.Fatal("Matches(router add_pool) = false, want true")
	}
	out, err := d.Decode(announce)
	if err != nil {
		t.Fatalf("Decode(router add_pool): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("Decode(router add_pool) emitted %d events, want 0", len(out))
	}

	if !d.Matches(preGate) {
		t.Fatal("router-announced pool still rejected after add_pool")
	}

	// A foreign contract emitting add_pool must not register anything.
	forged := events.Event{
		ContractID: "CFOREIGNFAKEROUTER000000000000000000000000000000000000000",
		Topic:      []string{TopicSymbolAddPool},
		Value:      encodeAddPoolBody(t, makeContractStrkey(t, 0x7B)),
	}
	if d.Matches(forged) {
		t.Fatal("foreign add_pool matched — a fake router could inject pools into the gate")
	}
}

// TestDecoder_ChHashOrderCanDropRegisteredPoolTrade pins Q040's hazard at the
// Decoder: its registry grows live from add_pool alone, so a same-ledger trade
// delivered BEFORE its pool's add_pool is permanently missed (Matches() false,
// and the projector's processEventSafely treats that as a silent skip).
// stellar.contract_events sorts a ledger by tx_hash, a lexical order that can
// put the trade first; the lake readers therefore re-sort each ledger into
// transaction apply order (internal/storage/clickhouse/apply_order.go, tested
// by TestApplyOrderer_SameLedgerAddPoolPrecedesTrade). This test keeps the
// decoder-side reason that ordering is load-bearing.
//
// This test proves the divergence exists deterministically, at the
// Decoder alone, independent of any live ClickHouse connection: same
// two events, only the callback ORDER changes, with the same outcome
// the lake's tx_hash sort would produce.
func TestDecoder_ChHashOrderCanDropRegisteredPoolTrade(t *testing.T) {
	newPool := makeContractStrkey(t, 0x9C)
	announce := events.Event{
		ContractID: MainnetRouter,
		Ledger:     64_000_000,
		TxHash:     "b_same_ledger_pool_create", // lexically AFTER the trade's hash
		Topic:      []string{TopicSymbolAddPool},
		Value:      encodeAddPoolBody(t, newPool),
	}
	trade := events.Event{
		ContractID: newPool,
		Ledger:     64_000_000,
		TxHash:     "a_same_ledger_pool_trade", // lexically BEFORE the add_pool's hash
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x01)),
			encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x02)),
			encodeAccountAddrFromStrkey(t, makeAccountStrkey(t, 0x03)),
		},
		Value:          encodeTradeBody(t, big.NewInt(1_000_000), big.NewInt(2_000_000), big.NewInt(0)),
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	if trade.TxHash >= announce.TxHash {
		t.Fatalf("test setup: want trade.TxHash < announce.TxHash lexically, got %q >= %q", trade.TxHash, announce.TxHash)
	}

	// True chronological order (add_pool really did happen first
	// on-chain — a pool must exist before it can be traded): the trade
	// matches once the pool is registered.
	chrono := NewDecoder()
	if !chrono.Matches(announce) {
		t.Fatal("chronological order: Matches(add_pool) = false, want true")
	}
	if _, err := chrono.Decode(announce); err != nil {
		t.Fatalf("chronological order: Decode(add_pool): %v", err)
	}
	if !chrono.Matches(trade) {
		t.Fatal("chronological order: Matches(trade) = false after add_pool registered it, want true")
	}

	// stellar.contract_events' ORDER BY (ledger_seq, tx_hash, op_index,
	// event_index) delivers these two SAME-LEDGER events in the
	// opposite (hash) order: trade, then add_pool. That is what the
	// live projector actually streams.
	hashOrder := NewDecoder()
	if hashOrder.Matches(trade) {
		t.Fatal("hash order: Matches(trade) = true before add_pool was ever seen — test setup invalid")
	}
	// The projector's caller treats this Matches()=false miss as
	// (0, false, nil) — no error, no count (internal/projector/
	// projector.go processEventSafely) — so this trade is now gone:
	// the cursor advances past it and it is never offered again.
	//
	// The pool DOES register once its own add_pool is later seen, but
	// that is too late for the trade that already came and went.
	if _, err := hashOrder.Decode(announce); err != nil {
		t.Fatalf("hash order: Decode(add_pool): %v", err)
	}
	if !hashOrder.Matches(trade) {
		t.Fatal("hash order: pool never registered — test setup invalid")
	}
	// The point: a real one-pass forward stream calls Matches()/Decode()
	// once per event, in stream order, and never replays a miss. The
	// registration succeeding on a SECOND look proves the trade's
	// original miss was a stream-ordering artifact, not a real
	// unregistered-pool rejection — exactly the silent loss Q040 names.
}

// TestDecoder_AddPoolMalformedBody: a router add_pool whose body
// isn't Vec[Address(contract), …] is a decode error (skip + count),
// never a registration.
func TestDecoder_AddPoolMalformedBody(t *testing.T) {
	d := NewDecoder()
	for name, body := range map[string]string{
		"not-base64":  "not-base64",
		"empty-vec":   encodeEmptyVec(t),
		"g-address":   encodeAddPoolBodyAccount(t, makeAccountStrkey(t, 0x03)),
		"i128-scalar": encodeTradeBody(t, big.NewInt(1), big.NewInt(1), big.NewInt(0)),
	} {
		t.Run(name, func(t *testing.T) {
			ev := events.Event{ContractID: MainnetRouter, Topic: []string{TopicSymbolAddPool}, Value: body}
			if _, err := d.Decode(ev); err == nil {
				t.Error("Decode(malformed add_pool body) err = nil, want error")
			}
		})
	}
}

func TestDecoder_Decode_HappyPathProducesOneTradeEvent(t *testing.T) {
	d := NewDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)
	ev := events.Event{
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
		Value:          encodeTradeBody(t, big.NewInt(1_000_000), big.NewInt(2_000_000), big.NewInt(0)),
		Ledger:         62_000_000,
		TxHash:         "deadbeef",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	te, ok := out[0].(TradeEvent)
	if !ok {
		t.Fatalf("expected TradeEvent, got %T", out[0])
	}
	if te.Trade.Source != SourceName {
		t.Errorf("Trade.Source = %q, want %q", te.Trade.Source, SourceName)
	}
	if te.Trade.Taker != user {
		t.Errorf("Trade.Taker = %q, want %q", te.Trade.Taker, user)
	}
}

func TestDecoder_Decode_MalformedClosedAtReturnsError(t *testing.T) {
	d := NewDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)
	ev := events.Event{
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
		Value:          encodeTradeBody(t, big.NewInt(1), big.NewInt(1), big.NewInt(0)),
		LedgerClosedAt: "not-a-timestamp",
	}
	if _, err := d.Decode(ev); err == nil {
		t.Error("expected EventClosedAt error on malformed timestamp, got nil")
	}
}

// TestDecoder_Decode_UnrecognizedKindFailsClosed pins Q041: Decode()'s
// switch must fail closed on a kind it has no explicit case for, not
// force it through decodeTrade just because the topic shape happens to
// resemble one. An event whose topic[0] classify()
// doesn't recognize — but which otherwise has trade-shaped topics/body —
// must not be silently decoded as a genuine trade (ADR-0035 violation).
func TestDecoder_Decode_UnrecognizedKindFailsClosed(t *testing.T) {
	d := NewDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)
	ev := events.Event{
		Topic: []string{
			encodeSymbol(t, "totally_unrecognized_topic"),
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
		Value:          encodeTradeBody(t, big.NewInt(1_000_000), big.NewInt(2_000_000), big.NewInt(0)),
		Ledger:         62_000_000,
		TxHash:         "unrecognized-kind",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	if classify(&ev) != "" {
		t.Fatalf("test setup: classify() recognized the topic, want unclassified")
	}
	out, err := d.Decode(ev)
	if err == nil {
		t.Fatalf("Decode(unrecognized kind, trade-shaped) = (%v, nil), want an error — it was silently decoded as a trade", out)
	}
}

func TestDecoder_Decode_MalformedBodyReturnsError(t *testing.T) {
	d := NewDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)
	ev := events.Event{
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
		Value:          "not-base64",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	if _, err := d.Decode(ev); err == nil {
		t.Error("expected decode error on malformed body, got nil")
	}
}

// TestDecoder_MatchesRewards_gated pins that the rewards-gauge events
// (migration 0099) are gated on contract identity
// IDENTICALLY to trade/liquidity/reserves: a REGISTERED pool matches,
// an unregistered look-alike emitting the exact same topic does not.
// Uses real captured bytes (see decode_rewards_test.go) for the topic
// shapes, swapping only the ContractID — Matches() never reads e.Value.
func TestDecoder_MatchesRewards_gated(t *testing.T) {
	d := NewDecoder()
	registered := MainnetPools[0]
	const foreign = "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000"

	cases := []struct {
		name  string
		topic []string
	}{
		{"pool_state", []string{"AAAADwAAAApwb29sX3N0YXRlAAA="}},
		{"claim_reward", []string{
			"AAAADwAAAAxjbGFpbV9yZXdhcmQ=",
			"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==",
			"AAAAEgAAAAAAAAAAGFJvImUhe1Um7DcQIll44FVjzfnDHLalppun+3zFidQ=",
		}},
		{"rewards_gauge_add", []string{"AAAADwAAABFyZXdhcmRzX2dhdWdlX2FkZAAAAA=="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !d.Matches(events.Event{ContractID: registered, Topic: tc.topic}) {
				t.Errorf("registered pool %s not matched for %s", registered, tc.name)
			}
			if d.Matches(events.Event{ContractID: foreign, Topic: tc.topic}) {
				t.Errorf("foreign contract matched for %s — CS-026 injection vector open", tc.name)
			}
		})
	}
}

// TestDecoder_MatchesAdmin_routerOnlyGated pins that the two
// ROUTER-SCOPED governance kinds (config_rewards, pool_gauge_switch_token
// — migration 0100) stay gated on the CANONICAL ROUTER
// trust root only: a full-history r1 census finds ZERO
// pool-emitted occurrences of either, so a registered pool (and any
// arbitrary contract) must NOT match — see decode_admin.go's package
// doc.
func TestDecoder_MatchesAdmin_routerOnlyGated(t *testing.T) {
	d := NewDecoder()
	pool := MainnetPools[0]
	const flaggedRouter = "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ"

	cases := []struct {
		name  string
		topic []string
	}{
		{"config_rewards", []string{
			"AAAADwAAAA5jb25maWdfcmV3YXJkcwAA",
			"AAAAEAAAAAEAAAACAAAAEgAAAAEBXYCbqoen8nj67TgxiToTyzhZ6BokJeLbYyJFVbtOGgAAABIAAAABJbT82FmuwvpjSEOMSJs8PBDJi20hvk/TyzDLaJU++Xc=",
		}},
		{"pool_gauge_switch_token", []string{
			"AAAADwAAABdwb29sX2dhdWdlX3N3aXRjaF90b2tlbgA=",
			"AAAAEgAAAAFQkI25aXl99CnhS5sIYZCU5/Wh49ZuSaRsWLA/BuP6Vg==",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !d.Matches(events.Event{ContractID: MainnetRouter, Topic: tc.topic}) {
				t.Errorf("canonical router not matched for %s", tc.name)
			}
			// A registered POOL must not match these two kinds — the
			// trust root for them is the router, not the pool registry
			// (zero pool-emitted occurrences in the lake).
			if d.Matches(events.Event{ContractID: pool, Topic: tc.topic}) {
				t.Errorf("registered pool incorrectly matched router-only topic %s", tc.name)
			}
			// The flagged parallel router deployment must still
			// fail-closed — same posture as its trade events.
			if d.Matches(events.Event{ContractID: flaggedRouter, Topic: tc.topic}) {
				t.Errorf("flagged parallel router matched %s — CS-026 gap not closed", tc.name)
			}
		})
	}
}

// TestDecoder_MatchesPoolGovernance_recognized is the recognition
// regression for the pool-governance decoder gap (fix
// fix/aquarius-pool-governance-events). The seven POOL-EMITTABLE
// governance kinds — apply_upgrade, commit_upgrade, set_privileged_addrs,
// apply_transfer_ownership, commit_transfer_ownership,
// enable_emergency_mode, disable_emergency_mode — are legitimately
// emitted by the REGISTERED Aquarius pools (a protocol-wide staged WASM
// upgrade + pool-level ownership/emergency actions, ~1,679 real events).
// A gate of reg.IsFactory ONLY would make every pool-emitted
// occurrence return Matches()==false — an ADR-0033 recognition gap
// that also dropped the event from Decode. This pins that a REGISTERED
// pool AND the router now match, while a foreign contract and the
// flagged parallel router still fail-closed. Matches() reads
// only topic[0], so a bare topic[0] proves the gate.
func TestDecoder_MatchesPoolGovernance_recognized(t *testing.T) {
	d := NewDecoder()
	pool := MainnetPools[0]
	const (
		foreign       = "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000"
		flaggedRouter = "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ"
	)

	cases := []struct {
		name   string
		topic0 string
	}{
		{"apply_upgrade", "AAAADwAAAA1hcHBseV91cGdyYWRlAAAA"},
		{"commit_upgrade", "AAAADwAAAA5jb21taXRfdXBncmFkZQAA"},
		{"set_privileged_addrs", "AAAADwAAABRzZXRfcHJpdmlsZWdlZF9hZGRycw=="},
		{"apply_transfer_ownership", "AAAADwAAABhhcHBseV90cmFuc2Zlcl9vd25lcnNoaXA="},
		{"commit_transfer_ownership", "AAAADwAAABljb21taXRfdHJhbnNmZXJfb3duZXJzaGlwAAAA"},
		{"enable_emergency_mode", "AAAADwAAABVlbmFibGVfZW1lcmdlbmN5X21vZGUAAAA="},
		{"disable_emergency_mode", "AAAADwAAABZkaXNhYmxlX2VtZXJnZW5jeV9tb2RlAAA="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			topic := []string{tc.topic0}
			// A registered pool must now be RECOGNIZED (gap closed).
			if !d.Matches(events.Event{ContractID: pool, Topic: topic}) {
				t.Errorf("registered pool NOT matched for %s — recognition gap still open", tc.name)
			}
			// The router still matches (its rows are unchanged).
			if !d.Matches(events.Event{ContractID: MainnetRouter, Topic: topic}) {
				t.Errorf("canonical router not matched for %s", tc.name)
			}
			// A foreign contract must still fail-closed.
			if d.Matches(events.Event{ContractID: foreign, Topic: topic}) {
				t.Errorf("foreign contract matched %s — CS-026 injection vector open", tc.name)
			}
			// The flagged parallel router (neither reg.Has nor
			// reg.IsFactory) must still fail-closed.
			if d.Matches(events.Event{ContractID: flaggedRouter, Topic: topic}) {
				t.Errorf("flagged parallel router matched %s — CS-026 gap not closed", tc.name)
			}
		})
	}
}

// TestDecoder_Decode_PoolApplyUpgrade_endToEnd drives the full
// dispatcher seam (Matches gate → Decode → AdminEvent) for a REGISTERED
// pool's real 2-hash apply_upgrade, proving the recognition gap fix
// carries the event all the way to a projectable AdminEvent stamped with
// the POOL's contract_id (the aquarius_admin emitter column that
// distinguishes pool rows from router rows).
func TestDecoder_Decode_PoolApplyUpgrade_endToEnd(t *testing.T) {
	d := NewDecoder()
	const poolID = "CDKVJYMN34ZIEXSLNFYHVAFF6M6FM5E2U6OHXOTBKH2WLBULXOE53YDP"
	ev := events.Event{
		ContractID:     poolID,
		Ledger:         56505116,
		TxHash:         "f38a9207f724b9624ce39e15a1aef59197db4913bdc7cf74867db3906ef7a852",
		EventIndex:     2,
		LedgerClosedAt: "2025-04-07T08:42:31Z",
		Topic:          []string{"AAAADwAAAA1hcHBseV91cGdyYWRlAAAA"},
		Value:          "AAAAEAAAAAEAAAACAAAADQAAACAubx2u2HKIGsUr9jcygHbNgaO1pw5GUkRTo1QwnHo3hgAAAA0AAAAgWWrOi4VUNkeFEoIaLg7LApc7G60KQFfcVB/Qyk188Dc=",
	}
	if !d.Matches(ev) {
		t.Fatal("registered pool apply_upgrade not matched — recognition gap still open")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	av, ok := out[0].(AdminEvent)
	if !ok {
		t.Fatalf("got %T, want AdminEvent", out[0])
	}
	if av.ContractID != poolID {
		t.Errorf("ContractID = %q, want the emitting pool %q", av.ContractID, poolID)
	}
	if av.Kind != AdminApplyUpgrade {
		t.Errorf("Kind = %q, want %q", av.Kind, AdminApplyUpgrade)
	}
	if av.Target != "2e6f1daed872881ac52bf637328076cd81a3b5a70e46524453a354309c7a3786" {
		t.Errorf("Target = %q", av.Target)
	}
	if got := av.Attributes["wasm_hash_1"]; got != "596ace8b855436478512821a2e0ecb02973b1bad0a4057dc541fd0ca4d7cf037" {
		t.Errorf("wasm_hash_1 = %v", got)
	}
}

// TestDecoder_Decode_RewardsAndAdmin_endToEnd exercises Decode() (not
// just Matches()) for one representative kind from each new family,
// using real captured bytes, proving the dispatcher-facing seam wires
// decodeRewardsEvent / decodeAdminEvent correctly end-to-end.
func TestDecoder_Decode_RewardsAndAdmin_endToEnd(t *testing.T) {
	d := NewDecoder()
	closedAtStr := "2026-07-10T00:00:00Z"

	t.Run("rewards", func(t *testing.T) {
		out, err := d.Decode(events.Event{
			ContractID:     "CCFGZJTHQZGDZP5PK6WMLKHKJ72ACSVMJGCI2NFR7Q6EAVSKWLJB3ZH3",
			Ledger:         62000053,
			TxHash:         "3c3a180d0a7d467621df239a9370355e4e4249c8f98729f7163510dde8a80899",
			EventIndex:     1,
			LedgerClosedAt: closedAtStr,
			Topic: []string{
				"AAAADwAAAAxjbGFpbV9yZXdhcmQ=",
				"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==",
				"AAAAEgAAAAAAAAAAGFJvImUhe1Um7DcQIll44FVjzfnDHLalppun+3zFidQ=",
			},
			Value: "AAAAEAAAAAEAAAABAAAACgAAAAAAAAAAAAAADA0rT7g=",
		})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("got %d events, want 1", len(out))
		}
		rv, ok := out[0].(RewardsEvent)
		if !ok {
			t.Fatalf("got %T, want RewardsEvent", out[0])
		}
		if rv.Kind != RewardsClaimReward {
			t.Errorf("Kind = %q", rv.Kind)
		}
		if rv.EventKind() != "aquarius.rewards" {
			t.Errorf("EventKind() = %q", rv.EventKind())
		}
		if rv.Source() != SourceName {
			t.Errorf("Source() = %q", rv.Source())
		}
	})

	t.Run("admin", func(t *testing.T) {
		out, err := d.Decode(events.Event{
			ContractID:     MainnetRouter,
			Ledger:         59270084,
			TxHash:         "795ca1edf536361904eaf9e830f80766febb6564e5e8e76c1f81e1138c2db983",
			LedgerClosedAt: closedAtStr,
			Topic: []string{
				"AAAADwAAABdwb29sX2dhdWdlX3N3aXRjaF90b2tlbgA=",
				"AAAAEgAAAAFQkI25aXl99CnhS5sIYZCU5/Wh49ZuSaRsWLA/BuP6Vg==",
			},
			Value: "AAAAEAAAAAEAAAABAAAAAAAAAAE=",
		})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("got %d events, want 1", len(out))
		}
		av, ok := out[0].(AdminEvent)
		if !ok {
			t.Fatalf("got %T, want AdminEvent", out[0])
		}
		if av.Kind != AdminPoolGaugeSwitchToken {
			t.Errorf("Kind = %q", av.Kind)
		}
		if av.EventKind() != "aquarius.admin" {
			t.Errorf("EventKind() = %q", av.EventKind())
		}
		if av.Source() != SourceName {
			t.Errorf("Source() = %q", av.Source())
		}
	})
}

// TestDecoder_MatchesLiquidityReserves_gated pins that the new
// liquidity/reserves events are gated on contract identity IDENTICALLY
// to trades: a REGISTERED pool matches, an unregistered look-alike
// emitting the exact same topics does NOT (so it can't inject
// fabricated reserves).
func TestDecoder_MatchesLiquidityReserves_gated(t *testing.T) {
	d := NewDecoder() // production seed — MainnetPools[0] is registered.
	registered := MainnetPools[0]
	const foreign = "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000"

	cases := []struct {
		name  string
		topic []string
		body  string
	}{
		{"update_reserves", []string{realReservesTopic0}, realReservesBody},
		{"deposit_liquidity", []string{realDepositTopic0, realDepositTokenA, realDepositTokenB}, realDepositBody},
		{"withdraw_liquidity", []string{realWithdrawTopic0, realDepositTokenA, realDepositTokenB}, realWithdrawBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !d.Matches(events.Event{ContractID: registered, Topic: tc.topic, Value: tc.body}) {
				t.Errorf("registered pool %s not matched for %s", registered, tc.name)
			}
			if d.Matches(events.Event{ContractID: foreign, Topic: tc.topic, Value: tc.body}) {
				t.Errorf("foreign contract matched for %s — CS-026 injection vector open", tc.name)
			}
		})
	}
}

func TestDecoder_Decode_ReservesAndLiquidity(t *testing.T) {
	d := NewDecoder()
	pool := MainnetPools[0]
	closedAtStr := "2026-04-23T12:00:00Z"

	t.Run("reserves", func(t *testing.T) {
		out, err := d.Decode(events.Event{
			ContractID: pool, LedgerClosedAt: closedAtStr,
			Topic: []string{realReservesTopic0}, Value: realReservesBody,
		})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("got %d events, want 1", len(out))
		}
		rv, ok := out[0].(ReservesEvent)
		if !ok {
			t.Fatalf("got %T, want ReservesEvent", out[0])
		}
		if len(rv.Reserves) != 2 || rv.Source() != SourceName {
			t.Errorf("unexpected reserves event: %+v", rv)
		}
	})

	t.Run("deposit", func(t *testing.T) {
		out, err := d.Decode(events.Event{
			ContractID: pool, LedgerClosedAt: closedAtStr,
			Topic: []string{realDepositTopic0, realDepositTokenA, realDepositTokenB}, Value: realDepositBody,
		})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		lq, ok := out[0].(LiquidityEvent)
		if !ok {
			t.Fatalf("got %T, want LiquidityEvent", out[0])
		}
		if lq.Action != LiquidityDeposit {
			t.Errorf("Action = %q, want deposit", lq.Action)
		}
	})

	t.Run("withdraw", func(t *testing.T) {
		out, err := d.Decode(events.Event{
			ContractID: pool, LedgerClosedAt: closedAtStr,
			Topic: []string{realWithdrawTopic0, realDepositTokenA, realDepositTokenB}, Value: realWithdrawBody,
		})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		lq, ok := out[0].(LiquidityEvent)
		if !ok {
			t.Fatalf("got %T, want LiquidityEvent", out[0])
		}
		if lq.Action != LiquidityWithdraw {
			t.Errorf("Action = %q, want withdraw", lq.Action)
		}
	})
}

func TestDecoder_NameMatchesSourceName(t *testing.T) {
	if got := NewDecoder().Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}
