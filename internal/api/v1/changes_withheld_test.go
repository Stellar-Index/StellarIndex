package v1_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// getChangeSummaryStatus fetches /v1/changes and returns the status and body.
func getChangeSummaryStatus(t *testing.T, srv *v1.Server, entityType, entityID string) (int, string) {
	t.Helper()
	ts := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, ts.URL+"/v1/changes/"+entityType+"/"+url.PathEscape(entityID))
	body, _ := readAll(resp)
	return resp.StatusCode, body
}

// /v1/changes published current_value, the window values and the 30-day
// ATH/ATL for a market /v1/price withholds: no scam gate and no substance
// gate stood between change_summary_5m and the wire (GH-757). Every value
// on the row is an aggregated price claim for that market.
func TestHandleChangeSummary_WithholdsWhatPriceWithholds(t *testing.T) {
	flagged := chartFlaggedBase(t).String()
	cases := []struct {
		name, entityType, entityID string
		opts                       func() v1.Options
		wantSurface                string
		wantTitle                  string
	}{
		{
			name: "scam-flagged base, pair row", entityType: "pair", entityID: flagged + "/native",
			opts: func() v1.Options {
				return v1.Options{Scam: &chartScamGate{withheld: map[string]bool{flagged: true}}}
			},
			wantTitle: "issuer flagged",
		},
		{
			name: "scam-flagged base, coin row", entityType: "coin", entityID: flagged,
			opts: func() v1.Options {
				return v1.Options{Scam: &chartScamGate{withheld: map[string]bool{flagged: true}}}
			},
			wantTitle: "issuer flagged",
		},
		{
			name: "thin market, pair row", entityType: "pair", entityID: flagged + "/native",
			opts:        func() v1.Options { return v1.Options{Substance: &stubSubstanceGate{allow: false}} },
			wantSurface: "change_summary",
			wantTitle:   "market too thin",
		},
		{
			name: "thin market on every backing pair, coin row", entityType: "coin", entityID: flagged,
			opts:      func() v1.Options { return v1.Options{Substance: &stubSubstanceGate{allow: false}} },
			wantTitle: "market too thin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts()
			opts.ChangeSummary = &stubChangeSummaryReader{row: rawChangeSummaryRow(tc.entityType, tc.entityID)}
			status, body := getChangeSummaryStatus(t, v1.New(opts), tc.entityType, tc.entityID)
			if status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 withheld (body=%s)", status, body)
			}
			if !strings.Contains(body, "errors/price-withheld") || !strings.Contains(body, tc.wantTitle) {
				t.Errorf("body = %s, want the price-withheld problem naming %q", body, tc.wantTitle)
			}
			if strings.Contains(body, "current_value") {
				t.Errorf("withheld body still carries the row: %s", body)
			}
			if tc.wantSurface != "" {
				gate := opts.Substance.(*stubSubstanceGate)
				if len(gate.surfaces) == 0 || gate.surfaces[0] != tc.wantSurface {
					t.Errorf("substance gate asked with surfaces %v, want %q", gate.surfaces, tc.wantSurface)
				}
			}
		})
	}
}

// Gates that clear the market leave the response exactly as before.
func TestHandleChangeSummary_ServesWhenGatesAllow(t *testing.T) {
	flagged := chartFlaggedBase(t).String()
	for _, entityType := range []string{"pair", "coin"} {
		id := flagged
		if entityType == "pair" {
			id += "/native"
		}
		srv := v1.New(v1.Options{
			ChangeSummary: &stubChangeSummaryReader{row: rawChangeSummaryRow(entityType, id)},
			Substance:     &stubSubstanceGate{allow: true},
			Scam:          &chartScamGate{withheld: map[string]bool{}},
		})
		got := getChangeSummary(t, srv, entityType, id)
		if got.CurrentValue != "1.15" {
			t.Errorf("%s: current_value = %q, want \"1.15\"", entityType, got.CurrentValue)
		}
	}
}
