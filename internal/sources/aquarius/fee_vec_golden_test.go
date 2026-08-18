package aquarius

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

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
// (stellar.contract_events); captured 2026-08-18.
//
// Wire shape / semantics (see decodeSetProtocolFee): the Aquarius pool
// contract's fee API is a single set_protocol_fee_fraction /
// get_protocol_fee_fraction with a `new_fraction` topic (verified across
// every pool-WASM generation's disassembly — docs/operations/wasm-audits
// /evidence/r1-walk-2026-05-01/disasm), so the one u32 is the new
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

// TestDecodeFee_setProtocolFee_mapPathUnchanged proves the two-shape
// branch left the Map path intact AND that the Map form is tagged
// HasOldFee=true (so its per-token old values are persisted, not
// NULLed). realSetFeeBody / realSetFeeTopic0 are defined in
// protocol_fee_test.go (same package).
func TestDecodeFee_setProtocolFee_mapPathUnchanged(t *testing.T) {
	e := &events.Event{
		ContractID: "CCNXGPE4AQCSNEBZO3XJDKKDI3CRLYMVS6UWBBTVDLALLWMJEXBORQ2A",
		Ledger:     63_000_000,
		TxHash:     "aa",
		Topic:      []string{realSetFeeTopic0},
		Value:      realSetFeeBody,
	}
	fe, err := decodeFee(e, closedAtTest, EventSetProtocolFee)
	if err != nil {
		t.Fatalf("decodeFee (Map): %v", err)
	}
	if fe.Fee0New != 4 || fe.Fee0Old != 0 || fe.Fee1New != 4 || fe.Fee1Old != 0 {
		t.Errorf("fees = (%d,%d,%d,%d), want (4,0,4,0)", fe.Fee0New, fe.Fee0Old, fe.Fee1New, fe.Fee1Old)
	}
	if !fe.HasOldFee {
		t.Errorf("HasOldFee = false, want true (Map body carries the old values)")
	}
}
