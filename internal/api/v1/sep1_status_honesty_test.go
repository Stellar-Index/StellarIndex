package v1

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sep1StateStub implements both the payload seam and the optional fetch-state
// seam, so a test can hold "no payload" fixed and vary only whether a fetch
// was attempted — which is the whole distinction under test.
type sep1StateStub struct {
	attempted bool
	stateErr  error
	asked     []string
}

func (s *sep1StateStub) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, nil // the state both statuses share: no payload
}

func (s *sep1StateStub) IssuerSep1Attempted(_ context.Context, g string) (bool, error) {
	s.asked = append(s.asked, g)
	return s.attempted, s.stateErr
}

// payloadOnlyStub is the pre-2026-09-16 shape: it answers about payloads and
// knows nothing about attempts.
type payloadOnlyStub struct{}

func (payloadOnlyStub) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

const sep1TestIssuer = "GDMBNMFJ3TRFLASJ6UGETFME3PJPNKPU24C7KFDBEBPQFG2CI6UC3JG6"

// TestSep1StatusSeparatesOurBacklogFromTheirBrokenFile is the regression.
// Both states reach this code as a nil payload, and they are opposite
// findings: one is our queue, the other is the issuer's published document.
//
// Measured on 2026-09-16: an asset manager's stellar.toml carried an
// unterminated string on line 20. One missing quote made the file
// unparseable, so thirteen live RWA-class declarations were refused — and
// every asset page reported `not_fetched`, which says we never tried. The
// fetch had run five days running.
func TestSep1StatusSeparatesOurBacklogFromTheirBrokenFile(t *testing.T) {
	for _, tt := range []struct {
		name      string
		attempted bool
		want      string
	}{
		{"fetched, nothing storable — the issuer's file", true, "unreachable"},
		{"never attempted — our backlog", false, "not_fetched"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := &sep1StateStub{attempted: tt.attempted}
			s := &Server{logger: discardLogger(), sep1Cache: stub}

			got := s.sep1StatusForNoPayload(context.Background(), sep1TestIssuer)

			if got != tt.want {
				t.Errorf("sep1_status = %q, want %q — a nil payload is two different "+
					"findings and the wire has to say which", got, tt.want)
			}
			if len(stub.asked) != 1 || stub.asked[0] != sep1TestIssuer {
				t.Errorf("fetch state asked for %v, want exactly [%s]", stub.asked, sep1TestIssuer)
			}
		})
	}
}

// TestSep1StatusClaimsNothingWithoutAFetchStateReader — the seam is optional,
// consulted by type assertion, and a deployment without it must behave exactly
// as it did before. The fallback direction is load-bearing: `not_fetched`
// blames nobody, `unreachable` blames the issuer, so an unwired reader must
// never produce the second.
func TestSep1StatusClaimsNothingWithoutAFetchStateReader(t *testing.T) {
	s := &Server{logger: discardLogger(), sep1Cache: payloadOnlyStub{}}

	if got := s.sep1StatusForNoPayload(context.Background(), sep1TestIssuer); got != "not_fetched" {
		t.Errorf("sep1_status = %q with no fetch-state reader wired, want \"not_fetched\" — "+
			"an unwired seam must not start attributing failures to issuers", got)
	}
}

// TestSep1StatusDoesNotBlameTheIssuerForOurReadFailure — when the fetch-state
// read itself errors we know nothing, and the honest answer is the one that
// makes no claim about them.
func TestSep1StatusDoesNotBlameTheIssuerForOurReadFailure(t *testing.T) {
	s := &Server{
		logger:    discardLogger(),
		sep1Cache: &sep1StateStub{attempted: true, stateErr: errors.New("db down")},
	}

	if got := s.sep1StatusForNoPayload(context.Background(), sep1TestIssuer); got != "not_fetched" {
		t.Errorf("sep1_status = %q when our own state read failed, want \"not_fetched\"", got)
	}
}

// TestApplySep1OverlayReportsTheIssuersBrokenFile is the WIRING half. The
// three tests above call the helper directly, so they stay green against a
// build that computes the right answer and then discards it at the call site —
// which is exactly the state this test was written after finding, twice in one
// session.
//
// It drives the production overlay and asserts on the field a client reads.
func TestApplySep1OverlayReportsTheIssuersBrokenFile(t *testing.T) {
	asset, err := canonical.ParseAsset("WTGX-" + sep1TestIssuer)
	if err != nil {
		t.Fatalf("parse asset: %v", err)
	}
	s := &Server{logger: discardLogger(), sep1Cache: &sep1StateStub{attempted: true}}
	var detail AssetDetail

	s.applySep1Overlay(context.Background(), &detail, asset)

	if detail.Sep1Status != "unreachable" {
		t.Errorf("sep1_status = %q, want \"unreachable\" — the fetch ran and the issuer's "+
			"document would not parse, and `not_fetched` tells the reader we never tried",
			detail.Sep1Status)
	}
}

// TestApplySep1OverlayStillSaysNotFetchedForOurBacklog — the control on the
// same path, so the wiring cannot pass by always answering "unreachable".
func TestApplySep1OverlayStillSaysNotFetchedForOurBacklog(t *testing.T) {
	asset, err := canonical.ParseAsset("WTGX-" + sep1TestIssuer)
	if err != nil {
		t.Fatalf("parse asset: %v", err)
	}
	s := &Server{logger: discardLogger(), sep1Cache: &sep1StateStub{attempted: false}}
	var detail AssetDetail

	s.applySep1Overlay(context.Background(), &detail, asset)

	if detail.Sep1Status != "not_fetched" {
		t.Errorf("sep1_status = %q, want \"not_fetched\"", detail.Sep1Status)
	}
}
