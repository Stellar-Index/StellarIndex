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
	// burnTopic0 is ScSymbol("burn"), the only topic the two pools' burns carry.
	burnTopic0 = "AAAADwAAAARidXJu"
)

func i128Body(t *testing.T, lo uint64) string {
	t.Helper()
	b, err := xdr.MarshalBase64(xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(lo)}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tokenBurnTopics is a token-shaped ["burn", from]: a judgement covers the body, so the topics must not decide first.
func tokenBurnTopics(t *testing.T) []string {
	t.Helper()
	return b64All(t, symScVal("burn"), accountAddrScVal(4))
}

// withJudgement installs a judgement for the test's duration, so the mechanism is tested without a real entry. Not for t.Parallel tests.
func withJudgement(t *testing.T, contractID, reason string) {
	t.Helper()
	prev, had := judgedNonToken[contractID]
	judgedNonToken[contractID] = Judgement{Reason: reason, Verdict: "test", Evidence: "test", Judged: "2026-11-26"}
	t.Cleanup(func() {
		if had {
			judgedNonToken[contractID] = prev
		} else {
			delete(judgedNonToken, contractID)
		}
	})
}

func TestClassifySupplyEventXDR_Judged(t *testing.T) {
	withJudgement(t, judgedPool, SupplySkipAmountNotI128)
	for _, tc := range []struct {
		name, contract, body, want string
	}{
		{"listed contract, judged reason", judgedPool, judgedPoolBurnBody, SupplySkipJudgedNonToken},
		{"unlisted contract, same body", unjudgedContract, judgedPoolBurnBody, SupplySkipAmountNotI128},
		{"listed contract, other failure", judgedPool, "not-base64-xdr!", SupplySkipBodyUnparseable},
	} {
		if amt, skip := ClassifySupplyEventXDR(tc.contract, tokenBurnTopics(t), tc.body); skip != tc.want || amt != nil {
			t.Errorf("%s: skip=%q amt=%v, want %q and no amount", tc.name, skip, amt, tc.want)
		}
	}
	amt, skip := ClassifySupplyEventXDR(judgedPool, tokenBurnTopics(t), i128Body(t, 1234))
	if skip != "" || amt == nil || amt.Int64() != 1234 {
		t.Errorf("listed contract, i128 body: skip=%q amt=%v, want amount 1234", skip, amt)
	}
}

// The two pools' recorded burns carry one topic, so the topic rule skips them before any judgement of their body can apply.
func TestClassifySupplyEventXDR_PoolBurnsNeedNoJudgement(t *testing.T) {
	burns := map[string]string{judgedPool: judgedPoolBurnBody, secondJudgedPool: secondJudgedPoolBurnBody}
	classify := func() map[string]string {
		got := map[string]string{}
		for id, body := range burns {
			_, got[id] = ClassifySupplyEventXDR(id, []string{burnTopic0}, body)
		}
		return got
	}
	unjudged := classify()
	for id := range burns {
		withJudgement(t, id, SupplySkipAmountNotI128)
	}
	judged := classify()
	for id := range burns {
		if unjudged[id] != SupplySkipTopicNotTokenShaped || judged[id] != unjudged[id] {
			t.Errorf("%s one-topic burn: unjudged=%q judged=%q, want both %q", id, unjudged[id], judged[id], SupplySkipTopicNotTokenShaped)
		}
	}
}

func TestJudgedNonToken_WellFormed(t *testing.T) {
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
	withJudgement(t, judgedPool, SupplySkipAmountNotI128)
	ext := &LedgerExtract{}
	at := time.Unix(0, 0)
	rec := func(contract, body string) {
		recordSupplyFlow(ext, ContractEventRow{ContractID: contract, Topic0Sym: "burn", TopicsXDR: tokenBurnTopics(t), DataXDR: body}, 1, at, "tx")
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
