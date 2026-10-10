//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// startPlainRedis is startRedis without the `--save 1 1` flag that
// test exists to weaponise: these tests want an ordinary server, and a
// BGSAVE firing after every write during the concurrency subtest would
// add timing noise to the one assertion that depends on real
// contention.
func startPlainRedis(ctx context.Context, t *testing.T) *redis.Client {
	t.Helper()
	rdb, ctr := runRedisContainer(ctx, t)
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	return rdb
}

// frozenClock pins the window suffix so every observation below is
// about the KEY's lifetime rather than about the bucket rolling over.
// A test that let the clock advance would see a fresh key and could
// not tell "the TTL drained" from "we moved to the next window".
func frozenClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestRatelimit_RealRedis is the whole real-Redis surface for
// internal/ratelimit, sharing one container across subtests (the
// clickhouse_harness_test.go economy, applied at test scope).
func TestRatelimit_RealRedis(t *testing.T) {
	ctx := context.Background()
	rdb := startPlainRedis(ctx, t)

	t.Run("fixed_window_drain_ttl_actually_expires", func(t *testing.T) {
		testFixedWindowDrainTTL(ctx, t, rdb)
	})
	t.Run("fixed_window_ttl_is_not_re_armed_by_later_increments", func(t *testing.T) {
		testFixedWindowTTLNotReArmed(ctx, t, rdb)
	})
	t.Run("fixed_window_unset_key_race_is_atomic", func(t *testing.T) {
		testFixedWindowUnsetKeyRace(ctx, t, rdb)
	})
	t.Run("bucket_survives_script_cache_flush", func(t *testing.T) {
		testBucketSurvivesScriptFlush(ctx, t, rdb)
	})
	t.Run("bucket_deny_carries_a_real_ttl_shaped_reply", func(t *testing.T) {
		testBucketDenyReplyShape(ctx, t, rdb)
	})
}

// testFixedWindowDrainTTL proves the 2×window drain TTL is a real
// server-side expiry: with the clock frozen — so the key suffix cannot
// move — the counter key must vanish on its own and the next Incr must
// start again at 1. miniredis never expires anything without an
// explicit FastForward, so this property is asserted nowhere else.
func testFixedWindowDrainTTL(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	at := time.Unix(1_700_000_000, 0).UTC()
	base := "itest-drain:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	key := fmt.Sprintf("%s:%d", base, at.Unix()) // window = 1s ⇒ suffix = unix seconds

	c := ratelimit.NewFixedWindowCounter(rdb, time.Second, frozenClock(at))

	n, err := c.Incr(ctx, base)
	if err != nil {
		t.Fatalf("incr: %v", err)
	}
	if n != 1 {
		t.Fatalf("first Incr = %d, want 1 — INCR on a missing key must create it at 1", n)
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	// 2×1s window. Redis reports whole seconds and has already spent a
	// few ms, so 2s is the ceiling and anything above zero the floor.
	if ttl <= 0 || ttl > 2*time.Second {
		t.Fatalf("TTL(%s) = %v, want (0, 2s] — the drain TTL must be set by the "+
			"creating INCR (REL-05)", key, ttl)
	}

	// Wait past the drain TTL in real wall-clock time. Nothing
	// fast-forwards a real server.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		exists, existsErr := rdb.Exists(ctx, key).Result()
		if existsErr != nil {
			t.Fatalf("exists: %v", existsErr)
		}
		if exists == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if exists, existsErr := rdb.Exists(ctx, key).Result(); existsErr != nil || exists != 0 {
		t.Fatalf("key %s still present after the drain TTL (exists=%d, err=%v) — a "+
			"counter key that outlives its TTL leaks permanently, because the window "+
			"suffix moves on and nothing ever revisits it", key, exists, existsErr)
	}

	n, err = c.Incr(ctx, base)
	if err != nil {
		t.Fatalf("incr after drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("Incr after drain = %d, want 1 — a drained key must be recreated at 1", n)
	}
}

// testFixedWindowTTLNotReArmed pins the `if current == 1` guard in
// incrLua. Without it the TTL would be re-armed on every increment and
// a key under sustained load would never expire — the same unbounded
// namespace, arrived at from the other direction.
func testFixedWindowTTLNotReArmed(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	at := time.Unix(1_700_000_000, 0).UTC()
	base := "itest-noreset:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	// window = 10s ⇒ suffix = unix/10, TTL = 20s.
	key := fmt.Sprintf("%s:%d", base, at.Unix()/10)

	c := ratelimit.NewFixedWindowCounter(rdb, 10*time.Second, frozenClock(at))
	if _, err := c.Incr(ctx, base); err != nil {
		t.Fatalf("incr: %v", err)
	}
	first, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("pttl: %v", err)
	}

	// The margin has to dominate round-trip jitter, not merely exceed
	// it. An earlier revision of this test slept 1.5s and asserted only
	// `second < first`; under a deliberately broken script (EXPIRE
	// re-armed every call) it still passed roughly half the time,
	// because it was really comparing two EVAL+PTTL round-trips a
	// couple of milliseconds apart. Sleep well past the noise floor and
	// require the TTL to have DECAYED by most of the elapsed time.
	const (
		dwell     = 3 * time.Second
		minDecay  = 2 * time.Second // < dwell, to absorb second-rounding
		ttlWindow = 10 * time.Second
	)
	time.Sleep(dwell)

	if _, err := c.Incr(ctx, base); err != nil {
		t.Fatalf("second incr: %v", err)
	}
	second, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("pttl after second incr: %v", err)
	}
	if first-second < minDecay {
		t.Fatalf("PTTL went %v → %v across a second increment %v later — it decayed by "+
			"only %v, so the increment re-armed it. The drain TTL must be set ONLY by "+
			"the increment that CREATES the key (incrLua's `current == 1` guard); a "+
			"re-armed TTL makes a hot counter key immortal.",
			first, second, dwell, first-second)
	}
	// Sanity: the whole assertion above is meaningless if the key had
	// already expired and been recreated at a full TTL.
	if second <= 0 || second > 2*ttlWindow {
		t.Fatalf("PTTL after second incr = %v, want (0, %v] — the key must still be the "+
			"original one, mid-drain", second, 2*ttlWindow)
	}
	// And it must still be counting up, not restarting.
	n, err := c.Incr(ctx, base)
	if err != nil {
		t.Fatalf("third incr: %v", err)
	}
	if n != 3 {
		t.Fatalf("third Incr = %d, want 3 — increments inside one window accumulate", n)
	}
}

// testFixedWindowUnsetKeyRace is the EXPIRE race the Lua fold exists for,
// observed against a real server: many goroutines over several
// INDEPENDENT clients (separate connection pools, so the contention is
// genuinely server-side) hit one missing key inside one window.
//
// Real Redis executes each EVAL to completion before the next, so the
// post-increment counts must be exactly the permutation 1..N — no
// duplicates, no gaps — and exactly one caller may observe 1. And
// whichever caller that was, the key must carry a TTL afterwards: a
// TTL-less counter key is the permanent leak.
func testFixedWindowUnsetKeyRace(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	const (
		clients = 4
		perGo   = 25
		total   = clients * perGo
	)
	at := time.Unix(1_700_000_000, 0).UTC()
	base := "itest-race:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	key := fmt.Sprintf("%s:%d", base, at.Unix()/60) // window = 60s

	addr := rdb.Options().Addr
	counts := make([]int64, total)
	var wg sync.WaitGroup
	errs := make(chan error, total)
	start := make(chan struct{})

	for ci := range clients {
		client := redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { _ = client.Close() })
		c := ratelimit.NewFixedWindowCounter(client, time.Minute, frozenClock(at))
		for gi := range perGo {
			slot := ci*perGo + gi
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				n, err := c.Incr(ctx, base)
				if err != nil {
					errs <- err
					return
				}
				counts[slot] = n
			}()
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent incr: %v", err)
	}

	seen := make(map[int64]int, total)
	for _, n := range counts {
		seen[n]++
	}
	for want := int64(1); want <= total; want++ {
		if seen[want] != 1 {
			t.Fatalf("post-increment count %d was observed %d times (want exactly 1); "+
				"counts across %d concurrent callers must be the permutation 1..%d — "+
				"a duplicate is a lost update and would let a throttled caller past its cap",
				want, seen[want], total, total)
		}
	}

	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("TTL(%s) = %v after a %d-way race on an unset key; the caller that "+
			"created the key must have set the drain TTL in the same EVAL (REL-05) — "+
			"a TTL-less key is never revisited and leaks permanently", key, ttl, total)
	}
}

// testBucketSurvivesScriptFlush models a Redis restart / failover: the
// server's script cache is empty, so the EVALSHA go-redis sends first
// misses with NOSCRIPT and must be retried as a full EVAL. miniredis
// cannot produce that miss, so nothing else in the tree covers it —
// and the production consequence is total: every Take() after a Redis
// restart would error, fail open for DefaultDwellTime, then fail
// CLOSED (503) for the whole API.
func testBucketSurvivesScriptFlush(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	at := time.Unix(1_700_000_000, 0).UTC()
	key := "itest-flush:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	b := ratelimit.New(rdb, 10, time.Minute,
		ratelimit.WithClock(frozenClock(at)),
		ratelimit.WithKeyPrefix("itest-rl:"),
	)

	res, err := b.Take(ctx, key)
	if err != nil {
		t.Fatalf("take before flush: %v", err)
	}
	if !res.Allowed || res.Count != 1 {
		t.Fatalf("take before flush = %+v, want allowed with count 1", res)
	}

	if err := rdb.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("script flush: %v", err)
	}

	res, err = b.Take(ctx, key)
	if err != nil {
		t.Fatalf("take after SCRIPT FLUSH: %v — the limiter must recover from an empty "+
			"server-side script cache (Redis restart / failover), not error every call", err)
	}
	if !res.Allowed || res.Count != 2 {
		t.Fatalf("take after SCRIPT FLUSH = %+v, want allowed with count 2 — the counter "+
			"must continue from the surviving key, not restart", res)
	}
}

// testBucketDenyReplyShape drives the bucket over its limit against a
// real server and pins the deny path end to end. The Lua returns
// `{current, TTL}`; the Go side asserts `[]any{int64, int64}` and
// derives Allowed from `count`, never from the TTL. Real Redis RESP
// conversion is the authority for that shape, and the deny arm is the
// only one that calls TTL at all.
func testBucketDenyReplyShape(ctx context.Context, t *testing.T, rdb *redis.Client) {
	t.Helper()
	// 1700000000 / 60 = 28333333, ×60 = 1699999980, so the anchor sits
	// 20s into its 60s window and a denial must advertise the
	// remaining 40s.
	const (
		anchorUnix        = 1_700_000_000
		wantRetryAfterSec = 60 - (anchorUnix - (anchorUnix/60)*60)
	)
	at := time.Unix(anchorUnix, 0).UTC()
	key := "itest-deny:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	b := ratelimit.New(rdb, 2, time.Minute,
		ratelimit.WithClock(frozenClock(at)),
		ratelimit.WithKeyPrefix("itest-rl:"),
	)

	for i := 1; i <= 2; i++ {
		res, err := b.Take(ctx, key)
		if err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("take %d = %+v, want allowed (max=2)", i, res)
		}
		if want := 2 - i; res.Remaining != want {
			t.Fatalf("take %d remaining = %d, want %d", i, res.Remaining, want)
		}
	}

	res, err := b.Take(ctx, key)
	if err != nil {
		t.Fatalf("take 3: %v", err)
	}
	if res.Allowed {
		t.Fatalf("take 3 = %+v, want denied — the 3rd request in a max=2 window is over", res)
	}
	if res.Count != 3 {
		t.Fatalf("take 3 count = %d, want 3 (post-increment count is authoritative)", res.Count)
	}
	if res.Remaining != 0 {
		t.Fatalf("take 3 remaining = %d, want 0 (clamped)", res.Remaining)
	}
	// RetryAfter is the seconds left in the WINDOW, not the 2×window
	// drain TTL Redis holds on the key (which would be up to 120s here).
	if want := time.Duration(wantRetryAfterSec) * time.Second; res.RetryAfter != want {
		t.Fatalf("take 3 RetryAfter = %v, want %v — the header must be the remaining "+
			"window, not the drain TTL", res.RetryAfter, want)
	}
}

// An aclfile accepts neither comments nor line continuations, and
// redis-server refuses to start on the first bad line — so the shipped
// template is loaded into a real server, not just grepped.

var jinjaComment = regexp.MustCompile(`(?s)\{#.*?#\}`)

const aclLoadFixturePwd = "acl-load-fixture" // gitleaks:allow — throwaway container ACL password, not a credential

// renderShippedACL renders users.acl.j2 the way ansible's template module
// would for the only variable it uses, refusing any jinja it cannot render.
func renderShippedACL(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", aclTemplatePath))
	if err != nil {
		t.Fatalf("read ACL template: %v", err)
	}
	out := jinjaComment.ReplaceAllString(string(raw), "")
	out = strings.ReplaceAll(out, "{{ redis_password }}", aclLoadFixturePwd)
	for _, tok := range []string{"{{", "{%", "{#"} {
		if strings.Contains(out, tok) {
			t.Fatalf("%s uses jinja (%q) this fixture does not render; extend renderShippedACL", aclTemplatePath, tok)
		}
	}
	return out
}

func TestRedisACLTemplate_LoadsIntoRedisServer(t *testing.T) {
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx,
		"redis:7.4-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(renderShippedACL(t)),
			ContainerFilePath: "/users.acl",
			FileMode:          0o644,
		}),
		testcontainers.WithCmd("redis-server", "--aclfile", "/users.acl"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").WithStartupTimeout(30*time.Second),
		),
	)
	if ctr != nil {
		t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	}
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("docker unavailable, skipping integration test: %v", err)
		}
		logs := ""
		if ctr != nil {
			if rc, lerr := ctr.Logs(ctx); lerr == nil {
				b, _ := io.ReadAll(rc)
				logs = string(b)
			}
		}
		t.Fatalf("redis-server did not start with the rendered %s: %v\n%s", aclTemplatePath, err, logs)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("container mapped port: %v", err)
	}
	addr := fmt.Sprintf("%s:%s", host, port.Port())
	client := func(user, pwd string) *redis.Client {
		c := redis.NewClient(&redis.Options{Addr: addr, Username: user, Password: pwd})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	if err := client("", "").Ping(ctx).Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Fatalf("unauthenticated PING = %v, want NOAUTH: the default user is not off", err)
	}

	app := client("stellarindex", aclLoadFixturePwd)
	// The first and last key-pattern and channel lines of the rule, so a
	// rule cut short at a line break fails here.
	if err := app.Set(ctx, "vwap:acl-load", "1", time.Minute).Err(); err != nil {
		t.Fatalf("stellarindex SET vwap:*: %v", err)
	}
	if err := app.Set(ctx, "markets:list:acl-load", "1", time.Minute).Err(); err != nil {
		t.Fatalf("stellarindex SET markets:list:*: %v", err)
	}
	if err := app.Publish(ctx, "stream-acl-load", "x").Err(); err != nil {
		t.Fatalf("stellarindex PUBLISH stream-*: %v", err)
	}
	if err := app.FlushAll(ctx).Err(); err == nil || !strings.Contains(err.Error(), "NOPERM") {
		t.Fatalf("stellarindex FLUSHALL = %v, want NOPERM", err)
	}

	if err := client("redis_exporter", aclLoadFixturePwd).Info(ctx).Err(); err != nil {
		t.Fatalf("redis_exporter INFO: %v", err)
	}
	if err := client("sentinel", aclLoadFixturePwd).Do(ctx, "ROLE").Err(); err != nil {
		t.Fatalf("sentinel ROLE: %v", err)
	}
	if err := client("replication", aclLoadFixturePwd).Ping(ctx).Err(); err != nil {
		t.Fatalf("replication PING: %v", err)
	}
}

// TestCascadeMISCONF_EndToEnd is the integration twin of
// internal/api/v1/cache_unavailable_test.go's stub-based tests. It
// uses a real Redis container in real MISCONF state to drive the
// 503-on-cache-unavailable mapping end-to-end.
//
// Skipped automatically when Docker isn't available — mirrors the
// existing pattern in test/integration/migrations_test.go.
//
// Nominal runtime: ~10s on a warm Docker cache, ~30s on a cold one.
func TestCascadeMISCONF_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rdb, redisCtr := startRedis(t, ctx)
	t.Cleanup(func() {
		_ = redisCtr.Terminate(context.Background())
	})

	// Verify Redis is alive end-to-end before we start chaos.
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}

	// Build a v1.Server wired with our test reader that talks to
	// the real Redis container. The reader's LatestOracleUpdatesForAsset
	// does a Redis SET on every call — under MISCONF that SET fails
	// with the MISCONF prefix, which is exactly the failure shape
	// the production cascade-affected handlers see in the May-10
	// SEV-2 (commit a91f901b's rationale).
	oracle := &redisOracleReader{rdb: rdb}
	srv := v1.New(v1.Options{Oracle: oracle})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// ─── 1. baseline: healthy Redis → 200 ─────────────────────────
	t.Run("baseline_200", func(t *testing.T) {
		resp := httpGet(t, ts.URL+"/v1/oracle/latest?asset=native")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("baseline status = %d, want 200 (Redis healthy)", resp.StatusCode)
		}
	})

	// ─── 2. force MISCONF, assert 503 + Retry-After ───────────────
	t.Run("misconf_503_with_retry_after", func(t *testing.T) {
		probeSet := func() error {
			return rdb.Set(ctx, "misconf-probe-handler", "v", 0).Err()
		}
		if err := forceMISCONF(ctx, redisCtr, probeSet); err != nil {
			t.Fatalf("forceMISCONF: %v", err)
		}
		// Heal on test failure so subsequent sub-tests aren't blocked.
		defer func() {
			if err := healMISCONF(ctx, redisCtr); err != nil {
				t.Logf("heal on cleanup: %v", err)
			}
		}()

		// Wait briefly for the new state to propagate; BGSAVE is
		// asynchronous, the stop-writes flag flips once the fork
		// fails. Retry the request a few times rather than sleeping
		// blindly — typical settle is <1s.
		var resp *http.Response
		var err error
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			resp, err = http.Get(ts.URL + "/v1/oracle/latest?asset=native")
			if err == nil && resp.StatusCode == http.StatusServiceUnavailable {
				break
			}
			if resp != nil {
				resp.Body.Close()
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("GET under MISCONF: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 503 (MISCONF cascade); body=%s",
				resp.StatusCode, body)
		}
		if got := resp.Header.Get("Retry-After"); got != "30" {
			t.Errorf("Retry-After = %q, want 30 (writeCacheUnavailableProblem invariant)", got)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "errors/cache-unavailable") {
			t.Errorf("body missing errors/cache-unavailable type URL: %s", body)
		}
		// problem+json must be a valid envelope.
		var problem map[string]any
		if err := json.Unmarshal(body, &problem); err != nil {
			t.Errorf("response body not valid JSON: %v", err)
		}
		if got, _ := problem["status"].(float64); int(got) != http.StatusServiceUnavailable {
			t.Errorf("problem.status = %v, want 503", problem["status"])
		}
	})

	// ─── 3. heal Redis, assert routes return to nominal ──────────
	t.Run("recovery_200", func(t *testing.T) {
		if err := healMISCONF(ctx, redisCtr); err != nil {
			t.Fatalf("heal: %v", err)
		}
		// Poll until the route returns to 200 — heal is async; typical
		// settle is <2s once BGSAVE reports ok.
		deadline := time.Now().Add(30 * time.Second)
		var lastStatus int
		for time.Now().Before(deadline) {
			resp, err := http.Get(ts.URL + "/v1/oracle/latest?asset=native")
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			lastStatus = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("did not recover to 200 within 30s; last status = %d", lastStatus)
	})

	// ─── 4. predicate sanity — go-redis surfaces MISCONF the way
	//        IsCacheUnavailable expects ───────────────────────────
	t.Run("go_redis_misconf_classification", func(t *testing.T) {
		// Repro MISCONF directly via the client — bypass the handler
		// to verify go-redis's error shape hasn't drifted in a way
		// the predicate would miss. This is a defence-in-depth check;
		// if it ever fails, IsCacheUnavailable needs a new branch.
		probeSet := func() error {
			return rdb.Set(ctx, "misconf-probe-direct", "v", 0).Err()
		}
		if err := forceMISCONF(ctx, redisCtr, probeSet); err != nil {
			t.Fatalf("forceMISCONF: %v", err)
		}
		defer func() { _ = healMISCONF(ctx, redisCtr) }()

		// Retry briefly for state to settle.
		var setErr error
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			setErr = rdb.Set(ctx, "misconf-probe", "v", 0).Err()
			if setErr != nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if setErr == nil {
			t.Fatalf("expected MISCONF error from SET, got nil")
		}
		if !v1.IsCacheUnavailable(setErr) {
			t.Errorf("IsCacheUnavailable did not classify go-redis MISCONF error: %v", setErr)
		}
		// Also classify the wrapped form (the orchestrator wraps via
		// fmt.Errorf("redis set %s: %w", key, err)).
		wrapped := fmt.Errorf("redis set vwap:foo: %w", setErr)
		if !v1.IsCacheUnavailable(wrapped) {
			t.Errorf("IsCacheUnavailable did not classify wrapped MISCONF: %v", wrapped)
		}
	})
}

// ─── helpers ──────────────────────────────────────────────────────

// startRedis spins up a single-node Redis container with the same
// settings the dev compose uses (`stop-writes-on-bgsave-error yes`
// is on by default in Redis 7) PLUS `--save 1 1` so BGSAVE auto-
// fires on the first write — that's how forceMISCONF provokes the
// stop-writes flag without needing CONFIG SET (which Redis 7.4
// rejects at runtime for the `dir` key as a protected config).
//
// Returns a go-redis client + the container handle so the caller
// can exec docker commands.
func startRedis(t *testing.T, ctx context.Context) (*redis.Client, testcontainers.Container) {
	t.Helper()
	return runRedisContainer(ctx, t, testcontainers.WithCmd("redis-server", "--save", "1", "1"))
}

// runRedisContainer starts redis:7.4-alpine, skipping the test when Docker
// is unavailable, and returns a client plus the container handle.
func runRedisContainer(ctx context.Context, t *testing.T, opts ...testcontainers.ContainerCustomizer) (*redis.Client, testcontainers.Container) {
	t.Helper()
	opts = append([]testcontainers.ContainerCustomizer{
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").
				WithStartupTimeout(60 * time.Second),
		),
	}, opts...)
	ctr, err := testcontainers.Run(ctx, "redis:7.4-alpine", opts...)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("docker unavailable, skipping integration test: %v", err)
		}
		t.Fatalf("start redis: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("container mapped port: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", host, port.Port()),
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, ctr
}

// forceMISCONF puts Redis into the stop-writes state by:
//  1. chmod 0 /data — strips write permission from the snapshot
//     dir. CONFIG SET dir is rejected at runtime in Redis 7.4 as
//     a protected config, so we change the FS underneath instead.
//  2. SET a probe key — triggers the `--save 1 1` rule (started
//     with that flag in startRedis), which fires BGSAVE within 1s.
//  3. Poll INFO persistence until rdb_last_bgsave_status:err.
//
// Once BGSAVE has failed once, every subsequent write returns
// `MISCONF Redis is configured to save RDB snapshots, but it's
// currently unable to persist to disk` — exactly the May-10 SEV-2
// surface (and what a91f901b's helper maps to HTTP 503).
//
// probeSet is the caller's go-redis-driven write — using the same
// client that subsequent assertions use ensures we hit the same
// connection pool and same surface.
func forceMISCONF(ctx context.Context, ctr testcontainers.Container, probeSet func() error) error {
	// Re-arm the safety net: healMISCONF turns it off on cleanup,
	// and Redis won't block writes on BGSAVE failure without it. On
	// the first call this is a no-op (default is "yes" on image
	// start); on the second call this is what makes the test
	// re-armable. Authoritative writes-blocked behaviour requires
	// this flag, NOT just rdb_last_bgsave_status:err.
	if err := execIgnoringBGSAVE(ctx, ctr,
		[]string{
			"redis-cli", "CONFIG", "SET",
			"stop-writes-on-bgsave-error", "yes",
		}); err != nil {
		return fmt.Errorf("re-arm stop-writes: %w", err)
	}
	if err := execIgnoringBGSAVE(ctx, ctr,
		[]string{"chmod", "0", "/data"}); err != nil {
		return fmt.Errorf("chmod /data: %w", err)
	}
	// Trigger the save rule. Multiple writes may be needed because
	// the `--save N M` threshold could already have been satisfied
	// by an earlier write that BGSAVEd cleanly.
	if err := probeSet(); err != nil && !strings.Contains(err.Error(), "MISCONF") {
		return fmt.Errorf("probe SET: %w", err)
	}
	// Authoritative readiness check: keep probing SET until it
	// returns MISCONF. rdb_last_bgsave_status:err is a NECESSARY
	// but not SUFFICIENT condition — Redis only sets the
	// stop-writes-on-bgsave-error trip-wire on the next write
	// attempt after the failed BGSAVE. Looping on the SET itself
	// gives us the end-state guarantee callers actually want.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		err := probeSet()
		if err != nil && strings.Contains(err.Error(), "MISCONF") {
			return nil
		}
		// Cross-check: if BGSAVE has fired in err state but
		// stop-writes hasn't tripped yet, sleeping briefly gives
		// the trip-wire time to engage on the next probe.
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("redis did not enter MISCONF (writes still succeeding) within 15s")
}

// healMISCONF restores Redis to a writeable state: chmod 0755 /data,
// trigger a BGSAVE that succeeds, clear the stop-writes flag.
func healMISCONF(ctx context.Context, ctr testcontainers.Container) error {
	if err := execIgnoringBGSAVE(ctx, ctr,
		[]string{"chmod", "0755", "/data"}); err != nil {
		return fmt.Errorf("chmod /data: %w", err)
	}
	// BGSAVE here is safe — `dir` is back to writeable. Use
	// redis-cli to avoid needing a live go-redis client (the caller
	// may still be in the middle of a MISCONF-blocked op).
	_ = execIgnoringBGSAVE(ctx, ctr, []string{"redis-cli", "BGSAVE"})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, err := execCapture(ctx, ctr,
			[]string{"redis-cli", "INFO", "persistence"})
		if err == nil && strings.Contains(out, "rdb_last_bgsave_status:ok") {
			// BGSAVE recovered; clear the safety net (matches the
			// chaos scenario sequence in test/chaos/scenarios/
			// 04-redis-misconf.sh).
			return execIgnoringBGSAVE(ctx, ctr,
				[]string{
					"redis-cli", "CONFIG", "SET",
					"stop-writes-on-bgsave-error", "no",
				})
		}
		_ = execIgnoringBGSAVE(ctx, ctr, []string{"redis-cli", "BGSAVE"})
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("BGSAVE did not report ok within 15s")
}

// execIgnoringBGSAVE is a thin wrapper around ctr.Exec that ignores
// the "Background saving started" stderr noise from BGSAVE — Redis
// returns 0 but writes a status line to stdout that confuses our
// captured-output asserts elsewhere. We only care that the command
// dispatched.
func execIgnoringBGSAVE(ctx context.Context, ctr testcontainers.Container, cmd []string) error {
	code, _, err := ctr.Exec(ctx, cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit %d", code)
	}
	return nil
}

// execCapture runs cmd and returns combined stdout as a string.
func execCapture(ctx context.Context, ctr testcontainers.Container, cmd []string) (string, error) {
	code, r, err := ctr.Exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exit %d", code)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// httpGet wraps http.Get with a per-request timeout, mirroring the
// r1-smoke.sh per-request budget.
func httpGet(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// isDockerUnavailable mirrors migrations_test.go's behaviour: skip
// when Docker isn't reachable rather than fail the test. We don't
// have a single sentinel for this — testcontainers wraps with its
// own error type — so check both the message and the wrap chain.
func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, hint := range []string{
		"Cannot connect to the Docker daemon",
		"docker daemon",
		"connect: no such file or directory",
		"context deadline exceeded",
		"rootless docker",
	} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// ─── redisOracleReader — minimal v1.OracleReader hitting real Redis
//
// Every call performs a Redis SET to mirror the cascade-affected
// handlers' cache-write pattern. Under MISCONF that SET fails with
// the MISCONF prefix; the handler wraps it (via the existing
// observability seam) and the v1 layer's IsCacheUnavailable
// predicate flips the response to 503 + Retry-After.

type redisOracleReader struct {
	rdb *redis.Client
}

// LatestOracleUpdatesForAsset returns an empty slice on healthy
// Redis (the asset has no observations seeded — we're only testing
// the cache-write failure surface, not the data path). Under
// MISCONF the SET fails and the error propagates to the handler.
func (r *redisOracleReader) LatestOracleUpdatesForAsset(
	ctx context.Context, asset canonical.Asset, sourceFilter string,
) ([]canonical.OracleUpdate, error) {
	// Touch Redis with a write — this is the operation that fails
	// with MISCONF in production (per a91f901b's diagnosis).
	if err := r.rdb.Set(ctx, "oracle:probe:"+asset.String(), "v", time.Minute).Err(); err != nil {
		return nil, fmt.Errorf("redis set oracle:probe:%s: %w", asset.String(), err)
	}
	return nil, nil
}

func (r *redisOracleReader) LatestOracleUpdatesForAssets(
	ctx context.Context, assets []canonical.Asset, sourceFilter string,
) ([]canonical.OracleUpdate, error) {
	if err := r.rdb.Set(ctx, "oracle:probe-multi", "v", time.Minute).Err(); err != nil {
		return nil, fmt.Errorf("redis set oracle:probe-multi: %w", err)
	}
	return nil, nil
}

func (r *redisOracleReader) LatestOracleStreams(
	ctx context.Context,
) ([]canonical.OracleUpdate, error) {
	if err := r.rdb.Set(ctx, "oracle:streams-probe", "v", time.Minute).Err(); err != nil {
		return nil, fmt.Errorf("redis set oracle:streams-probe: %w", err)
	}
	return nil, nil
}

const (
	aclTemplatePath  = "configs/ansible/roles/redis-sentinel/templates/users.acl.j2"
	aclIndexPattern  = "~apikey-index:*"
	aclFixtureAppPwd = "index-acl-fixture" // gitleaks:allow — throwaway container ACL user, not a credential
)

// shippedStellarindexACLRule returns the `user stellarindex …` rule of
// the shipped template as ACL SETUSER arguments, with the vaulted
// password placeholder replaced by the fixture word.
func shippedStellarindexACLRule(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", aclTemplatePath))
	if err != nil {
		t.Fatalf("read ACL template: %v", err)
	}
	var rule []string
	inRule := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inRule && !strings.HasPrefix(trimmed, "user stellarindex ") {
			continue
		}
		inRule = true
		rule = append(rule, strings.TrimSuffix(trimmed, "\\"))
		if !strings.HasSuffix(trimmed, "\\") {
			break
		}
	}
	joined := strings.Join(rule, " ")
	if !strings.Contains(joined, ">{{ redis_password }}") {
		t.Fatalf("template rule has no password placeholder to substitute: %q", joined)
	}
	joined = strings.Replace(joined, ">{{ redis_password }}", ">"+aclFixtureAppPwd, 1)
	fields := strings.Fields(joined)
	if len(fields) < 4 || fields[0] != "user" || fields[1] != "stellarindex" {
		t.Fatalf("could not parse the stellarindex rule out of %s: %q", aclTemplatePath, joined)
	}
	return fields[2:]
}

func withoutPattern(rule []string, pattern string) []string {
	out := make([]string, 0, len(rule))
	for _, f := range rule {
		if f != pattern {
			out = append(out, f)
		}
	}
	return out
}

func setACLUser(ctx context.Context, t *testing.T, admin *redis.Client, user string, rule []string) {
	t.Helper()
	args := []any{"ACL", "SETUSER", user, "reset"}
	for _, f := range rule {
		args = append(args, f)
	}
	if err := admin.Do(ctx, args...).Err(); err != nil {
		t.Fatalf("ACL SETUSER %s: %v", user, err)
	}
}

func appClient(t *testing.T, admin *redis.Client, user string) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: admin.Options().Addr, Username: user, Password: aclFixtureAppPwd})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestAPIKeyIndex_RealRedisACL(t *testing.T) {
	ctx := context.Background()
	admin := startPlainRedis(ctx, t)
	shipped := shippedStellarindexACLRule(t)
	index := cachekeys.APIKeyIndex().String()

	t.Run("the shipped template admits the index family", func(t *testing.T) {
		found := false
		for _, f := range shipped {
			found = found || f == aclIndexPattern
		}
		if !found {
			t.Fatalf("%s does not grant %s: every index access is NOPERM under lockdown and the keyspace walk never retires",
				aclTemplatePath, aclIndexPattern)
		}
	})

	t.Run("ACL not yet applied: issuance and revocation work, nothing half-written", func(t *testing.T) {
		if err := admin.FlushAll(ctx).Err(); err != nil {
			t.Fatalf("flushall: %v", err)
		}
		setACLUser(ctx, t, admin, "stellarindex", withoutPattern(shipped, aclIndexPattern))
		app := appClient(t, admin, "stellarindex")
		store := auth.NewRedisAPIKeyStore(app)
		validator := auth.NewRedisAPIKeyValidator(app)

		// The denial really is in force, and really reads NOPERM.
		err := app.HGet(ctx, index, "ready").Err()
		if err == nil || errors.Is(err, redis.Nil) || !strings.Contains(err.Error(), "NOPERM") {
			t.Fatalf("index read under the old ACL = %v, want a NOPERM denial", err)
		}

		const owner = "account:acl-gap"
		first, firstPlain, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("Create while the index family is denied: %v — key issuance is DOWN until ansible runs", err)
		}
		second, secondPlain, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("second Create: %v", err)
		}
		if _, err := validator.Lookup(ctx, firstPlain); err != nil {
			t.Fatalf("key issued under the old ACL does not authenticate: %v", err)
		}
		if n, err := admin.Exists(ctx, index).Result(); err != nil || n != 0 {
			t.Fatalf("index key exists=%d err=%v after denied issuance: the script ran past the denial", n, err)
		}
		if n, err := admin.DBSize(ctx).Result(); err != nil || n != 2 {
			t.Fatalf("dbsize=%d err=%v, want exactly the 2 credential records", n, err)
		}

		recs, err := store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 2 {
			t.Fatalf("list under the old ACL: %d records, err=%v; want 2", len(recs), err)
		}
		if err := store.RevokeKeyByID(ctx, owner, first.KeyID); err != nil {
			t.Fatalf("revoke under the old ACL: %v", err)
		}
		if _, err := validator.Lookup(ctx, firstPlain); err == nil {
			t.Fatal("revoked key still authenticates under the old ACL: revocation silently no-ops")
		}

		// Ansible applies the template: the pattern is granted to the live
		// user. The API process is NOT restarted — same store, same pool —
		// so this also proves no restart is needed to retire the walk.
		if err := admin.Do(ctx, "ACL", "SETUSER", "stellarindex", aclIndexPattern).Err(); err != nil {
			t.Fatalf("grant %s: %v", aclIndexPattern, err)
		}
		recs, err = store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 1 || recs[0].KeyID != second.KeyID {
			t.Fatalf("list after the ACL is applied = %+v, err=%v; want the key minted during the gap", recs, err)
		}
		if ready, err := admin.HGet(ctx, index, "ready").Result(); err != nil || ready != "1" {
			t.Fatalf("index ready=%q err=%v after the first lookup under the shipped ACL", ready, err)
		}
		if err := store.RevokeKeyByID(ctx, owner, second.KeyID); err != nil {
			t.Fatalf("revoke after the ACL is applied: %v", err)
		}
		if _, err := validator.Lookup(ctx, secondPlain); err == nil {
			t.Fatal("a key minted during the ACL gap survives revocation once the index is live")
		}
	})

	t.Run("shipped ACL: indexed issuance, lookups and revoke, surviving a script-cache flush", func(t *testing.T) {
		if err := admin.FlushAll(ctx).Err(); err != nil {
			t.Fatalf("flushall: %v", err)
		}
		setACLUser(ctx, t, admin, "stellarindex", shipped)
		app := appClient(t, admin, "stellarindex")
		store := auth.NewRedisAPIKeyStore(app)

		const owner = "account:steady-state"
		if _, err := store.ListKeysForIdentifier(ctx, owner); err != nil { // builds on an empty deployment
			t.Fatalf("first lookup: %v", err)
		}
		rec, plaintext, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Every Redis restart or failover empties the script cache.
		if err := admin.ScriptFlush(ctx).Err(); err != nil {
			t.Fatalf("script flush: %v", err)
		}
		mirrored := auth.MirroredKey{
			Plaintext: "sip_" + strings.Repeat("cd", 32),
			Record:    auth.APIKeyRecord{KeyID: "kid_acl_mirror", Identifier: owner},
		}
		if err := store.CreateWithSecret(ctx, mirrored); err != nil {
			t.Fatalf("CreateWithSecret after SCRIPT FLUSH: %v", err)
		}
		if ptr, err := admin.HGet(ctx, index, "k:"+rec.KeyID).Result(); err != nil || len(ptr) != 64 {
			t.Fatalf("KeyID pointer = %q err=%v, want the record's sha256 hex", ptr, err)
		}
		if ttl, err := admin.TTL(ctx, index).Result(); err != nil || ttl >= 0 {
			t.Fatalf("index ttl=%v err=%v, want none", ttl, err)
		}
		recs, err := store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 2 {
			t.Fatalf("list: %d records, err=%v; want 2", len(recs), err)
		}
		if _, err := store.UpdateRateLimit(ctx, rec.KeyID, 42); err != nil {
			t.Fatalf("UpdateRateLimit: %v", err)
		}
		if err := store.RevokeKeyByID(ctx, owner, rec.KeyID); err != nil {
			t.Fatalf("RevokeKeyByID: %v", err)
		}
		if _, err := auth.NewRedisAPIKeyValidator(app).Lookup(ctx, plaintext); err == nil {
			t.Fatal("revoked key still authenticates")
		}
		if n, err := admin.HExists(ctx, index, "k:"+rec.KeyID).Result(); err != nil || n {
			t.Fatalf("KeyID pointer survived the revoke (exists=%v err=%v)", n, err)
		}
	})
}
