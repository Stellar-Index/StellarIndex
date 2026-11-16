package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// serveThrough sends one GET through the full middleware stack, ETag included.
func serveThrough(t *testing.T, h http.Handler, target, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func envelopeAsOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		AsOf string `json:"as_of"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v: %s", err, rec.Body.String())
	}
	return env.AsOf
}

// assertUnchangedPayloadAnswers304 polls target twice with no data change
// in between: both bodies must carry the same ETag, and replaying that tag
// as If-None-Match must answer 304.
func assertUnchangedPayloadAnswers304(t *testing.T, h http.Handler, target string) {
	t.Helper()
	first := serveThrough(t, h, target, "")
	if first.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", target, first.Code, first.Body.String())
	}
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatalf("GET %s: no ETag (Cache-Control %q)", target, first.Header().Get("Cache-Control"))
	}
	time.Sleep(2 * time.Millisecond) // a now-stamped as_of would move by now

	second := serveThrough(t, h, target, "")
	if got := second.Header().Get("ETag"); got != tag {
		t.Fatalf("GET %s: ETag moved with no data change: %s then %s\n%s\n%s",
			target, tag, got, first.Body.String(), second.Body.String())
	}
	if got := serveThrough(t, h, target, tag); got.Code != http.StatusNotModified {
		t.Fatalf("GET %s If-None-Match %s: status %d, want 304", target, tag, got.Code)
	}
}

func TestSources_UnchangedCacheAnswers304(t *testing.T) {
	up := &fakeUpstream{}
	s := New(Options{SourcesStats: NewCachedSourcesStatsReader(up, time.Minute)})
	assertUnchangedPayloadAnswers304(t, s.Handler(), "/v1/sources?include=stats")
	if n := up.statsCalls.Load(); n != 1 {
		t.Fatalf("upstream stats calls = %d, want 1", n)
	}
}

func TestSources_CacheRefillMovesAsOfAndETag(t *testing.T) {
	up := &fakeUpstream{}
	c := NewCachedSourcesStatsReader(up, time.Minute)
	h := New(Options{SourcesStats: c}).Handler()
	const target = "/v1/sources?include=stats"

	first := serveThrough(t, h, target, "")
	tag, asOf := first.Header().Get("ETag"), envelopeAsOf(t, first)
	if tag == "" {
		t.Fatalf("no ETag on the first fill")
	}
	c.mu.Lock()
	c.stats.at = c.stats.at.Add(-2 * c.ttl)
	c.mu.Unlock()

	refilled := serveThrough(t, h, target, tag)
	if refilled.Code != http.StatusOK {
		t.Fatalf("after refill: status %d, want 200 (the old tag must not match)", refilled.Code)
	}
	if n := up.statsCalls.Load(); n != 2 {
		t.Fatalf("upstream stats calls = %d, want 2 (one refill)", n)
	}
	if got := envelopeAsOf(t, refilled); got == asOf {
		t.Errorf("as_of did not move on refill: %s", got)
	}
	if got := refilled.Header().Get("ETag"); got == tag || got == "" {
		t.Errorf("ETag after refill = %q, want a new tag (was %s)", got, tag)
	}
}

func TestOracle_UnchangedCacheAnswers304(t *testing.T) {
	for _, target := range []string{"/v1/oracle/streams", "/v1/oracle/latest?asset=native"} {
		t.Run(target, func(t *testing.T) {
			s := New(Options{Oracle: NewCachedOracleReader(&fakeOracleUpstream{}, time.Minute)})
			assertUnchangedPayloadAnswers304(t, s.Handler(), target)
		})
	}
}

// A cached read filled at hh:58 and served after the hour turned ends at the hour it was read in. Anchored on the
// clock, the series gained an hour newer than its own as_of holding a made-up zero.
func TestBuildSourceVolumeHistory_EndsAtTheHourTheBucketsWereReadIn(t *testing.T) {
	filled := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Minute) // hh:58 of the hour before this one
	readHour := filled.Truncate(time.Hour)
	series := buildSourceVolumeHistory([]timescale.SourceVolumeBucket{
		{Source: "binance", Hour: readHour, VolumeUSD: "12.5", TradeCount: 3},
	}, 24, filled)["binance"]

	if len(series) != 24 {
		t.Fatalf("series length = %d, want 24", len(series))
	}
	newest := series[len(series)-1]
	if !time.Time(newest.Hour).Equal(readHour) || newest.VolumeUSD != "12.5" || newest.TradeCount != 3 {
		t.Fatalf("newest bucket = %+v, want the read hour %s with the filled value: no hour later than the read exists in it", newest, readHour)
	}
}

// A failed include read drops its fields from every row, which is the same body as sources with no trades. The
// envelope names the sections that were asked for and not read.
func TestSources_FailedIncludeIsNamedOnTheWire(t *testing.T) {
	flagsOf := func(target string, up *fakeUpstream) (map[string]any, string) {
		rec := serveThrough(t, New(Options{SourcesStats: up}).Handler(), target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", target, rec.Code, rec.Body.String())
		}
		var env struct {
			Flags map[string]any `json:"flags"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env.Flags, rec.Header().Get("Cache-Control")
	}

	flags, cc := flagsOf("/v1/sources?include=stats,sparkline", &fakeUpstream{statsErr: errors.New("statement timeout")})
	if got := fmt.Sprint(flags["includes_unread"]); got != "[stats]" {
		t.Errorf("flags.includes_unread = %s, want [stats]: the sparkline read succeeded, the stats read did not", got)
	}
	if cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a listing missing a section", cc)
	}

	flags, _ = flagsOf("/v1/sources?include=stats", &fakeUpstream{})
	if _, present := flags["includes_unread"]; present {
		t.Errorf("flags.includes_unread present on a fully read listing: %v", flags["includes_unread"])
	}
}

// An uncached reader has no fill time to replay, so as_of stays the
// response time.
func TestSources_UncachedReaderStampsNow(t *testing.T) {
	h := New(Options{SourcesStats: &fakeUpstream{}}).Handler()
	const target = "/v1/sources?include=stats"
	before := time.Now().UTC().Add(-time.Second)
	got, err := time.Parse(time.RFC3339Nano, envelopeAsOf(t, serveThrough(t, h, target, "")))
	if err != nil {
		t.Fatal(err)
	}
	if got.Before(before) {
		t.Errorf("as_of = %s, want the response time", got)
	}
}

// assertAsOf checks the envelope as_of is exactly want: the cache fill
// time, which a request-time stamp could never equal.
func assertAsOf(t *testing.T, rec *httptest.ResponseRecorder, want time.Time) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got, err := time.Parse(time.RFC3339Nano, envelopeAsOf(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Errorf("as_of = %s, want the fill time %s", got, want)
	}
}

// filledAt is a fill time inside every cache's TTL and well before any request.
func filledAt() time.Time { return time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second) }

func TestIssuers_AsOfIsFillTime(t *testing.T) {
	up := &fakeIssuersUpstream{rows: []timescale.IssuerSummary{{GStrkey: "GISSUER"}}}
	c := NewCachedIssuersReader(up, time.Minute)
	h := New(Options{Issuers: c}).Handler()
	const target = "/v1/issuers?limit=10"
	serveThrough(t, h, target, "")

	at := filledAt()
	c.list.mu.Lock()
	for _, e := range c.list.entries {
		e.at = at
	}
	c.list.mu.Unlock()
	assertAsOf(t, serveThrough(t, h, target, ""), at)
	assertUnchangedPayloadAnswers304(t, h, target)
	if n := up.listCalls.Load(); n != 1 {
		t.Fatalf("upstream list calls = %d, want 1", n)
	}
}

func TestProtocolDetail_AsOfIsBuildTime(t *testing.T) {
	s := New(Options{})
	name := protocolRegistry[0].Name
	at := filledAt()
	s.protoDetailMu.Lock()
	s.protoDetailInitLocked()
	s.protoDetailCache[protocolDetailCacheKey(name, protocolActivityWindowDays)] = protoDetailEntry{
		view: ProtocolDetailView{
			ProtocolView: ProtocolView{Name: name},
			Analytics:    &ProtocolAnalyticsStatus{Status: protocolAnalyticsOK},
		},
		at: at,
	}
	s.protoDetailMu.Unlock()
	h := s.Handler()
	target := "/v1/protocols/" + name
	assertAsOf(t, serveThrough(t, h, target, ""), at)
	assertUnchangedPayloadAnswers304(t, h, target)
}

func TestProtocolTVL_AsOfIsRefreshTime(t *testing.T) {
	name := protocolRegistry[0].Name
	c := NewDEXTVLCache(DEXTVLSources{})
	at := filledAt()
	c.snapshot = map[string]ProtocolTVLView{name: {TVLUSD: "1"}}
	c.fetchedAt = at
	h := New(Options{DEXTVL: c}).Handler()
	target := "/v1/protocols/" + name + "/tvl"
	assertAsOf(t, serveThrough(t, h, target, ""), at)
	assertUnchangedPayloadAnswers304(t, h, target)
}

// A kept last-good snapshot keeps its build time even though the
// refresher re-stamps computedAt.
func TestDiagnosticsIngestion_AsOfIsBuildTime(t *testing.T) {
	s := New(Options{})
	at := filledAt()
	s.ingestionSnapshot.Store(&ingestionSnapshotEntry{computedAt: time.Now(), builtAt: at})
	h := s.Handler()
	const target = "/v1/diagnostics/ingestion"
	assertAsOf(t, serveThrough(t, h, target, ""), at)
	assertUnchangedPayloadAnswers304(t, h, target)
}

// A carried-forward figure keeps its own computed time: the envelope must
// agree with data.tvl.as_of, not report the cycle that carried it.
func TestProtocolTVL_CarriedForwardAsOfIsFigureTime(t *testing.T) {
	name := protocolRegistry[0].Name
	c := NewDEXTVLCache(DEXTVLSources{})
	figure := filledAt().Add(-10 * time.Minute)
	c.snapshot = map[string]ProtocolTVLView{name: {TVLUSD: "1", AsOf: figure.Format(time.RFC3339)}}
	c.carried = map[string]bool{name: true}
	c.fetchedAt = filledAt()
	h := New(Options{DEXTVL: c}).Handler()
	assertAsOf(t, serveThrough(t, h, "/v1/protocols/"+name+"/tvl", ""), figure)
}
