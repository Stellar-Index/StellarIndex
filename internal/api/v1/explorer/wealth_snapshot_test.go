package explorer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These tests pin the /v1/accounts degraded contract (route-sweep
// 2026-07-29): a wealth snapshot past its refresh TTL is SERVED — 200 +
// flags.stale + the snapshot's real as_of — never 503'd; only a process
// that has never computed a ranking at all returns the 503 warming state.

// wealthSnapshotReader serves a configurable cached-ranking state; every
// other read falls through to capReader's harmless zero values.
type wealthSnapshotReader struct {
	*capReader
	rows []clickhouse.AccountWealth
	asOf time.Time
	ok   bool
}

func (r *wealthSnapshotReader) AccountsByWealthCached(context.Context, []string, []float64, int) ([]clickhouse.AccountWealth, time.Time, bool) {
	return r.rows, r.asOf, r.ok
}

// wealthTestHandler wires the minimal seams AccountsList needs, recording
// what was written.
func wealthTestHandler(reader ExplorerReader) (*Handler, *struct {
	status int
	stale  bool
	asOf   time.Time
	viaAt  bool
},
) {
	rec := &struct {
		status int
		stale  bool
		asOf   time.Time
		viaAt  bool
	}{}
	h := &Handler{
		Reader:         reader,
		PricingEnabled: true,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ParseLimit: func(_ http.ResponseWriter, _ *http.Request, def, _ int) (int, bool) {
			return def, true
		},
		LakeWatermark:  func(context.Context) (uint32, bool, bool) { return 42, false, true },
		LookupUSDPrice: func(context.Context, canonical.Asset) (string, bool) { return "", false },
		ClientAborted:  func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			rec.status = status
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, _ any, stale bool) {
			rec.status, rec.stale = http.StatusOK, stale
			w.WriteHeader(http.StatusOK)
		},
		WriteJSONAt: func(w http.ResponseWriter, _ any, stale bool, asOf time.Time) {
			rec.status, rec.stale, rec.asOf, rec.viaAt = http.StatusOK, stale, asOf, true
			w.WriteHeader(http.StatusOK)
		},
	}
	return h, rec
}

func serveAccountsList(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	w := httptest.NewRecorder()
	h.AccountsList(w, r)
	return w
}

func TestAccountsList_FreshSnapshot_NotDegraded(t *testing.T) {
	asOf := time.Now().Add(-time.Minute)
	h, rec := wealthTestHandler(&wealthSnapshotReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		rows:      []clickhouse.AccountWealth{{AccountID: validTestAccount, USD: 12.5}},
		asOf:      asOf, ok: true,
	})
	serveAccountsList(t, h)
	if rec.status != http.StatusOK {
		t.Fatalf("fresh snapshot served %d, want 200", rec.status)
	}
	if rec.stale {
		t.Error("fresh snapshot flagged stale")
	}
	if !rec.viaAt || !rec.asOf.Equal(asOf) {
		t.Errorf("as_of not stamped from the snapshot: viaAt=%v asOf=%v want %v", rec.viaAt, rec.asOf, asOf)
	}
}

func TestAccountsList_StaleSnapshot_Serves200Degraded(t *testing.T) {
	asOf := time.Now().Add(-3 * clickhouse.AccountsWealthCacheTTL)
	h, rec := wealthTestHandler(&wealthSnapshotReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		rows:      []clickhouse.AccountWealth{{AccountID: validTestAccount, USD: 12.5}},
		asOf:      asOf, ok: true,
	})
	serveAccountsList(t, h)
	if rec.status != http.StatusOK {
		t.Fatalf("stale snapshot served %d, want 200 (degraded, not 503)", rec.status)
	}
	if !rec.stale {
		t.Error("stale snapshot not flagged degraded (flags.stale)")
	}
	if !rec.asOf.Equal(asOf) {
		t.Errorf("degraded response as_of = %v, want the snapshot's real %v", rec.asOf, asOf)
	}
}

func TestAccountsList_ColdSnapshot_503Warming(t *testing.T) {
	h, rec := wealthTestHandler(&wealthSnapshotReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		ok:        false,
	})
	serveAccountsList(t, h)
	if rec.status != http.StatusServiceUnavailable {
		t.Fatalf("cold (never computed) snapshot served %d, want 503 warming", rec.status)
	}
}

// TestAccountsList_NoWriteJSONAtSeam_FallsBack — handlers constructed
// without the optional WriteJSONAt seam (older tests, partial wirings)
// still serve via WriteJSON.
func TestAccountsList_NoWriteJSONAtSeam_FallsBack(t *testing.T) {
	h, rec := wealthTestHandler(&wealthSnapshotReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		rows:      []clickhouse.AccountWealth{{AccountID: validTestAccount, USD: 1}},
		asOf:      time.Now(), ok: true,
	})
	h.WriteJSONAt = nil
	serveAccountsList(t, h)
	if rec.status != http.StatusOK || rec.viaAt {
		t.Fatalf("fallback path: status=%d viaAt=%v, want 200 via WriteJSON", rec.status, rec.viaAt)
	}
}
