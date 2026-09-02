package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These pin the #332 F1 serving contract for GET /v1/accounts/{g}/positions.
// Measured cause: the six protocol folds ran inline on the request context
// with no memoisation and no flight — live on r1 the endpoint answered in
// 1.253 s then 1.148 s back-to-back, i.e. every visitor paid the whole
// fan-out, while the sibling /v1/accounts/{g} had been SWR-cached since
// 2026-07-30.

// countingPositionsReader answers every fold immediately, counting how many
// times the BLEND fold (the first) was entered — a proxy for "how many full
// six-fold fan-outs ran".
type countingPositionsReader struct {
	*slowPositionsReader
	blendCalls atomic.Int32
}

func (r *countingPositionsReader) BlendPositionsByUser(ctx context.Context, g string) ([]timescale.BlendPositionFold, error) {
	r.blendCalls.Add(1)
	return r.slowPositionsReader.BlendPositionsByUser(ctx, g)
}

// newPositionsCacheHandler wires a Handler with the response seams
// AccountPositions needs, capturing the envelope's stale flag + as_of.
func newPositionsCacheHandler(reader PositionsReader) (*Handler, *writeCapture) {
	captured := &writeCapture{}
	h := newProbeHandler(&capReader{probe: &deadlineProbe{}}, reader)
	h.WriteJSON = func(w http.ResponseWriter, _ any, stale bool) {
		captured.calls++
		captured.stale = stale
		captured.asOf = time.Time{}
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSONAt = func(w http.ResponseWriter, _ any, stale bool, asOf time.Time) {
		captured.calls++
		captured.stale = stale
		captured.asOf = asOf
		w.WriteHeader(http.StatusOK)
	}
	return h, captured
}

func getPositions(t *testing.T, h *Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/positions"+query, nil)
	r.SetPathValue("g_strkey", validTestAccount)
	h.AccountPositions(rec, r)
	return rec
}

// A second request inside the TTL must be served from the entry without
// re-reading Postgres — and ?include_closed must share it, since the cached
// set is UNFILTERED and the filter is applied at serve time.
func TestAccountPositions_MemoisedAcrossRequests(t *testing.T) {
	reader := &countingPositionsReader{slowPositionsReader: &slowPositionsReader{}}
	h, captured := newPositionsCacheHandler(reader)

	if got := getPositions(t, h, "").Code; got != http.StatusOK {
		t.Fatalf("cold status = %d, want 200", got)
	}
	if got := reader.blendCalls.Load(); got != 1 {
		t.Fatalf("cold request ran %d fan-outs, want 1", got)
	}
	if captured.stale {
		t.Error("a freshly-computed positions page was served with flags.stale")
	}
	if captured.asOf.IsZero() {
		t.Error("served as_of is zero — the entry's real computation time must reach the envelope")
	}

	if got := getPositions(t, h, "").Code; got != http.StatusOK {
		t.Fatalf("warm status = %d, want 200", got)
	}
	if got := reader.blendCalls.Load(); got != 1 {
		t.Errorf("warm request ran %d fan-outs, want 1 (served from cache) — this is the #332 F1 defect", got)
	}

	// include_closed is a SERVE-TIME filter over the same cached set.
	if got := getPositions(t, h, "?include_closed=true").Code; got != http.StatusOK {
		t.Fatalf("include_closed status = %d, want 200", got)
	}
	if got := reader.blendCalls.Load(); got != 1 {
		t.Errorf("?include_closed ran %d fan-outs, want 1 — it must share the unfiltered entry", got)
	}
}

// An entry past accountPositionsTTL must be SERVED (flags.stale + its real
// as_of) while one detached rebuild runs, not recomputed inline.
func TestAccountPositions_StaleEntryIsServedNotRecomputedInline(t *testing.T) {
	reader := &countingPositionsReader{slowPositionsReader: &slowPositionsReader{delay: 500 * time.Millisecond}}
	h, captured := newPositionsCacheHandler(reader)

	staleAt := time.Now().Add(-2 * accountPositionsTTL)
	key := positionsCacheKey + validTestAccount
	h.contractDetail.mu.Lock()
	h.contractDetail.entries = map[string]contractDetailEntry{
		key: {v: accountPositionsSnapshot{coverageNote: "seeded"}, cachedAt: staleAt},
	}
	h.contractDetail.mu.Unlock()

	start := time.Now()
	if got := getPositions(t, h, "").Code; got != http.StatusOK {
		t.Fatalf("stale-serve status = %d, want 200", got)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("request took %v against a 500ms-per-fold reader — it waited on the rebuild instead "+
			"of serving the stale entry (this is the #332 F1 defect)", elapsed)
	}
	if !captured.stale {
		t.Error("stale positions served without flags.stale — the response claims to be current")
	}
	if !captured.asOf.Equal(staleAt) {
		t.Errorf("served as_of = %v, want the entry's real computation time %v", captured.asOf, staleAt)
	}

	// Exactly one detached rebuild, and it replaced the entry.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, fresh := h.contractDetail.get(key); fresh {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := reader.blendCalls.Load(); got != 1 {
		t.Errorf("stale hit kicked %d rebuilds, want 1 (single-flight)", got)
	}
	if _, _, fresh := h.contractDetail.get(key); !fresh {
		t.Error("the detached rebuild never replaced the stale entry")
	}
}

// The "pos:" key class must carry its OWN, shorter freshness window: a DeFi
// positions list is a statement about right now, and inheriting the 5-minute
// contract-detail TTL would tell a user who just deposited that they had not,
// for five minutes.
func TestDetailTTLForKey_PositionsClassIsShorterThanContractDetail(t *testing.T) {
	if got := detailTTLForKey(positionsCacheKey + validTestAccount); got != accountPositionsTTL {
		t.Errorf("detailTTLForKey(%q…) = %v, want %v", positionsCacheKey, got, accountPositionsTTL)
	}
	if accountPositionsTTL >= contractDetailTTL {
		t.Errorf("accountPositionsTTL (%v) must be shorter than contractDetailTTL (%v)",
			accountPositionsTTL, contractDetailTTL)
	}
	// …and it must not have disturbed the sibling classes.
	if got := detailTTLForKey("ch:" + validTestContract); got != contractCodeHistoryTTL {
		t.Errorf("code-history TTL regressed to %v", got)
	}
	if got := detailTTLForKey("ev:" + validTestContract); got != contractDetailTTL {
		t.Errorf("contract-detail TTL regressed to %v", got)
	}
}

// Positions must get their OWN detached-refresh class, so a burst of cold
// accounts cannot starve the contract-detail panels (and vice versa) — the
// per-class cap's whole purpose (2026-08-13).
func TestAccountPositions_HasItsOwnRefreshClass(t *testing.T) {
	got := detachedClassForKey(positionsCacheKey + validTestAccount)
	if got == detachedClassForKey("ev:"+validTestContract) {
		t.Fatalf("positions share the contract-events refresh class (%q) — one can starve the other", got)
	}
	if got != "contract_detail_pos" {
		t.Errorf("positions refresh class = %q, want contract_detail_pos", got)
	}
}
