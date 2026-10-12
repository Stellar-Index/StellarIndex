package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A contract token has no issuer account; the directory labels its own
// C-address, so the scam suppression must key on that.
const flaggedContractToken = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

func flaggedContractDirectory() fixedDirectory {
	return fixedDirectory{flaggedContractToken: {
		Address: flaggedContractToken, Name: "Example Token",
		Tags: []string{"malicious"}, Source: "stellar-expert",
	}}
}

func pricedContractRow(contract string) AssetDetail {
	price, mcap := "2.50", "250000"
	return AssetDetail{AssetID: contract, Type: "soroban", PriceUSD: &price, MarketCapUSD: &mcap}
}

func TestFillIssuerDirectoryTags_FlaggedContractTokenWithholdsPricing(t *testing.T) {
	srv := New(Options{Directory: flaggedContractDirectory()})
	const unlisted = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	rows := []AssetDetail{pricedContractRow(flaggedContractToken), pricedContractRow(unlisted)}

	srv.fillIssuerDirectoryTags(t.Context(), rows)

	flagged := rows[0]
	if len(flagged.IssuerDirectoryTags) == 0 {
		t.Fatalf("issuer_directory_tags empty, want the contract's own directory tags")
	}
	if flagged.PriceUSD != nil || flagged.MarketCapUSD != nil {
		t.Errorf("flagged contract price=%v mcap=%v, want both withheld", flagged.PriceUSD, flagged.MarketCapUSD)
	}
	if rows[1].PriceUSD == nil || rows[1].MarketCapUSD == nil {
		t.Errorf("unlisted contract lost its figures (price=%v mcap=%v)", rows[1].PriceUSD, rows[1].MarketCapUSD)
	}
}

func TestApplyIssuerDirectoryTags_FlaggedContractTokenWithholdsPricing(t *testing.T) {
	srv := New(Options{Directory: flaggedContractDirectory()})
	detail := pricedContractRow(flaggedContractToken)

	srv.applyIssuerDirectoryTags(t.Context(), &detail)
	suppressScamIssuerPricing(&detail) // the detail handler's next step

	if len(detail.IssuerDirectoryTags) == 0 || detail.IssuerDirectoryName != "Example Token" {
		t.Fatalf("directory label not stamped: tags=%v name=%q", detail.IssuerDirectoryTags, detail.IssuerDirectoryName)
	}
	if detail.PriceUSD != nil || detail.MarketCapUSD != nil {
		t.Errorf("flagged contract detail price=%v mcap=%v, want both withheld", detail.PriceUSD, detail.MarketCapUSD)
	}
}

// A classic row is still keyed on its issuer, never on its SAC address.
func TestDirectoryAddress_ClassicKeysOnIssuer(t *testing.T) {
	issuer, sac := tduUSDT0Issuer, flaggedContractToken
	d := AssetDetail{AssetID: "USDT0-" + issuer, Type: "classic", Issuer: &issuer, ContractID: &sac}
	if got := directoryAddress(&d); got != issuer {
		t.Errorf("directoryAddress(classic) = %q, want issuer %q", got, issuer)
	}
	if got := directoryAddress(&AssetDetail{AssetID: "native", Type: "native"}); got != "" {
		t.Errorf("directoryAddress(native) = %q, want empty", got)
	}
}

// The listing spine's real order: directory fill, then the listing
// valuation arm. With the directory down the arm must not publish a
// listing-sourced price or market cap for a row nobody checked.
func TestFillIssuerDirectoryTags_ReadFailureWithholdsAndBlocksListingValuation(t *testing.T) {
	srv := tlvServer(t, &stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
		tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1.0002", time.Hour),
	}}, map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply})

	// Instrument check: with a directory that answers "not listed", the
	// arm does publish for this row, so a nil below means the refusal.
	srv.Directory = fixedDirectory{}
	control := []AssetDetail{tlvDustSuppressedRow(tlvUSDT0Asset, "1.0001", tlvUSDT0TrustlineSupply)}
	issuer := tduUSDT0Issuer
	control[0].Issuer = &issuer
	srv.fillIssuerDirectoryTags(t.Context(), control)
	srv.applyListingValuations(t.Context(), control)
	if control[0].PriceUSD == nil || control[0].ListingValuation == nil {
		t.Fatalf("precondition: an answered, untagged read must leave the row priced "+
			"(price=%v valuation=%+v)", control[0].PriceUSD, control[0].ListingValuation)
	}

	srv.Directory = failingDirectory{}
	rows := []AssetDetail{tlvDustSuppressedRow(tlvUSDT0Asset, "1.0001", tlvUSDT0TrustlineSupply)}
	rows[0].Issuer = &issuer
	srv.fillIssuerDirectoryTags(t.Context(), rows)
	srv.applyListingValuations(t.Context(), rows)

	row := rows[0]
	if row.PriceUSD != nil {
		t.Errorf("price_usd = %q, want nil — the scam check did not run", *row.PriceUSD)
	}
	if row.ListingReference != nil || row.ListingValuation != nil {
		t.Errorf("listing pair = (%+v, %+v), want both nil — the arm ran after the fill "+
			"and read the unanswered lookup as 'untagged'", row.ListingReference, row.ListingValuation)
	}
	if row.CirculatingSupply == nil {
		t.Error("circulating_supply = nil, want the raw chain reading kept")
	}
	if len(row.IssuerDirectoryTags) != 0 {
		t.Errorf("issuer_directory_tags = %v, want none — a failed read must not accuse", row.IssuerDirectoryTags)
	}
}

// A failed directory read is not "no tags". Every fill that
// consults the directory to decide whether a row may publish a dollar
// figure must withhold that figure when the read did not answer, and the
// answer must survive to the arms that run after the fill.

var errDirectoryDown = errors.New("account_directory: connection refused")

type failingDirectory struct{}

func (failingDirectory) DirectoryEntryByAddress(context.Context, string) (timescale.DirectoryEntry, bool, error) {
	return timescale.DirectoryEntry{}, false, errDirectoryDown
}

func (failingDirectory) DirectoryEntriesByAddresses(context.Context, []string) (map[string]timescale.DirectoryEntry, error) {
	return nil, errDirectoryDown
}

const tduUSDT0Issuer = "GATISXX6BZ6NC7IKQBY37CJD4SOZL3CYZJWXEDG6JVIY4WBS6KXJHN6Q"

// The catalogue row carries its twin's answer; an unchecked twin must
// withhold the independently-priced catalogue row too.
func TestCarryTwinIssuerVerdict_CarriesUncheckedDirectory(t *testing.T) {
	price, mcap := "1.00", "1000"
	dst := AssetDetail{AssetID: "USD", PriceUSD: &price, MarketCapUSD: &mcap}
	twin := AssetDetail{issuerDirectoryUnchecked: true}

	carryTwinIssuerVerdict(&dst, twin)

	if dst.PriceUSD != nil || dst.MarketCapUSD != nil {
		t.Errorf("catalogue row price=%v mcap=%v, want both withheld — its twin's issuer was never checked",
			dst.PriceUSD, dst.MarketCapUSD)
	}
}

// The contract-keyed sibling fill on the RWA contracts surface.
func TestFillContractDirectoryTags_ReadFailureWithholds(t *testing.T) {
	srv := New(Options{Directory: failingDirectory{}})
	price, mcap := "2.50", "250000"
	rows := []AssetDetail{{AssetID: tlvUSDT0SAC, PriceUSD: &price, MarketCapUSD: &mcap}}

	srv.fillContractDirectoryTags(t.Context(), rows)

	if rows[0].PriceUSD != nil || rows[0].MarketCapUSD != nil {
		t.Errorf("contract row price=%v mcap=%v, want both withheld on a failed directory read",
			rows[0].PriceUSD, rows[0].MarketCapUSD)
	}
}

type fixedTokenDecimals uint32

func (d fixedTokenDecimals) TokenDecimals(context.Context, string) (uint32, bool, error) {
	return uint32(d), true, nil
}

// Wallets key holdings by contract address, so a listing row must carry the same C-address the
// asset's detail page does: the derived SAC for classic and native, the token itself for Soroban.
func TestAssetDetailFromAssetRow_CarriesContractID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"native",
		"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J",
	} {
		a, err := canonical.ParseAsset(id)
		if err != nil {
			t.Fatalf("ParseAsset(%q): %v", id, err)
		}
		want := detailFromAsset(a).ContractID
		if want == nil {
			t.Fatalf("%s: detail has no contract_id", id)
		}
		got := assetDetailFromAssetRow(timescale.AssetRow{AssetID: id}).ContractID
		if got == nil || *got != *want {
			t.Errorf("%s: listing contract_id = %v, want %s", id, got, *want)
		}
	}
	if got := assetDetailFromAssetRow(timescale.AssetRow{AssetID: "not-an-asset"}).ContractID; got != nil {
		t.Errorf("unparseable id: contract_id = %s, want none", *got)
	}
}
