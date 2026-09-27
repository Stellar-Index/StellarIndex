package main

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeObservationReader struct {
	rows map[string]timescale.AccountObservation
	err  error
}

func (f fakeObservationReader) LatestAccountObservationAtOrBefore(_ context.Context, accountID string, _ uint32) (timescale.AccountObservation, error) {
	if f.err != nil {
		return timescale.AccountObservation{}, f.err
	}
	row, ok := f.rows[accountID]
	if !ok {
		return timescale.AccountObservation{}, timescale.ErrNotFound
	}
	return row, nil
}

// TestHomeDomainLookup_ObservedAbsenceBeatsStatic drives the production
// composition (store adapter → ChainedHomeDomainLookup → assetToDetail):
// an issuer whose latest account_observations row has no home_domain —
// cleared by SetOptions (stored NULL) or merged (is_removal) — must serve
// no home_domain, never the operator's static entry.
func TestHomeDomainLookup_ObservedAbsenceBeatsStatic(t *testing.T) {
	const (
		cleared   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		merged    = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
		live      = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		unwatched = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	)
	onChain := "aqua.network"
	stale := "stale.example.com"
	reader := fakeObservationReader{rows: map[string]timescale.AccountObservation{
		cleared: {AccountID: cleared, Ledger: 10, Balance: big.NewInt(1)},
		merged:  {AccountID: merged, Ledger: 11, Balance: big.NewInt(0), IsRemoval: true, HomeDomain: &stale},
		live:    {AccountID: live, Ledger: 12, Balance: big.NewInt(1), HomeDomain: &onChain},
	}}
	static := func(string) (string, bool) { return "operator.example.com", true }
	lookup := metadata.ChainedHomeDomainLookup(
		metadata.NewLCMHomeDomainResolver(metadataStoreLookup{s: reader}), static, nil)

	cases := []struct {
		issuer string
		want   string // "" = HomeDomain must be nil
	}{
		{cleared, ""},
		{merged, ""},
		{live, onChain},
		{unwatched, "operator.example.com"},
	}
	for _, tc := range cases {
		d := assetToDetail(context.Background(), canonical.Asset{Type: canonical.AssetClassic, Code: "TST", Issuer: tc.issuer}, lookup)
		got := ""
		if d.HomeDomain != nil {
			got = *d.HomeDomain
		}
		if got != tc.want {
			t.Errorf("issuer %s: home_domain=%q, want %q", tc.issuer, got, tc.want)
		}
	}
}

func TestMetadataStoreLookup_StorageErrorPropagates(t *testing.T) {
	boom := errors.New("conn reset")
	_, err := metadataStoreLookup{s: fakeObservationReader{err: boom}}.HomeDomainAtOrBefore(context.Background(), "GA", 1)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}
