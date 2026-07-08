package timescale

import (
	"math/big"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/obs"
)

// TestIsDexUnitRatioTrade covers the pure predicate in isolation —
// the 2026-07-07 Phoenix incident signature (on-chain, base_amount ==
// quote_amount, both nonzero) vs. the cases it must NOT flag.
func TestIsDexUnitRatioTrade(t *testing.T) {
	tests := []struct {
		name   string
		ledger uint32
		base   canonical.Amount
		quote  canonical.Amount
		want   bool
	}{
		{
			name:   "on-chain unit-ratio trade — the Phoenix incident signature",
			ledger: 1000,
			base:   canonical.NewAmount(big.NewInt(500)),
			quote:  canonical.NewAmount(big.NewInt(500)),
			want:   true,
		},
		{
			name:   "on-chain normal trade — base != quote",
			ledger: 1000,
			base:   canonical.NewAmount(big.NewInt(500)),
			quote:  canonical.NewAmount(big.NewInt(1234)),
			want:   false,
		},
		{
			name:   "off-chain (CEX/FX) trade — ledger==0 excluded even if base==quote",
			ledger: 0,
			base:   canonical.NewAmount(big.NewInt(500)),
			quote:  canonical.NewAmount(big.NewInt(500)),
			want:   false,
		},
		{
			name:   "zero amounts — defence-in-depth, never flagged even though equal",
			ledger: 1000,
			base:   canonical.NewAmount(big.NewInt(0)),
			quote:  canonical.NewAmount(big.NewInt(0)),
			want:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDexUnitRatioTrade(tc.ledger, tc.base, tc.quote); got != tc.want {
				t.Errorf("isDexUnitRatioTrade(ledger=%d, base=%s, quote=%s) = %v; want %v",
					tc.ledger, tc.base.String(), tc.quote.String(), got, tc.want)
			}
		})
	}
}

// TestRecordDexTradeUnitRatio_MetricIncrements exercises the actual
// production call site (recordDexTradeUnitRatio, called from
// InsertTrade after a row lands) end to end against the real
// obs.DexTradeUnitRatioTotal counter — without a live database
// connection, since the decision + metric bump don't touch *sql.DB.
// Each case uses a distinct fake source label so the assertions can't
// be polluted by counter state left behind by other tests/cases.
func TestRecordDexTradeUnitRatio_MetricIncrements(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		ledger      uint32
		base, quote int64
		wantBump    bool
	}{
		{
			name:     "on-chain base==quote trade bumps the counter",
			source:   "test-unit-ratio-onchain-hit",
			ledger:   12345,
			base:     500,
			quote:    500,
			wantBump: true,
		},
		{
			name:     "CEX trade (ledger==0) does NOT bump even though base==quote",
			source:   "test-unit-ratio-cex-noop",
			ledger:   0,
			base:     500,
			quote:    500,
			wantBump: false,
		},
		{
			name:     "normal on-chain trade (base != quote) does NOT bump",
			source:   "test-unit-ratio-normal-noop",
			ledger:   12345,
			base:     500,
			quote:    1234,
			wantBump: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.ToFloat64(obs.DexTradeUnitRatioTotal.WithLabelValues(tc.source))

			trade := canonical.Trade{
				Source:      tc.source,
				Ledger:      tc.ledger,
				TxHash:      "deadbeef",
				BaseAmount:  canonical.NewAmount(big.NewInt(tc.base)),
				QuoteAmount: canonical.NewAmount(big.NewInt(tc.quote)),
			}
			recordDexTradeUnitRatio(trade)

			after := testutil.ToFloat64(obs.DexTradeUnitRatioTotal.WithLabelValues(tc.source))
			gotBump := after > before
			if gotBump != tc.wantBump {
				t.Errorf("recordDexTradeUnitRatio bumped=%v (before=%v after=%v); want bumped=%v",
					gotBump, before, after, tc.wantBump)
			}
		})
	}
}
