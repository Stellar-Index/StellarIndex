package wiring

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type countingOracleReader struct{ calls atomic.Int64 }

func (f *countingOracleReader) LatestOracleUpdatesForAsset(ctx context.Context, a canonical.Asset, src string) ([]canonical.OracleUpdate, error) {
	return f.LatestOracleUpdatesForAssets(ctx, []canonical.Asset{a}, src)
}

func (f *countingOracleReader) LatestOracleUpdatesForAssets(context.Context, []canonical.Asset, string) ([]canonical.OracleUpdate, error) {
	f.calls.Add(1)
	return []canonical.OracleUpdate{}, nil
}

func (f *countingOracleReader) LatestOracleStreams(context.Context) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

// productionOracleStack mirrors main.go: the in-process TTL cache over the
// Redis read-through over the store.
func productionOracleStack(t *testing.T, inProcessTTL time.Duration) (http.Handler, *countingOracleReader, redis.UniversalClient) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	inner := &countingOracleReader{}
	redisLayer := CachedOracleReader{Inner: inner, RDB: rdb, Log: slog.New(slog.DiscardHandler)}
	h := v1.New(v1.Options{Oracle: v1.NewCachedOracleReader(redisLayer, inProcessTTL)}).Handler()
	return h, inner, rdb
}

func getOracleLatest(t *testing.T, h http.Handler, ifNoneMatch string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/oracle/latest?asset=native", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotModified {
		return rec, ""
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		AsOf string `json:"as_of"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	return rec, env.AsOf
}

// An in-process refill that re-reads an unchanged Redis entry must replay
// the DB read's time, not stamp the refill time.
func TestOracleLatest_InProcessRefillOverUnchangedRedisKeepsAsOfAndETag(t *testing.T) {
	const ttl = 20 * time.Millisecond
	h, inner, _ := productionOracleStack(t, ttl)

	first, asOf := getOracleLatest(t, h, "")
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag")
	}
	time.Sleep(3 * ttl) // in-process entry expires; the 30 s Redis entry does not

	second, asOf2 := getOracleLatest(t, h, "")
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("DB reads = %d, want 1 (the refill must come from Redis)", n)
	}
	if asOf2 != asOf {
		t.Errorf("as_of moved on a Redis replay: %s then %s", asOf, asOf2)
	}
	if got := second.Header().Get("ETag"); got != tag {
		t.Errorf("ETag moved on a Redis replay: %s then %s", tag, got)
	}
	if rec, _ := getOracleLatest(t, h, tag); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match replay: status %d, want 304", rec.Code)
	}
}

// A pre-upgrade entry has no compute time, so it is re-read rather than
// stamped with a time later than its data.
func TestCachedOracleReader_LegacyEntryIsAMiss(t *testing.T) {
	_, inner, rdb := productionOracleStack(t, 0)
	ctx := context.Background()
	key := cachekeys.OracleLatest([]string{canonical.NativeAsset().String()}, "").String()
	if err := rdb.Set(ctx, key, "[]", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	r := CachedOracleReader{Inner: inner, RDB: rdb, Log: slog.New(slog.DiscardHandler)}
	before := time.Now()
	_, at, err := r.LatestOracleUpdatesForAssetsAt(ctx, []canonical.Asset{canonical.NativeAsset()}, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("DB reads = %d, want 1", n)
	}
	if at.Before(before) || at.After(time.Now()) {
		t.Errorf("computed_at = %s, want the DB read time", at)
	}
	_, again, err := r.LatestOracleUpdatesForAssetsAt(ctx, []canonical.Asset{canonical.NativeAsset()}, "")
	if err != nil || !again.Equal(at) || inner.calls.Load() != 1 {
		t.Errorf("rewritten entry: at=%s (want %s), err=%v, DB reads=%d", again, at, err, inner.calls.Load())
	}
}
