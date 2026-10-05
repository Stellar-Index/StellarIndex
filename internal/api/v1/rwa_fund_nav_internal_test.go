// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/tiingo"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	wttsIssuer = "GBBV5CF7UPA2PYRPA632URLB55BWML7X4H33ZRCDWMTULOXDGPHJR5VI"
	spxuIssuer = "GDJBVX3QA5HJPBSAU5VIX2W6MC37NU4UFXPKEGK42SJCYN6AEQ4Z6COM"
	benjiFTIss = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
)

var fundNAVNow = time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC)

func fundNAVAsset(code, issuer string) *RWAAsset {
	supply := "10000000000" // 1,000 tokens at 7 decimals
	return &RWAAsset{AssetID: code + "-" + issuer, Code: code, Issuer: issuer, CirculatingSupply: &supply, Decimals: intPtr(7)}
}

// tiingoRow is one stored fund-NAV row as the poller writes it.
func tiingoRow(t *testing.T, ticker string, price int64, at time.Time) canonical.OracleUpdate {
	t.Helper()
	asset, err := canonical.NewOracleRawAsset(ticker)
	if err != nil {
		t.Fatalf("NewOracleRawAsset(%q): %v", ticker, err)
	}
	return canonical.OracleUpdate{
		Source: tiingo.SourceName, Asset: asset, Quote: fiatAsset(t, "USD"),
		Price: canonical.NewAmount(big.NewInt(price)), Decimals: tiingo.Decimals, Timestamp: at,
	}
}

func fiatAsset(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		t.Fatalf("NewFiatAsset(%q): %v", code, err)
	}
	return a
}

func fundNAVSnap(t *testing.T, rows ...canonical.OracleUpdate) rwaReferences {
	t.Helper()
	return rwaReferenceSnapshotFrom(rows)
}

// The fund arm sits below the oracle arm and above the listing and
// prospectus arms, and falls through to them when it holds no NAV.
func TestRWAFundNAV_ArmOrder(t *testing.T) {
	snap := fundNAVSnap(t, tiingoRow(t, "WTTSX", 9_440_000, fundNAVNow.Add(-22*time.Hour)))
	listed := func(a *RWAAsset) map[string]timescale.ListingEntry {
		return map[string]timescale.ListingEntry{a.AssetID: {PriceUSD: "9.61", PricedAt: fundNAVNow.Add(-time.Hour), Source: "listing", ListingID: "x"}}
	}

	a := fundNAVAsset("WTTS", wttsIssuer)
	rwaApplyReference(a, snap, nil, listed(a), fundNAVNow)
	r := a.Reference
	if r == nil || r.Provenance != RWAReferenceFundNAV {
		t.Fatalf("WTTS with a NAV and a listing price: %+v, want the fund_nav arm", r)
	}
	if r.PriceUSD != "9.44" || r.Source != tiingo.SourceName || r.Feed != "raw:WTTSX" || r.Quote != "fiat:USD" ||
		r.DecimalsPublished == nil || *r.DecimalsPublished != 2 || r.Stale || r.NAVDisagreement {
		t.Errorf("WTTS reference = %+v", r)
	}
	if got := time.Time(r.AsOf); !got.Equal(fundNAVNow.Add(-22 * time.Hour)) {
		t.Errorf("as_of = %v, want the NAV date", got)
	}
	if a.ReferenceValuation.ValueUSD == nil || *a.ReferenceValuation.ValueUSD != "9440.00" {
		t.Errorf("valuation = %+v, want 1,000 shares × 9.44", a.ReferenceValuation)
	}
	if a.Premium.Status != RWAPremiumReferenceNotOracleFundNAV {
		t.Errorf("premium = %+v, want %s", a.Premium, RWAPremiumReferenceNotOracleFundNAV)
	}

	// SPXU is an off-chain reference code; its binding still prices it.
	s := fundNAVAsset("SPXU", spxuIssuer)
	rwaApplyReference(s, fundNAVSnap(t, tiingoRow(t, "SPXUX", 25_120_000, fundNAVNow.Add(-22*time.Hour))), nil, nil, fundNAVNow)
	if s.Reference == nil || s.Reference.Provenance != RWAReferenceFundNAV || s.Reference.PriceUSD != "25.12" {
		t.Errorf("SPXU = %+v premium %q, want fund_nav 25.12", s.Reference, s.Premium.Status)
	}

	// No NAV for the pair: the listing arm answers exactly as before.
	b := fundNAVAsset("WTTS", wttsIssuer)
	rwaApplyReference(b, fundNAVSnap(t), nil, listed(b), fundNAVNow)
	if b.Reference == nil || b.Reference.Provenance != RWAReferenceListingPrice {
		t.Errorf("WTTS without a NAV: %+v, want the listing arm", b.Reference)
	}

	// Same code, other issuer: never the fund's NAV.
	c := fundNAVAsset("WTTS", benjiFTIss)
	rwaApplyReference(c, snap, nil, nil, fundNAVNow)
	if c.Reference != nil {
		t.Errorf("WTTS under a foreign issuer got %+v", c.Reference)
	}

	// An oracle binding outranks a fund binding on the same pair.
	bindFundNAV(t, "BENJI", benjiFTIss, "FTXXX")
	o := fundNAVAsset("BENJI", benjiFTIss)
	oracleSnap := fundNAVSnap(t, tiingoRow(t, "FTXXX", 1_500_000, fundNAVNow.Add(-22*time.Hour)))
	oracleSnap.byFeed["BENJI"] = rwaReference{priceUSD: big.NewRat(1, 1), wire: "1.000000", source: "redstone", feed: "rwa:BENJI", asOf: fundNAVNow.Add(-time.Hour)}
	rwaApplyReference(o, oracleSnap, nil, nil, fundNAVNow)
	if o.Reference == nil || o.Reference.Provenance != RWAReferenceOracleNAV || o.Reference.PriceUSD != "1.000000" {
		t.Errorf("oracle- and fund-bound BENJI = %+v, want the oracle figure", o.Reference)
	}
}

// bindFundNAV adds a fund binding for a pair the oracle arm also binds;
// none exists today, so the disagreement check needs one made.
func bindFundNAV(t *testing.T, code, issuer, ticker string) {
	t.Helper()
	orig := rwaFundNAVTicker
	rwaFundNAVTicker = func(c, i string) (string, bool) {
		if c == code && i == issuer {
			return ticker, true
		}
		return orig(c, i)
	}
	t.Cleanup(func() { rwaFundNAVTicker = orig })
}

// The flag is raised strictly beyond half a cent, on either side, and
// never swaps the served figure.
func TestRWAFundNAV_NAVDisagreementBoundary(t *testing.T) {
	bindFundNAV(t, "BENJI", benjiFTIss, "FTXXX")
	cases := []struct {
		oracle string
		want   bool
	}{
		{"1.005000", false},
		{"1.005001", true},
		{"0.995000", false},
		{"0.994999", true},
		{"1.000000", false},
	}
	for _, tc := range cases {
		oracle, ok := new(big.Rat).SetString(tc.oracle)
		if !ok {
			t.Fatalf("bad fixture %q", tc.oracle)
		}
		snap := fundNAVSnap(t, tiingoRow(t, "FTXXX", 1_000_000, fundNAVNow.Add(-22*time.Hour)))
		snap.byFeed["BENJI"] = rwaReference{priceUSD: oracle, wire: tc.oracle, source: "redstone", feed: "rwa:BENJI", asOf: fundNAVNow.Add(-time.Hour)}
		a := fundNAVAsset("BENJI", benjiFTIss)
		rwaApplyReference(a, snap, nil, nil, fundNAVNow)
		if a.Reference == nil || a.Reference.Provenance != RWAReferenceOracleNAV || a.Reference.PriceUSD != tc.oracle {
			t.Fatalf("oracle %s: reference = %+v, want the oracle figure served", tc.oracle, a.Reference)
		}
		if a.Reference.NAVDisagreement != tc.want {
			t.Errorf("oracle %s vs NAV 1.00: nav_disagreement = %v, want %v", tc.oracle, a.Reference.NAVDisagreement, tc.want)
		}
	}
}

// A fund NAV is served up to five days old and refused one second past.
func TestRWAFundNAV_StaleRefusal(t *testing.T) {
	at5d := fundNAVSnap(t, tiingoRow(t, "WTTSX", 9_440_000, fundNAVNow.Add(-5*24*time.Hour)))
	a := fundNAVAsset("WTTS", wttsIssuer)
	rwaApplyReference(a, at5d, nil, nil, fundNAVNow)
	if a.Reference == nil || a.Reference.Provenance != RWAReferenceFundNAV || !a.Reference.Stale {
		t.Errorf("NAV exactly 5d old: %+v, want served and labelled stale", a.Reference)
	}

	past := fundNAVSnap(t, tiingoRow(t, "WTTSX", 9_440_000, fundNAVNow.Add(-5*24*time.Hour-time.Second)))
	listed := map[string]timescale.ListingEntry{"WTTS-" + wttsIssuer: {PriceUSD: "9.61", PricedAt: fundNAVNow.Add(-time.Hour), Source: "listing", ListingID: "x"}}
	b := fundNAVAsset("WTTS", wttsIssuer)
	rwaApplyReference(b, past, nil, listed, fundNAVNow)
	if b.Reference != nil || b.Premium.Status != RWAPremiumReferenceExpired {
		t.Errorf("NAV 5d+1s old: reference=%+v premium=%q, want refused %s", b.Reference, b.Premium.Status, RWAPremiumReferenceExpired)
	}

	// An expired NAV cannot flag an oracle reference either.
	bindFundNAV(t, "BENJI", benjiFTIss, "FTXXX")
	snap := fundNAVSnap(t, tiingoRow(t, "FTXXX", 2_000_000, fundNAVNow.Add(-5*24*time.Hour-time.Second)))
	snap.byFeed["BENJI"] = rwaReference{priceUSD: big.NewRat(1, 1), wire: "1.000000", source: "redstone", feed: "rwa:BENJI", asOf: fundNAVNow.Add(-time.Hour)}
	o := fundNAVAsset("BENJI", benjiFTIss)
	rwaApplyReference(o, snap, nil, nil, fundNAVNow)
	if o.Reference == nil || o.Reference.NAVDisagreement {
		t.Errorf("oracle row beside an expired NAV: %+v, want no disagreement flag", o.Reference)
	}
}

// Only tiingo's raw rows reach the fund map, dollars only, newest wins,
// and the stored 6-dp figure reads at its published cent precision.
func TestRWAFundNAV_Snapshot(t *testing.T) {
	older := tiingoRow(t, "WTTSX", 9_430_000, fundNAVNow.Add(-46*time.Hour))
	newer := tiingoRow(t, "WTTSX", 9_440_000, fundNAVNow.Add(-22*time.Hour))
	eur := tiingoRow(t, "FLTTX", 1_000_000, fundNAVNow.Add(-time.Hour))
	eur.Quote = fiatAsset(t, "EUR")
	other := tiingoRow(t, "TIPSX", 1_000_000, fundNAVNow.Add(-time.Hour))
	other.Source = "redstone"

	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{newer, older, eur, other})
	if len(snap.byFeed) != 0 {
		t.Errorf("fund rows leaked into the oracle feed map: %v", snap.byFeed)
	}
	if len(snap.fundNAV) != 1 || snap.fundNAV["WTTSX"].wire != "9.44" {
		t.Errorf("fundNAV = %+v, want only WTTSX at the newer 9.44", snap.fundNAV)
	}

	for in, want := range map[string]string{"9.440000": "9.44", "9.445000": "9.445", "1.000000": "1.00", "12": "12", "0.100001": "0.100001"} {
		if got := trimScaledZeros(in, 2); got != want {
			t.Errorf("trimScaledZeros(%q, 2) = %q, want %q", in, got, want)
		}
	}
}

// Every one of the twelve bound fund shares is priced by its own NAV.
func TestRWAFundNAV_AllTwelveBindingsResolve(t *testing.T) {
	pairs := map[string]string{
		"WTTS": wttsIssuer,
		"WTST": "GDEBI5X7J4IDXCSVV3KPFZIHQRCBVF3DAZMS5H7KYOBK45T6XYGDE77P",
		"FLTT": "GBTZKH3RNKW46XEZNCGZEBAGJISKDZKQXKSQ2N5G5SFX36TLWKKR6QJ6",
		"WTLG": "GAK7PE7DD4ZRJQN3VBCQFBKFV53JGUM2SQATQAKLFK6MVONPGNYK34XH",
		"WTSI": "GAD22PDBRFEMXAKPFDP4JGDFWKKD6VPXWUWEAXBS6ZYJYFFQDUN7HAFG",
		"WTSY": "GB3ZUC7FGDEEBXY3BDEJWMPNGBFA66YRI4QQT6PBO3ZT6F33S7RL36VF",
		"TIPS": "GAJ4KSYLVBJKQ4UBPKJJXPYWVIRZWVTIYRMHBXTHGCDS4XJXXYEUALVD",
		"EQTY": "GAKODZFS4MV36JGDTULJACWJKBJCO33CJTVTWSQFSUV7XLZJNXTDH6D6",
		"LNGV": "GAHOGWBAWNIKESGNNW7Y7JU5KL54HIEHJGY6Y5QLY6YR3J7WZIDHLC6D",
		"MODR": "GANULT25TFO6V6BFWSEG4VSCR4QXBNHV5T344R2AFZEPE6B324LVLOOJ",
		"SPXU": spxuIssuer,
		"TECH": "GDSAW27GPR7EWKPTFDPGN2WWZYUHBFKVDBLOUUEKSNKHID4ZWUVOBF5R",
	}
	tickers := rwa.FundNAVTickers()
	if len(tickers) != len(pairs) {
		t.Fatalf("%d fund tickers, want %d", len(tickers), len(pairs))
	}
	var rows []canonical.OracleUpdate
	for _, tk := range tickers {
		rows = append(rows, tiingoRow(t, tk, 10_000_000, fundNAVNow.Add(-22*time.Hour)))
	}
	snap := rwaReferenceSnapshotFrom(rows)
	for code, issuer := range pairs {
		a := fundNAVAsset(code, issuer)
		rwaApplyReference(a, snap, nil, nil, fundNAVNow)
		if a.Reference == nil || a.Reference.Provenance != RWAReferenceFundNAV || a.Reference.PriceUSD != "10.00" {
			t.Errorf("%s-%s: reference=%+v premium=%q, want fund_nav 10.00", code, issuer, a.Reference, a.Premium.Status)
		}
	}
}
