package v1

import (
	"context"
	"reflect"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tokenDisplaysOnly serves TokenDisplays from a map; every other ExplorerReader method panics via the nil embed.
type tokenDisplaysOnly struct {
	ExplorerReader
	m map[string]clickhouse.TokenDisplayMeta
}

func (f tokenDisplaysOnly) TokenDisplays(_ context.Context, tokens []string) (map[string]clickhouse.TokenDisplayMeta, error) {
	out := map[string]clickhouse.TokenDisplayMeta{}
	for _, t := range tokens {
		if m, ok := f.m[t]; ok {
			out[t] = m
		}
	}
	return out, nil
}

const (
	usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	poolC  = "CPOOL000000000000000000000000000000000000000000000000001"
)

// Valid contract ids that are no catalogued SAC: the SACs of uncatalogued classic assets.
var unknownC, vaultC, sixDecimalC string

func newBespokeTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("currency.LoadEmbedded: %v", err)
	}
	xlmSAC := sacFor(t, canonical.NativeAsset())
	usdcSAC := classicSAC(t, usdcID)
	issuer := usdcID[len("USDC-"):]
	unknownC, vaultC, sixDecimalC = classicSAC(t, "ZZA-"+issuer), classicSAC(t, "ZZB-"+issuer), classicSAC(t, "ZZC-"+issuer)
	s := New(Options{
		VerifiedCurrencies: cat,
		ProtocolPoolTokens: &scriptedPoolTokens{resp: map[string][]string{poolC: {usdcSAC, xlmSAC}}},
		Explorer: tokenDisplaysOnly{m: map[string]clickhouse.TokenDisplayMeta{
			vaultC:      {Name: "DeFindex-Vault-Greep Vault", Symbol: "GRPVLT", Decimals: 7, HasMeta: true},
			sixDecimalC: {Symbol: "USDC", Decimals: 6, HasMeta: true},
		}},
	})
	return s, xlmSAC, usdcSAC
}

func TestBespokeResolver_Labels(t *testing.T) {
	s, xlmSAC, usdcSAC := newBespokeTestServer(t)
	b := &ProtocolBespoke{Series: []BespokeSeries{
		{ID: xlmSAC, Kind: timescale.BespokeKindAsset},
		{ID: "native", Kind: timescale.BespokeKindAsset},
		{ID: "crypto:XLM", Kind: timescale.BespokeKindAsset},
		{ID: usdcSAC, Kind: timescale.BespokeKindAsset},
		{ID: usdcID, Kind: timescale.BespokeKindAsset},
		{ID: unknownC, Kind: timescale.BespokeKindAsset},
		{ID: sixDecimalC, Kind: timescale.BespokeKindAsset},
		{ID: "USDC", Kind: timescale.BespokeKindAsset},
		{ID: poolC, Kind: timescale.BespokeKindPool},
		{ID: unknownC, Kind: timescale.BespokeKindPool},
		{ID: vaultC, Kind: timescale.BespokeKindVault},
		{ID: unknownC, Kind: timescale.BespokeKindVault},
		{ID: usdcSAC, Kind: timescale.BespokeKindContract},
	}}
	s.resolveBespoke(context.Background(), "comet", b)
	want := []string{
		"XLM", "XLM", "XLM", "USDC", "USDC", "",
		"", // a contract's self-declared symbol is not trusted as a ticker
		"", // a bare code is not an asset identity
		"USDC / XLM", "", "Greep Vault (GRPVLT)", "", "",
	}
	for i, e := range b.Series {
		if e.Display != want[i] {
			t.Errorf("series %d (%s %s) display = %q, want %q", i, e.Kind, e.ID, e.Display, want[i])
		}
	}
}

func TestScaleBaseUnits(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		decimals int
		want     string
		ok       bool
	}{
		{"12345678", 7, "1.2345678", true},
		{"-12345678", 7, "-1.2345678", true},
		{"10000000", 7, "1", true},
		{"5", 7, "0.0000005", true},
		{"0", 7, "0", true},
		{"42", 0, "42", true},
		// Above 2^63: exact, no float rounding.
		{"123456789012345678901234567890", 18, "123456789012.34567890123456789", true},
		{"-170141183460469231731687303715884105728", 7, "-17014118346046923173168730371588.4105728", true},
		{"—", 7, "", false},
		{"1.5", 7, "", false},
		{"1", -1, "", false},
	} {
		got, ok := scaleBaseUnits(tc.raw, tc.decimals)
		if got != tc.want || ok != tc.ok {
			t.Errorf("scaleBaseUnits(%q, %d) = %q, %v; want %q, %v", tc.raw, tc.decimals, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBespokeFromStore_TypedTableKeepsRowsAndAddsDisplay(t *testing.T) {
	s, _, usdcSAC := newBespokeTestServer(t)
	seven := 7
	rows := [][]string{
		{usdcSAC, "-12345678", "0", "3"},
		{sixDecimalC, "1500000", "-2500000", "2"},
		{unknownC, "99", "1", "1"},
		{"native", "9223372036854775808000", "1", "1"},
	}
	blk := &timescale.BespokeBlock{
		Category: "lending",
		Tables: []timescale.BespokeTable{{
			Title:   "Net flow by asset (30d)",
			Columns: []string{"Asset", "Net supplied", "Net borrowed", "Events"},
			Rows:    rows,
			ColumnKinds: []timescale.BespokeColumnKind{
				timescale.BespokeCol(timescale.BespokeKindAsset), timescale.BespokeAmountOf(0),
				timescale.BespokeAmountOf(0), timescale.BespokeCol(timescale.BespokeKindCount),
			},
		}, {
			Title: "Fixed", Columns: []string{"Amount"}, Rows: [][]string{{"10"}},
			ColumnKinds: []timescale.BespokeColumnKind{{Kind: timescale.BespokeKindAmount, Decimals: &seven}},
		}, {
			Title: "Untyped", Columns: []string{"Pool"}, Rows: [][]string{{poolC}},
		}},
	}
	view := bespokeFromStore(blk)
	s.resolveBespoke(context.Background(), "comet", view)

	got := view.Tables[0]
	if !reflect.DeepEqual(got.Rows, rows) {
		t.Fatalf("rows changed: %v", got.Rows)
	}
	if len(got.ColumnKinds) != 4 || got.ColumnKinds[1].Kind != "amount" || *got.ColumnKinds[1].AssetColumn != 0 {
		t.Fatalf("column kinds not mapped: %+v", got.ColumnKinds)
	}
	wantDisplay := [][]string{
		{"USDC", "-1.2345678", "0", ""},
		{"", "1.5", "-2.5", ""},
		{"", "", "", ""}, // unknown decimals: amounts stay raw base units
		{"XLM", "922337203685477.5808", "0.0000001", ""},
	}
	if !reflect.DeepEqual(got.Display, wantDisplay) {
		t.Fatalf("display = %q, want %q", got.Display, wantDisplay)
	}
	if d := view.Tables[1].Display; !reflect.DeepEqual(d, [][]string{{"0.000001"}}) {
		t.Errorf("fixed-decimals display = %q", d)
	}
	if view.Tables[2].Display != nil || view.Tables[2].ColumnKinds != nil {
		t.Errorf("untyped table grew typed fields: %+v", view.Tables[2])
	}
}
