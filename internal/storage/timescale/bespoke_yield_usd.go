package timescale

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// defindexStrategyAmountDecimals is every strategy's amount scale: each wraps a classic asset's SAC (see defindexStrategyAsset).
const defindexStrategyAmountDecimals = 7

// defindexStrategyAsset is the one asset a DeFindex strategy manages, in both spellings the price tables store: the classic id
// (CODE-ISSUER, or native) and its SAC. Every strategy wraps a classic asset's SAC, so its scalar amounts are 7-decimal base units.
type defindexStrategyAsset struct {
	Classic, SAC string
}

const (
	dfxAssetCETES   = "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	dfxAssetUSTRY   = "USTRY-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	dfxAssetTESOURO = "TESOURO-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	dfxAssetEURC    = "EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	dfxAssetUSDC    = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

var (
	dfxSacCETES   = defindexStrategyAsset{dfxAssetCETES, "CAL6ER2TI6CTRAY6BFXWNWA7WTYXUXTQCHUBCIBU5O6KM3HJFG6Z6VXV"}
	dfxSacUSTRY   = defindexStrategyAsset{dfxAssetUSTRY, "CBLV4ATSIWU67CFSQU2NVRKINQIKUZ2ODSZBUJTJ43VJVRSBTZYOPNUR"}
	dfxSacTESOURO = defindexStrategyAsset{dfxAssetTESOURO, "CD6M4R2322BYCY2LNWM74PEBQAQ63SA3DUJLI3L4225U4ZVCLMSCBCIS"}
	dfxSacEURC    = defindexStrategyAsset{dfxAssetEURC, "CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV"}
	dfxSacUSDC    = defindexStrategyAsset{dfxAssetUSDC, "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"}
	dfxSacXLM     = defindexStrategyAsset{"native", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"}
)

// defindexStrategyAssets maps each defindex.MainnetStrategies entry to the asset its `asset()` view returns on mainnet; the SAC's
// name()/decimals() gave the classic id and the 7 places.
var defindexStrategyAssets = map[string]defindexStrategyAsset{
	"CA33NXYN7H3EBDSA3U2FPSULGJTTL3FQRHD2ADAAPTKS3FUJOE73735A": dfxSacEURC,
	"CA3SO5RRKOONAPWVR5XY6CMOYZGN4M4QKVIGX5DFRIIJUJW2SFSELBXL": dfxSacUSTRY,
	"CAHXQWU2HB74PIBT2BUIPYUZXMGZJEQUCNMQLEZR4OMNXMCEHYNEUWZQ": dfxSacUSDC,
	"CAZ3LLLKPWEOVK6K4G5NCQ2VXWABLFIPKKNMN5GLKMZKEN7JSKTEMIKN": dfxSacCETES,
	"CB5FP32DQKDA7Z7SJ7DGP2FRJRQLPXBSRMSK2KNLGC3V4SXWIIWLJWKM": dfxSacUSTRY,
	"CBDOIGFO2QOOZTWQZ7AFPH5JOUS2SBN5CTTXR665NHV6GOCM6OUGI5KP": dfxSacXLM,
	"CBTSRJLN5CVVOWLTH2FY5KNQ47KW5KKU3VWGASDN72STGMXLRRNHPRIL": dfxSacCETES,
	"CBTX63BX2I6E2VG2SMFQXDHLAPDOANUWBTMXQNWBV2FT6DIMVQPCSOBW": dfxSacXLM,
	"CBWD2EKIMVG6PM6VZEOCFJ3HPRZ2MDOIT3JCCK6WX2JURISQ3LWWBUIT": dfxSacCETES,
	"CC5CE6MWISDXT3MLNQ7R3FVILFVFEIH3COWGH45GJKL6BD2ZHF7F7JVI": dfxSacEURC,
	"CCBTSHPUVNKCT5V675AAVYNANHXBU26PTZK2QLS7ZLFNYRJZT5HW3VL6": dfxSacUSDC,
	"CCSRX5E4337QMCMC3KO3RDFYI57T5NZV5XB3W3TWE4USCASKGL5URKJL": dfxSacUSDC,
	"CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP": dfxSacUSDC,
	"CDDXPBOF727FDVTNV4I3G4LL4BHTJHE5BBC4W6WZAHMUPFDPBQBL6K7Y": dfxSacUSTRY,
	"CDPWNUW7UMCSVO36VAJSQHQECISPJLCVPDASKHRC5SEROAAZDUQ5DG2Z": dfxSacXLM,
	"CDSCVJHJWUZQMR64FVK3XMND5NKSN7Z23KPRCHKFHVGOEJBWPVH5B5XA": dfxSacTESOURO,
}

// defindexStrategyVolumeSeriesQuery is the flow sums at the window's series grain, complete days only past 24h. $1 = interval.
func defindexStrategyVolumeSeriesQuery(windowDays int) string {
	trunc, format := bridgeSeriesGrain(windowDays)
	return defindexStrategyFlowsQuery(trunc, format, completeDaysOnly(windowDays, "ledger_close_time"))
}

// defindexStrategyVolumeKPIQuery is the flow sums by day over the whole window, today included, like the other window KPIs. $1 = interval.
func defindexStrategyVolumeKPIQuery() string {
	return defindexStrategyFlowsQuery("day", "YYYY-MM-DD", "")
}

// defindexStrategyFlowsQuery returns strategy-layer deposit/withdraw sums per (strategy, direction, bucket, UTC day). A bucket never
// spans days, so each sum is valued at one day's price. 'harvest' is yield, not capital moved. $1 = interval.
func defindexStrategyFlowsQuery(trunc, format, extra string) string {
	return `
		SELECT contract_id, direction, to_char(date_trunc('` + trunc + `', ledger_close_time), '` + format + `'),
		  to_char(date_trunc('day', ledger_close_time), 'YYYY-MM-DD'), sum(amount)::text, count(amount)
		FROM defindex_flows
		WHERE ledger_close_time > now() - $1::interval AND layer = 'strategy' AND direction IN ('deposit', 'withdraw')
		  AND amount IS NOT NULL` + extra + `
		GROUP BY 1, 2, 3, 4`
}

// defindexDailyUSDPricesSQL reads each strategy asset's USD-quoted daily rows in both market directions: (asset, USD) rows and the
// flipped (USD, asset) rows. prices_1d leaves yesterday unmaterialized for hours after midnight, so the last two days also come from
// prices_1h, used only for a day prices_1d lacks. $1 = interval, $2 = base spellings, $3 = non-VWAP sources.
var defindexDailyUSDPricesSQL = strings.Join([]string{
	usdPriceArm("prices_1d", false, "date_trunc('day', now() - $1::interval)"),
	usdPriceArm("prices_1d", true, "date_trunc('day', now() - $1::interval)"),
	usdPriceArm("prices_1h", false, "GREATEST(date_trunc('day', now() - $1::interval), date_trunc('day', now()) - interval '1 day')"),
	usdPriceArm("prices_1h", true, "GREATEST(date_trunc('day', now() - $1::interval), date_trunc('day', now()) - interval '1 day')"),
}, "\nUNION ALL")

// usdPriceArm selects (daily, flipped, day, asset, vwap, volume_priced) from one table and direction. A flipped arm admits only the
// on-chain USDC proxies as base: their 7-decimal volume shares the asset's scale, which fiat:USD's does not.
func usdPriceArm(table string, flipped bool, from string) string {
	asset, usd, onChainOnly := "base_asset", "quote_asset", ""
	if flipped {
		asset, usd, onChainOnly = "quote_asset", "base_asset", " AND base_asset NOT LIKE 'fiat:%'"
	}
	return fmt.Sprintf(`
SELECT %t, %t, to_char(bucket, 'YYYY-MM-DD'), %s, vwap::text, volume_priced::text
  FROM %s
 WHERE %s IN (`+usdProxyQuotes+`)%s AND %s = ANY($2::text[])
   AND bucket >= %s
   AND vwap IS NOT NULL AND vwap > 0 AND volume_priced > 0
   AND NOT COALESCE(sources <@ string_to_array($3, ','), false)`, table == "prices_1d", flipped, asset, table, usd, onChainOnly, asset, from)
}

// strategyFlowSum is one row of defindexStrategyFlowsQuery, amount in base units.
type strategyFlowSum struct {
	strategy, direction, bucket, day string
	amount                           *big.Rat
	flows                            int64
}

// usdPriceRow is one row of defindexDailyUSDPricesSQL.
type usdPriceRow struct {
	daily, flipped  bool
	day, asset      string
	vwap, volPriced *big.Rat
}

type assetDay struct{ asset, day string }

// yieldUSD is strategy-layer volume valued in USD, keyed by direction then bucket; unpriced names what each direction excludes.
type yieldUSD struct {
	byDirBucket map[string]map[string]*big.Rat
	unpriced    map[string]*unpricedTally
}

// unpricedTally is one direction's excluded flows and why each was excluded.
type unpricedTally struct {
	flows  int64
	assets map[string]bool
}

// defindexPriceSpellings maps every stored spelling of a strategy asset (classic id and SAC) to its classic id, the price fold key.
func defindexPriceSpellings(assets map[string]defindexStrategyAsset) map[string]string {
	out := map[string]string{}
	for _, a := range assets {
		out[a.Classic] = a.Classic
		out[a.SAC] = a.Classic
	}
	return out
}

// isUSDProxyAsset reports an asset that is itself a USD proxy quote, worth 1 USD by the same rule the live price path applies.
func isUSDProxyAsset(id string) bool {
	return strings.Contains(usdProxyQuotes, "'"+id+"'")
}

// foldDailyUSDPrices is each (asset, day)'s volume-weighted USD price across its spellings, proxies and directions: Σ USD / Σ asset,
// where an (asset, USD) row adds vwap·vol USD for vol asset and a flipped (USD, asset) row adds vol USD for vwap·vol asset. All sides
// are 7-decimal on-chain amounts, so the weights share one scale. A day prices_1d holds never reads prices_1h.
func foldDailyUSDPrices(rows []usdPriceRow, spellings map[string]string) map[assetDay]*big.Rat {
	type fold struct{ usd, asset *big.Rat }
	folds := [2]map[assetDay]*fold{{}, {}} // [0] hourly, [1] daily
	for _, r := range rows {
		asset, ok := spellings[r.asset]
		if !ok {
			continue
		}
		tier := 0
		if r.daily {
			tier = 1
		}
		k := assetDay{asset, r.day}
		f := folds[tier][k]
		if f == nil {
			f = &fold{new(big.Rat), new(big.Rat)}
			folds[tier][k] = f
		}
		quoteSide := new(big.Rat).Mul(r.vwap, r.volPriced)
		if r.flipped {
			f.usd.Add(f.usd, r.volPriced)
			f.asset.Add(f.asset, quoteSide)
		} else {
			f.usd.Add(f.usd, quoteSide)
			f.asset.Add(f.asset, r.volPriced)
		}
	}
	out := map[assetDay]*big.Rat{}
	for _, tier := range folds {
		for k, f := range tier {
			if f.asset.Sign() > 0 {
				out[k] = new(big.Rat).Quo(f.usd, f.asset)
			}
		}
	}
	return out
}

// valueStrategyFlows values each flow sum at its day's USD price of the strategy's asset. A sum whose strategy is unmapped or whose
// asset has no price that day is excluded and counted, never guessed.
func valueStrategyFlows(flows []strategyFlowSum, assets map[string]defindexStrategyAsset, prices map[assetDay]*big.Rat) yieldUSD {
	out := yieldUSD{byDirBucket: map[string]map[string]*big.Rat{}, unpriced: map[string]*unpricedTally{}}
	scale := tenToMinus(defindexStrategyAmountDecimals)
	for _, f := range flows {
		a, mapped := assets[f.strategy]
		var price *big.Rat
		missing := ""
		switch {
		case !mapped:
			missing = "strategy " + f.strategy + " (no asset mapping)"
		case isUSDProxyAsset(a.Classic):
			price = big.NewRat(1, 1)
		default:
			if price = prices[assetDay{a.Classic, f.day}]; price == nil {
				missing = assetCode(a.Classic) + " (no USD price that day)"
			}
		}
		if price == nil {
			t := out.unpriced[f.direction]
			if t == nil {
				t = &unpricedTally{assets: map[string]bool{}}
				out.unpriced[f.direction] = t
			}
			t.flows += f.flows
			t.assets[missing] = true
			continue
		}
		byBucket := out.byDirBucket[f.direction]
		if byBucket == nil {
			byBucket = map[string]*big.Rat{}
			out.byDirBucket[f.direction] = byBucket
		}
		if byBucket[f.bucket] == nil {
			byBucket[f.bucket] = new(big.Rat)
		}
		byBucket[f.bucket].Add(byBucket[f.bucket], new(big.Rat).Mul(new(big.Rat).Mul(f.amount, scale), price))
	}
	return out
}

// assetCode is a classic id's display code: "CETES" for CETES-G…, "XLM" for native.
func assetCode(classic string) string {
	if classic == "native" {
		return "XLM"
	}
	code, _, _ := strings.Cut(classic, "-")
	return code
}

// total sums one direction across every bucket.
func (y yieldUSD) total(direction string) *big.Rat {
	sum := new(big.Rat)
	for _, v := range y.byDirBucket[direction] {
		sum.Add(sum, v)
	}
	return sum
}

// lowerBoundClause names what one direction's USD figure excludes, or "" when nothing was excluded.
func (y yieldUSD) lowerBoundClause(direction string) string {
	t := y.unpriced[direction]
	if t == nil {
		return ""
	}
	names := make([]string, 0, len(t.assets))
	for a := range t.assets {
		names = append(names, a)
	}
	sort.Strings(names)
	return fmt.Sprintf("excludes %d strategy flows it could not value in USD: %s", t.flows, strings.Join(names, ", "))
}

// strategyVolumeUSD runs a defindexStrategyFlowsQuery and values its sums in USD.
func (s *Store) strategyVolumeUSD(ctx context.Context, query, since string) (yieldUSD, error) {
	flows, err := s.scanStrategyFlowSums(ctx, query, since)
	if err != nil {
		return yieldUSD{}, err
	}
	spellings := defindexPriceSpellings(defindexStrategyAssets)
	bases := make([]string, 0, len(spellings))
	for sp := range spellings {
		bases = append(bases, sp)
	}
	sort.Strings(bases)
	prices, err := s.scanUSDPriceRows(ctx, since, bases)
	if err != nil {
		return yieldUSD{}, err
	}
	return valueStrategyFlows(flows, defindexStrategyAssets, foldDailyUSDPrices(prices, spellings)), nil
}

func (s *Store) scanStrategyFlowSums(ctx context.Context, query, since string) ([]strategyFlowSum, error) {
	rows, err := s.db.QueryContext(ctx, query, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeYield strategy flows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []strategyFlowSum
	for rows.Next() {
		var f strategyFlowSum
		var amount string
		if err := rows.Scan(&f.strategy, &f.direction, &f.bucket, &f.day, &amount, &f.flows); err != nil {
			return nil, fmt.Errorf("timescale: bespokeYield strategy flows scan: %w", err)
		}
		var ok bool
		if f.amount, ok = new(big.Rat).SetString(amount); !ok {
			return nil, fmt.Errorf("timescale: bespokeYield strategy flows: unparseable amount %q for %s", amount, f.strategy)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) scanUSDPriceRows(ctx context.Context, since string, bases []string) ([]usdPriceRow, error) {
	rows, err := s.db.QueryContext(ctx, defindexDailyUSDPricesSQL, since, bases, nonVWAPSourcesArg())
	if err != nil {
		return nil, fmt.Errorf("timescale: bespokeYield USD prices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []usdPriceRow
	for rows.Next() {
		var r usdPriceRow
		var vwap, vol string
		if err := rows.Scan(&r.daily, &r.flipped, &r.day, &r.asset, &vwap, &vol); err != nil {
			return nil, fmt.Errorf("timescale: bespokeYield USD prices scan: %w", err)
		}
		var ok1, ok2 bool
		r.vwap, ok1 = new(big.Rat).SetString(vwap)
		r.volPriced, ok2 = new(big.Rat).SetString(vol)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("timescale: bespokeYield USD prices: unparseable numeric for %s @ %s (vwap=%q volume=%q)", r.asset, r.day, vwap, vol)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// usdString renders a USD amount at cents; the sum itself stays exact.
func usdString(v *big.Rat) string {
	return v.FloatString(2)
}

// seriesFromBuckets turns one direction's bucket map into date-ordered points.
func seriesFromBuckets(byBucket map[string]*big.Rat) []BespokeSeriesPt {
	pts := make([]BespokeSeriesPt, 0, len(byBucket))
	for b, v := range byBucket {
		pts = append(pts, BespokeSeriesPt{Date: b, Value: usdString(v)})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].Date < pts[j].Date })
	return pts
}

// seriesLowerBoundClause names, per direction, what the volume series exclude, or "" when nothing was excluded.
func (y yieldUSD) seriesLowerBoundClause() string {
	var parts []string
	for _, d := range []string{"deposit", "withdraw"} {
		if c := y.lowerBoundClause(d); c != "" {
			parts = append(parts, d+" "+c)
		}
	}
	return strings.Join(parts, "; ")
}
