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

// feedAssets is the test feed's asset list; every asset gets the same history, so whichever one a case prices
// sees the rows it was given.
var feedAssets = []Asset{{Symbol: "XLM"}, {Symbol: "USDC"}, {Symbol: "BTC"}}

// feed builds a consistent Reflector v7.1 feed at 300 s: last_timestamp lastS (seconds), our stored rows (seconds →
// raw price), and a history mask that marks exactly the stored periods.
func feed(lastS uint64, rows map[uint64]int64) Feed {
	p := map[uint64]*big.Int{}
	for s, v := range rows {
		p[s*1000] = big.NewInt(v)
	}
	return consistent(lastS, 300_000, p)
}

func bigFeed(lastS uint64, rows map[uint64]string) Feed {
	p := map[uint64]*big.Int{}
	for s, v := range rows {
		p[s*1000], _ = new(big.Int).SetString(v, 10)
	}
	return consistent(lastS, 300_000, p)
}

func consistent(lastS, resMs uint64, prices map[uint64]*big.Int) Feed {
	f := Feed{
		Program: FeedReflectorV7,
		State: FeedState{
			LastTimestampMs: lastS * 1000, ResolutionMs: resMs, RetentionMs: 86_400_000, Decimals: 14, Protocol: ReflectorProtocol,
			Base: Asset{Symbol: "USD"}, Assets: feedAssets, History: make([]byte, 32*len(feedAssets)),
		},
		Prices: prices,
	}
	for ts := range prices {
		if ts > f.StoredLastRoundMs {
			f.StoredLastRoundMs = ts
		}
		if ts <= f.State.LastTimestampMs {
			for i := range feedAssets {
				setHistory(&f.State, uint32(i), uint32((f.State.LastTimestampMs-ts)/resMs))
			}
		}
	}
	return f
}

// setHistory is the inverse of check_history_updated.
func setHistory(s *FeedState, index, period uint32) {
	if period > 255 {
		return
	}
	s.History[index*32+(31-period/8)] |= 1 << (period % 8)
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
		{name: "feed program unknown", cfg: cfg, asset: xlm, t: r, feed: Feed{}, verdict: VerdictUnsupported},
		{name: "feed asset is the feed base", cfg: cfg, asset: xlm, t: r, feed: withBase(feed(r, nil), Asset{Symbol: "XLM"}), verdict: VerdictUnsupported},
		{name: "feed does not quote the asset", cfg: cfg, asset: xlm, t: r, feed: withAssets(feed(r, nil), Asset{Symbol: "BTC"}), verdict: VerdictFeedAssetUnknown},
		// assets.rs:31 resolves by list position; a legacy index entry does not move it.
		{
			name: "legacy index entry ignored", cfg: cfg, asset: xlm, t: r,
			feed: withLegacyIndex(feed(r, map[uint64]int64{r: 1e14, r - 300: 1e14})), price: "10000000", ts: r,
		},
		{name: "storage protocol 1", cfg: cfg, asset: xlm, t: r, feed: withProtocol(feed(r, nil), 1), verdict: VerdictUnsupported},
		{
			name: "max_age above the ledger time traps", cfg: cfg, asset: xlm, t: 899,
			feed: feed(0, nil), verdict: VerdictContractPanics,
		},
	})
}

func withBase(f Feed, b Asset) Feed { f.State.Base = b; return f }

func withAssets(f Feed, a ...Asset) Feed { f.State.Assets = a; return f }

// withLegacyIndex gives XLM a stale legacy entry pointing at BTC's slot, whose history is cleared: resolving by
// the entry would read "never priced" against our XLM rows.
func withLegacyIndex(f Feed) Feed {
	f.State.LegacyIndex = map[Asset]uint32{{Symbol: "XLM"}: 2}
	clear(f.State.History[2*32 : 3*32])
	return f
}

func withProtocol(f Feed, p uint32) Feed { f.State.Protocol = p; return f }

// missing drops our stored row at round s (seconds) while the chain's history mask still says it was priced.
func missing(f Feed, s uint64) Feed {
	delete(f.Prices, s*1000)
	f.StoredLastRoundMs = 0
	for ts := range f.Prices {
		f.StoredLastRoundMs = max(f.StoredLastRoundMs, ts)
	}
	return f
}

// A round on chain but not in oracle_updates (a decode-dropped event, a skipped poison row) must not let the recipe
// read an older round: the feed's own last_timestamp and history mask decide, and a disagreement is a verdict.
func TestLastPrice_FeedIncomplete(t *testing.T) {
	xlm := Stellar(xlmSAC)
	two := map[uint64]int64{r: 2e14, r - 300: 1e14, r - 600: 1e14}
	cached := feed(r, two)
	cached.State.Cache = []CachedRound{{TimestampMs: r * 1000, Mask: []byte{0b001}, Prices: []*big.Int{big.NewInt(3e14)}}}
	// The decoder's closedAt fallback: a row 4 s past a round, behind an all-zero last round that excuses it.
	offBoundary := feed(r, map[uint64]int64{r - 300: 1e14})
	offBoundary.StoredLastRoundMs = (r-300)*1000 + 4_000
	unstored := feed(r, map[uint64]int64{r - 300: 1e14, r - 600: 1e14})
	setHistory(&unstored.State, 1, 0) // the last round priced USDC only: not an all-zero round
	runRecipe(t, []recipeCase{
		{name: "v2 last round on chain, absent here", cfg: v2Config(), asset: xlm, t: r + 10, feed: missing(feed(r, two), r), verdict: VerdictFeedIncomplete},
		{name: "v1 last round on chain, absent here", cfg: v1Config(false), asset: xlm, t: r + 10, feed: missing(feed(r, two), r), verdict: VerdictFeedIncomplete},
		{name: "v2 last round stored for other assets only", cfg: v2Config(), asset: xlm, t: r + 10, feed: unstored, verdict: VerdictFeedIncomplete},
		{
			name: "v2 deviation round on chain, absent here", cfg: v2Config(), asset: xlm, t: r + 10,
			feed: func() Feed { f := feed(r, two); delete(f.Prices, (r-300)*1000); return f }(), verdict: VerdictFeedIncomplete,
		},
		{
			name: "stored row the chain says was not priced", cfg: v1Config(false), asset: xlm, t: r + 10,
			feed: func() Feed {
				f := feed(r, two)
				f.State.History = make([]byte, 96)
				setHistory(&f.State, 1, 0)
				return f
			}(), verdict: VerdictFeedIncomplete,
		},
		{name: "stored price disagrees with the chain cache", cfg: v1Config(false), asset: xlm, t: r + 10, feed: cached, verdict: VerdictFeedIncomplete},
		{name: "last stored round off the resolution boundary", cfg: v1Config(false), asset: xlm, t: r + 10, feed: offBoundary, verdict: VerdictFeedIncomplete},
		// prices.rs:118-123: an all-zero round sets last_timestamp and no history bit; the walk goes on to R−300.
		{
			name: "v2 all-zero last round walks back", cfg: v2Config(), asset: xlm, t: r + 10,
			feed: feed(r, map[uint64]int64{r - 300: 1e14, r - 600: 1e14}), price: "10000000", ts: r - 300,
		},
		{name: "v1 all-zero last round has no price", cfg: v1Config(false), asset: xlm, t: r + 10, feed: feed(r, map[uint64]int64{r - 300: 1e14}), verdict: VerdictPriceAbsent},
		{name: "cache agrees", cfg: v1Config(false), asset: xlm, t: r + 10, feed: withCache(feed(r, two), 2e14), price: "20000000", ts: r},
	})
}

func withCache(f Feed, p int64) Feed {
	f.State.Cache = []CachedRound{{TimestampMs: f.State.LastTimestampMs, Mask: []byte{0b111}, Prices: []*big.Int{big.NewInt(p), big.NewInt(1), big.NewInt(1)}}}
	return f
}

// The walk steps by the adapter's cached resolution while price() normalises by the feed's live one (price_data.rs:17,
// prices.rs:36); neither is a constant.
func TestLastPriceV2_FeedResolution(t *testing.T) {
	xlm := Stellar(xlmSAC)
	rows := map[uint64]*big.Int{(r - 60) * 1000: big.NewInt(1e14), (r - 120) * 1000: big.NewInt(1e14)}
	at60 := v2Config()
	rule := at60.Assets[xlm]
	rule.ResolutionSeconds = 60
	at60.Assets[xlm] = rule
	runRecipe(t, []recipeCase{
		{name: "60 s feed and adapter", cfg: at60, asset: xlm, t: r + 30, feed: consistent(r, 60_000, rows), price: "10000000", ts: r - 60},
		// Adapter caches 300 s against a 60 s feed: it probes R, R−300, R−600, R−900 and never sees R−60.
		{name: "adapter 300 s, feed 60 s", cfg: v2Config(), asset: xlm, t: r + 30, feed: consistent(r, 60_000, rows), verdict: VerdictPriceStale},
	})
}

// prices.rs:181-188: store_prices extends a round's temporary entry to (period/1000/5 + 1)·2 ledgers. A 1 h
// retention gives (720 + 1)·2 = 1442 ledgers = 7210 s at 5 s; load_history_record (prices.rs:136-147) reads the
// cache first, so only an uncached round past that life is unreachable.
func TestLastPriceV1_TemporaryEntryLife(t *testing.T) {
	xlm := Stellar(xlmSAC)
	hour := func(f Feed) Feed { f.State.RetentionMs = 3_600_000; return f }
	unset := func(f Feed) Feed { f.State.RetentionMs = 0; return f }
	rows := map[uint64]int64{r: 1e14}
	runRecipe(t, []recipeCase{
		{name: "1 h retention, round 7209 s old", cfg: v1Config(false), asset: xlm, t: r + 7209, feed: hour(feed(r, rows)), price: "10000000", ts: r},
		{name: "1 h retention, round 7210 s old", cfg: v1Config(false), asset: xlm, t: r + 7210, feed: hour(feed(r, rows)), verdict: VerdictUnsupported},
		{name: "1 h retention, old round still cached", cfg: v1Config(false), asset: xlm, t: r + 7210, feed: hour(withCache(feed(r, rows), 1e14)), price: "10000000", ts: r},
		// (0/1000/5 + 1)·2 = 2 ledgers <= 16: no extension, the network minimum is unknown.
		{name: "retention unset", cfg: v1Config(false), asset: xlm, t: r + 1, feed: unset(feed(r, rows)), verdict: VerdictUnsupported},
	})
}

// prices.rs:46-48: a round more than 255 periods back is never priced, whatever the next asset's mask holds.
func TestHasPrice_256PeriodBound(t *testing.T) {
	s := FeedState{Assets: feedAssets, History: make([]byte, 32*len(feedAssets))}
	setHistory(&s, 1, 255)
	setHistory(&s, 0, 0) // byte 31 of asset 0, where an unguarded period 256 of asset 1 would land
	if !s.HasPrice(1, 255) {
		t.Fatal("period 255 must be readable")
	}
	if s.HasPrice(1, 256) {
		t.Fatal("period 256 must read not-priced")
	}
}

// One asset whose config entry is unreadable fails alone.
func TestLastPrice_UnreadableAssetIsolated(t *testing.T) {
	cfg := v2Config()
	cfg.Unreadable = map[Asset]error{Stellar(eurcSAC + "X"): ErrEntryNewer, Stellar(eurcSAC + "Y"): ErrConfig}
	f := feed(r, map[uint64]int64{r: 1e14, r - 300: 1e14})
	runRecipe(t, []recipeCase{
		{name: "newer entry", cfg: cfg, asset: Stellar(eurcSAC + "X"), t: r, feed: f, verdict: VerdictStateNewer},
		{name: "undecodable entry", cfg: cfg, asset: Stellar(eurcSAC + "Y"), t: r, feed: f, verdict: VerdictUnsupported},
		{name: "sibling still prices", cfg: cfg, asset: Stellar(xlmSAC), t: r, feed: f, price: "10000000", ts: r},
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
