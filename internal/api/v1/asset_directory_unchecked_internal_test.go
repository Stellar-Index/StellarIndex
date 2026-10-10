package v1

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
