package v1_test

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubNonstandardDecimalsReader implements v1.NonstandardDecimalsReader.
type stubNonstandardDecimalsReader struct {
	rows []timescale.NonstandardDecimalsAsset
	err  error
}

func (r *stubNonstandardDecimalsReader) LoadNonstandardDecimalsAssets(_ context.Context) ([]timescale.NonstandardDecimalsAsset, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.rows, nil
}

// TestNonstandardDecimalsCache_NilCache_LookupFailsOpen proves a nil
// *NonstandardDecimalsCache (the "guard not wired" deployment shape) never
// panics and always reports "not flagged" — the guard is opt-in.
func TestNonstandardDecimalsCache_NilCache_LookupFailsOpen(t *testing.T) {
	var c *v1.NonstandardDecimalsCache
	if _, found := c.Lookup("C-anything"); found {
		t.Fatal("nil cache reported found=true, want false (fail open)")
	}
}

// TestNonstandardDecimalsCache_ColdCache_LookupFailsOpen proves a
// constructed-but-never-refreshed cache also reports "not flagged" rather
// than treating the empty snapshot as "everything flagged".
func TestNonstandardDecimalsCache_ColdCache_LookupFailsOpen(t *testing.T) {
	c := v1.NewNonstandardDecimalsCache(&stubNonstandardDecimalsReader{}, nil)
	if _, found := c.Lookup("CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"); found {
		t.Fatal("cold cache reported found=true, want false")
	}
	if count, fetchedAt := c.Snapshot(); count != 0 || !fetchedAt.IsZero() {
		t.Fatalf("cold cache Snapshot() = (%d, %v), want (0, zero time)", count, fetchedAt)
	}
}

// TestNonstandardDecimalsCache_RefreshPopulatesLookup proves a successful
// Refresh makes the confirmed rows visible via Lookup.
func TestNonstandardDecimalsCache_RefreshPopulatesLookup(t *testing.T) {
	const asset = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	reader := &stubNonstandardDecimalsReader{
		rows: []timescale.NonstandardDecimalsAsset{{Asset: asset, Decimals: 9, Source: "aquarius"}},
	}
	c := v1.NewNonstandardDecimalsCache(reader, nil)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	dec, found := c.Lookup(asset)
	if !found {
		t.Fatal("Lookup after Refresh = found=false, want true")
	}
	if dec != 9 {
		t.Fatalf("Lookup decimals = %d, want 9", dec)
	}
	if _, found := c.Lookup("some-other-asset"); found {
		t.Fatal("Lookup on an unflagged asset = found=true, want false")
	}
}

// TestNonstandardDecimalsCache_RefreshError_FailsOpenKeepsPreviousSnapshot
// is the guard-cache fail-open test: a refresh that errors must (a) leave
// the PREVIOUS good snapshot in place rather than clearing it, and (b)
// increment obs.NonstandardDecimalsCacheRefreshFailuresTotal so the
// failure is observable. Availability wins over the guard for infra
// errors — the guard itself stays effective off the last-good snapshot.
func TestNonstandardDecimalsCache_RefreshError_FailsOpenKeepsPreviousSnapshot(t *testing.T) {
	const asset = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	reader := &stubNonstandardDecimalsReader{
		rows: []timescale.NonstandardDecimalsAsset{{Asset: asset, Decimals: 9, Source: "aquarius"}},
	}
	c := v1.NewNonstandardDecimalsCache(reader, nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}

	before := testutil.ToFloat64(obs.NonstandardDecimalsCacheRefreshFailuresTotal)

	reader.err = errors.New("connection refused")
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh with a failing reader returned nil error, want the reader's error")
	}

	// Fail-open: the asset flagged by the last GOOD refresh is still found.
	if _, found := c.Lookup(asset); !found {
		t.Fatal("Lookup after a failed refresh = found=false, want true (previous snapshot must be retained)")
	}

	if got := testutil.ToFloat64(obs.NonstandardDecimalsCacheRefreshFailuresTotal) - before; got != 1 {
		t.Fatalf("NonstandardDecimalsCacheRefreshFailuresTotal delta = %v, want 1", got)
	}
}
