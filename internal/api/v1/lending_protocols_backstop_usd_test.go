package v1_test

import "testing"

// TestLendingProtocols_BackstopUSDNeverSilent: every backstop[] entry is either valued or names why not, Blend's Comet LP share is
// never valued, and a protocol total over a partial set is flagged a lower bound.
func TestLendingProtocols_BackstopUSDNeverSilent(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true})
	stats, _ := getProtocols(t, f.srv)
	sawBackstop := false
	for _, st := range stats {
		unvalued := 0
		for _, b := range st.Backstop {
			sawBackstop = true
			if (b.TokensUSD == nil) == (b.USDReason == nil) {
				t.Errorf("%s backstop %s: tokens_usd=%v usd_reason=%v, want exactly one", st.Protocol, b.BackstopToken, strOrNil(b.TokensUSD), strOrNil(b.USDReason))
			}
			if st.Protocol == "blend" && strOrNil(b.USDReason) != "lp_share_unvalued" {
				t.Errorf("blend backstop %s valued: %+v", b.BackstopToken, b)
			}
			if b.TokensUSD == nil {
				unvalued++
			}
		}
		if st.BackstopTotalUSD != nil && unvalued > 0 && !st.BackstopUSDLowerBound {
			t.Errorf("%s backstop_total_usd %s omits %d entries but is not a lower bound", st.Protocol, *st.BackstopTotalUSD, unvalued)
		}
		if st.BackstopTotalUSD == nil && st.BackstopUSDLowerBound {
			t.Errorf("%s: a null backstop_total_usd cannot be a lower bound", st.Protocol)
		}
	}
	if !sawBackstop {
		t.Fatal("fixture served no backstop entries; the test is vacuous")
	}
}
