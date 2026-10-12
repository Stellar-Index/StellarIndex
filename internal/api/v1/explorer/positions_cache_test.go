package explorer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These pin the serving contract for GET /v1/accounts/{g}/positions: the six protocol folds are
// memoised and single-flighted like the SWR-cached sibling /v1/accounts/{g}, so a visitor does not
// pay the whole fan-out inline on the request context.

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
	h.WriteJSONAt = func(w http.ResponseWriter, _ any, stale, _ bool, asOf time.Time) {
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

// The "pos:" key class needs its own shorter freshness window: a DeFi positions list describes right now.
// Inheriting the 5-minute contract-detail TTL would tell a user who just deposited that they had not.
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
// per-class cap's whole purpose.
func TestAccountPositions_HasItsOwnRefreshClass(t *testing.T) {
	got := detachedClassForKey(positionsCacheKey + validTestAccount)
	if got == detachedClassForKey("ev:"+validTestContract) {
		t.Fatalf("positions share the contract-events refresh class (%q) — one can starve the other", got)
	}
	if got != "contract_detail_pos" {
		t.Errorf("positions refresh class = %q, want contract_detail_pos", got)
	}
}

// The six protocol folds are independent reads and must run concurrently (latency = max, not sum); serial execution
// would show max in-flight of 1 and take 6x the delay.
func TestAccountPositions_FoldsRunConcurrently(t *testing.T) {
	const delay = 60 * time.Millisecond
	reader := &slowPositionsReader{delay: delay}
	h := &Handler{Positions: reader, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	start := time.Now()
	var cov positionsCoverage
	_ = h.collectPositions(context.Background(), "GTEST", func(string) string { return "" }, &cov)
	elapsed := time.Since(start)

	if got := reader.maxFlight.Load(); got < 2 {
		t.Fatalf("max concurrent folds = %d, want >= 2 — the folds ran serially, which is the 1.99s breach", got)
	}
	// Serial would be >= 6*delay; concurrent should land near one delay.
	if elapsed > 4*delay {
		t.Errorf("elapsed %v with %v per fold — looks serial, want ~1 fold's latency", elapsed, delay)
	}
}

// Parallelising must not make the degraded-protocol list order (rendered into the wire coverage_note) depend on
// goroutine scheduling: it stays in the fixed fold order, run after run.
func TestAccountPositions_CoverageOrderIsDeterministic(t *testing.T) {
	var first string
	for i := 0; i < 20; i++ {
		reader := &slowPositionsReader{blendFails: true}
		h := &Handler{Positions: reader, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		var cov positionsCoverage
		_ = h.collectPositions(context.Background(), "GTEST", func(string) string { return "" }, &cov)
		note := cov.note()
		if note == "" {
			t.Fatal("expected a degraded coverage note (three folds fail in this fixture)")
		}
		if i == 0 {
			first = note
			continue
		}
		if note != first {
			t.Fatalf("coverage note is scheduling-dependent: run %d = %q, run 0 = %q", i, note, first)
		}
	}
}

type blendRowsReader struct {
	*slowPositionsReader
	rows []timescale.BlendPositionFold
}

func (r *blendRowsReader) BlendPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	return r.rows, nil
}

// Only a net-underlying leg names a token, so only it carries a scale; a
// superseded leg's amount is not in token units and an unreadable token
// omits the field rather than guessing 7.
func TestBuildBlendPositions_DecimalsOnlyOnNetUnderlying(t *testing.T) {
	const unreadable = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	reader := &blendRowsReader{slowPositionsReader: &slowPositionsReader{}, rows: []timescale.BlendPositionFold{
		{Pool: "P1", Asset: validTestContract, HasSupplyLeg: true, SupplyNet: "5", HasBorrowLeg: true, BorrowSuperseded: true, BorrowTokens: "3"},
		{Pool: "P2", Asset: unreadable, HasSupplyLeg: true, SupplyNet: "5"},
	}}
	h := newProbeHandler(&capReader{probe: &deadlineProbe{}}, reader)
	h.TokenDecimals = func(_ context.Context, contractID string) (int, bool) {
		return 18, contractID == validTestContract
	}
	got := h.buildBlendPositions(context.Background(), validTestAccount, func(s string) string { return s }, &positionsCoverage{})
	var dec []any
	for _, e := range got {
		if e.Decimals == nil {
			dec = append(dec, nil)
		} else {
			dec = append(dec, *e.Decimals)
		}
	}
	if want := []any{18, nil, nil}; fmt.Sprint(dec) != fmt.Sprint(want) {
		t.Errorf("decimals = %v, want %v (supply, superseded borrow, unreadable)", dec, want)
	}
}

// TestAmountSemanticsMatchOpenAPIEnum keeps the AmountSemantics*
// constants and the spec's amount_semantics enum in lockstep: a value
// the handler emits but the spec omits breaks every generated client.
func TestAmountSemanticsMatchOpenAPIEnum(t *testing.T) {
	src, err := os.ReadFile("positions.go")
	if err != nil {
		t.Fatal(err)
	}
	var consts []string
	for _, m := range regexp.MustCompile(`(?m)^\s*AmountSemantics\w+\s*=\s*"([^"]+)"`).FindAllSubmatch(src, -1) {
		consts = append(consts, string(m[1]))
	}

	spec, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "openapi", "stellar-index.v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*enum: \[(net_underlying_at_event_time[^\]]*)\]`).FindSubmatch(spec)
	if m == nil {
		t.Fatal("amount_semantics enum not found in openapi/stellar-index.v1.yaml")
	}
	var enum []string
	for _, v := range strings.Split(string(m[1]), ",") {
		enum = append(enum, strings.TrimSpace(v))
	}

	slices.Sort(consts)
	slices.Sort(enum)
	if len(consts) == 0 || !slices.Equal(consts, enum) {
		t.Errorf("AmountSemantics constants %v, spec enum %v", consts, enum)
	}
}

// slowPositionsReader answers every fold after `delay`, counting how
// many folds are in flight at once. Two folds fail so the coverage
// merge is exercised too.
type slowPositionsReader struct {
	delay      time.Duration
	inFlight   atomic.Int32
	maxFlight  atomic.Int32
	blendFails bool
}

func (r *slowPositionsReader) enter() {
	n := r.inFlight.Add(1)
	for {
		m := r.maxFlight.Load()
		if n <= m || r.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(r.delay)
	r.inFlight.Add(-1)
}

func (r *slowPositionsReader) BlendPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	r.enter()
	if r.blendFails {
		return nil, errors.New("blend down")
	}
	return nil, nil
}

func (r *slowPositionsReader) BlendBackstopSharesByUser(context.Context, string) ([]timescale.BlendBackstopFold, error) {
	r.enter()
	return nil, errors.New("backstop down")
}

func (r *slowPositionsReader) PhoenixStakeByUser(context.Context, string) ([]timescale.PhoenixStakeFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) DefindexVaultSharesByUser(context.Context, string) ([]timescale.DefindexVaultFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) CreditPositionsByOwner(context.Context, string) ([]timescale.CreditPositionFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) AquariusGaugeByUser(context.Context, string) ([]timescale.AquariusGaugeFold, error) {
	r.enter()
	return nil, errors.New("aquarius down")
}
