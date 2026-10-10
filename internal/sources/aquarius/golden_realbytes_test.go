package aquarius

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── Golden real-lake decode-VALUE tests ───────────────────
//
// See internal/sources/defindex/golden_realbytes_test.go for the full
// statement of the convention. In one line: capture the EXACT on-chain
// bytes of one real event, drive the REAL production decoder through
// BOTH seams — Decoder.Matches (the ADR-0035 contract-identity gate)
// and Decoder.Decode (classify + body decode + consumer.Event wrap) —
// and assert the EXACT decoded field VALUES of the emitted
// consumer.Event. This is the axis ADR-0033 completeness does not
// cover: recognition (Matches) and projection (row count) can BOTH pass
// while the decoded values are wrong.
//
// decode_admin_test.go already pins these governance bodies at the
// decodeAdminEvent() helper level; the tests below are the strictly
// additive complement — they prove the SAME real bytes survive the
// WHOLE production path, i.e. the registry gate admits the emitting
// POOL (not just the router) AND the arity-decoded values reach the
// consumer.Event. That end-to-end gate+value pairing is what a
// projection/recognition check alone can't see.
//
// All fixtures are byte-identical to what r1's ClickHouse lake
// (stellar.contract_events) stores; each cites its ledger_seq /
// tx_hash / op_index / event_index for re-verification.

// mainnetPool is a curated Aquarius pool (in MainnetPools) that emitted
// the arity-sensitive governance events below during the protocol-wide
// staged WASM upgrade. Because it is registered, Matches() admits its
// pool-emitted governance events (reg.Has) — the exact path a
// router-only gate fails closed into an ADR-0033 recognition gap.
const mainnetPool = "CDKVJYMN34ZIEXSLNFYHVAFF6M6FM5E2U6OHXOTBKH2WLBULXOE53YDP"

// decodeOneAdmin drives a real governance event through the production
// seams and returns the single AdminEvent the pipeline would emit.
func decodeOneAdmin(t *testing.T, ev events.Event) AdminEvent {
	t.Helper()
	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatalf("Matches(%s) = false — a registered pool's own governance event must pass the gate", ev.Topic[0])
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode err = %v, want a decoded AdminEvent", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want exactly 1", len(out))
	}
	av, ok := out[0].(AdminEvent)
	if !ok {
		t.Fatalf("Decode emitted %T, want aquarius.AdminEvent", out[0])
	}
	if av.EventKind() != "aquarius.admin" {
		t.Errorf("EventKind = %q, want aquarius.admin", av.EventKind())
	}
	return av
}

// TestGolden_aquariusPoolApplyUpgrade2Hash_ledger56505116 — the
// headline arity surface. A POOL apply_upgrade carries a
// 2-element Vec[Bytes] (applied hash + one staged predecessor). The
// old `len != 1` guard rejected every pool body (686 apply_upgrade
// events / 320 pools dropped). By arity: element[0] → Target,
// element[1] → Attributes["wasm_hash_1"], and NO wasm_hash_2.
//
//	ledger_seq 56,505,116 (2025-04-07 08:42:31 UTC)
//	tx_hash    f38a9207f724b9624ce39e15a1aef59197db4913bdc7cf74867db3906ef7a852
//	op 0, event 2, pool CDKVJYMN…
func TestGolden_aquariusPoolApplyUpgrade2Hash_ledger56505116(t *testing.T) {
	t.Parallel()
	av := decodeOneAdmin(t, events.Event{
		ContractID:     mainnetPool,
		Ledger:         56505116,
		LedgerClosedAt: "2025-04-07T08:42:31Z",
		TxHash:         "f38a9207f724b9624ce39e15a1aef59197db4913bdc7cf74867db3906ef7a852",
		OperationIndex: 0,
		EventIndex:     2,
		Topic:          []string{"AAAADwAAAA1hcHBseV91cGdyYWRlAAAA"},
		Value:          "AAAAEAAAAAEAAAACAAAADQAAACAubx2u2HKIGsUr9jcygHbNgaO1pw5GUkRTo1QwnHo3hgAAAA0AAAAgWWrOi4VUNkeFEoIaLg7LApc7G60KQFfcVB/Qyk188Dc=",
	})

	if av.Kind != AdminApplyUpgrade {
		t.Errorf("Kind = %q, want %q", av.Kind, AdminApplyUpgrade)
	}
	// element[0] (the applied wasm hash) → Target.
	if want := "2e6f1daed872881ac52bf637328076cd81a3b5a70e46524453a354309c7a3786"; av.Target != want {
		t.Errorf("Target = %q, want element[0] hash %q", av.Target, want)
	}
	// element[1] (staged predecessor) → wasm_hash_1.
	// Mutation guard (non-vacuous): flip one nibble of either want value
	// and the test FAILS — the arity-decoded hashes are pinned by value.
	if want := "596ace8b855436478512821a2e0ecb02973b1bad0a4057dc541fd0ca4d7cf037"; av.Attributes["wasm_hash_1"] != want {
		t.Errorf("wasm_hash_1 = %v, want element[1] hash %q", av.Attributes["wasm_hash_1"], want)
	}
	if _, present := av.Attributes["wasm_hash_2"]; present {
		t.Error("2-element apply_upgrade must NOT produce wasm_hash_2 (arity guard)")
	}
}

// TestGolden_aquariusPoolCommitUpgrade3Hash_ledger62265929 — the busiest
// arity variation: a POOL commit_upgrade with a 3-element body (proposed
// hash + two staged). element[0] → Target, [1] → wasm_hash_1,
// [2] → wasm_hash_2.
//
//	ledger_seq 62,265,929 (2026-04-24 13:25:37 UTC)
//	tx_hash    f9bfe04a5b82777e669d0a7571e6912273a52957149ab41f9d51004790890e54
//	op 0, event 7, pool CDKVJYMN…
func TestGolden_aquariusPoolCommitUpgrade3Hash_ledger62265929(t *testing.T) {
	t.Parallel()
	av := decodeOneAdmin(t, events.Event{
		ContractID:     mainnetPool,
		Ledger:         62265929,
		LedgerClosedAt: "2026-04-24T13:25:37Z",
		TxHash:         "f9bfe04a5b82777e669d0a7571e6912273a52957149ab41f9d51004790890e54",
		OperationIndex: 0,
		EventIndex:     7,
		Topic:          []string{"AAAADwAAAA5jb21taXRfdXBncmFkZQAA"},
		Value:          "AAAAEAAAAAEAAAADAAAADQAAACDxB34Ld9peYtWW4Trq5BYBBMrZnn738xg6bJtuyedHzQAAAA0AAAAgB7qzDrk/nRlLB5o/ArqGghroK51HcKOMa3uFJN+eKE8AAAANAAAAIPeTf46MBtjP0jgzqmEu0HRatu5q32atO/e7r1n9Ya4Z",
	})

	if av.Kind != AdminCommitUpgrade {
		t.Errorf("Kind = %q, want %q", av.Kind, AdminCommitUpgrade)
	}
	if want := "f1077e0b77da5e62d596e13aeae4160104cad99e7ef7f3183a6c9b6ec9e747cd"; av.Target != want {
		t.Errorf("Target = %q, want element[0] hash %q", av.Target, want)
	}
	if want := "07bab30eb93f9d194b079a3f02ba86821ae82b9d4770a38c6b7b8524df9e284f"; av.Attributes["wasm_hash_1"] != want {
		t.Errorf("wasm_hash_1 = %v, want %q", av.Attributes["wasm_hash_1"], want)
	}
	if want := "f7937f8e8c06d8cfd23833aa612ed0745ab6ee6adf66ad3bf7bbaf59fd61ae19"; av.Attributes["wasm_hash_2"] != want {
		t.Errorf("wasm_hash_2 = %v, want %q", av.Attributes["wasm_hash_2"], want)
	}
	if _, present := av.Attributes["wasm_hash_3"]; present {
		t.Error("3-element commit_upgrade must NOT produce wasm_hash_3 (arity guard)")
	}
}

// TestGolden_aquariusPoolSetPrivilegedAddrs_ledger57697794 — the third
// governance shape (the "at least one of…" requirement): a POOL
// set_privileged_addrs in its 5-element (post-57.7M) wire generation.
// Exercises the richest admin decode: three role addresses + an
// addr_list Vec + the v2 trailing addr_3.
//
//	ledger_seq 57,697,794 (2025-06-24 17:08:38 UTC)
//	tx_hash    30ba5813f4e584ce152865799460d53d5d16830450f7d2bedfa582373d3f6e1d
//	op 0, event 0, pool CBL7MWLE…
func TestGolden_aquariusPoolSetPrivilegedAddrs_ledger57697794(t *testing.T) {
	t.Parallel()
	av := decodeOneAdmin(t, events.Event{
		ContractID:     "CBL7MWLEZ4SU6YC5XL4T3WXKNKNO2UQVDVONOQSW5VVCYFWORROHY4AM",
		Ledger:         57697794,
		LedgerClosedAt: "2025-06-24T17:08:38Z",
		TxHash:         "30ba5813f4e584ce152865799460d53d5d16830450f7d2bedfa582373d3f6e1d",
		OperationIndex: 0,
		EventIndex:     0,
		Topic:          []string{"AAAADwAAABRzZXRfcHJpdmlsZWdlZF9hZGRycw=="},
		Value:          "AAAAEAAAAAEAAAAFAAAAEgAAAAAAAAAAr4UDYWd/ywvTsSRB0NRM2w7KoisPZcPb4fpZk+XD67QAAAASAAAAAAAAAABrB99Lh3p1xYtFgkxWsF7lnsSvirC4yXmnLxRYF7aVBAAAABIAAAAAAAAAADzAe929VHnCmayZRVHmn90SJaJYM9yQ/RXerE7FSrO8AAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAPMrM0BiS9+voAw3nyHOzk4mPbaTXHVu8AMIg4+5A+4oAAAASAAAAAAAAAAB7/A6mWvQQVdc774FmP/p4xb1sMlRWTophyo0AhpckYw==",
	})

	if av.Kind != AdminSetPrivilegedAddrs {
		t.Errorf("Kind = %q, want %q", av.Kind, AdminSetPrivilegedAddrs)
	}
	for _, tc := range []struct{ key, want string }{
		{"addr_0", "GCXYKA3BM574WC6TWESEDUGUJTNQ5SVCFMHWLQ634H5FTE7FYPV3JH3X"},
		{"addr_1", "GBVQPX2LQ55HLRMLIWBEYVVQL3SZ5RFPRKYLRSLZU4XRIWAXW2KQIMMD"},
		{"addr_2", "GA6MA665XVKHTQUZVSMUKUPGT7OREJNCLAZ5ZEH5CXPKYTWFJKZ3YSEK"},
		{"addr_3", "GB57YDVGLL2BAVOXHPXYCZR77J4MLPLMGJKFMTUKMHFI2AEGS4SGGW7N"}, // v2 trailing role addr
	} {
		if got := av.Attributes[tc.key]; got != tc.want {
			t.Errorf("%s = %v, want %q", tc.key, got, tc.want)
		}
	}
	list, ok := av.Attributes["addr_list"].([]string)
	if !ok || len(list) != 1 || list[0] != "GA6MVTGQDCJPP27IAMG6PSDTWOJYTD3NUTLR2W54ADBCBY7OID5YUDSI" {
		t.Errorf("addr_list = %v, want one entry GA6MVTGQ…", av.Attributes["addr_list"])
	}
}

// TestGolden_aquariusPoolTrade_ledger55436194 — a real pool trade,
// decoded to exact token_in/token_out identities + sold/bought amounts
// + taker. This is the value axis for the price-bearing event: an
// arity/positional bug in the 3-i128 tuple (sold, bought, fee), or a
// topic-slot mix-up (token_in vs token_out), would produce the right
// row COUNT with wrong prices, which only a golden value assertion
// catches.
//
//	ledger_seq 55,436,194 (2025-01-25 12:42:09 UTC)
//	tx_hash    90565c41f4a73a443e861e01ba725ae44b38bf1f5a25e12a69b68dcc20c793ff
//	op 0, event 7, pool CDKVJYMN…
//	sold  180307530471 of CAUIKL3I…  (token_in / topic[1])
//	bought 21417376123 of CAS3J7GY…  (the XLM SAC — token_out / topic[2])
//	taker  CBQDHNBF… (the Aquarius router — a contract taker, realistic)
func TestGolden_aquariusPoolTrade_ledger55436194(t *testing.T) {
	t.Parallel()
	const (
		wantTokenIn  = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
		wantTokenOut = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" // MainnetXLMSAC
		wantSold     = "180307530471"
		wantBought   = "21417376123"
		wantTaker    = MainnetRouter
	)

	ev := events.Event{
		Type:           "contract",
		ContractID:     mainnetPool,
		Ledger:         55436194,
		LedgerClosedAt: "2025-01-25T12:42:09Z",
		TxHash:         "90565c41f4a73a443e861e01ba725ae44b38bf1f5a25e12a69b68dcc20c793ff",
		OperationIndex: 0,
		EventIndex:     7,
		Topic: []string{
			"AAAADwAAAAV0cmFkZQAAAA==",                                 // Symbol("trade")
			"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==", // Address(token_in)
			"AAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dw==", // Address(token_out)
			"AAAAEgAAAAFgM7QlDnBOMU+wZJc9GF25IsrgvScrpb/xmqxXDxKsLw==", // Address(user)
		},
		Value: "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAAKfsqkucAAAAKAAAAAAAAAAAAAAAE/JM5ewAAAAoAAAAAAAAAAAAAAAABRyFf",
	}

	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatal("Matches(real pool trade) = false — a curated MainnetPool's own trade must pass the gate")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode(real pool trade) err = %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want exactly 1 (Aquarius trades are single-pair)", len(out))
	}
	te, ok := out[0].(TradeEvent)
	if !ok {
		t.Fatalf("Decode emitted %T, want aquarius.TradeEvent", out[0])
	}
	tr := te.Trade

	// Both legs are Soroban tokens; the pair direction is
	// (token_in / token_out) = (base / quote).
	if tr.Pair.Base.Type != canonical.AssetSoroban || tr.Pair.Base.ContractID != wantTokenIn {
		t.Errorf("Base (token_in) = %+v, want soroban %q", tr.Pair.Base, wantTokenIn)
	}
	if tr.Pair.Quote.Type != canonical.AssetSoroban || tr.Pair.Quote.ContractID != wantTokenOut {
		t.Errorf("Quote (token_out) = %+v, want soroban %q", tr.Pair.Quote, wantTokenOut)
	}
	// Amounts: BaseAmount = sold (body[0]), QuoteAmount = bought (body[1]).
	// Mutation guard (non-vacuous): change wantSold/wantBought by one
	// digit, or swap wantTokenIn/wantTokenOut, and the test FAILS — the
	// values and their positions are pinned.
	if got := tr.BaseAmount.String(); got != wantSold {
		t.Errorf("BaseAmount (sold) = %q, want %q", got, wantSold)
	}
	if got := tr.QuoteAmount.String(); got != wantBought {
		t.Errorf("QuoteAmount (bought) = %q, want %q", got, wantBought)
	}
	if tr.Taker != wantTaker {
		t.Errorf("Taker = %q, want %q (the router)", tr.Taker, wantTaker)
	}
	// OpIndex fans out by (op, event) so multi-event ops don't collide on
	// the trades PK — pin the exact fanout of (0, 7).
	if want := canonical.FanoutOpIndex(0, 7); tr.OpIndex != want {
		t.Errorf("OpIndex = %d, want FanoutOpIndex(0,7)=%d", tr.OpIndex, want)
	}
	if tr.Source != SourceName || tr.Ledger != 55436194 || tr.TxHash != ev.TxHash {
		t.Errorf("header not preserved: source=%q ledger=%d tx=%q", tr.Source, tr.Ledger, tr.TxHash)
	}
}

// ─── Golden real-lake decode-VALUE test: set_protocol_fee Vec body ──
//
// The migration-0129 decoder only handled the Map body shape
// (Map[fee_protocol{0,1}_{new,old}: u32]) — the shape sampled from the
// three lake-wide events on contracts that are NOT registered Aquarius
// pools. But EVERY set_protocol_fee event on a REGISTERED Aquarius pool
// carries a DIFFERENT body: a single-element Vec[u32] holding the new
// pool-wide protocol-fee fraction. 163 such events exist lake-wide, all
// byte-identical Vec[u32(5000)] (the June-2025 governance sweep that set
// 160 registered pools in one tx at ledger 57,697,910, plus later
// stragglers). Under the ADR-0035 contract-identity gate ONLY registered
// pools reach the decoder, so in production the Vec form is the ONLY
// set_protocol_fee shape that arrives — and the Map-only decoder dropped
// every one of them (scval.AsMap fails on a Vec), leaving them
// "undecodable-but-matched" and blocking aquarius completeness
// certification.
//
// This test captures the EXACT on-chain bytes of two real registered-
// pool events and drives them through BOTH production seams —
// Decoder.Matches (the registry gate) and Decoder.Decode (classify +
// body decode + consumer.Event wrap) — asserting the exact decoded
// FeeEvent field values. Fixtures byte-identical to r1's ClickHouse lake
// (stellar.contract_events).
//
// Wire shape / semantics (see decodeSetProtocolFee): the Aquarius pool
// contract's fee API is a single set_protocol_fee_fraction /
// get_protocol_fee_fraction with a `new_fraction` topic (verified across
// every pool-WASM generation's disassembly — docs/operations/wasm-audits),
// so the one u32 is the new
// fraction for the WHOLE pool: Fee0New == Fee1New == fraction. The body
// carries NO old value, so HasOldFee is false (the sink lands the *_old
// columns NULL, never a fabricated 0).

// realVecSetFeeBody is the data_xdr of every registered-pool
// set_protocol_fee event: SCV_VEC of length 1 holding SCV_U32(5000)
// (raw hex 00000010 00000001 00000001 00000003 00001388).
const realVecSetFeeBody = "AAAAEAAAAAEAAAABAAAAAwAAE4g="

// The new pool-wide protocol-fee fraction carried by every observed
// registered-pool set_protocol_fee Vec body (0x1388).
const wantVecFeeFraction = 5000

// decodeOneFee drives a real fee event through the production seams
// (Matches gate + Decode) and returns the single FeeEvent emitted.
func decodeOneFee(t *testing.T, ev events.Event) FeeEvent {
	t.Helper()
	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatalf("Matches(%s) = false — a registered pool's set_protocol_fee must pass the gate", ev.Topic[0])
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode err = %v, want a decoded FeeEvent", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want exactly 1", len(out))
	}
	fe, ok := out[0].(FeeEvent)
	if !ok {
		t.Fatalf("Decode emitted %T, want aquarius.FeeEvent", out[0])
	}
	if fe.EventKind() != "aquarius.fee" {
		t.Errorf("EventKind = %q, want aquarius.fee", fe.EventKind())
	}
	return fe
}

// TestGolden_aquariusSetProtocolFeeVec_ledger57697910 — the headline
// fix. Two registered pools (both in MainnetPools) had their protocol
// fee set in one governance tx; the decoder must land BOTH.
//
//	ledger 57,697,910
//	tx     f06be70b03e1a32fe5584aa014e55858e299d0a40d2bb7dcdbbabb4a6822bc7e
//	op 0, event 0 → pool CBL7MWLE…
//	op 0, event 1 → pool CATP23X4…
func TestGolden_aquariusSetProtocolFeeVec_ledger57697910(t *testing.T) {
	const txHash = "f06be70b03e1a32fe5584aa014e55858e299d0a40d2bb7dcdbbabb4a6822bc7e"
	cases := []struct {
		name     string
		contract string
		eventIdx int
	}{
		{"pool_CBL7MWLE", "CBL7MWLEZ4SU6YC5XL4T3WXKNKNO2UQVDVONOQSW5VVCYFWORROHY4AM", 0},
		{"pool_CATP23X4", "CATP23X4FIYDOSPTGJVVI7RTINYQUA32UGG5CORAXDGUMJCHQFFCERN6", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := decodeOneFee(t, events.Event{
				ContractID:     tc.contract,
				Ledger:         57_697_910,
				LedgerClosedAt: "2025-06-24T17:19:41Z",
				TxHash:         txHash,
				OperationIndex: 0,
				EventIndex:     tc.eventIdx,
				Topic:          []string{realSetFeeTopic0},
				Value:          realVecSetFeeBody,
			})
			if fe.Kind != EventSetProtocolFee {
				t.Errorf("Kind = %q, want %q", fe.Kind, EventSetProtocolFee)
			}
			// The single pool-wide new fraction lands on BOTH token sides.
			if fe.Fee0New != wantVecFeeFraction || fe.Fee1New != wantVecFeeFraction {
				t.Errorf("(Fee0New,Fee1New) = (%d,%d), want (%d,%d)",
					fe.Fee0New, fe.Fee1New, wantVecFeeFraction, wantVecFeeFraction)
			}
			// The Vec body carries no old value: HasOldFee false so the
			// sink lands *_old NULL, and the struct's old fields stay zero.
			if fe.HasOldFee {
				t.Errorf("HasOldFee = true, want false (Vec body has no old value)")
			}
			if fe.Fee0Old != 0 || fe.Fee1Old != 0 {
				t.Errorf("(Fee0Old,Fee1Old) = (%d,%d), want (0,0) — no old on the wire",
					fe.Fee0Old, fe.Fee1Old)
			}
			if fe.ContractID != tc.contract {
				t.Errorf("ContractID = %q, want %q", fe.ContractID, tc.contract)
			}
		})
	}
}
