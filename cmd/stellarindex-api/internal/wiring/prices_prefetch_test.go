package wiring

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
)

var (
	_ v1.TriangulatedPriceLooker = PrefetchTriangulatedLooker{}
	_ v1.CompositeMetaLooker     = PrefetchTriangulatedLooker{}
	_ v1.FrozenLooker            = PrefetchFreezeLooker{}
)

const pfIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func pfAsset(t *testing.T, id string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func pfRedis(t *testing.T) (*miniredis.Miniredis, redis.UniversalClient) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func pfSetVWAP(t *testing.T, mr *miniredis.Miniredis, r readset.PairRead, val, prov string, at time.Time, cov *cachekeys.WindowCoverage) {
	t.Helper()
	keys := vwapMGetKeys(r.Base, r.Quote, r.Window)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(mr.Set(keys[0], val))
	if prov != "" {
		must(mr.Set(keys[1], prov))
	}
	if !at.IsZero() {
		must(mr.Set(keys[2], cachekeys.FormatVWAPObservedAt(at)))
	}
	if cov != nil {
		must(mr.Set(keys[3], cachekeys.FormatVWAPCoverage(*cov)))
	}
}

type pfAnswer struct {
	v     v1.CachedVWAP
	found bool
	err   bool
}

// pfReads is a fixture over every key shape a seam must keep apart: both XLM spellings, two quotes, two windows, a
// value with no stamp (a miss), and a pair with nothing.
func pfReads(t *testing.T, mr *miniredis.Miniredis) []readset.PairRead {
	t.Helper()
	native, cxlm, usd, eur := pfAsset(t, "native"), pfAsset(t, "crypto:XLM"), pfAsset(t, "fiat:USD"), pfAsset(t, "fiat:EUR")
	dirc, miss := pfAsset(t, "DIRC-"+pfIssuer), pfAsset(t, "MISS-"+pfIssuer)
	at := time.Now().UTC().Truncate(time.Minute)
	reads := []readset.PairRead{
		{Base: native, Quote: usd, Window: time.Minute},
		{Base: native, Quote: eur, Window: time.Minute},
		{Base: cxlm, Quote: usd, Window: time.Minute},
		{Base: native, Quote: usd, Window: time.Hour},
		{Base: dirc, Quote: usd, Window: time.Minute},
		{Base: miss, Quote: usd, Window: time.Minute},
	}
	cov := cachekeys.WindowCoverage{Truncated: true, CoveredFrom: at.Add(-30 * time.Second)}
	pfSetVWAP(t, mr, reads[0], "0.12", "", at, &cov)
	pfSetVWAP(t, mr, reads[1], "0.11", cachekeys.VWAPProvenanceTriangulated, at, nil)
	pfSetVWAP(t, mr, reads[2], "0.13", "", at.Add(-time.Minute), &cachekeys.WindowCoverage{})
	pfSetVWAP(t, mr, reads[3], "0.125", "", at, nil)
	pfSetVWAP(t, mr, reads[4], "0.42", "", time.Time{}, nil)
	return reads
}

func pfLookup(ctx context.Context, l v1.TriangulatedPriceLooker, r readset.PairRead) pfAnswer {
	v, found, err := l.LookupTriangulatedVWAP(ctx, r.Base, r.Quote, r.Window)
	return pfAnswer{v, found, err != nil}
}

func TestPrefetchTriangulated_AnswersAsTheSingleRead(t *testing.T) {
	mr, rdb := pfRedis(t)
	reads := pfReads(t, mr)
	single := RedisTriangulatedLooker{RDB: rdb}
	dec := PrefetchTriangulatedLooker{single}
	want := make([]pfAnswer, len(reads))
	for i, r := range reads {
		want[i] = pfLookup(context.Background(), single, r)
		if got := pfLookup(context.Background(), dec, r); !reflect.DeepEqual(got, want[i]) {
			t.Errorf("no memo %+v: %+v, single read %+v", r, got, want[i])
		}
	}
	if !want[0].found || want[0].v.Coverage == nil || want[4].found || want[5].found {
		t.Fatalf("fixture lost its shapes: %+v", want)
	}

	ctx := readset.With(context.Background(), readset.New())
	dec.PrefetchTriangulatedVWAP(ctx, reads)
	mr.FlushAll() // every answer below must now come from the prefetch instant
	for i, r := range reads {
		if got := pfLookup(ctx, dec, r); !reflect.DeepEqual(got, want[i]) {
			t.Errorf("prefetched %+v: %+v, single read %+v", r, got, want[i])
		}
	}
	got := pfLookup(ctx, dec, reads[0])
	got.v.Coverage.Truncated = false
	if again := pfLookup(ctx, dec, reads[0]); !again.v.Coverage.Truncated {
		t.Error("a caller's mutation of Coverage reached the memo")
	}

	unread := readset.PairRead{Base: reads[0].Base, Quote: reads[0].Quote, Window: 5 * time.Minute}
	pfSetVWAP(t, mr, unread, "0.2", "", time.Now().UTC(), nil)
	if got := pfLookup(ctx, dec, unread); !got.found || got.v.Value != "0.2" {
		t.Errorf("a key the prefetch did not read must be read as before: %+v", got)
	}
}

func TestPrefetchTriangulated_FailedPrefetchFallsThrough(t *testing.T) {
	mr, rdb := pfRedis(t)
	reads := pfReads(t, mr)
	dec := PrefetchTriangulatedLooker{RedisTriangulatedLooker{RDB: rdb}}
	ctx := readset.With(context.Background(), readset.New())
	mr.Close()
	dec.PrefetchTriangulatedVWAP(ctx, reads)
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	if got := pfLookup(ctx, dec, reads[0]); !got.found || got.v.Value != "0.12" {
		t.Errorf("a failed prefetch was memoised (%+v); the key must be read as before", got)
	}
}

func TestPrefetchFreeze_AnswersAsTheSingleRead(t *testing.T) {
	mr, rdb := pfRedis(t)
	native, cxlm, usd, eur := pfAsset(t, "native"), pfAsset(t, "crypto:XLM"), pfAsset(t, "fiat:USD"), pfAsset(t, "fiat:EUR")
	wrong := pfAsset(t, "WRNG-"+pfIssuer)
	if err := mr.Set(cachekeys.Freeze(native, usd).String(), "1"); err != nil {
		t.Fatal(err)
	}
	mr.HSet(cachekeys.Freeze(wrong, usd).String(), "f", "v") // GET fails WRONGTYPE: an error, not a miss
	fl, err := freeze.NewLooker(rdb)
	if err != nil {
		t.Fatal(err)
	}
	dec := PrefetchFreezeLooker{Looker: fl, RDB: rdb}
	reads := []readset.PairRead{{Base: native, Quote: usd}, {Base: cxlm, Quote: usd}, {Base: native, Quote: eur}, {Base: wrong, Quote: usd}}
	type answer struct{ frozen, err bool }
	want := make([]answer, len(reads))
	for i, r := range reads {
		f, err := fl.FrozenForPair(context.Background(), r.Base, r.Quote)
		want[i] = answer{f, err != nil}
		f, err = dec.FrozenForPair(context.Background(), r.Base, r.Quote)
		if got := (answer{f, err != nil}); got != want[i] {
			t.Errorf("no memo %+v: %+v, single read %+v", r, got, want[i])
		}
	}
	if !want[0].frozen || want[1].frozen || !want[3].err {
		t.Fatalf("fixture lost its shapes: %+v", want)
	}

	ctx := readset.With(context.Background(), readset.New())
	dec.PrefetchFrozen(ctx, reads)
	mr.Del(cachekeys.Freeze(native, usd).String())
	if err := mr.Set(cachekeys.Freeze(native, eur).String(), "1"); err != nil {
		t.Fatal(err)
	}
	for i, r := range reads {
		f, err := dec.FrozenForPair(ctx, r.Base, r.Quote)
		if got := (answer{f, err != nil}); got != want[i] {
			t.Errorf("prefetched %+v: %+v, single read at the prefetch instant %+v", r, got, want[i])
		}
	}
}
