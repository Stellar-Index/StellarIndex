//go:build integration

package wiring

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // migrate's postgres:// driver
	_ "github.com/golang-migrate/migrate/v4/source/file"       // migrate's file:// source
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/test/harness"
)

func itStore(t *testing.T, ctx context.Context) *timescale.Store {
	t.Helper()
	dsn := harness.StartTimescale(t, ctx)
	_, thisFile, _, _ := runtime.Caller(0)
	m, err := migrate.New("file://"+filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "migrations"), dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Refresh by hand only: the policy job would race the manual refresh (55P03).
	if _, err := store.DB().ExecContext(ctx, `SELECT alter_job(job_id, scheduled => false)
		FROM timescaledb_information.jobs WHERE proc_name = 'policy_refresh_continuous_aggregate'`); err != nil {
		t.Fatal(err)
	}
	return store
}

func itRedis(t *testing.T, ctx context.Context) redis.UniversalClient {
	t.Helper()
	ctr, err := testcontainers.Run(ctx, "redis:7.4-alpine", testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections").WithStartupTimeout(60*time.Second)))
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.WithoutCancel(ctx)) })
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// itSeed writes the market shapes a batch row meets against quote and returns the requested ids: both stored
// directions, the flipped direction only, rows 20 days old (failing the 14-day existence check, inside 400 days), a
// Redis-only triangulated value, and nothing.
func itSeed(t *testing.T, ctx context.Context, store *timescale.Store, rdb redis.UniversalClient, prefix string, n int, quote canonical.Asset) []string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Minute)
	ids := make([]string, n)
	for i := range n {
		a, err := canonical.NewClassicAsset(fmt.Sprintf("%s%d", prefix, i), pfIssuer)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = a.String()
		direct, flipped := canonical.Pair{Base: a, Quote: quote}, canonical.Pair{Base: quote, Quote: a}
		at := now.Add(-40 * time.Minute)
		switch i % 5 {
		case 0:
			itTrade(t, ctx, store, i*4, at, direct, 1_000_000_000, int64(12_000_000+i))
			itTrade(t, ctx, store, i*4+1, at.Add(5*time.Minute), flipped, int64(12_000_000+i), 1_000_000_000)
		case 1:
			itTrade(t, ctx, store, i*4, at, flipped, int64(9_000_000+i), 1_000_000_000)
		case 2:
			itTrade(t, ctx, store, i*4, now.Add(-20*24*time.Hour), direct, 1_000_000_000, int64(7_000_000+i))
		case 3:
			keys := vwapMGetKeys(a, quote, 5*time.Minute)
			if err := rdb.Set(ctx, keys[0], fmt.Sprintf("0.%d", 300+i), time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
			if err := rdb.Set(ctx, keys[2], cachekeys.FormatVWAPObservedAt(now.Add(-time.Minute)), time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ids
}

func itTrade(t *testing.T, ctx context.Context, store *timescale.Store, nonce int, ts time.Time, p canonical.Pair, base, quote int64) {
	t.Helper()
	err := store.InsertTrade(ctx, canonical.Trade{
		Source: "integ-prefetch", Ledger: uint32(60_000_000 + nonce), TxHash: fmt.Sprintf("%064x", nonce), Timestamp: ts, Pair: p,
		BaseAmount: canonical.NewAmount(big.NewInt(base)), QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	})
	if err != nil {
		t.Fatalf("InsertTrade %v: %v", p, err)
	}
}

var itVolatile = regexp.MustCompile(`"(as_of|request_id|instance)":"[^"]*"`)

func itGet(t *testing.T, base, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(base + path) //nolint:noctx // test server
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, itVolatile.ReplaceAll(body, []byte(`"$1":""`))
}

type itRow struct {
	AssetID string `json:"asset_id"`
	Price   string `json:"price"`
}

// The batch prefetch over the production readers (timescale.PrefetchStore under StorePriceReader, the Redis
// decorator) answers /v1/price/batch byte for byte as the same readers without it, and each row as /v1/price.
func TestPriceBatchPrefetch_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store, rdb := itStore(t, ctx), itRedis(t, ctx)
	usd, xlm := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}, canonical.NativeAsset()
	usdIDs := itSeed(t, ctx, store, rdb, "PU", 100, usd)
	xlmIDs := itSeed(t, ctx, store, rdb, "PX", 40, xlm)
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatal(err)
	}
	fl, err := freeze.NewLooker(rdb)
	if err != nil {
		t.Fatal(err)
	}
	off := httptest.NewServer(v1.New(v1.Options{
		Prices: StorePriceReader{S: store}, Triangulated: RedisTriangulatedLooker{RDB: rdb}, Freeze: fl,
	}).Handler())
	defer off.Close()
	on := httptest.NewServer(v1.New(v1.Options{
		Prices:       StorePriceReader{S: timescale.PrefetchStore{Store: store}},
		Triangulated: PrefetchTriangulatedLooker{RedisTriangulatedLooker: RedisTriangulatedLooker{RDB: rdb}},
		Freeze:       fl,
	}).Handler())
	defer on.Close()

	hits := func() float64 {
		return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("latest_vwap_1m", "hit")) +
			testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("triangulated", "hit"))
	}
	for _, tc := range []struct {
		name, quote string
		ids         []string
	}{{"fiat:USD", "fiat:USD", usdIDs}, {"XLM-quoted", "native", xlmIDs}} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/price/batch?quote=" + tc.quote + "&asset_ids=" + join(tc.ids)
			h0 := hits()
			sOff, bOff := itGet(t, off.URL, path)
			sOn, bOn := itGet(t, on.URL, path)
			if sOff != sOn || string(bOff) != string(bOn) {
				t.Fatalf("planner on differs\n off %d %s\n on  %d %s", sOff, bOff, sOn, bOn)
			}
			if hits() == h0 {
				t.Fatal("no row was answered from the prefetch")
			}
			var env struct{ Data []itRow }
			if err := json.Unmarshal(bOn, &env); err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, r := range env.Data {
				got[r.AssetID] = r.Price
			}
			if len(got) == 0 || len(got) == len(tc.ids) {
				t.Fatalf("%d of %d ids priced: the fixture lost its misses or its hits", len(got), len(tc.ids))
			}
			for _, id := range tc.ids {
				status, body := itGet(t, off.URL, "/v1/price?asset="+id+"&quote="+tc.quote)
				var single struct{ Data itRow }
				_ = json.Unmarshal(body, &single)
				price, inBatch := got[id]
				if (status == http.StatusOK) != inBatch || (inBatch && single.Data.Price != price) {
					t.Errorf("%s: /v1/price %d %q, batch row %v %q", id, status, single.Data.Price, inBatch, price)
				}
			}
		})
	}
}

// The change_24h anchor prefetch over real Postgres answers every anchor and guard-baseline read exactly as the store's
// single reads at the same instant: both stored directions, the flipped direction only, an anchor too old for a
// baseline, and none.
func TestChange24hAnchorPrefetch_AnswersAsTheStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := itStore(t, ctx)
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	anchorAt := time.Now().UTC().Truncate(time.Minute).Add(-24*time.Hour - 5*time.Minute)
	pairs := make([]canonical.Pair, 40)
	for i := range pairs {
		a, err := canonical.NewClassicAsset(fmt.Sprintf("CA%d", i), pfIssuer)
		if err != nil {
			t.Fatal(err)
		}
		pairs[i] = canonical.Pair{Base: a, Quote: usd}
		flipped := canonical.Pair{Base: usd, Quote: a}
		for j := range 13 { // the anchor and a 12-bucket baseline behind it
			ts, nonce := anchorAt.Add(-time.Duration(j)*time.Minute), 10_000+i*20+j
			switch i % 4 {
			case 0:
				itTrade(t, ctx, store, nonce, ts, pairs[i], 1_000_000_000, int64(5_000_000+i))
			case 1:
				itTrade(t, ctx, store, nonce, ts, flipped, int64(5_000_000+i), 1_000_000_000)
			case 2:
				itTrade(t, ctx, store, nonce, ts.Add(-2*timescale.Change24hAnchorTolerance), pairs[i], 1_000_000_000, int64(5_000_000+i))
			}
		}
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatal(err)
	}
	if s := time.Now().Second(); s > 55 {
		time.Sleep(time.Duration(61-s) * time.Second)
	}
	at := time.Now().Add(-24 * time.Hour)
	notBefore := at.Add(-timescale.Change24hAnchorTolerance)
	count := func(seam, result string) float64 {
		return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues(seam, result))
	}
	hits := count("anchor_vwap_1m", "hit") + count("anchor_trailing_vwap_1m", "hit")
	fell := count("anchor_vwap_1m", "fallthrough") + count("anchor_trailing_vwap_1m", "fallthrough")
	p := timescale.PrefetchAnchors{AnchorReads: store}
	mctx := readset.With(ctx, readset.New())
	p.PrefetchChange24hAnchors(mctx, pairs, at, notBefore, pricingguard.SampleFetch)
	reads, found := 0, 0
	for i, pair := range pairs {
		got, err := p.ClosedVWAP1mAtOrBefore(mctx, pair, at)
		want, wantErr := store.ClosedVWAP1mAtOrBefore(ctx, pair, at)
		reads++
		if !reflect.DeepEqual(got, want) || (err == nil) != (wantErr == nil) || (i%4 == 3) != errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%v: memo %+v %v, store %+v %v", pair, got, err, want, wantErr)
		}
		if err != nil || got.Bucket.Before(notBefore) {
			continue
		}
		found++
		gotRows, err := p.ClosedVWAP1mCombinedBefore(mctx, pair, got.Bucket, pricingguard.SampleFetch)
		wantRows, wantErr := store.ClosedVWAP1mCombinedBefore(ctx, pair, got.Bucket, pricingguard.SampleFetch)
		reads++
		if err != nil || wantErr != nil || len(wantRows) != 12 || !reflect.DeepEqual(gotRows, wantRows) {
			t.Fatalf("%v baseline: memo %d rows %v, store %d rows %v", pair, len(gotRows), err, len(wantRows), wantErr)
		}
	}
	if found != 20 {
		t.Fatalf("%d anchors inside the tolerance, want 20 (both directions and flipped only)", found)
	}
	if d := count("anchor_vwap_1m", "hit") + count("anchor_trailing_vwap_1m", "hit") - hits; int(d) != reads {
		t.Errorf("%v of %d reads answered from the memo, want all", d, reads)
	}
	if d := count("anchor_vwap_1m", "fallthrough") + count("anchor_trailing_vwap_1m", "fallthrough") - fell; d != 0 {
		t.Errorf("%v reads fell through", d)
	}
}

func join(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}
