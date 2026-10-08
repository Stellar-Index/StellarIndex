package rwa

import (
	"sort"
	"strings"
)

// Oracle reference bindings: which feed measures the same quantity a specific Stellar
// (code, issuer) prices. Keyed on the exact pair, never the code, or an unrelated USTRY would
// publish a false discount to a real NAV. ADR-0040 curated, fail-closed, served for audit.
//
// An entry needs the issuer's SEP-1 naming the instrument, a directory attribution of the
// G-address, and a feed-to-issuer tie (price agreement, or a weaker product-line match marked
// as such). Adding a pair is a code change; nothing is inferred at runtime.

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

// etherfuseIssuer issues the Stablebond line. Its SEP-1 declares CETES, USTRY and TESOURO
// as bonds and the directory attributes it to Etherfuse; all three are price-proved against
// their own 24h SDEX VWAP (1.19, 15.35, 20.23 bps), non-circular since redstone is excluded from VWAP.
const etherfuseIssuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"

// ondoIssuer issues USDY. Weaker than Etherfuse: no Stellar market price, so it rests on
// ADR-0028 and the directory agreeing on Ondo.
const ondoIssuer = "GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"

// franklinTempletonIssuer issues BENJI (FOBXX), declared by the verified SEP-1 at its
// on-chain home domain and attributed by the directory; of five share classes only BENJI is
// what the oracle prices.
const franklinTempletonIssuer = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"

// instrumentBindings is deliberately smaller than every oracle code: a code binds only once
// THIS issuer is observed to have issued it, else it is the code-keyed join again. The
// evidence grade is stated per entry so it travels with the row a reviewer reads.
var instrumentBindings = []instrumentBinding{
	// Price-proved against the token's own SDEX VWAP — see etherfuseIssuer.
	{Code: "CETES", Issuer: etherfuseIssuer, Feed: "CETES"},
	{Code: "USTRY", Issuer: etherfuseIssuer, Feed: "USTRY"},
	{Code: "TESOURO", Issuer: etherfuseIssuer, Feed: "TESOURO"},
	// WEAKER: attribution-only. No Stellar market price exists for this
	// token, so nothing corroborates the two matching attributions.
	// Challenge this one first.
	{Code: "USDY", Issuer: ondoIssuer, Feed: "USDY"},
	// WEAKER per row: same account evidence, but no Stellar market price.
	{Code: "GILTS", Issuer: etherfuseIssuer, Feed: "GILTS"},
	{Code: "KTB", Issuer: etherfuseIssuer, Feed: "KTB"},
	// WEAKEST GRADE and largest figure: challenge first. No market price, and a pegged 1.00
	// feed cannot prove itself the way a drifting one does. Twenty-six BENJI assets exist on
	// lookalike domains; binding on the pair is what keeps them out.
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

// offChainReferenceCodes price an off-chain unit (an ounce, an ETF share), so no binding
// may target one (TestNoBindingTargetsAnOffChainReference); they are named so a row can
// state that refusal instead of the generic one.
var offChainReferenceCodes = map[string]struct{}{
	"XAU":  {}, // spot gold, one troy ounce (Reflector FX slot)
	"SPXU": {}, // one share of an inverse S&P 500 ETF
}

// InstrumentFeed returns the feed bound to this exact (code, issuer). The code is not
// case-folded (XAUM is not XAUm); surrounding whitespace is trimmed.
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

// fundNAVBindings are WisdomTree digital funds: one token is one share, so the SEC-reported
// NAV is its value. Keyed on the pair, as stellar.wisdomtree.com's SEP-1 declares them.
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
