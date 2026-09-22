package v1_test

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// wiringPayloadOnlySep1Cache is the shape a future caching wrapper could
// take: it answers GetIssuerSep1Cached but was never given
// IssuerSep1Attempted, so it does not satisfy the optional
// Sep1FetchStateReader seam consulted via type assertion in
// sep1StatusForNoPayload.
type wiringPayloadOnlySep1Cache struct{}

func (wiringPayloadOnlySep1Cache) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

// wiringFetchStateSep1Cache implements both seams, mirroring
// *timescale.Store.
type wiringFetchStateSep1Cache struct{}

func (wiringFetchStateSep1Cache) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

func (wiringFetchStateSep1Cache) IssuerSep1Attempted(context.Context, string) (bool, error) {
	return false, nil
}

// TestNewWarnsWhenSep1CacheLacksFetchState is the regression for RDTM-F4.
// The compile-time guard at assets.go:57 only proves the bare
// *timescale.Store still satisfies Sep1FetchStateReader; it says nothing
// about whatever value a future caching wrapper actually wires as
// Options.Sep1Cache. Without a boot-time check, wiring a value that lacks
// the fetch-state method silently reverts every issuer's sep1_status to
// "not_fetched" with nothing red anywhere.
func TestNewWarnsWhenSep1CacheLacksFetchState(t *testing.T) {
	logger, buf := captureLogger()

	v1.New(v1.Options{
		Logger:    logger,
		Sep1Cache: wiringPayloadOnlySep1Cache{},
	})

	if !strings.Contains(buf.String(), "Sep1FetchStateReader") {
		t.Fatalf("expected a boot-time warning naming Sep1FetchStateReader when the wired "+
			"Sep1Cache lacks the fetch-state seam, got log: %s", buf.String())
	}
}

// TestNewStaysQuietWhenSep1CacheHasFetchState is the control: a
// Sep1Cache that does implement the seam (the production shape) must not
// trip the warning.
func TestNewStaysQuietWhenSep1CacheHasFetchState(t *testing.T) {
	logger, buf := captureLogger()

	v1.New(v1.Options{
		Logger:    logger,
		Sep1Cache: wiringFetchStateSep1Cache{},
	})

	if strings.Contains(buf.String(), "Sep1FetchStateReader") {
		t.Fatalf("unexpected fetch-state warning for a Sep1Cache that implements the seam: %s", buf.String())
	}
}
