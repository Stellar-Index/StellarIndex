package v1_test

// §3 scam-label surfacing — the issuer_directory_{tags,domain,name}
// overlay on /v1/assets + /v1/assets/{id}, joined from account_directory
// (migration 0136). Oracle: the operator-reported scam AUD
// (AUD-GAIF52QZ…GAUD, issuer audrev-stellar.com) whose issuer the
// directory tags {malicious, unsafe} — but which the in-binary
// scamIssuers list misses. These tests pin (a) that the tags surface and
// (b) the DISPLAY-ONLY invariant: the malicious tag does NOT suppress the
// asset's price or any gate.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The exact asset from the 2026-08-24 operator report.
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

// TestAssetGet_IssuerDirectoryTags_Surfaced — the scam AUD's issuer
// directory label lands on the detail payload with the exact tags,
// domain, and name from account_directory.
func TestAssetGet_IssuerDirectoryTags_Surfaced(t *testing.T) {
	srv, aud := audDetailServer(t, v1.Options{Directory: scamAUDDirectoryStub()})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if got := d.IssuerDirectoryTags; len(got) != 2 || got[0] != "malicious" || got[1] != "unsafe" {
		t.Errorf("issuer_directory_tags = %v, want [malicious unsafe]", got)
	}
	if d.IssuerDirectoryDomain != scamAUDDomain {
		t.Errorf("issuer_directory_domain = %q, want %q", d.IssuerDirectoryDomain, scamAUDDomain)
	}
	if d.IssuerDirectoryName != "Fake AUD" {
		t.Errorf("issuer_directory_name = %q, want %q", d.IssuerDirectoryName, "Fake AUD")
	}
}

// TestAssetGet_MaliciousDirectoryTag_DoesNotSuppressPrice — the
// DISPLAY-ONLY invariant (§3.3): a malicious-tagged asset still prices
// normally. The label is orthogonal — price_usd is byte-identical with
// and without the directory reader wired, and carries the real market
// price either way.
func TestAssetGet_MaliciousDirectoryTag_DoesNotSuppressPrice(t *testing.T) {
	get := func(t *testing.T, directory v1.Options) v1.AssetDetail {
		t.Helper()
		srv, aud := audDetailServer(t, directory)
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var env struct {
			Data v1.AssetDetail `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env.Data
	}

	withDir := get(t, v1.Options{Directory: scamAUDDirectoryStub()})
	withoutDir := get(t, v1.Options{})

	// The tag surfaced only on the directory-wired server.
	if len(withDir.IssuerDirectoryTags) == 0 {
		t.Fatalf("precondition: expected the malicious tag to surface with a directory wired")
	}
	if len(withoutDir.IssuerDirectoryTags) != 0 {
		t.Fatalf("precondition: no directory wired must omit the tags, got %v", withoutDir.IssuerDirectoryTags)
	}
	// Price is the market price, UNCHANGED by the malicious label.
	if withDir.PriceUSD == nil || *withDir.PriceUSD != "0.65" {
		t.Errorf("price_usd (malicious-tagged) = %v, want 0.65 — the label must not suppress pricing", withDir.PriceUSD)
	}
	if withoutDir.PriceUSD == nil || *withoutDir.PriceUSD != "0.65" {
		t.Errorf("price_usd (no directory) = %v, want 0.65", withoutDir.PriceUSD)
	}
	if (withDir.PriceUSD == nil) != (withoutDir.PriceUSD == nil) ||
		(withDir.PriceUSD != nil && *withDir.PriceUSD != *withoutDir.PriceUSD) {
		t.Errorf("price_usd diverged: with-directory=%v without=%v — tag must be display-only",
			withDir.PriceUSD, withoutDir.PriceUSD)
	}
}

// TestAssetList_IssuerDirectoryTags_BatchedNoN1 — the listing joins the
// page's issuer set in ONE batch query (not N+1) and stamps the tags onto
// the row.
func TestAssetList_IssuerDirectoryTags_BatchedNoN1(t *testing.T) {
	aud, err := canonical.NewClassicAsset("AUD", scamAUDIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	other, err := canonical.NewClassicAsset("USDC", testUSDCIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	assetsReader := &listStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		rows: []timescale.AssetRow{
			{AssetID: aud.String(), Code: "AUD", IssuerGStrkey: scamAUDIssuer},
			{AssetID: other.String(), Code: "USDC", IssuerGStrkey: testUSDCIssuer},
		},
	}
	dir := &countingDirectoryReader{entries: scamAUDDirectoryStub().entries}
	srv := v1.New(v1.Options{Assets: &stubAssetReader{}, AssetsReader: assetsReader, Directory: dir})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var audRow *v1.AssetDetail
	for i := range env.Data {
		if env.Data[i].Code == "AUD" {
			audRow = &env.Data[i]
		}
	}
	if audRow == nil {
		t.Fatalf("AUD row missing from listing: %+v", env.Data)
	}
	if got := audRow.IssuerDirectoryTags; len(got) != 2 || got[0] != "malicious" {
		t.Errorf("listing AUD issuer_directory_tags = %v, want [malicious unsafe]", got)
	}
	if audRow.IssuerDirectoryDomain != scamAUDDomain {
		t.Errorf("listing AUD issuer_directory_domain = %q, want %q", audRow.IssuerDirectoryDomain, scamAUDDomain)
	}
	if n := atomic.LoadInt64(&dir.batchCalls); n != 1 {
		t.Errorf("directory batch calls = %d, want exactly 1 (page issuer set resolved in one query)", n)
	}
	if n := atomic.LoadInt64(&dir.oneCalls); n != 0 {
		t.Errorf("directory single-lookup calls = %d, want 0 (must not N+1 per row)", n)
	}
}
