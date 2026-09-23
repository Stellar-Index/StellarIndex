package aquarius

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

func i128Val(n *big.Int) xdr.ScVal {
	hi, lo := splitBigInt128(n)
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func contractAddrVal(t *testing.T, c string) xdr.ScVal {
	t.Helper()
	sv := decodeB64ScVal(t, encodeContractAddrFromStrkey(t, c))
	return sv
}

func marshalB64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func vecVal(elts ...xdr.ScVal) xdr.ScVal {
	vec := xdr.ScVec(elts)
	pvec := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pvec}
}

// claimAmountEvents builds the three Aquarius claim events whose i128
// amount lands in a column carrying CHECK (amount >= 0) — migration
// 0099 (aquarius_rewards_events.amount) and 0129
// (aquarius_protocol_fee.amount).
func claimAmountEvents(t *testing.T, amount *big.Int) map[string]events.Event {
	t.Helper()
	token := makeContractStrkey(t, 0x11)
	user := makeAccountStrkey(t, 0x22)
	return map[string]events.Event{
		EventClaimReward: {
			Topic: []string{TopicSymbolClaimReward, encodeContractAddrFromStrkey(t, token), encodeAccountAddrFromStrkey(t, user)},
			Value: marshalB64(t, vecVal(i128Val(amount))),
		},
		EventGaugeClaim: {
			Topic: []string{TopicSymbolGaugeClaim, encodeAccountAddrFromStrkey(t, user)},
			Value: marshalB64(t, i128Val(amount)),
		},
		EventClaimProtocolFee: {
			Topic: []string{TopicSymbolClaimProtocolFee, encodeContractAddrFromStrkey(t, token)},
			Value: marshalB64(t, vecVal(contractAddrVal(t, makeContractStrkey(t, 0x33)), i128Val(amount))),
		},
	}
}

// decodeClaimAmount runs the production decode arm for kind and returns
// the decoded amount as a decimal string.
func decodeClaimAmount(t *testing.T, kind string, e events.Event) (string, error) {
	t.Helper()
	if got := classify(&e); got != kind {
		t.Fatalf("classify = %q, want %q", got, kind)
	}
	if kind == EventClaimProtocolFee {
		fe, err := decodeFee(&e, closedAtTest, kind)
		if err != nil {
			return "", err
		}
		return fe.Amount.String(), nil
	}
	rv, err := decodeRewardsEvent(&e, kind, closedAtTest)
	if err != nil {
		return "", err
	}
	if rv.Amount == nil {
		t.Fatalf("%s: Amount nil on a successful decode", kind)
	}
	return rv.Amount.String(), nil
}

// A negative i128 claim is a schema violation: the promoted Amount is
// documented always-non-negative and the served column CHECKs >= 0, so a
// decode that accepts it hands the sink a row it can only drop.
func TestClaimAmounts_negativeRejectedAtDecode(t *testing.T) {
	for _, amt := range []*big.Int{
		big.NewInt(-1),
		mustBig(t, "-170141183460469231731687303715884105728"), // i128 min
	} {
		for kind, e := range claimAmountEvents(t, amt) {
			got, err := decodeClaimAmount(t, kind, e)
			if !errors.Is(err, ErrMalformedPayload) {
				t.Errorf("%s amount=%s: got (%q, %v), want ErrMalformedPayload", kind, amt, got, err)
			}
		}
	}
}

// Zero and the i128 max both satisfy the CHECK and must decode exactly.
func TestClaimAmounts_nonNegativeDecodeExactly(t *testing.T) {
	for _, amt := range []*big.Int{
		big.NewInt(0),
		mustBig(t, "170141183460469231731687303715884105727"), // i128 max
	} {
		for kind, e := range claimAmountEvents(t, amt) {
			got, err := decodeClaimAmount(t, kind, e)
			if err != nil {
				t.Fatalf("%s amount=%s: %v", kind, amt, err)
			}
			if got != amt.String() {
				t.Errorf("%s: amount = %s, want %s", kind, got, amt)
			}
		}
	}
}
