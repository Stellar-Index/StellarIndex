package v1_test

// Scam-label surfacing: the issuer_directory_{tags,domain,name} overlay on /v1/assets and
// /v1/assets/{id}, joined from account_directory. Oracle: a scam AUD (AUD-GAIF52QZ…GAUD, issuer
// audrev-stellar.com) whose issuer the directory tags {malicious, unsafe} but the in-binary
// scamIssuers list misses. These tests pin (a) that the tags surface and (b) the DISPLAY-ONLY
// invariant: the malicious tag does NOT suppress the asset's price or any gate.

import (
	"context"
	"sync/atomic"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The exact asset from an operator report.
const (
	scamAUDIssuer = "GAIF52QZUPYCADXF7I7RNPMED7DT2B5JGPR7DEHCC5TPDPUJTMLGGAUD"
	scamAUDDomain = "audrev-stellar.com"
)

// listStub wires ListAssetsExt with canned rows while promoting the rest
// of the AssetsReader surface from the shared extension stub.
type listStub struct {
	*stubAssetsReaderExt
	rows []timescale.AssetRow
}

func (l *listStub) ListAssetsExt(_ context.Context, _ timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	return l.rows, nil
}

// countingDirectoryReader records how the join reaches the directory so a
// test can assert the listing path batches (one DirectoryEntriesByAddresses
// call) instead of N+1 single lookups.
type countingDirectoryReader struct {
	entries    map[string]timescale.DirectoryEntry
	batchCalls int64
	oneCalls   int64
}

func (c *countingDirectoryReader) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	atomic.AddInt64(&c.oneCalls, 1)
	e, ok := c.entries[address]
	return e, ok, nil
}

func (c *countingDirectoryReader) DirectoryEntriesByAddresses(_ context.Context, addresses []string) (map[string]timescale.DirectoryEntry, error) {
	atomic.AddInt64(&c.batchCalls, 1)
	out := map[string]timescale.DirectoryEntry{}
	for _, a := range addresses {
		if e, ok := c.entries[a]; ok {
			out[a] = e
		}
	}
	return out, nil
}

func scamAUDDirectoryStub() *stubDirectoryReader {
	return &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{
		scamAUDIssuer: {
			Address: scamAUDIssuer,
			Name:    "Fake AUD",
			Domain:  scamAUDDomain,
			Tags:    []string{"malicious", "unsafe"},
			Source:  "stellar-expert",
		},
	}}
}

// audDetailServer builds a Server serving the scam AUD detail with a
// market price on the AssetsReader overlay. directory may be nil.
func audDetailServer(t *testing.T, directory v1.Options) (*v1.Server, canonical.Asset) {
	t.Helper()
	aud, err := canonical.NewClassicAsset("AUD", scamAUDIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	issuer := scamAUDIssuer
	reader := &stubAssetReader{
		byID: map[string]v1.AssetDetail{
			aud.String(): {AssetID: aud.String(), Type: "classic", Code: "AUD", Issuer: &issuer},
		},
	}
	assetsReader := &stubAssetsReaderExt{
		row: timescale.AssetRow{
			Slug:          "AUD-" + scamAUDIssuer,
			AssetID:       aud.String(),
			Code:          "AUD",
			IssuerGStrkey: scamAUDIssuer,
			PriceUSD:      sptr("0.65"),
		},
	}
	opts := directory
	opts.Assets = reader
	opts.AssetsReader = assetsReader
	return v1.New(opts), aud
}
