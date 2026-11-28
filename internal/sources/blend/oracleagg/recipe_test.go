package oracleagg

import (
	"math/big"
	"testing"
)

const (
	feedCEX = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	xlmSAC  = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	eurcSAC = "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV"
	// r is a round timestamp in seconds, aligned to the 300 s resolution (5_666_666 × 300).
	r = uint64(1_699_999_800)
)

func v2Config() Config {
	rule := func(sym string, maxDev uint32) AssetRule {
		return AssetRule{Feed: feedCEX, FeedAsset: Asset{Symbol: sym}, FeedDecimals: 14, ResolutionSeconds: 300, MaxDev: maxDev}
	}
	return Config{
		Program: ProgramV2, Base: Asset{Symbol: "USD"}, BaseAssets: []Asset{Stellar(eurcSAC)}, Decimals: 7, MaxAge: 900,
		Assets: map[Asset]AssetRule{
			Stellar(xlmSAC):  rule("XLM", 20),
			Stellar(usdcSAC): rule("USDC", 0),
			Stellar(feedCEX): rule("BTC", 100),
		},
	}
}

func v1Config(blocked bool) Config {
	return Config{
		Program: ProgramV1, Base: Asset{Symbol: "USD"}, Decimals: 7,
		Assets: map[Asset]AssetRule{
			Stellar(xlmSAC):  {Feed: feedCEX, FeedAsset: Asset{Symbol: "XLM"}, FeedDecimals: 14, ResolutionSeconds: 300, Blocked: blocked},
			Stellar(usdcSAC): {Feed: feedCEX, FeedAsset: Asset{Symbol: "USDC"}, FeedDecimals: 5, ResolutionSeconds: 300},
		},
	}
}

// feed builds a Reflector v7.1 feed whose last round is lastS (seconds) with the given (seconds → raw price) rows.
func feed(lastS uint64, rows map[uint64]int64) Feed {
	f := Feed{Program: FeedReflectorV7, ResolutionMs: 300_000, LastRoundMs: lastS * 1000, Prices: map[uint64]*big.Int{}}
	for s, p := range rows {
		f.Prices[s*1000] = big.NewInt(p)
	}
	return f
}

func bigFeed(lastS uint64, rows map[uint64]string) Feed {
	f := feed(lastS, nil)
	for s, p := range rows {
		v, _ := new(big.Int).SetString(p, 10)
		f.Prices[s*1000] = v
	}
	return f
}

type recipeCase struct {
	name    string
	cfg     Config
	asset   Asset
	t       uint64
	feed    Feed
	price   string // expected raw price when priced
	ts      uint64
	verdict Verdict
}

func runRecipe(t *testing.T, cases []recipeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LastPrice(c.cfg, c.asset, c.t, c.feed)
			if got.Verdict != c.verdict {
				t.Fatalf("verdict = %q (%s), want %q", got.Verdict, got.Detail, c.verdict)
			}
			if c.verdict != "" {
				return
			}
			if got.Price.Price.String() != c.price || got.Price.Timestamp != c.ts || got.Price.Decimals != c.cfg.Decimals {
				t.Fatalf("price = %s @ %d (%d dp), want %s @ %d (%d dp)", got.Price.Price, got.Price.Timestamp,
					got.Price.Decimals, c.price, c.ts, c.cfg.Decimals)
			}
			if got.Price.Quote != c.cfg.Base {
				t.Fatalf("quote = %s, want the adapter base %s", got.Price.Quote, c.cfg.Base)
			}
		})
	}
}

func TestLastPriceV2(t *testing.T) {
	xlm, usdc, btc := Stellar(xlmSAC), Stellar(usdcSAC), Stellar(feedCEX)
	cfg := v2Config()
	runRecipe(t, []recipeCase{
		// price_data.rs:65 normalize_price: 12_345_678_901_234_567 / 10^(14−7) truncates to 1_234_567_890.
		{
			name: "latest period, rescaled by floor", cfg: cfg, asset: xlm, t: r + 100,
			feed: feed(r, map[uint64]int64{r: 12_345_678_901_234_567, r - 300: 12_345_678_901_234_567}), price: "1234567890", ts: r,
		},
		// price_data.rs:18 `next_timestamp >= oldest_timestamp`: a period exactly max_age old is still taken.
		{
			name: "period exactly max_age old accepted", cfg: cfg, asset: xlm, t: r + 900,
			feed: feed(r, map[uint64]int64{r: 1e14, r - 300: 1e14}), price: "10000000", ts: r,
		},
		{
			name: "period one second past max_age is stale", cfg: cfg, asset: xlm, t: r + 901,
			feed: feed(r, map[uint64]int64{r: 1e14, r - 300: 1e14}), verdict: VerdictPriceStale,
		},
		// price_data.rs:18-21: no row at the last round, so the walk takes R−300 and checks deviation from R−600.
		{
			name: "walks back one period", cfg: cfg, asset: xlm, t: r + 10,
			feed: feed(r, map[uint64]int64{r - 300: 2e14, r - 600: 2e14}), price: "20000000", ts: r - 300,
		},
		{
			name: "no period within max_age", cfg: cfg, asset: xlm, t: r + 10,
			feed: feed(r, map[uint64]int64{r - 1200: 1e14, r - 1500: 1e14}), verdict: VerdictPriceStale,
		},
		// price_data.rs:42-43: limit = 10^14·20/100 = 2·10^13; diff 2·10^13 is not > limit.
		{
			name: "deviation exactly at the limit accepted", cfg: cfg, asset: xlm, t: r,
			feed: feed(r, map[uint64]int64{r: 1e14 + 2e13, r - 300: 1e14}), price: "12000000", ts: r,
		},
		{
			name: "deviation one unit past the limit", cfg: cfg, asset: xlm, t: r,
			feed: feed(r, map[uint64]int64{r: 1e14 + 2e13 + 1, r - 300: 1e14}), verdict: VerdictDeviation,
		},
		{
			name: "price fell by exactly the limit", cfg: cfg, asset: xlm, t: r,
			feed: feed(r, map[uint64]int64{r: 1e14 - 2e13, r - 300: 1e14}), price: "8000000", ts: r,
		},
		// limit = floor(99·20/100) = floor(19.8) = 19: diff 19 passes, diff 20 fails.
		{
			name: "deviation limit floors", cfg: cfg, asset: xlm, t: r,
			feed: feed(r, map[uint64]int64{r: 118, r - 300: 99}), price: "0", ts: r,
		},
		{
			name: "deviation limit floors, one past", cfg: cfg, asset: xlm, t: r,
			feed: feed(r, map[uint64]int64{r: 119, r - 300: 99}), verdict: VerdictDeviation,
		},
		// price_data.rs:46-48: no earlier period, so max_dev cannot be verified and lastprice is None.
		{
			name: "missing earlier period", cfg: cfg, asset: xlm, t: r,
			feed: feed(r, map[uint64]int64{r: 1e14}), verdict: VerdictDeviationUnverifiable,
		},
		// price_data.rs:30-37: max_steps = 900/300 = 3 lookups (T−300, T−600, T−900), not bounded by oldest.
		{
			name: "earlier period at the third lookup, older than max_age", cfg: cfg, asset: xlm, t: r + 900,
			feed: feed(r, map[uint64]int64{r: 1e14, r - 900: 1e14}), price: "10000000", ts: r,
		},
		{
			name: "earlier period beyond the third lookup", cfg: cfg, asset: xlm, t: r,
			feed: feed(r, map[uint64]int64{r: 1e14, r - 1200: 1e14}), verdict: VerdictDeviationUnverifiable,
		},
		// price_data.rs:26: max_dev 0 and 100 skip the check entirely.
		{
			name: "max_dev 0 skips the check", cfg: cfg, asset: usdc, t: r,
			feed: feed(r, map[uint64]int64{r: 99_990_000_000_000}), price: "9999000", ts: r,
		},
		{
			name: "max_dev 100 skips the check", cfg: cfg, asset: btc, t: r,
			feed: feed(r, map[uint64]int64{r: 5e14}), price: "50000000", ts: r,
		},
		// contract.rs:87-93: Base and BaseAssets are 10^decimals at the ledger time, with no feed read.
		{name: "base asset", cfg: cfg, asset: Asset{Symbol: "USD"}, t: r + 7, feed: Feed{}, price: "10000000", ts: r + 7},
		{name: "base_assets member", cfg: cfg, asset: Stellar(eurcSAC), t: r + 7, feed: Feed{}, price: "10000000", ts: r + 7},
		// contract.rs:103-104: an unconfigured asset panics AssetNotFound.
		{name: "asset not configured", cfg: cfg, asset: Stellar(eurcSAC + "X"), t: r, feed: feed(r, nil), verdict: VerdictAssetMissing},
		{
			name: "feed program unknown", cfg: cfg, asset: xlm, t: r,
			feed: Feed{ResolutionMs: 300_000, LastRoundMs: r * 1000}, verdict: VerdictUnsupported,
		},
		{
			name: "feed asset is the feed base", cfg: cfg, asset: xlm, t: r,
			feed: Feed{Program: FeedReflectorV7, ResolutionMs: 300_000, LastRoundMs: r * 1000, AssetIsBase: true}, verdict: VerdictUnsupported,
		},
	})
}

func TestLastPriceV2_FullWidthIntegers(t *testing.T) {
	cfg := v2Config()
	// 2^100 + 2^99 against 2^100 at 60 %: limit floor(2^100·60/100), diff 2^99 < limit; exact beyond int64.
	cfg.Assets[Stellar(xlmSAC)] = AssetRule{Feed: feedCEX, FeedAsset: Asset{Symbol: "XLM"}, FeedDecimals: 14, ResolutionSeconds: 300, MaxDev: 60}
	f := bigFeed(r, map[uint64]string{r: "1901475900342344102245054808064", r - 300: "1267650600228229401496703205376"})
	got := LastPrice(cfg, Stellar(xlmSAC), r, f)
	if got.Verdict != "" || got.Price.Price.String() != "190147590034234410224505" {
		t.Fatalf("got %+v", got)
	}
}

func TestLastPriceV1(t *testing.T) {
	xlm, usdc := Stellar(xlmSAC), Stellar(usdcSAC)
	v6 := func(f Feed) Feed { f.Program = FeedReflectorV6; return f }
	runRecipe(t, []recipeCase{
		// contract.rs:48-52 + price_data.rs:11: last round's price, truncated to 7 dp; timestamp = round / 1000.
		{
			name: "last round", cfg: v1Config(false), asset: xlm, t: r + 299,
			feed: feed(r, map[uint64]int64{r: 12_345_678_901_234_567}), price: "1234567890", ts: r,
		},
		// Reflector prices.rs:46-48: the asset is checked in the last round only; there is no walk back.
		{
			name: "asset absent from the last round", cfg: v1Config(false), asset: xlm, t: r + 10,
			feed: feed(r, map[uint64]int64{r - 300: 1e14}), verdict: VerdictPriceAbsent,
		},
		// contract.rs:42-44: Blocked(asset) == true panics AssetBlocked.
		{name: "blocked", cfg: v1Config(true), asset: xlm, t: r, feed: feed(r, map[uint64]int64{r: 1e14}), verdict: VerdictAssetBlocked},
		{name: "not configured", cfg: v1Config(false), asset: Stellar(eurcSAC), t: r, feed: feed(r, nil), verdict: VerdictAssetMissing},
		// Reflector prices.rs:21-25 obtain_last_record_timestamp: no round, or a round after the ledger time, is None.
		{name: "no round", cfg: v1Config(false), asset: xlm, t: r, feed: feed(0, nil), verdict: VerdictPriceStale},
		{
			name: "round after ledger time", cfg: v1Config(false), asset: xlm, t: r - 1,
			feed: feed(r, map[uint64]int64{r: 1e14}), verdict: VerdictPriceStale,
		},
		// v7.1.0 has no age limit: a round ten hours old still prices.
		{
			name: "v7 old round still prices", cfg: v1Config(false), asset: xlm, t: r + 36_000,
			feed: feed(r, map[uint64]int64{r: 1e14}), price: "10000000", ts: r,
		},
		// v6 prices.rs:25 `ledger_timestamp - last_timestamp >= resolution * 2`: 599.999 s prices, 600 s does not.
		{
			name: "v6 under two resolutions", cfg: v1Config(false), asset: xlm, t: r + 599,
			feed: v6(feed(r, map[uint64]int64{r: 1e14})), price: "10000000", ts: r,
		},
		{
			name: "v6 at two resolutions", cfg: v1Config(false), asset: xlm, t: r + 600,
			feed: v6(feed(r, map[uint64]int64{r: 1e14})), verdict: VerdictPriceStale,
		},
		// price_data.rs:13-15: fewer feed decimals scale up (5 → 7 multiplies by 100).
		{name: "rescale up", cfg: v1Config(false), asset: usdc, t: r, feed: feed(r, map[uint64]int64{r: 99_987}), price: "9998700", ts: r},
	})
}
