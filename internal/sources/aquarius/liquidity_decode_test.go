package aquarius

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Golden decode tests for the update_reserves / deposit_liquidity /
// withdraw_liquidity surface (migration 0089). The topic+body blobs
// below are UNTOUCHED base64 SCVals captured from the r1 ClickHouse
// lake (stellar.contract_events) on 2026-07-06 — real production wire
// format, decoded here to prove the reserve / amount / share i128
// paths line up with what the pool actually emitted.
//
// Source pool: CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE
// (a curated MainnetPools entry). All are 2-token (volatile) pools
// except realDeposit3Token, which is a 3-token stableswap deposit from
// CC3HFYXIBYO3NPPKHQMWZDNSGSTZ6DUG26WHDF2UGTC3VBY56SESDFQM — proving
// the N-token fan-out, not a hard-coded a/b shape.

const (
	// update_reserves — topic[0] only (no token addresses); body is a
	// Vec<i128> of post-state reserves.
	realReservesTopic0 = "AAAADwAAAA91cGRhdGVfcmVzZXJ2ZXMA"
	realReservesBody   = "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAI1MwpaN94AAAAKAAAAAAAAAAAAAApZwurvlA=="

	// deposit_liquidity — topics [Symbol, token_a, token_b];
	// body Vec<i128> = [amount_a, amount_b, shares].
	realDepositTopic0 = "AAAADwAAABFkZXBvc2l0X2xpcXVpZGl0eQAAAA=="
	realDepositTokenA = "AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w=="
	realDepositTokenB = "AAAAEgAAAAH11jazyNfMbuETxJua4J17ahzuszBS8wEhqdKJ5mYnUw=="
	realDepositBody   = "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAARdlkuAAAAAAKAAAAAAAAAAAAAAK6fe8wAAAAAAoAAAAAAAAAAAAAAAb8I6wA"

	// withdraw_liquidity — same shape as deposit.
	realWithdrawTopic0 = "AAAADwAAABJ3aXRoZHJhd19saXF1aWRpdHkAAA=="
	realWithdrawBody   = "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAAAHSpJMIAAAAKAAAAAAAAAAAAAAAEcbOuyAAAAAoAAAAAAAAAAAAAAAAL+QqG"

	// 3-token stableswap deposit (topic_count 4, body length 4).
	realDeposit3Topic0 = "AAAADwAAABFkZXBvc2l0X2xpcXVpZGl0eQAAAA=="
	realDeposit3TokenA = "AAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dw=="
	realDeposit3TokenB = "AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w=="
	realDeposit3TokenC = "AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg=="
	realDeposit3Body   = "AAAAEAAAAAEAAAAEAAAACgAAAAAAAAAAAAAAAAxXQGAAAAAKAAAAAAAAAAAAAAAAAIVHAAAAAAoAAAAAAAAAAAAAAAA7msoAAAAACgAAAAAAAAAAAAAAAAAMChw="
)

var closedAtTest = time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad big.Int literal %q", s)
	}
	return n
}

// ─── update_reserves ─────────────────────────────────────────────

func TestDecodeReserves_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID:     "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE",
		Ledger:         57_725_480,
		TxHash:         "76cc361f2530929b738ed7f4e61c8ee9764281f7a3ef74904215fb4c0ce512e2",
		OperationIndex: 0,
		EventIndex:     5,
		Topic:          []string{realReservesTopic0},
		Value:          realReservesBody,
	}
	// classify must match the package constant, or the decoder never
	// fires on a real event.
	if got := classify(e); got != EventUpdateReserves {
		t.Fatalf("classify real update_reserves = %q, want %q", got, EventUpdateReserves)
	}
	rv, err := decodeReserves(e, closedAtTest)
	if err != nil {
		t.Fatalf("decodeReserves: %v", err)
	}
	want := []string{"621443286710238", "11380638543764"}
	if len(rv.Reserves) != len(want) {
		t.Fatalf("reserves len = %d, want %d", len(rv.Reserves), len(want))
	}
	for i, w := range want {
		if rv.Reserves[i].String() != w {
			t.Errorf("reserve[%d] = %s, want %s", i, rv.Reserves[i], w)
		}
	}
	if rv.ContractID != e.ContractID || rv.Ledger != e.Ledger || rv.EventIndex != 5 {
		t.Errorf("identity mismatch: %+v", rv)
	}
	if !rv.ObservedAt.Equal(closedAtTest) {
		t.Errorf("ObservedAt = %v, want %v", rv.ObservedAt, closedAtTest)
	}
}

// ─── deposit_liquidity / withdraw_liquidity ──────────────────────

func TestDecodeLiquidity_realDeposit(t *testing.T) {
	e := &events.Event{
		ContractID: "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE",
		Ledger:     53_158_643,
		TxHash:     "de0ffb02a16e43d721be2849f82d26bfec1bd533f2c25b7a453dd6e07b60ba3d",
		EventIndex: 3,
		Topic:      []string{realDepositTopic0, realDepositTokenA, realDepositTokenB},
		Value:      realDepositBody,
	}
	if got := classify(e); got != EventDepositLiquidity {
		t.Fatalf("classify real deposit = %q, want %q", got, EventDepositLiquidity)
	}
	lq, err := decodeLiquidity(e, LiquidityDeposit, closedAtTest)
	if err != nil {
		t.Fatalf("decodeLiquidity(deposit): %v", err)
	}
	if lq.Action != LiquidityDeposit {
		t.Errorf("Action = %q, want deposit", lq.Action)
	}
	wantTokens := []string{
		"CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK",
		"CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY",
	}
	wantAmounts := []string{"300000000000", "3000000000000"}
	if len(lq.Tokens) != 2 || len(lq.Amounts) != 2 {
		t.Fatalf("tokens=%d amounts=%d, want 2/2", len(lq.Tokens), len(lq.Amounts))
	}
	for i := range wantTokens {
		if lq.Tokens[i] != wantTokens[i] {
			t.Errorf("token[%d] = %s, want %s", i, lq.Tokens[i], wantTokens[i])
		}
		if lq.Amounts[i].String() != wantAmounts[i] {
			t.Errorf("amount[%d] = %s, want %s", i, lq.Amounts[i], wantAmounts[i])
		}
	}
	if lq.Shares.String() != "30000000000" {
		t.Errorf("shares = %s, want 30000000000", lq.Shares)
	}
}

func TestDecodeLiquidity_realWithdraw(t *testing.T) {
	e := &events.Event{
		ContractID: "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE",
		Ledger:     53_317_087,
		TxHash:     "d9b47f6360660d2c36d8cafd21516a827b5e8794ef3c532663125f7d7fabb87c",
		EventIndex: 4,
		Topic:      []string{realWithdrawTopic0, realDepositTokenA, realDepositTokenB},
		Value:      realWithdrawBody,
	}
	if got := classify(e); got != EventWithdrawLiquidity {
		t.Fatalf("classify real withdraw = %q, want %q", got, EventWithdrawLiquidity)
	}
	lq, err := decodeLiquidity(e, LiquidityWithdraw, closedAtTest)
	if err != nil {
		t.Fatalf("decodeLiquidity(withdraw): %v", err)
	}
	wantAmounts := []string{"1957242050", "19087470280"}
	for i := range wantAmounts {
		if lq.Amounts[i].String() != wantAmounts[i] {
			t.Errorf("amount[%d] = %s, want %s", i, lq.Amounts[i], wantAmounts[i])
		}
	}
	if lq.Shares.String() != "200870534" {
		t.Errorf("shares = %s, want 200870534", lq.Shares)
	}
}

// TestDecodeLiquidity_real3TokenDeposit proves the N-token fan-out:
// a stableswap deposit with 3 tokens (topic_count 4, body length 4 =
// 3 amounts + shares) decodes to 3 (token, amount) pairs, not a
// truncated 2-token a/b shape.
func TestDecodeLiquidity_real3TokenDeposit(t *testing.T) {
	e := &events.Event{
		ContractID: "CC3HFYXIBYO3NPPKHQMWZDNSGSTZ6DUG26WHDF2UGTC3VBY56SESDFQM",
		Ledger:     60_000_000,
		EventIndex: 1,
		Topic:      []string{realDeposit3Topic0, realDeposit3TokenA, realDeposit3TokenB, realDeposit3TokenC},
		Value:      realDeposit3Body,
	}
	lq, err := decodeLiquidity(e, LiquidityDeposit, closedAtTest)
	if err != nil {
		t.Fatalf("decodeLiquidity(3-token deposit): %v", err)
	}
	if len(lq.Tokens) != 3 || len(lq.Amounts) != 3 {
		t.Fatalf("tokens=%d amounts=%d, want 3/3", len(lq.Tokens), len(lq.Amounts))
	}
	for i, a := range lq.Amounts {
		if a.Sign() <= 0 {
			t.Errorf("amount[%d] = %s, want positive", i, a)
		}
	}
	if lq.Shares.Sign() <= 0 {
		t.Errorf("shares = %s, want positive", lq.Shares)
	}
}

// ─── i128 discipline + malformed rejection (SDK-built) ───────────

// encodeAmountVec builds a Vec<i128> body from arbitrary big.Ints —
// the reserve vector / [amounts…, shares] shape.
func encodeAmountVec(t *testing.T, ns ...*big.Int) string {
	t.Helper()
	elts := make([]xdr.ScVal, len(ns))
	for i, n := range ns {
		hi, lo := splitBigInt128(n)
		p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
		elts[i] = xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
	}
	vec := xdr.ScVec(elts)
	pvec := &vec
	body := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pvec}
	b, err := body.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestDecodeReserves_i128Discipline(t *testing.T) {
	// A reserve past 2^64 — catches a truncate-to-int64/uint64 bug in
	// the hi word (ADR-0003).
	big1 := mustBig(t, "123456789012345678901234567890")
	e := &events.Event{
		ContractID: "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE",
		Topic:      []string{realReservesTopic0},
		Value:      encodeAmountVec(t, big1, big.NewInt(0)),
	}
	rv, err := decodeReserves(e, closedAtTest)
	if err != nil {
		t.Fatalf("decodeReserves: %v", err)
	}
	if rv.Reserves[0].BigInt().Cmp(big1) != 0 {
		t.Errorf("large i128 lost bits: got %s, want %s", rv.Reserves[0], big1)
	}
	// A zero reserve is legal (freshly drained pool) — must not reject.
	if rv.Reserves[1].Sign() != 0 {
		t.Errorf("reserve[1] = %s, want 0", rv.Reserves[1])
	}
}

func TestDecodeReserves_emptyVectorRejected(t *testing.T) {
	e := &events.Event{
		ContractID: "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE",
		Topic:      []string{realReservesTopic0},
		Value:      encodeAmountVec(t),
	}
	if _, err := decodeReserves(e, closedAtTest); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("empty reserve vector: err = %v, want ErrMalformedPayload", err)
	}
}

func TestDecodeLiquidity_bodyLengthMismatchRejected(t *testing.T) {
	// 2 tokens in topics but only 2 body elements (need 2 amounts + 1
	// share = 3). A schema-drift guard, not a truncation.
	e := &events.Event{
		ContractID: "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE",
		Topic:      []string{realDepositTopic0, realDepositTokenA, realDepositTokenB},
		Value:      encodeAmountVec(t, big.NewInt(1), big.NewInt(2)),
	}
	if _, err := decodeLiquidity(e, LiquidityDeposit, closedAtTest); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("body-length mismatch: err = %v, want ErrMalformedPayload", err)
	}
}

func TestDecodeLiquidity_tooFewTopicsRejected(t *testing.T) {
	// Only the symbol topic, no token addresses (the 1 anomalous
	// withdraw_liquidity row observed in the lake). Must reject.
	e := &events.Event{
		ContractID: "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE",
		Topic:      []string{realWithdrawTopic0},
		Value:      encodeAmountVec(t, big.NewInt(1)),
	}
	if _, err := decodeLiquidity(e, LiquidityWithdraw, closedAtTest); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("too-few-topics: err = %v, want ErrMalformedPayload", err)
	}
}

// ─── adapter gating (ADR-0035/0040, CS-026) ──────────────────────

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
