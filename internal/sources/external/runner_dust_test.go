package external

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// C2-016 (audit-2026-07-23). The streamer dust guard used to be a
// flat 100_000 units at the external 10^8 scale — 0.001 of WHATEVER
// the quote asset is. binance/pairs.yaml and bitstamp/pairs.go both
// configure XLM/BTC, where 0.001 BTC is ~$100, so every XLM/BTC print
// under roughly a thousand XLM was silently dropped. The drop is
// size-biased, so the surviving XLM/BTC volume + VWAP skewed toward
// large trades.
//
// These tests pin the CORRECTED thresholds: the floor is $0.001 of
// notional expressed in the quote asset's own units, so the same
// $0.001 applies on XLM/BTC as on XLM/USDT.

// dustPair builds an XLM/<quote> pair for the dust tests.
func dustPair(t *testing.T, quote canonical.Asset) canonical.Pair {
	t.Helper()
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset(XLM): %v", err)
	}
	p, err := canonical.NewPair(xlm, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

func cryptoAsset(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewCryptoAsset(code)
	if err != nil {
		t.Fatalf("NewCryptoAsset(%s): %v", code, err)
	}
	return a
}

func fiatAsset(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		t.Fatalf("NewFiatAsset(%s): %v", code, err)
	}
	return a
}

// TestMinStreamQuoteUnits_IsUSDDenominated asserts the CORRECTED
// floor value per quote asset, not merely that one exists.
//
//	units = $0.001 / usd_per_whole_unit × 10^8
func TestMinStreamQuoteUnits_IsUSDDenominated(t *testing.T) {
	cases := []struct {
		name  string
		quote canonical.Asset
		want  string
	}{
		// $1/unit → 0.001 units → 100_000 at 10^8. This is the
		// pre-fix constant; USDT/fiat behaviour must not change.
		{"usdt_stablecoin", cryptoAsset(t, "USDT"), "100000"},
		{"fiat_usd", fiatAsset(t, "USD"), "100000"},
		{"fiat_eur", fiatAsset(t, "EUR"), "100000"},
		// ~$100k/unit → $0.001 is 1e-8 BTC → 1 unit at 10^8. The
		// pre-fix code used 100_000 here, i.e. ~$100 of notional.
		{"btc", cryptoAsset(t, "BTC"), "1"},
		// ~$3k/unit → 1e11/3e9 = 33 units.
		{"eth", cryptoAsset(t, "ETH"), "33"},
		// $0.30/unit → 1e11/3e5 = 333_333 units.
		{"xlm", cryptoAsset(t, "XLM"), "333333"},
		// No USD reference → floor of 1, i.e. only a zero quote leg
		// counts as dust. SHIB is on the ADR-0014 allow-list but is
		// not a quote leg on any venue we stream.
		{"unreferenced_crypto", cryptoAsset(t, "SHIB"), "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := minStreamQuoteUnits(tc.quote).String()
			if got != tc.want {
				t.Errorf("minStreamQuoteUnits(%s) = %s, want %s",
					tc.quote, got, tc.want)
			}
		})
	}
}

// TestForwardTrades_DustFloorIsQuoteAssetAware drives the real
// streamer path. The BTC-quoted case is the C2-016 regression: a
// 0.0005 BTC fill (~$50 of notional) is a genuine retail-size
// XLM/BTC print and MUST reach the sink. Pre-fix it was dropped by
// the flat 100_000-unit floor.
func TestForwardTrades_DustFloorIsQuoteAssetAware(t *testing.T) {
	cases := []struct {
		name        string
		quote       canonical.Asset
		quoteAmount int64
		wantForward bool
	}{
		{
			// 0.0005 BTC ≈ $50. Real print; pre-fix: DROPPED.
			name:        "btc_half_milli_is_real_money",
			quote:       cryptoAsset(t, "BTC"),
			quoteAmount: 50_000,
			wantForward: true,
		},
		{
			// 1 satoshi ≈ $0.001 — exactly at the corrected floor.
			name:        "btc_one_satoshi_at_floor",
			quote:       cryptoAsset(t, "BTC"),
			quoteAmount: 1,
			wantForward: true,
		},
		{
			// A zero quote leg has no price at any denomination.
			name:        "btc_zero_quote_is_dust",
			quote:       cryptoAsset(t, "BTC"),
			quoteAmount: 0,
			wantForward: false,
		},
		{
			// $0.0005 in USDT — genuine dust, still dropped.
			name:        "usdt_sub_tenth_cent_is_dust",
			quote:       cryptoAsset(t, "USDT"),
			quoteAmount: 50_000,
			wantForward: false,
		},
		{
			// $0.001 in USDT — exactly at the floor, kept.
			name:        "usdt_at_floor_is_kept",
			quote:       cryptoAsset(t, "USDT"),
			quoteAmount: 100_000,
			wantForward: true,
		},
		{
			// Fiat quote legs keep the pre-fix threshold.
			name:        "fiat_eur_sub_tenth_cent_is_dust",
			quote:       fiatAsset(t, "EUR"),
			quoteAmount: 99_999,
			wantForward: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			trade := testTrade(t, "dust-venue", 1)
			trade.Pair = dustPair(t, tc.quote)
			trade.QuoteAmount = canonical.NewAmount(big.NewInt(tc.quoteAmount))

			in := make(chan canonical.Trade, 1)
			in <- trade
			close(in)

			sink := make(chan consumer.Event, 1)
			forwardTrades(ctx, "dust-venue", in, sink,
				slog.New(slog.NewTextHandler(io.Discard, nil)))

			select {
			case ev := <-sink:
				if !tc.wantForward {
					t.Fatalf("quote=%d %s: forwarded %T, want dropped as dust",
						tc.quoteAmount, tc.quote, ev)
				}
				te, ok := ev.(TradeEvent)
				if !ok {
					t.Fatalf("got %T want TradeEvent", ev)
				}
				if te.Trade.QuoteAmount.String() != trade.QuoteAmount.String() {
					t.Errorf("QuoteAmount = %s, want %s",
						te.Trade.QuoteAmount, trade.QuoteAmount)
				}
			default:
				if tc.wantForward {
					t.Fatalf("quote=%d %s: dropped as dust, want forwarded",
						tc.quoteAmount, tc.quote)
				}
			}
		})
	}
}

// The USD reference table must only name tickers the canonical
// allow-list accepts — otherwise an entry is silently dead and the
// quote it was meant to cover falls back to noDustFloor.
func TestCryptoQuoteUSDMicros_CodesAreOnCanonicalAllowList(t *testing.T) {
	for code := range cryptoQuoteUSDMicros {
		if !canonical.IsKnownCrypto(code) {
			t.Errorf("cryptoQuoteUSDMicros has %q, not on the ADR-0014 crypto allow-list", code)
		}
	}
}

// Every quote asset in every shipped venue pair table must resolve
// to a USD reference — otherwise a configured production pair runs
// with the dust guard inert. This is the guard that would have
// caught C2-016's XLM/BTC at the time the pair was added.
func TestShippedVenueQuotes_HaveUSDReference(t *testing.T) {
	// Quote legs configured across binance/pairs.yaml,
	// bitstamp, coinbase and kraken DefaultPairs as of 2026-07-26.
	quotes := []canonical.Asset{
		cryptoAsset(t, "USDT"),
		cryptoAsset(t, "BTC"),
		fiatAsset(t, "USD"),
		fiatAsset(t, "EUR"),
		fiatAsset(t, "GBP"),
		fiatAsset(t, "AUD"),
		fiatAsset(t, "CAD"),
		fiatAsset(t, "CHF"),
	}
	for _, q := range quotes {
		if _, ok := quoteUSDReferenceMicros(q); !ok {
			t.Errorf("configured venue quote %s has no USD dust-floor reference", q)
		}
	}
}
