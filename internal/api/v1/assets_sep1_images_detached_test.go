package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// blockingSep1Reader is a SEP-1 image reader that stands in for the r1
// scan: it announces that it started, then blocks until the test releases
// it, and records whether the context it was handed was cancelled while it
// worked.
//
// That last field is the whole point. On r1 the refresh ran on the calling
// request's context, so when curl gave up at 10 s the scan died with it —
// `WARN "sep1 image refresh failed" err="timescale: AllSep1Images rows:
// context canceled"`, logged at the exact second of each smoke timeout.
// A detached refresh must never see its caller's cancellation.
type blockingSep1Reader struct {
	entered   chan struct{} // one send per call
	release   chan struct{} // test closes this to let the scan finish
	finished  chan struct{} // closed when the first scan returns
	once      sync.Once     // so a second (buggy) call cannot panic over the real failure
	imgs      []timescale.Sep1Image
	sawCancel atomic.Bool
}

func newBlockingSep1Reader(imgs []timescale.Sep1Image) *blockingSep1Reader {
	return &blockingSep1Reader{
		entered:  make(chan struct{}, 8),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
		imgs:     imgs,
	}
}

func (b *blockingSep1Reader) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, sql.ErrNoRows
}

func (b *blockingSep1Reader) AllSep1Images(ctx context.Context) ([]timescale.Sep1Image, error) {
	b.entered <- struct{}{}
	defer b.once.Do(func() { close(b.finished) })
	select {
	case <-b.release:
	case <-ctx.Done():
		// Only reachable if the refresh inherited a caller's cancellation.
		b.sawCancel.Store(true)
		return nil, ctx.Err()
	}
	return b.imgs, nil
}

// TestCachedSep1Images_RequestNeverBlocksOnRefresh is the regression for
// the /v1/assets stall: 202 failing smoke samples over 4 days, 10-13 s
// against a normal 10 ms, because whichever request found the logo map
// expired rebuilt it INLINE on its own context.
//
// The assertion needs no timing heuristic. The reader is held open for the
// whole test, so if cachedSep1Images returns at all, it returned without
// waiting for the scan. Against the pre-fix code this test does not fail
// on a threshold — it deadlocks until the outer timeout.
func TestCachedSep1Images_RequestNeverBlocksOnRefresh(t *testing.T) {
	t.Parallel()

	reader := newBlockingSep1Reader([]timescale.Sep1Image{
		{Code: "USDC", Issuer: imgIssuerUSDC, Image: "https://circle.com/usdc.svg"},
	})
	s := discardServer(reader)

	returned := make(chan map[string]string, 1)
	go func() { returned <- s.cachedSep1Images(context.Background()) }()

	var got map[string]string
	select {
	case got = <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("cachedSep1Images blocked on the refresh — a listing request is waiting for the SEP-1 scan")
	}

	// The scan must still be running: that is what proves the return above
	// was a stale-while-revalidate read and not a completed refresh.
	select {
	case <-reader.finished:
		t.Fatal("the scan completed before cachedSep1Images returned — it was not detached")
	default:
	}
	if got != nil {
		t.Errorf("cold cache should serve no logos, got %v", got)
	}

	// And the detached refresh must still land, filling the cache for the
	// NEXT request rather than being abandoned with its caller.
	close(reader.release)
	<-reader.finished
	waitForSep1Flight(t, s)

	warm := s.cachedSep1Images(context.Background())
	if warm[sep1ImageKey("USDC", imgIssuerUSDC)] != "https://circle.com/usdc.svg" {
		t.Errorf("cache not filled by the detached refresh: %v", warm)
	}
}

// TestCachedSep1Images_CancelledRequestDoesNotDiscardRefresh pins the
// second half of the r1 failure: the abandoned request took the scan down
// with it, nothing was cached, and the next request started the whole
// thing again — a loop that ran for two days.
func TestCachedSep1Images_CancelledRequestDoesNotDiscardRefresh(t *testing.T) {
	t.Parallel()

	reader := newBlockingSep1Reader([]timescale.Sep1Image{
		{Code: "AQUA", Issuer: imgIssuerAQUA, Image: "https://aqua.network/aqua.png"},
	})
	s := discardServer(reader)

	// A client that has already hung up, exactly like curl at 10 s.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := s.cachedSep1Images(ctx); got != nil {
		t.Errorf("cold cache should serve no logos, got %v", got)
	}

	// The scan is running on a context of its own. Give the cancellation
	// every chance to propagate before releasing it: if the refresh had
	// inherited ctx, AllSep1Images would already have taken the
	// ctx.Done() branch and set sawCancel.
	<-reader.entered
	time.Sleep(50 * time.Millisecond)
	close(reader.release)
	<-reader.finished
	waitForSep1Flight(t, s)

	if reader.sawCancel.Load() {
		t.Error("the refresh was cancelled by its caller's context — this is the r1 " +
			`"AllSep1Images rows: context canceled" defect`)
	}
	warm := s.cachedSep1Images(context.Background())
	if warm[sep1ImageKey("AQUA", imgIssuerAQUA)] != "https://aqua.network/aqua.png" {
		t.Errorf("a cancelled request discarded the refresh; cache = %v", warm)
	}
}

// TestCachedSep1Images_FailedRefreshServesLastGoodAndDoesNotStorm pins the
// retry gap. Before it, a failing scan left sep1ImagesAt un-advanced, so
// every subsequent request kicked the whole 448 MB scan again for as long
// as the failure lasted.
func TestCachedSep1Images_FailedRefreshServesLastGoodAndDoesNotStorm(t *testing.T) {
	t.Parallel()

	stub := &stubSep1ImagesReader{imgs: []timescale.Sep1Image{
		{Code: "USDC", Issuer: imgIssuerUSDC, Image: "https://circle.com/usdc.svg"},
	}}
	s := discardServer(stub)
	s.PrewarmSep1Images(context.Background())
	if stub.calls != 1 {
		t.Fatalf("prewarm made %d scans, want 1", stub.calls)
	}

	// Expire the map and make the next scan fail.
	s.sep1ImagesMu.Lock()
	s.sep1ImagesAt = time.Now().Add(-2 * sep1ImagesTTL)
	s.sep1ImagesAttemptAt = time.Time{}
	s.sep1ImagesMu.Unlock()
	stub.err = errStubSep1Scan

	s.PrewarmSep1Images(context.Background()) // kicks + waits for the failing scan
	if stub.calls != 2 {
		t.Fatalf("scans = %d, want 2 (the failing refresh)", stub.calls)
	}

	// The last good map must still be served — a failed refresh must never
	// blank logos that were rendering a moment ago.
	for i := range 5 {
		m := s.cachedSep1Images(context.Background())
		if m[sep1ImageKey("USDC", imgIssuerUSDC)] != "https://circle.com/usdc.svg" {
			t.Fatalf("call %d served no logo after a failed refresh: %v", i, m)
		}
	}
	// ...and none of those five requests may have kicked a scan of its own.
	if stub.calls != 2 {
		t.Errorf("scans = %d after 5 reads inside the retry gap, want 2 — the retry gap is not holding", stub.calls)
	}
}

// TestNewServer_PrewarmSep1ImagesThroughProductionConstructor exercises the
// constructor production actually uses — v1.New(v1.Options{Sep1Cache:
// store}) at cmd/stellarindex-api/main.go:1334 — rather than a &Server{}
// literal, so a regression that drops Sep1Cache from the Options mapping
// is caught here and not on r1. It then serves a real /v1/assets response
// through the real handler and checks the logo actually lands on a row.
func TestNewServer_PrewarmSep1ImagesThroughProductionConstructor(t *testing.T) {
	t.Parallel()

	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubSep1ImagesReader{imgs: []timescale.Sep1Image{
		{Code: "USDC", Issuer: imgIssuerUSDC, Image: "https://circle.com/usdc.svg"},
	}}

	s := New(Options{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sep1Cache:          stub,
		VerifiedCurrencies: cat,
	})

	// Cold: the listing must still answer, just without logos. This is the
	// trade the fix makes — decoration degrades, the product does not.
	if got := s.cachedSep1Images(context.Background()); got != nil {
		t.Errorf("cold map should be nil, got %v", got)
	}

	// The prewarm main.go runs on boot and every 5 minutes. Same function
	// the handler calls, so there is no second cache slot to warm.
	s.PrewarmSep1Images(context.Background())
	if stub.calls != 1 {
		t.Fatalf("prewarm scans = %d, want 1", stub.calls)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=stablecoin", nil)
	w := httptest.NewRecorder()
	s.handleAssetListFromCatalogue(w, req, "stablecoin", assetListFilters{}, 100, "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var env struct {
		Data []AssetDetail `json:"data"`
	}
	if uerr := json.Unmarshal(w.Body.Bytes(), &env); uerr != nil {
		t.Fatalf("decode response: %v", uerr)
	}
	var found bool
	for _, row := range env.Data {
		if row.Slug != "usdc" {
			continue
		}
		found = true
		if row.Image == nil || *row.Image != "https://circle.com/usdc.svg" {
			t.Errorf("usdc row image = %v, want the prewarmed logo", row.Image)
		}
	}
	if !found {
		t.Fatal("usdc row not found in asset_class=stablecoin listing")
	}
	// The handler must not have kicked a scan of its own: one warm slot,
	// one scan, whatever the request shape.
	if stub.calls != 1 {
		t.Errorf("scans = %d after a warm request, want 1 — the handler warmed a different slot", stub.calls)
	}
}

// waitForSep1Flight blocks until no detached SEP-1 refresh is in flight,
// so a test can assert on the cache the refresh was supposed to fill
// without racing it.
func waitForSep1Flight(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.sep1ImagesMu.Lock()
		inFlight := s.sep1ImagesFlight
		s.sep1ImagesMu.Unlock()
		if inFlight == nil {
			return
		}
		select {
		case <-inFlight:
		case <-time.After(30 * time.Second):
			t.Fatal("detached SEP-1 refresh never finished")
		}
	}
	t.Fatal("detached SEP-1 refresh never finished")
}
