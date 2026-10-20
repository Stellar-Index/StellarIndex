package sep41_supply

import (
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// The SEP-41 supply amount lands in a NUMERIC column and drives `total_supply`, so ADR-0003's
// money invariant applies: parsing xdr.Int128Parts to int64(parts.Lo) is a rejected bug.
//
// Fuzzing beats table-testing here for two reasons:
//
//  1. The amount arrives in TWO on-wire SHAPES, a bare i128 or the CAP-67 map
//     { amount, to_muxed_id } (amountScVal). They MUST agree, or mints are dropped and
//     mint_total goes to zero. A differential target proves agreement over the whole domain.
//  2. The interesting values are the ones a hand-written fixture never uses: Lo above 2^63,
//     Hi non-zero, the 2^64 carry boundary, each a place an int64 truncation survives an
//     amount==int64(1_000_000) test unnoticed.
//
// Runs as a plain seed-corpus test under `go test`; the generative run is
// `go test -run=^$ -fuzz=FuzzSEP41Amount -fuzztime=20s ./internal/sources/sep41_supply/`.
func FuzzSEP41Amount(f *testing.F) {
	// Seeds: the real fixture amounts (dispatcher_adapter_test.go), the
	// int64/uint64 boundaries, and the largest representable i128.
	f.Add(int64(0), uint64(realMintAmount))
	f.Add(int64(0), uint64(realBurnAmount))
	f.Add(int64(0), uint64(0))
	f.Add(int64(0), uint64(1))
	f.Add(int64(0), uint64(1)<<62)
	f.Add(int64(0), uint64(1)<<63)          // first Lo that is negative as an int64
	f.Add(int64(0), ^uint64(0))             // 2^64-1: the carry boundary
	f.Add(int64(1), uint64(0))              // exactly 2^64
	f.Add(int64(1), ^uint64(0))             // 2^65-1
	f.Add(int64(1)<<62, ^uint64(0))         // deep in the Hi limb
	f.Add(int64(^uint64(0)>>1), ^uint64(0)) // max i128
	f.Add(int64(-1), uint64(0))             // negative: must be refused
	f.Add(int64(-1), ^uint64(0))            // -1

	f.Fuzz(func(t *testing.T, hi int64, lo uint64) {
		// The exact value the wire carries, computed independently of the
		// decoder: (hi << 64) + lo in two's complement.
		want := new(big.Int).Lsh(big.NewInt(hi), 64)
		want.Add(want, new(big.Int).SetUint64(lo))

		parts := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}

		bare := fuzzEncode(t, xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts})
		mapped := fuzzEncode(t, fuzzCAP67Body(t, parts))

		bareAmt, bareErr := decodeAmount(&events.Event{Value: bare})
		mapAmt, mapErr := decodeAmount(&events.Event{Value: mapped})

		// Property 1: the two on-wire shapes are one value; the supply row must be identical either way, since a shape-dependent
		// answer is how mint_total went to zero.
		if (bareErr == nil) != (mapErr == nil) {
			t.Fatalf("shapes disagree on acceptance for hi=%d lo=%d: bare err=%v, CAP-67 map err=%v",
				hi, lo, bareErr, mapErr)
		}

		// Property 2 — negatives are refused, in BOTH shapes. The kind
		// (mint/burn/clawback) discriminates direction, so a signed
		// amount would double-count the sign into the rollup.
		if want.Sign() < 0 {
			if bareErr == nil {
				t.Fatalf("negative amount %s was accepted (hi=%d lo=%d)", want, hi, lo)
			}
			return
		}

		if bareErr != nil {
			t.Fatalf("non-negative amount %s (hi=%d lo=%d) was refused: %v", want, hi, lo, bareErr)
		}

		// Property 3 (ADR-0003): the decoded amount is the exact i128, not its low limb; int64(parts.Lo) agrees only below 2^63, so
		// the assertion must run over the whole domain.
		if bareAmt.Cmp(want) != 0 {
			t.Fatalf("bare i128 decoded to %s, want %s (hi=%d lo=%d) — the i128 "+
				"was truncated, and this value lands in a NUMERIC supply column",
				bareAmt, want, hi, lo)
		}
		if mapAmt.Cmp(want) != 0 {
			t.Fatalf("CAP-67 map decoded to %s, want %s (hi=%d lo=%d)", mapAmt, want, hi, lo)
		}
	})
}

// fuzzCAP67Body wraps an i128 in the real CAP-67 body shape: Map { amount: i128, to_muxed_id:
// String }. Field order is deliberately amount-first here and to_muxed_id-first in the golden
// test's helper, because decode is by field NAME and must not depend on position.
func fuzzCAP67Body(t *testing.T, parts xdr.Int128Parts) xdr.ScVal {
	t.Helper()
	amountKey := xdr.ScSymbol("amount")
	muxedKey := xdr.ScSymbol("to_muxed_id")
	m := xdr.ScMap{
		{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &amountKey},
			Val: xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts},
		},
		{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &muxedKey},
			Val: stringScVal("Auto recharge transaction"),
		},
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func fuzzEncode(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}
