package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssetClassRefusesWhatItCannotFilter is the regression. An unrecognised
// asset_class fell through to the default listing and returned 200, so the
// filter silently did not apply.
//
// Measured on the live surface: `asset_class=rwa` returned USDC, yXLM, AQUA,
// SHX and VELO — byte-identical to `asset_class=bogus` and to no asset_class
// at all. A consumer asking for real-world assets got a governance token and
// a wrapped lumen, with nothing in the response to reveal it.
//
// `rwa` is in the table deliberately: it is the value someone reaches for
// first, there is no such class by design, and the refusal has to name where
// that set actually lives. `%20rwa%20` is there because the normaliser trims
// before the guard sees the value, so a padded spelling must refuse too
// rather than slip through as the empty default.
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

// TestAssetClassAcceptsEveryValueItDispatchesOn is the control. The refusal
// must not narrow what already worked — every accepted spelling, including
// the three aliases the normaliser folds and the empty default, has to get
// past the guard.
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
