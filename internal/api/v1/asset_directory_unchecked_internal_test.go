package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A failed directory read is not "no tags" (RLT-089). Every fill that
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

// The listing spine's real order: directory fill, then the listing
// valuation arm. With the directory down the arm must not publish a
// listing-sourced price or market cap for a row nobody checked.
func TestFillIssuerDirectoryTags_ReadFailureWithholdsAndBlocksListingValuation(t *testing.T) {
	srv := tlvServer(t, &stubAssetListingDirectory{rows: map[string]timescale.ListingEntry{
		tlvUSDT0SAC: tlvEntry(tlvUSDT0SAC, "usdt0", "1.0002", time.Hour),
	}}, map[string]string{tlvUSDT0SAC: tlvUSDT0LakeSupply})

	// Instrument check: with a directory that answers "not listed", the
	// arm does publish for this row, so a nil below means the refusal.
	srv.directory = fixedDirectory{}
	control := []AssetDetail{tlvDustSuppressedRow(tlvUSDT0Asset, "1.0001", tlvUSDT0TrustlineSupply)}
	issuer := tduUSDT0Issuer
	control[0].Issuer = &issuer
	srv.fillIssuerDirectoryTags(t.Context(), control)
	srv.applyListingValuations(t.Context(), control)
	if control[0].PriceUSD == nil || control[0].ListingValuation == nil {
		t.Fatalf("precondition: an answered, untagged read must leave the row priced "+
			"(price=%v valuation=%+v)", control[0].PriceUSD, control[0].ListingValuation)
	}

	srv.directory = failingDirectory{}
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

// The contract fill again, through the RWA contract arm's whole pipeline:
// its own cap fill runs after the shared directory fill, so the refusal
// must hold on the figures that arm produces too.
func TestRWAContractListingRows_DirectoryReadFailureWithholds(t *testing.T) {
	const sorobanContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	price, volume := "2.5000000000", "250000.00"
	sources := 3
	row := timescale.AssetRow{AssetID: sorobanContract, PriceUSD: &price, Volume24hUSD: &volume, SourceCount: &sources}

	listed := func(dir explorerpkg.DirectoryReader) AssetDetail {
		t.Helper()
		srv := New(Options{Directory: dir})
		srv.assetsReader = &preciseSupplyStub{obs: map[string]timescale.SupplyObservation{
			sorobanContract: {CirculatingSupply: "10000000000", Basis: "sep41_lake_flows"},
		}}
		srv.contractCatalogue = capDecimalsContractCatalogue{rows: map[string]timescale.AssetRow{sorobanContract: row}}
		srv.tokenSupply = capDecimalsTokenSupply{byID: map[string]string{sorobanContract: "10000000000"}}
		srv.tokenDecimals = fixedTokenDecimals(7)
		out, _, err := srv.rwaContractListingRows(t.Context(), []rwaContractMember{{contractID: sorobanContract}})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := out[sorobanContract]
		if !ok {
			t.Fatal("contract row missing from the listing")
		}
		return got
	}

	// Instrument check: an answered "not listed" read prices the row.
	control := listed(fixedDirectory{})
	if control.PriceUSD == nil || control.MarketCapUSD == nil || *control.MarketCapUSD != "2500.00" {
		t.Fatalf("precondition: an answered, unlisted read must price the row (price=%v mcap=%v)",
			control.PriceUSD, control.MarketCapUSD)
	}

	got := listed(failingDirectory{})
	if got.PriceUSD != nil || got.MarketCapUSD != nil {
		t.Errorf("contract row price=%v mcap=%v, want both withheld — the directory never answered",
			got.PriceUSD, got.MarketCapUSD)
	}
	if got.CirculatingSupply == nil {
		t.Error("circulating_supply = nil, want the raw chain reading kept")
	}
}
