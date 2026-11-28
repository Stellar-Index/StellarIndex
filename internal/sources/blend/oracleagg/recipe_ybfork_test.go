package oracleagg

import (
	"math/big"
	"reflect"
	"testing"
)

const (
	ybForkAdapter = "CD74A3C54EKUVEGUC6WNTUPOTHB624WFKXN3IYTFJGX3EHXDXHCYMXXR"
	ustrySAC      = "CBLV4ATSIWU67CFSQU2NVRKINQIKUZ2ODSZBUJTJ43VJVRSBTZYOPNUR"
	// Reflector DEX round timestamps (seconds) around the manipulated USTRY rounds, named by UTC time of day.
	r0010 = uint64(1_771_719_000)
	r0015 = uint64(1_771_719_300)
	r0020 = uint64(1_771_719_600)
	r0025 = uint64(1_771_719_900)
	r0030 = uint64(1_771_720_200)
)

// The Reflector DEX feed's raw USTRY prices (14 dp) per round, 23:50 to 00:30, read from its round entries.
var ustryRounds = map[uint64]string{
	1_771_717_800: "105742670126650", 1_771_718_100: "105742670126650", 1_771_718_400: "105742670126650",
	1_771_718_700: "105742642288368", r0010: "105742379403813",
	r0015: "10673728301028137", r0020: "10673728301028137", // the single 106.737283 trade, written twice
	r0025: "106129555648952", r0030: "106129555648952",
}

var xlmRounds = map[uint64]string{r0010: "16098754487562", r0015: "16093501531739", r0020: "16093480931573", r0025: "16067996185710"}

func ybForkConfig(t *testing.T) Config {
	t.Helper()
	es := readEntries(t, "ybfork-instance.tsv", 2)
	if len(es) != 1 || es[0].contract != ybForkAdapter {
		t.Fatalf("fixture holds %d entries", len(es))
	}
	if ProgramForHash(es[0].wasm) != ProgramYBForkV2 {
		t.Fatalf("wasm %s is not the audited YieldBlox fork build", es[0].wasm)
	}
	cfg, err := DecodeYBForkV2Instance(storageOf(t, es[0]))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDecodeYBForkV2Instance_RealEntry(t *testing.T) {
	assets := map[Asset]AssetRule{}
	for a, d := range map[string]uint32{
		ustrySAC: 10, eurcSAC: 10, "CAL6ER2TI6CTRAY6BFXWNWA7WTYXUXTQCHUBCIBU5O6KM3HJFG6Z6VXV": 10,
		xlmSAC: 0, "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK": 0,
	} {
		assets[Stellar(a)] = dexRule(a, d)
	}
	want := Config{
		Program: ProgramYBForkV2, Base: Stellar(usdcSAC), Decimals: 7, MaxAge: 900, Assets: assets,
		BaseAssets: []Asset{
			Stellar("CB226ZOEYXTBPD3QEGABTJYSKZVBP2PASEISLG3SBMTN5CE4QZUVZ3CE"),
			Stellar("CCCRWH6Q3FNP3I2I57BDLM5AFAT7O6OF6GKQOC6SSJNDAVRZ57SPHGU2"),
		},
	}
	if got := ybForkConfig(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestProgramForHash_YBFork(t *testing.T) {
	if ProgramForHash(HashYBForkV2) != ProgramYBForkV2 || ProgramForHash(HashV2) != ProgramV2 {
		t.Fatal("audited hashes must map to their own program")
	}
	// One nibble off the fork's hash is a different build.
	if p := ProgramForHash("8cf43882ff2e6757bef1900973ab26efb1b42b832a640a320a9b881fa501cf77"); p != ProgramUnknown {
		t.Fatalf("foreign hash mapped to %s", p)
	}
	cfg := Config{Program: ProgramUnknown, Assets: map[Asset]AssetRule{Stellar(ustrySAC): dexRule(ustrySAC, 10)}}
	if got := LastPrice(cfg, Stellar(ustrySAC), r0020, Feed{}); got.Verdict != VerdictUnsupported {
		t.Fatalf("unknown program = %+v, want oracle_unsupported", got)
	}
}

// dexFeed is a Reflector v7.1 DEX feed at 300 s whose last round is lastS, quoting USTRY (index 0) and XLM (index 1)
// with the given stored rows (seconds → raw); each asset's history mask marks exactly its own rows.
func dexFeed(lastS uint64, ustry, xlm map[uint64]string) Feed {
	f := Feed{
		Program: FeedReflectorV7,
		State: FeedState{
			LastTimestampMs: lastS * 1000, ResolutionMs: 300_000, RetentionMs: 86_400_000, Decimals: 14, Protocol: ReflectorProtocol,
			Base: Stellar(usdcSAC), Assets: []Asset{Stellar(ustrySAC), Stellar(xlmSAC)}, History: make([]byte, 64),
		},
		StoredLastRoundMs: lastS * 1000,
	}
	for _, s := range []struct {
		index uint32
		rows  map[uint64]string
	}{{0, ustry}, {1, xlm}} {
		for ts := range s.rows {
			if ts > lastS {
				continue
			}
			setHistory(&f.State, s.index, uint32((lastS-ts)/300))
		}
	}
	return f
}

// withRows sets the stored rows the recipe reads for one asset (the Feed carries one asset's prices).
func withRows(f Feed, rows map[uint64]string) Feed {
	f.Prices = map[uint64]*big.Int{}
	for ts, v := range rows {
		f.Prices[ts*1000], _ = new(big.Int).SetString(v, 10)
	}
	return f
}

// upTo keeps the rounds written by the feed's last round lastS.
func upTo(rows map[uint64]string, lastS uint64) map[uint64]string {
	out := map[uint64]string{}
	for ts, v := range rows {
		if ts <= lastS {
			out[ts] = v
		}
	}
	return out
}

func febFeed(lastS uint64, asset string) Feed {
	u, x := upTo(ustryRounds, lastS), upTo(xlmRounds, lastS)
	rows := u
	if asset == xlmSAC {
		rows = x
	}
	return withRows(dexFeed(lastS, u, x), rows)
}

// The manipulated rounds as pure inputs, run under the Reflector v7.1 feed model. These reproduce the
// contract; they do not judge the price. The feed then ran an older Reflector build (df88820e), which this package
// does not model, so the reader cannot replay that day from the lake; only the recipe is exercised here.
func TestLastPriceYBFork_Feb22Rounds(t *testing.T) {
	cfg := ybForkConfig(t)
	ustry, xlm := Stellar(ustrySAC), Stellar(xlmSAC)
	runRecipe(t, []recipeCase{
		// 00:15: p0 = 10_673_728_301_028_137, p1 = 105_742_379_403_813. Limit = p1·10/100 = 10_574_237_940_381
		// (truncated); |p0 − p1| = 10_567_985_921_624_324 is not below it → None.
		{
			name: "00:15 one inflated round is refused", cfg: cfg, asset: ustry, t: r0015 + 60,
			feed: febFeed(r0015, ustrySAC), verdict: VerdictDeviation,
		},
		// 00:20: p0 = p1 = 10_673_728_301_028_137, diff 0 < limit 1_067_372_830_102_813 → the contract returns the
		// inflated price, 10_673_728_301_028_137 / 10^7 = 1_067_372_830 (106.7372830). This is the documented
		// weakness: two equal inflated rounds pass, and we serve what the contract would return.
		{
			name: "00:20 two equal inflated rounds are accepted", cfg: cfg, asset: ustry, t: r0020 + 50,
			feed: febFeed(r0020, ustrySAC), price: "1067372830", ts: r0020,
		},
		// 00:25: p0 = 106_129_555_648_952, p1 = 10_673_728_301_028_137. Limit = 1_067_372_830_102_813;
		// |p0 − p1| = 10_567_598_745_379_185 is not below it → None: the corrective round is refused too.
		{
			name: "00:25 the corrective round is refused", cfg: cfg, asset: ustry, t: r0025 + 30,
			feed: febFeed(r0025, ustrySAC), verdict: VerdictDeviation,
		},
		// 00:30: p0 = p1 = 106_129_555_648_952 → 10_612_955 (1.0612955).
		{
			name: "00:30 two equal rounds again", cfg: cfg, asset: ustry, t: r0030 + 30,
			feed: febFeed(r0030, ustrySAC), price: "10612955", ts: r0030,
		},
		// XLM has max_dev 0: price(last_timestamp) with no check, 16_093_480_931_573 / 10^7 = 1_609_348.
		{
			name: "00:20 XLM max_dev 0 takes the last round unchecked", cfg: cfg, asset: xlm, t: r0020 + 50,
			feed: febFeed(r0020, xlmSAC), price: "1609348", ts: r0020,
		},
		{
			name: "base asset is one USDC", cfg: cfg, asset: Stellar(usdcSAC), t: r0020 + 50,
			feed: Feed{}, price: "10000000", ts: r0020 + 50,
		},
	})
}

func TestLastPriceYBFork_Rules(t *testing.T) {
	cfg := ybForkConfig(t)
	ustry, xlm := Stellar(ustrySAC), Stellar(xlmSAC)
	one := func(lastS uint64, rows map[uint64]string) Feed { return withRows(dexFeed(lastS, rows, nil), rows) }
	r := r0020
	runRecipe(t, []recipeCase{
		{
			name: "stale: newest record older than max_age", cfg: cfg, asset: ustry, t: r + 901,
			feed: one(r, map[uint64]string{r: "100000000000000", r - 300: "100000000000000"}), verdict: VerdictPriceStale,
		},
		{
			name: "exactly max_age old is accepted", cfg: cfg, asset: ustry, t: r + 900,
			feed: one(r, map[uint64]string{r: "100000000000000", r - 300: "100000000000000"}), price: "10000000", ts: r,
		},
		{
			name: "stale: max_dev 0 last round older than max_age", cfg: cfg, asset: xlm, t: r + 901,
			feed:    withRows(dexFeed(r, nil, map[uint64]string{r: "16000000000000"}), map[uint64]string{r: "16000000000000"}),
			verdict: VerdictPriceStale,
		},
		{
			name: "max_dev 0 asset sat out the last round", cfg: cfg, asset: xlm, t: r + 10,
			feed: withRows(dexFeed(r, map[uint64]string{r: "1"}, map[uint64]string{r - 300: "16000000000000"}),
				map[uint64]string{r - 300: "16000000000000"}),
			verdict: VerdictPriceAbsent,
		},
		// The asset sat out the last round: prices() skips it, so p0 is the round before.
		{
			name: "skipped last round compares the two before", cfg: cfg, asset: ustry, t: r + 10,
			feed: withRows(dexFeed(r, map[uint64]string{r - 300: "105000000000000", r - 600: "100000000000000"}, map[uint64]string{r: "1"}),
				map[uint64]string{r - 300: "105000000000000", r - 600: "100000000000000"}),
			price: "10500000", ts: r - 300,
		},
		// p1 is whatever record the walk finds next: here 100 periods (8 h 20 m) back, with no age bound.
		{
			name: "previous record far back still counts", cfg: cfg, asset: ustry, t: r + 10,
			feed: one(r, map[uint64]string{r: "105000000000000", r - 30_000: "100000000000000"}), price: "10500000", ts: r,
		},
		{
			name: "one record cannot be checked", cfg: cfg, asset: ustry, t: r + 10,
			feed: one(r, map[uint64]string{r: "100000000000000"}), verdict: VerdictDeviationUnverifiable,
		},
		// A record at last − 4·resolution ends the walk (prices.rs:219), so the one below it is never read.
		{
			name: "walk stops at the lower boundary", cfg: cfg, asset: ustry, t: r + 10,
			feed: one(r, map[uint64]string{r - 1200: "100000000000000", r - 1500: "100000000000000"}), verdict: VerdictDeviationUnverifiable,
		},
		{
			name: "no record at all", cfg: cfg, asset: ustry, t: r + 10,
			feed: withRows(dexFeed(r, nil, map[uint64]string{r: "1"}), nil), verdict: VerdictPriceAbsent,
		},
		// Limit 10_000_000_000_000 exactly: `diff < max_dev` refuses it (v2.0.0's `diff > max_dev` would accept).
		{
			name: "a move of exactly max_dev is refused", cfg: cfg, asset: ustry, t: r + 10,
			feed: one(r, map[uint64]string{r: "110000000000000", r - 300: "100000000000000"}), verdict: VerdictDeviation,
		},
		{
			name: "a move just under max_dev passes", cfg: cfg, asset: ustry, t: r + 10,
			feed: one(r, map[uint64]string{r: "109999999999999", r - 300: "100000000000000"}), price: "10999999", ts: r,
		},
		// The chain says USTRY priced the last round but our store lacks it: never the older stored round.
		{
			name: "round on chain but not stored", cfg: cfg, asset: ustry, t: r + 10,
			feed:    withRows(dexFeed(r, map[uint64]string{r: "1", r - 300: "1"}, nil), map[uint64]string{r - 300: "100000000000000"}),
			verdict: VerdictFeedIncomplete,
		},
		{
			name: "last round after the ledger time", cfg: cfg, asset: ustry, t: r - 10,
			feed: one(r, map[uint64]string{r: "100000000000000", r - 300: "100000000000000"}), verdict: VerdictPriceStale,
		},
		{name: "unconfigured asset panics AssetNotFound", cfg: cfg, asset: Stellar(feedDEX), t: r, verdict: VerdictAssetMissing},
	})
}

func TestDeviationRule(t *testing.T) {
	yb := ybForkConfig(t)
	v2 := v2Config()
	cases := []struct {
		name  string
		cfg   Config
		asset Asset
		want  DeviationRule
		ok    bool
	}{
		{"fork USTRY", yb, Stellar(ustrySAC), DeviationRule{Kind: DeviationNewestTwoPricedRounds, MaxDevPercent: 10, MaxLookbackPeriods: 255, RejectsAtLimit: true}, true},
		{"fork XLM max_dev 0", yb, Stellar(xlmSAC), DeviationRule{Kind: DeviationNone}, true},
		{"fork base", yb, Stellar(usdcSAC), DeviationRule{Kind: DeviationNone}, true},
		{"fork base asset", yb, Stellar("CB226ZOEYXTBPD3QEGABTJYSKZVBP2PASEISLG3SBMTN5CE4QZUVZ3CE"), DeviationRule{Kind: DeviationNone}, true},
		{"fork unconfigured", yb, Stellar(feedDEX), DeviationRule{}, false},
		{"v2 XLM", v2, Stellar(xlmSAC), DeviationRule{Kind: DeviationPreviousPricedPeriod, MaxDevPercent: 20, MaxLookbackPeriods: 3}, true},
		{"v2 max_dev 100", v2, Stellar(feedCEX), DeviationRule{Kind: DeviationNone}, true},
		{"v1", v1Config(false), Stellar(xlmSAC), DeviationRule{Kind: DeviationNone}, true},
		{"unknown program", Config{Assets: yb.Assets}, Stellar(ustrySAC), DeviationRule{}, false},
	}
	for _, c := range cases {
		got, ok := c.cfg.DeviationRule(c.asset)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
