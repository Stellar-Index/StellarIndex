package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

type stubPulse struct {
	full, present bool
	err           error
}

func (p stubPulse) Pulse(context.Context) (bool, bool, error) { return p.full, p.present, p.err }

func mevFlags(t *testing.T, opts v1.Options) v1.Flags {
	t.Helper()
	ts := httpTestServer(t, v1.New(opts))
	resp := mustGet(t, ts.URL+"/v1/mev")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var page struct {
		Flags v1.Flags `json:"flags"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return page.Flags
}

// No detector writes oracle_deviation, so its empty list is flagged even on a fresh full beat.
func TestMEVEvents_ReservedKindIsAlwaysPartial(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{MEV: &stubMEVReader{}, MEVPulse: stubPulse{full: true, present: true}}))
	for kind, want := range map[string]bool{"oracle_deviation": true, "arbitrage": false} {
		resp := mustGet(t, ts.URL+"/v1/mev?kind="+kind)
		body, _ := readAll(resp)
		var page struct {
			Flags v1.Flags `json:"flags"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("decode: %v: %s", err, body)
		}
		if page.Flags.DetectorsPartial != want {
			t.Errorf("kind=%s: detectors_partial = %v, want %v", kind, page.Flags.DetectorsPartial, want)
		}
	}
}
