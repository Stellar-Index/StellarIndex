package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These tests pin the native-XLM holders fix (2026-07-31):
// /v1/assets/native/holders served {"holder_count":0,"holders":[]} instantly
// and BY CONSTRUCTION, because the holders read was trustline-shaped and
// native has no trustlines — every account holds XLM in its AccountEntry
// balance. The handler now folds every non-SAC XLM alias form (native,
// crypto:XLM, the "XLM" shorthand — the CLAUDE.md dual-form rule) down to
// the single "native" board key, so the reader's account-balance arm runs
// once and every spelling shares one SWR cache entry.

func TestIsNativeHoldersAsset(t *testing.T) {
	parse := func(s string) canonical.Asset {
		t.Helper()
		a, err := canonical.ParseAsset(s)
		if err != nil {
			t.Fatalf("ParseAsset(%q): %v", s, err)
		}
		return a
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"native", true},
		{"crypto:XLM", true},
		// ParseAsset's case-insensitive shorthand for native (F-0024).
		{"XLM", true},
		{"xlm", true},
		// The SAC wrapper is the one XLM alias-family member deliberately
		// NOT folded in — it names the wrapper's own balance domain.
		{canonical.XLMSacContractID, false},
		// Ordinary assets never fold.
		{"USDC-" + validTestAccount, false},
		{"fiat:USD", false},
		{"crypto:BTC", false},
	}
	for _, tc := range cases {
		if got := isNativeHoldersAsset(parse(tc.in)); got != tc.want {
			t.Errorf("isNativeHoldersAsset(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// holdersKeyReader records the asset key each AssetHolders read receives and
// serves a fixed account-balance board, so tests can assert both the
// normalization seam (what key reached the reader) and the served payload.
type holdersKeyReader struct {
	*capReader
	mu     sync.Mutex
	assets []string
}

func (r *holdersKeyReader) AssetHolders(_ context.Context, asset string, _ int) ([]clickhouse.AssetHolder, int64, error) {
	r.mu.Lock()
	r.assets = append(r.assets, asset)
	r.mu.Unlock()
	return []clickhouse.AssetHolder{{AccountID: validTestAccount, Balance: 554421152474348098}}, 9915982, nil
}

func (r *holdersKeyReader) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.assets...)
}

// getHolders drives the real handler for one asset_id spelling and returns
// the response view + status.
func getHolders(t *testing.T, h *Handler, captured *AssetHoldersView, assetID string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/assets/"+assetID+"/holders", nil)
	r.SetPathValue("asset_id", assetID)
	w := httptest.NewRecorder()
	h.WriteJSON = func(rw http.ResponseWriter, data any, _ bool) {
		if v, ok := data.(AssetHoldersView); ok {
			*captured = v
		}
		rw.WriteHeader(http.StatusOK)
	}
	h.AssetHolders(w, r)
	return w.Code
}

func TestAssetHolders_NativeServesAccountBalanceBoard(t *testing.T) {
	reader := &holdersKeyReader{capReader: &capReader{probe: &deadlineProbe{}}}
	h := newProbeHandler(reader, nil)

	var view AssetHoldersView
	if code := getHolders(t, h, &view, "native"); code != http.StatusOK {
		t.Fatalf("native holders status = %d, want 200", code)
	}
	if got := reader.seen(); len(got) != 1 || got[0] != "native" {
		t.Fatalf("reader keys = %v, want exactly [native]", got)
	}
	if view.Asset != "native" || view.HolderCount != 9915982 || len(view.Holders) != 1 {
		t.Fatalf("view = %+v, want the native account-balance board (count 9915982, 1 row)", view)
	}
	if view.Holders[0].Balance != "554421152474348098" {
		t.Errorf("balance = %q, want the stroop amount as a string (ADR-0003)", view.Holders[0].Balance)
	}
}

// TestAssetHolders_AliasFormsShareTheNativeBoard: crypto:XLM (and the XLM
// shorthand) must fold to the SAME cache key as native — one detached
// flight, one entry. A second spelling after the first is a warm cache hit:
// the reader runs exactly once across all three requests.
func TestAssetHolders_AliasFormsShareTheNativeBoard(t *testing.T) {
	reader := &holdersKeyReader{capReader: &capReader{probe: &deadlineProbe{}}}
	h := newProbeHandler(reader, nil)

	var view AssetHoldersView
	for _, spelling := range []string{"crypto:XLM", "native", "XLM"} {
		view = AssetHoldersView{}
		if code := getHolders(t, h, &view, spelling); code != http.StatusOK {
			t.Fatalf("%s holders status = %d, want 200", spelling, code)
		}
		if view.Asset != "native" {
			t.Errorf("%s response echoes asset %q, want the canonical board key \"native\"", spelling, view.Asset)
		}
		if view.HolderCount != 9915982 {
			t.Errorf("%s holder_count = %d, want 9915982", spelling, view.HolderCount)
		}
	}
	if got := reader.seen(); len(got) != 1 || got[0] != "native" {
		t.Fatalf("reader keys across all alias spellings = %v, want exactly [native] (shared cache entry)", got)
	}
}

// TestAssetHolders_SACFormKeepsItsOwnBoard: the XLM SAC wrapper's C-address
// is NOT folded to native — it identifies the wrapper's own balance domain
// and must reach the reader under its own key.
func TestAssetHolders_SACFormKeepsItsOwnBoard(t *testing.T) {
	reader := &holdersKeyReader{capReader: &capReader{probe: &deadlineProbe{}}}
	h := newProbeHandler(reader, nil)

	var view AssetHoldersView
	if code := getHolders(t, h, &view, canonical.XLMSacContractID); code != http.StatusOK {
		t.Fatalf("SAC holders status = %d, want 200", code)
	}
	if got := reader.seen(); len(got) != 1 || got[0] != canonical.XLMSacContractID {
		t.Fatalf("reader keys = %v, want the SAC C-address itself", got)
	}
	if view.Asset != canonical.XLMSacContractID {
		t.Errorf("SAC response echoes asset %q, want the C-address", view.Asset)
	}
}
