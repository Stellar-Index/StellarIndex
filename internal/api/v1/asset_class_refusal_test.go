package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssetClassRefusesWhatItCannotFilter: an unrecognised asset_class must be refused, not fall through to the default
// listing with 200, which would silently ignore the filter. `rwa` is deliberately in the table (no such class exists; the
// refusal must name where the set lives) and `%20rwa%20` checks the normaliser trims before the guard.
func TestAssetClassRefusesWhatItCannotFilter(t *testing.T) {
	for _, class := range []string{"rwa", "real_world_asset", "tokenized", "bogus", "RWA", "%20rwa%20"} {
		t.Run(class, func(t *testing.T) {
			s := &Server{logger: discardLogger()}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class="+class, nil)

			s.handleAssetList(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("asset_class=%q returned %d, want 400 — a filter that is accepted "+
					"and not applied is a wrong answer dressed as a right one", class, rec.Code)
			}
			if body := rec.Body.String(); !strings.Contains(body, "rwa/assets") {
				t.Errorf("the refusal does not say where real-world assets are served: %s", body)
			}
		})
	}
}

// TestAssetClassAcceptsEveryValueItDispatchesOn is the control: the refusal must not narrow what already worked, including the
// three folded aliases and the empty default.
func TestAssetClassAcceptsEveryValueItDispatchesOn(t *testing.T) {
	for _, class := range []string{
		"", "all", "fiat", "stablecoin", "crypto",
		"blockchain", "cryptocurrency", "cryptocurrencies",
		"CRYPTO", " fiat ",
	} {
		t.Run("accepts_"+class, func(t *testing.T) {
			if !validAssetClass(normaliseAssetClass(class)) {
				t.Errorf("asset_class=%q was refused; it is a value this handler dispatches on", class)
			}
		})
	}
}
