package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// sacRowOf returns a priced contract listing row for the SAC of code:issuer, and a server whose lake names that asset.
func sacRowOf(t *testing.T, code, issuer string, dir fixedDirectory) (*Server, AssetDetail) {
	t.Helper()
	asset, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		t.Fatal(err)
	}
	sac, err := asset.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Directory: dir, Explorer: &sacStubReader{name: code + ":" + issuer, found: true}})
	return srv, pricedContractRow(sac)
}

// A SAC is the same token under another address: a flag on the wrapped asset's issuer withholds the SAC row's price too.
func TestFillIssuerDirectoryTags_SACRowIsJudgedOnItsWrappedIssuer(t *testing.T) {
	dir := fixedDirectory{tduUSDT0Issuer: {Address: tduUSDT0Issuer, Tags: []string{"malicious"}, Source: "stellar-expert"}}
	srv, row := sacRowOf(t, "AUD", tduUSDT0Issuer, dir)
	rows := []AssetDetail{row}

	srv.fillIssuerDirectoryTags(t.Context(), rows)

	if rows[0].PriceUSD != nil || rows[0].MarketCapUSD != nil {
		t.Errorf("SAC of a flagged issuer: price=%v mcap=%v, want both withheld", rows[0].PriceUSD, rows[0].MarketCapUSD)
	}
	if len(rows[0].IssuerDirectoryTags) == 0 || rows[0].PriceWithheldReason != PriceWithheldScamIssuer {
		t.Errorf("SAC row tags=%v reason=%q, want the issuer's tags and the scam-issuer reason",
			rows[0].IssuerDirectoryTags, rows[0].PriceWithheldReason)
	}
}

// The curated list flags an issuer whether or not the directory holds a row for it, on the classic row and on its SAC.
func TestFillIssuerDirectoryTags_CuratedIssuerWithholdsWithoutADirectoryRow(t *testing.T) {
	const curated = "GBNLJIYH34UWO5YZFA3A3HD3N76R6DOI33N4JONUOHEEYZYCAYTEJ5AK"
	if scamReason(curated) == "" {
		t.Fatal("fixture issuer is no longer on the curated list")
	}
	srv, sacRow := sacRowOf(t, "RIO", curated, fixedDirectory{})
	issuer, price, mcap := curated, "0.54", "540000"
	classic := AssetDetail{
		AssetID: "RIO-" + curated, Type: "classic", Issuer: &issuer, PriceUSD: &price, MarketCapUSD: &mcap,
		IssuerScamReason: scamReason(curated),
	}
	rows := []AssetDetail{classic, sacRow}

	srv.fillIssuerDirectoryTags(t.Context(), rows)

	for _, r := range rows {
		if r.PriceUSD != nil || r.MarketCapUSD != nil || r.PriceWithheldReason != PriceWithheldScamIssuer {
			t.Errorf("%s: price=%v mcap=%v reason=%q, want withheld as scam_issuer", r.AssetID, r.PriceUSD, r.MarketCapUSD, r.PriceWithheldReason)
		}
		if r.IssuerScamReason == "" {
			t.Errorf("%s: no issuer_scam_reason", r.AssetID)
		}
	}
}

// An unread wrapper is not "not a SAC": the row's figures wait for the lake.
func TestFillIssuerDirectoryTags_UnreadSACLookupWithholds(t *testing.T) {
	srv := New(Options{Directory: fixedDirectory{}, Explorer: errSACReader{}})
	rows := []AssetDetail{pricedContractRow(flaggedContractToken)}

	srv.fillIssuerDirectoryTags(t.Context(), rows)

	if rows[0].PriceUSD != nil || !rows[0].issuerDirectoryUnchecked {
		t.Errorf("price=%v unchecked=%v, want the price withheld and the row marked unchecked", rows[0].PriceUSD, rows[0].issuerDirectoryUnchecked)
	}
}

// A contract that is no SAC, with a clean directory, keeps its figures.
func TestFillIssuerDirectoryTags_PlainContractKeepsItsPrice(t *testing.T) {
	srv := New(Options{Directory: fixedDirectory{}, Explorer: &sacStubReader{}})
	rows := []AssetDetail{pricedContractRow(flaggedContractToken)}

	srv.fillIssuerDirectoryTags(t.Context(), rows)

	if rows[0].PriceUSD == nil || rows[0].issuerDirectoryUnchecked {
		t.Errorf("price=%v unchecked=%v, want the price kept", rows[0].PriceUSD, rows[0].issuerDirectoryUnchecked)
	}
}

func TestSuppressFlaggedIssuerIdentity_ClearsBackingClaimsAndFailsClosed(t *testing.T) {
	s := func(v string) *string { return &v }
	yes, dd := true, 2
	claims := func() AssetDetail {
		d := identityFixture()
		d.Sep1Status = "verified"
		d.Sep1CurrencyAuthority = Sep1CurrencyAuthority{
			IsAssetAnchored: &yes, Regulated: &yes, AttestationOfReserve: s("https://brand.example/proof"),
			RedemptionInstructions: s("redeem 1:1"), ApprovalServer: s("https://evil.example"),
		}
		d.FixedNumber, d.MaxNumber, d.IsUnlimited, d.DisplayDecimals = s("1"), s("2"), &yes, &dd
		return d
	}
	for name, flag := range map[string]func(*AssetDetail){
		"curated":   func(d *AssetDetail) { d.IssuerScamReason = "Scam Assets" },
		"directory": func(d *AssetDetail) { d.IssuerDirectoryTags = []string{"malicious"} },
		"unchecked": func(d *AssetDetail) { d.issuerDirectoryUnchecked = true },
	} {
		d := claims()
		flag(&d)
		suppressFlaggedIssuerIdentity(&d)
		if d.Sep1CurrencyAuthority != (Sep1CurrencyAuthority{}) || d.FixedNumber != nil || d.MaxNumber != nil ||
			d.IsUnlimited != nil || d.DisplayDecimals != nil || d.OrgName != nil || d.Image != nil {
			t.Errorf("%s: issuer-declared claims still served: %+v", name, d)
		}
	}
	clean := claims()
	suppressFlaggedIssuerIdentity(&clean)
	if clean.AttestationOfReserve == nil || clean.FixedNumber == nil || clean.OrgName == nil {
		t.Errorf("unflagged row lost its declarations: %+v", clean)
	}
}

// A lake error on the wrapper lookup must not hide the flag the directory gave the contract's own address.
func TestFillIssuerDirectoryTags_UnreadSACLookupKeepsTheContractsOwnFlag(t *testing.T) {
	dir := fixedDirectory{flaggedContractToken: {Address: flaggedContractToken, Tags: []string{"malicious"}, Source: "stellar-expert"}}
	srv := New(Options{Directory: dir, Explorer: errSACReader{}})
	rows := []AssetDetail{pricedContractRow(flaggedContractToken)}

	srv.fillIssuerDirectoryTags(t.Context(), rows)

	if len(rows[0].IssuerDirectoryTags) != 1 || rows[0].PriceWithheldReason != PriceWithheldScamIssuer || rows[0].PriceUSD != nil {
		t.Errorf("tags=%v reason=%q price=%v, want the malicious tag, scam_issuer and no price",
			rows[0].IssuerDirectoryTags, rows[0].PriceWithheldReason, rows[0].PriceUSD)
	}
}
