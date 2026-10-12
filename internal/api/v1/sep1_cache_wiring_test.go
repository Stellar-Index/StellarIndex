package v1_test

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// wiringPayloadOnlySep1Cache answers GetIssuerSep1Cached but lacks IssuerSep1Unreachable, so
// it does not satisfy the optional Sep1FetchStateReader seam that sep1StatusForNoPayload
// consults via type assertion.
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

func (wiringFetchStateSep1Cache) IssuerSep1Unreachable(context.Context, string) (bool, error) {
	return false, nil
}

// TestNewWarnsWhenSep1CacheLacksFetchState: the compile-time guard only proves the bare *timescale.Store satisfies
// Sep1FetchStateReader, not the value wired as Options.Sep1Cache. Without a boot-time check, a cache lacking it
// silently reverts every sep1_status to "not_fetched".
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
