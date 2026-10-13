package v1_test

import (
	"context"
	"encoding/json"
	"errors"
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

// TestMEVEvents_DetectorsPartial pins that detectors_partial is false only on a fresh full beat; an
// unwired reader or looker, a missing or partial beat, and a read error all flag the feed.
func TestMEVEvents_DetectorsPartial(t *testing.T) {
	reader := &stubMEVReader{}
	cases := map[string]struct {
		opts v1.Options
		want bool
	}{
		"no reader":        {v1.Options{MEVPulse: stubPulse{full: true, present: true}}, true},
		"no looker":        {v1.Options{MEV: reader}, true},
		"fresh full beat":  {v1.Options{MEV: reader, MEVPulse: stubPulse{full: true, present: true}}, false},
		"partial beat":     {v1.Options{MEV: reader, MEVPulse: stubPulse{full: false, present: true}}, true},
		"no beat":          {v1.Options{MEV: reader, MEVPulse: stubPulse{}}, true},
		"heartbeat unread": {v1.Options{MEV: reader, MEVPulse: stubPulse{err: errors.New("redis down")}}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := mevFlags(t, tc.opts).DetectorsPartial; got != tc.want {
				t.Errorf("detectors_partial = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMEVEvents_DetectorsPartialOmittedWhenFalse(t *testing.T) {
	ts := httpTestServer(t, v1.New(v1.Options{MEV: &stubMEVReader{}, MEVPulse: stubPulse{full: true, present: true}}))
	resp := mustGet(t, ts.URL+"/v1/mev")
	body, _ := readAll(resp)
	var page struct {
		Flags map[string]any `json:"flags"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if _, ok := page.Flags["detectors_partial"]; ok {
		t.Errorf("detectors_partial present on a full beat: %s", body)
	}
}
