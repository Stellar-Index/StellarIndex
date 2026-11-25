package clickhouse

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	judgedPool = "CAOAVZOUDGYDI3QJTCDEQ3GJPWFN7QHM632EQZLWAIUWKILEFTKZUPE6"
	// The pool's `burn` body at mainnet ledger 65648178: a map whose `amount` is a u128 liquidity figure.
	judgedPoolBurnBody = "AAAAEQAAAAEAAAAGAAAADwAAAAZhbW91bnQAAAAAAAkAAAAAAAAAAAAAs/+k8uDsAAAADwAAAAdhbW91bnQwAAAAAAkAAAAAAAAAAAAAASUJspE0AAAADwAAAAdhbW91bnQxAAAAAAkAAAAAAAAAAAAAAPnT7H7cAAAADwAAAAVvd25lcgAAAAAAABIAAAABIzovoLDzgMm/PSicyUIChIApn2d5n/jeVHAlcu+O3HoAAAAPAAAACnRpY2tfbG93ZXIAAAAAAAQAAAhwAAAADwAAAAp0aWNrX3VwcGVyAAAAAAAEAAAJYA=="
	secondJudgedPool   = "CBFY4Q26RW5TYMWFFIVBP3CRMWHWNUD2GUFRQWNKRG4U53VELSSZ2OFQ"
	// Its `burn` body at mainnet ledger 65660039.
	secondJudgedPoolBurnBody = "AAAAEQAAAAEAAAAGAAAADwAAAAZhbW91bnQAAAAAAAkAAAAAAAAAAAAAAhu6WyJ1AAAADwAAAAdhbW91bnQwAAAAAAkAAAAAAAAAAAAAAFIMHjt5AAAADwAAAAdhbW91bnQxAAAAAAkAAAAAAAAAAAAAAAAAAAAAAAAADwAAAAVvd25lcgAAAAAAABIAAAABXU1ExcP57syWoe2HRKg3Ug6rh87TcybzwVhIsVhLprsAAAAPAAAACnRpY2tfbG93ZXIAAAAAAAT//3UEAAAADwAAAAp0aWNrX3VwcGVyAAAAAAAE//93DA=="
	unjudgedContract         = "CCKCKCPHYVXQD4NECBFJTFSCU2AMSJGCNG4O6K4JVRE2BLPR7WNDBQIQ"
)

func i128Body(t *testing.T, lo uint64) string {
	t.Helper()
	b, err := xdr.MarshalBase64(xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(lo)}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClassifySupplyEventXDR_Judged(t *testing.T) {
	for _, tc := range []struct {
		name, contract, body, want string
	}{
		{"listed contract, judged reason", judgedPool, judgedPoolBurnBody, SupplySkipJudgedNonToken},
		{"unlisted contract, same body", unjudgedContract, judgedPoolBurnBody, SupplySkipAmountNotI128},
		{"second listed pool, its own burn body", secondJudgedPool, secondJudgedPoolBurnBody, SupplySkipJudgedNonToken},
		{"unlisted contract, second pool's body", unjudgedContract, secondJudgedPoolBurnBody, SupplySkipAmountNotI128},
		{"listed contract, other failure", judgedPool, "not-base64-xdr!", SupplySkipBodyUnparseable},
	} {
		if amt, skip := ClassifySupplyEventXDR(tc.contract, tc.body); skip != tc.want || amt != nil {
			t.Errorf("%s: skip=%q amt=%v, want %q and no amount", tc.name, skip, amt, tc.want)
		}
	}
	amt, skip := ClassifySupplyEventXDR(judgedPool, i128Body(t, 1234))
	if skip != "" || amt == nil || amt.Int64() != 1234 {
		t.Errorf("listed contract, i128 body: skip=%q amt=%v, want amount 1234", skip, amt)
	}
}

func TestJudgedNonToken_WellFormed(t *testing.T) {
	if len(judgedNonToken) == 0 {
		t.Fatal("judgedNonToken is empty")
	}
	for id, j := range judgedNonToken {
		if _, err := strkey.Decode(strkey.VersionByteContract, id); err != nil {
			t.Errorf("%s: not a contract strkey: %v", id, err)
		}
		if j.Reason == "" || j.Verdict == "" || j.Evidence == "" || j.Judged == "" {
			t.Errorf("%s: empty field in %+v", id, j)
		}
		if _, err := time.Parse(time.DateOnly, j.Judged); err != nil {
			t.Errorf("%s: Judged %q: %v", id, j.Judged, err)
		}
		if !slices.Contains(SupplySkipReasons, j.Reason) || !SupplySkipAlerts(j.Reason) {
			t.Errorf("%s: Reason %q must be an alerting SupplySkipReasons member", id, j.Reason)
		}
	}
}

// An alerting skip names its contract (bounded per ledger); a non-alerting one does not.
func TestRecordSupplyFlow_SamplesAlertingSkipContracts(t *testing.T) {
	ext := &LedgerExtract{}
	at := time.Unix(0, 0)
	rec := func(contract, body string) {
		recordSupplyFlow(ext, ContractEventRow{ContractID: contract, Topic0Sym: "burn", DataXDR: body}, 1, at, "tx")
	}
	rec(judgedPool, judgedPoolBurnBody)
	rec(unjudgedContract, judgedPoolBurnBody)
	rec(unjudgedContract, judgedPoolBurnBody)
	if want := []SupplySkipSample{{ContractID: unjudgedContract, Reason: SupplySkipAmountNotI128, TxHash: "tx"}}; !slices.Equal(ext.SupplyFlowSkipSamples, want) {
		t.Fatalf("samples = %+v, want %+v", ext.SupplyFlowSkipSamples, want)
	}
	for i := range 2 * supplySkipSampleCap {
		rec(fmt.Sprintf("C%d", i), judgedPoolBurnBody)
	}
	if got := len(ext.SupplyFlowSkipSamples); got != supplySkipSampleCap {
		t.Errorf("samples = %d, want the cap %d", got, supplySkipSampleCap)
	}
	if ext.SupplyFlowSkips[SupplySkipJudgedNonToken] != 1 || ext.SupplyFlowSkips[SupplySkipAmountNotI128] != 2+2*supplySkipSampleCap {
		t.Errorf("SupplyFlowSkips = %v", ext.SupplyFlowSkips)
	}
}
