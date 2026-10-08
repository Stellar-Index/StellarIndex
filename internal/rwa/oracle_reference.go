package rwa

import (
	"sort"
	"strings"
)

// Oracle reference bindings — which oracle feed, if any, prices the
// instrument behind a specific Stellar asset: given an admitted
// (code, issuer), is there a feed measuring the SAME QUANTITY the
// Stellar market prices for that exact token?
//
// The binding is keyed on the exact (code, issuer), never the code. A
// code-keyed reference would publish an unrelated token called USTRY or
// BENJI at a large "discount" to a real security's net asset value: a
// false financial claim, attacker-authored pricing in a new coordinate.
//
// It is an ADR-0040 curated set, fail-closed and visible: an unbound
// pair gets a stated reason, never a number, and the served set is
// published as `definition.bound_instruments` so consumers can audit it.
//
// # What binds an entry
//
// Each entry below records the evidence for it. An entry needs all
// three:
//
//  1. The issuer's SEP-1 [[CURRENCIES]] entry for this exact (code,
//     issuer) names the real-world instrument (requirement R2).
//  2. The curated account directory attributes that G-address to a named
//     entity (R3). Where ADR-0028 also attributes the feed to an entity,
//     the two must AGREE; for its ticker-only feeds (CETES, USTRY,
//     TESOURO, GILTS, KTB, SPXU) requirement 3 carries the binding.
//  3. Something ties the FEED to that issuer specifically: price
//     agreement with the token's Stellar market (strongest), or a
//     documented product-line correspondence (weaker, marked as such).
//
// Entries carrying only the weaker form are the ones to challenge first
// in review. Nothing here is inferred at runtime: adding a pair is a
// code change, exactly as changing the audited wasm-hash set is.

// instrumentBinding is one curated (code, issuer) → feed pair.
type instrumentBinding struct {
	// Code and Issuer are the exact on-chain identity. Matched exactly:
	// a case variant is a different token unless someone says otherwise,
	// and saying otherwise is what this table is for.
	Code   string
	Issuer string
	// Feed is the ADR-0028 instrument code, spelled as
	// [canonical.KnownRWACodes] spells it.
	Feed string
}

// etherfuseIssuer issues the Stablebond line whose five instruments the
// oracle registry prices under their sovereign-instrument names.
//
// Evidence: the account's own stellar.toml at the domain it names on
// chain declares `Etherfuse CETES`, `Etherfuse USTRY` and
// `Etherfuse TESOURO` under this exact G-address, each `is_asset_anchored`
// with `anchor_asset_type = "bond"`; the curated directory attributes the
// account to `Etherfuse`; and the oracle's CETES / TESOURO / GILTS /
// USTRY / KTB set is that issuer's product line rather than a generic
// list of sovereign debt.
//
// The tie to the FEED is price-proved on all three, measured
// against each token's own 24h SDEX VWAP:
//
//	USTRY    1.0739100040 vs feed 1.07403800   —  1.19 bps
//	CETES    0.0697817092 vs feed 0.06988900   — 15.35 bps
//	TESOURO  0.2459935362 vs feed 0.24549700   — 20.23 bps
//
// The agreement is NOT circular: redstone is registered with
// IncludeInVWAP false (see internal/sources/external/registry.go), so an
// oracle reading cannot contribute to the VWAP it is being compared
// against. USTRY's dominant book is USTRY/USDC, so its USD leg comes
// from real SDEX trades. No unrelated issuer's token tracks a feed to
// one basis point through an independent path.
//
// Only CETES additionally has a SERVED price today, which is why it is
// the only one of the three currently publishing a premium
// (0.0698283638 against 0.06988900 = 8.68 bps). The other two hold a
// reference but no premium until the substance gate admits a market
// price for them.
const etherfuseIssuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"

// ondoIssuer issues the tokenized treasury-backed note the registry
// prices as USDY.
//
// Evidence: ADR-0028 attributes the feed to `Ondo US Dollar Yield`; the
// curated directory attributes this G-address to `Ondo`; the account
// names `ondo.finance` on chain and serves its SEP-1 from there. Both
// attributions are by entity and they agree.
//
// Weaker than the Etherfuse binding: the token has no Stellar market
// price, so there is no price agreement corroborating it. It rests on
// the two attributions matching.
const ondoIssuer = "GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"

// franklinTempletonIssuer issues the tokenized money-market fund the
// registry prices as BENJI.
//
// Evidence: the account sets www.franklintempleton.com as its on-chain
// home_domain, and the SEP-1 served from that domain verifies and
// declares BENJI with this account as its own issuer, anchored to FOBXX
// — "Franklin OnChain U.S. Government Money Fund". The curated directory
// independently attributes this G-address to `Franklin Templeton`,
// tagged `issuer`. The toml declares five share classes against the same
// fund (BENJI, FOCGX, gBENJI, grBENJI, sgBENJI); only BENJI is bound
// here, because only BENJI is what the oracle prices.
//
// When measured, this account held 12,498 trustlines and had never
// traded, so a registry populated from trades alone did not list it.
const franklinTempletonIssuer = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"

// instrumentBindings is the curated set.
//
// Deliberately smaller than "every code an oracle prices". A binding
// that fires the first time some account issues a matching code is the
// code-keyed join again with extra steps, so a code is bound only once
// THIS issuer is observed to have issued it.
//
// GILTS and KTB meet that bar: both are issued by this account and
// carry holding evidence on chain. Neither had traded when measured —
// which is what a held-to-maturity instrument looks like — so a registry
// populated from trades alone would not see them.
//
// Evidence grade is stated per entry, because the file's own policy
// above says the weaker form is marked as such and a grade recorded only
// on the issuer constant does not travel with the row a reviewer reads.
var instrumentBindings = []instrumentBinding{
	// Price-proved against the token's own SDEX VWAP — see etherfuseIssuer.
	{Code: "CETES", Issuer: etherfuseIssuer, Feed: "CETES"},
	{Code: "USTRY", Issuer: etherfuseIssuer, Feed: "USTRY"},
	{Code: "TESOURO", Issuer: etherfuseIssuer, Feed: "TESOURO"},
	// WEAKER: attribution-only. No Stellar market price exists for this
	// token, so nothing corroborates the two matching attributions.
	// Challenge this one first.
	{Code: "USDY", Issuer: ondoIssuer, Feed: "USDY"},
	// Same issuer as the three above, same evidence for the ACCOUNT, but
	// WEAKER per row: neither has a Stellar market price, so the
	// price-agreement that corroborates CETES/USTRY/TESOURO does not
	// reach these two. Both are declared in this account's own SEP-1
	// bound to itself, and the oracle prices an instrument of the same
	// name.
	{Code: "GILTS", Issuer: etherfuseIssuer, Feed: "GILTS"},
	{Code: "KTB", Issuer: etherfuseIssuer, Feed: "KTB"},
	// WEAKEST GRADE PRESENT, and the largest figure — challenge this one
	// before the others.
	//
	// Evidence: the account names www.franklintempleton.com on chain and
	// serves a SEP-1 from it that verifies, declaring BENJI bound to
	// ITSELF and anchored to FOBXX, "Franklin OnChain U.S. Government
	// Money Fund". The curated directory attributes the same G-address to
	// `Franklin Templeton` with tag `issuer`. Two independent
	// attributions, by entity, agreeing.
	//
	// What does NOT corroborate it: no Stellar market price exists, so
	// there is no price agreement; and the feed is a constant 1.00, which
	// is what a money-market fund holding a stable NAV should read but is
	// also what a broken feed reads. A drifting feed proves itself right
	// by tracking; a pegged one cannot.
	//
	// Why the code alone is not enough here, concretely: this network
	// carries TWENTY-SIX assets with the code BENJI and exactly one of
	// them is this issuer. The rest sit on lookalike domains —
	// franklintempleton.co.com, franklintempleton.hqlumens.com,
	// stellar.dtcc.network — and are kept out by requirement 3, not by
	// this table. Binding on the pair is what makes that safe.
	{Code: "BENJI", Issuer: franklinTempletonIssuer, Feed: "BENJI"},
}

// bindingIndex is instrumentBindings keyed for lookup. Built once; the
// table is a compile-time constant in every meaningful sense.
var bindingIndex = func() map[instrumentKey]string {
	m := make(map[instrumentKey]string, len(instrumentBindings))
	for _, b := range instrumentBindings {
		m[instrumentKey{code: b.Code, issuer: b.Issuer}] = b.Feed
	}
	return m
}()

type instrumentKey struct{ code, issuer string }

// offChainReferenceCodes are the ADR-0028 codes whose feed prices an
// OFF-CHAIN quantity in its own unit — a troy ounce of spot metal, one
// share of an exchange-traded fund — rather than one token of anything.
//
// No binding may target one of these; a token's price and a per-ounce
// spot price are different quantities, and their ratio is a unit
// conversion rather than a premium.
// TestNoBindingTargetsAnOffChainReference enforces it.
//
// They are named rather than merely absent so a row for a token of that
// code can state the informative refusal — "the oracle prices an ounce,
// not your token" — instead of the generic "nothing binds this pair".
var offChainReferenceCodes = map[string]struct{}{
	"XAU":  {}, // spot gold, one troy ounce (Reflector FX slot)
	"SPXU": {}, // one share of an inverse S&P 500 ETF
}

// InstrumentFeed returns the ADR-0028 feed code bound to this exact
// (code, issuer), and whether any binding exists.
//
// EXACT on both halves. The code is not case-folded: `XAUM` is not
// `XAUm`, and treating them as one token is precisely the collision this
// function exists to refuse. Surrounding whitespace is trimmed because
// it is never part of an identity.
func InstrumentFeed(code, issuer string) (string, bool) {
	feed, ok := bindingIndex[instrumentKey{
		code:   strings.TrimSpace(code),
		issuer: strings.TrimSpace(issuer),
	}]
	return feed, ok
}

// OffChainReferenceCode reports whether an oracle feed of this code
// prices an off-chain quantity rather than a token. Used only to choose
// which refusal to report; it grants nothing.
func OffChainReferenceCode(code string) bool {
	_, ok := offChainReferenceCodes[strings.TrimSpace(code)]
	return ok
}

// InstrumentBinding is one served binding — the pair this surface will
// compare, and the feed it will compare it against.
type InstrumentBinding struct {
	Code   string `json:"code"`
	Issuer string `json:"issuer"`
	Feed   string `json:"feed"`
}

// InstrumentBindings lists the curated set in a stable order, for the
// same reason [AnchorClasses] is served: the rule travels with the rows.
// The feed is rendered in its canonical `rwa:` form, the id a consumer
// can take straight to the oracle endpoints.
func InstrumentBindings() []InstrumentBinding {
	out := make([]InstrumentBinding, 0, len(instrumentBindings))
	for _, b := range instrumentBindings {
		out = append(out, InstrumentBinding{
			Code:   b.Code,
			Issuer: b.Issuer,
			Feed:   "rwa:" + b.Feed,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Issuer < out[j].Issuer
	})
	return out
}

// fundNAVBinding binds one Stellar (code, issuer) to the ticker of the
// registered fund whose published daily NAV is the value of one token.
type fundNAVBinding struct {
	Code, Issuer, Ticker string
}

// fundNAVBindings are the WisdomTree digital funds: one token is one fund
// share, so the fund's SEC-reported NAV is the token's value. Each fund
// has its own issuer, and stellar.wisdomtree.com's SEP-1 declares every
// (code, issuer) below with anchor_asset set to the ticker.
//
// Keyed on the pair for the same reason as [instrumentBindings]: a token
// merely named TIPS or SPXU is not a WisdomTree fund share.
var fundNAVBindings = []fundNAVBinding{
	{Code: "WTTS", Issuer: "GBBV5CF7UPA2PYRPA632URLB55BWML7X4H33ZRCDWMTULOXDGPHJR5VI", Ticker: "WTTSX"},
	{Code: "WTST", Issuer: "GDEBI5X7J4IDXCSVV3KPFZIHQRCBVF3DAZMS5H7KYOBK45T6XYGDE77P", Ticker: "WTSTX"},
	{Code: "FLTT", Issuer: "GBTZKH3RNKW46XEZNCGZEBAGJISKDZKQXKSQ2N5G5SFX36TLWKKR6QJ6", Ticker: "FLTTX"},
	{Code: "WTLG", Issuer: "GAK7PE7DD4ZRJQN3VBCQFBKFV53JGUM2SQATQAKLFK6MVONPGNYK34XH", Ticker: "WTLGX"},
	{Code: "WTSI", Issuer: "GAD22PDBRFEMXAKPFDP4JGDFWKKD6VPXWUWEAXBS6ZYJYFFQDUN7HAFG", Ticker: "WTSIX"},
	{Code: "WTSY", Issuer: "GB3ZUC7FGDEEBXY3BDEJWMPNGBFA66YRI4QQT6PBO3ZT6F33S7RL36VF", Ticker: "WTSYX"},
	{Code: "TIPS", Issuer: "GAJ4KSYLVBJKQ4UBPKJJXPYWVIRZWVTIYRMHBXTHGCDS4XJXXYEUALVD", Ticker: "TIPSX"},
	{Code: "EQTY", Issuer: "GAKODZFS4MV36JGDTULJACWJKBJCO33CJTVTWSQFSUV7XLZJNXTDH6D6", Ticker: "EQTYX"},
	{Code: "LNGV", Issuer: "GAHOGWBAWNIKESGNNW7Y7JU5KL54HIEHJGY6Y5QLY6YR3J7WZIDHLC6D", Ticker: "LNGVX"},
	{Code: "MODR", Issuer: "GANULT25TFO6V6BFWSEG4VSCR4QXBNHV5T344R2AFZEPE6B324LVLOOJ", Ticker: "MODRX"},
	{Code: "SPXU", Issuer: "GDJBVX3QA5HJPBSAU5VIX2W6MC37NU4UFXPKEGK42SJCYN6AEQ4Z6COM", Ticker: "SPXUX"},
	{Code: "TECH", Issuer: "GDSAW27GPR7EWKPTFDPGN2WWZYUHBFKVDBLOUUEKSNKHID4ZWUVOBF5R", Ticker: "TECHX"},
}

var fundNAVIndex = func() map[instrumentKey]string {
	m := make(map[instrumentKey]string, len(fundNAVBindings))
	for _, b := range fundNAVBindings {
		m[instrumentKey{code: b.Code, issuer: b.Issuer}] = b.Ticker
	}
	return m
}()

// FundNAVTicker returns the fund ticker bound to this exact (code,
// issuer), and whether any binding exists. Exact on both halves, as
// [InstrumentFeed] is.
func FundNAVTicker(code, issuer string) (string, bool) {
	t, ok := fundNAVIndex[instrumentKey{
		code:   strings.TrimSpace(code),
		issuer: strings.TrimSpace(issuer),
	}]
	return t, ok
}

// FundNAVTickers lists the bound tickers, sorted — the set the NAV poller
// fetches.
func FundNAVTickers() []string {
	out := make([]string, 0, len(fundNAVBindings))
	for _, b := range fundNAVBindings {
		out = append(out, b.Ticker)
	}
	sort.Strings(out)
	return out
}
