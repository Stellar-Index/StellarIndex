package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type countingLendingReader struct {
	stubLendingReader
	calls atomic.Int64
}

func (r *countingLendingReader) ListBlendPools(ctx context.Context) ([]timescale.BlendPoolSummary, error) {
	r.calls.Add(1)
	return r.stubLendingReader.ListBlendPools(ctx)
}

// The pool list is read once per TTL, and the envelope dates it by its fill time, not the serve time.
func TestCachedLendingReader_ListServedFromCacheWithFillTime(t *testing.T) {
	up := &countingLendingReader{stubLendingReader: stubLendingReader{
		pools: []timescale.BlendPoolSummary{{Pool: "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"}},
	}}
	srv := v1.New(v1.Options{Lending: v1.NewCachedLendingReader(up, time.Minute)})
	ts := startHTTPTest(t, srv.Handler())

	asOf := func() string {
		t.Helper()
		resp := mustGet(t, ts.URL+"/v1/lending/pools")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		body, _ := readAll(resp)
		var got struct {
			AsOf string `json:"as_of"`
			Data []struct {
				Pool string `json:"pool"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Data) != 1 {
			t.Fatalf("data = %d rows, want 1: %s", len(got.Data), body)
		}
		return got.AsOf
	}
	first := asOf()
	time.Sleep(5 * time.Millisecond)
	if second := asOf(); second != first {
		t.Errorf("as_of moved on a cached read: %s then %s", first, second)
	}
	if n := up.calls.Load(); n != 1 {
		t.Errorf("upstream ListBlendPools calls = %d, want 1", n)
	}
}

// A failed fill is not cached: the next request reads upstream again.
func TestCachedLendingReader_ErrorNotCached(t *testing.T) {
	up := &countingLendingReader{stubLendingReader: stubLendingReader{err: errors.New("boom")}}
	c := v1.NewCachedLendingReader(up, time.Minute)
	for range 2 {
		if _, err := c.ListBlendPools(context.Background()); err == nil {
			t.Fatal("want the upstream error")
		}
	}
	if n := up.calls.Load(); n != 2 {
		t.Errorf("upstream calls = %d, want 2", n)
	}
}

// A pool's reserves view is built once per TTL: the page fans out one request per pool, and a repeat must not re-read the lake.
func TestLendingPoolReserves_BuiltOncePerTTL(t *testing.T) {
	pool := mkCStrkey(t, 7)
	explorer := &stubExplorerReader{}
	srv := v1.New(v1.Options{
		Explorer: explorer,
		Lending:  &stubLendingReader{assets: []string{mkCStrkey(t, 20)}},
	})
	base := httpTestServer(t, srv).URL

	for range 2 {
		resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		_, _ = readAll(resp)
	}
	if n := len(explorer.reserveCalls); n != 1 {
		t.Errorf("BlendPoolReserves calls = %d, want 1 (second request served from cache)", n)
	}
}
