package v1_test

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestLendingProtocols_BLNTLPTierUnvalued sums a BLNT LP-share tier (BlntUsdc) apart from a plain XLM tier and never values it,
// though its token (the USDC SAC) is priced: the protocol total is the XLM tier alone, flagged a lower bound.
func TestLendingProtocols_BLNTLPTierUnvalued(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true})
	bal := func() *clickhouse.BackstopBalance {
		return &clickhouse.BackstopBalance{Shares: big.NewInt(1e9), Tokens: big.NewInt(1e9), Q4W: big.NewInt(0)}
	}
	f.explorer.blntTiers = []clickhouse.BackstopTierState{
		{Tier: "FirstLoss", Asset: "Xlm", Token: f.xlmSAC, Balance: bal()},
		{Tier: "SecondLoss", Asset: "BlntUsdc", Token: f.usdc, Balance: bal()},
	}
	stats, _ := getProtocols(t, f.srv)
	for _, st := range stats {
		if st.Protocol != "blnt" {
			continue
		}
		var xlmUSD string
		sawLP := false
		for _, b := range st.Backstop {
			switch b.BackstopToken {
			case f.usdc:
				sawLP = true
				if b.TokensUSD != nil || strOrNil(b.USDReason) != "lp_share_unvalued" {
					t.Errorf("BlntUsdc backstop = %+v, want null tokens_usd with lp_share_unvalued", b)
				}
			case f.xlmSAC:
				xlmUSD = strOrNil(b.TokensUSD)
			}
		}
		if !sawLP || xlmUSD == "" {
			t.Fatalf("blnt backstop = %+v, want a valued XLM entry and an LP entry", st.Backstop)
		}
		if strOrNil(st.BackstopTotalUSD) != xlmUSD || !st.BackstopUSDLowerBound {
			t.Errorf("backstop_total_usd = %s lower_bound = %v, want %s (XLM tier only) as a lower bound",
				strOrNil(st.BackstopTotalUSD), st.BackstopUSDLowerBound, xlmUSD)
		}
		return
	}
	t.Fatal("no blnt protocol in the response")
}

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
