package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type mapDirectory map[string]timescale.DirectoryEntry

func (d mapDirectory) DirectoryEntryByAddress(_ context.Context, a string) (timescale.DirectoryEntry, bool, error) {
	e, ok := d[a]
	return e, ok, nil
}

func (d mapDirectory) DirectoryEntriesByAddresses(context.Context, []string) (map[string]timescale.DirectoryEntry, error) {
	return d, nil
}

// A SAC row already carrying its wrapped issuer's scam flag keeps it when the SAC's own clean directory entry is stamped.
func TestFillContractDirectoryTags_KeepsTheWrappedIssuersScamFlag(t *testing.T) {
	srv := New(Options{Directory: mapDirectory{
		tlvUSDT0SAC: {Address: tlvUSDT0SAC, Name: "Wrapped token", Tags: []string{"issuer"}},
	}})
	price := "2.50"
	rows := []AssetDetail{{AssetID: tlvUSDT0SAC, PriceUSD: &price, IssuerDirectoryTags: []string{"malicious"}}}

	srv.fillContractDirectoryTags(t.Context(), rows)

	if !pricingguard.IsDirectoryScamFlagged(rows[0].IssuerDirectoryTags) || !issuerScamFlagged(&rows[0]) {
		t.Errorf("tags = %v, want the wrapped issuer's scam flag kept", rows[0].IssuerDirectoryTags)
	}
	if rows[0].PriceUSD != nil {
		t.Errorf("price = %s, want withheld for a scam-flagged issuer", *rows[0].PriceUSD)
	}
}
