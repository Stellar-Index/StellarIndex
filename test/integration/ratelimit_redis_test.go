//go:build integration

package integration_test

// Real-Redis coverage for internal/ratelimit (#340 item 8).
//
// Why this exists alongside internal/ratelimit/*_test.go
// ─────────────────────────────────────────────────────
// The unit suite drives both primitives against miniredis, which is a
// re-implementation of the Redis command surface, not Redis. Four
// behaviours the throttles depend on are precisely the ones an
// in-memory double is free to get wrong, and each has a production
// failure mode:
//
//   - **Wall-clock key expiry.** miniredis only expires on an explicit
//     FastForward, so "the 2×window drain TTL actually drains" is
//     asserted nowhere today. If it didn't, the throttle namespace
//     would grow without bound — the REL-05 leak, re-introduced from
//     the server side instead of the client side.
//   - **TTL is set on the creating INCR and never re-armed.** A
//     sliding TTL under sustained load makes a hot key immortal, which
//     is the same leak wearing a different hat.
//   - **The EXPIRE race on an unset key.** Two clients, two connection
//     pools, one missing key, same window: real Redis single-threads
//     the EVAL so exactly one caller observes `current == 1`. That is
//     the atomicity REL-05 bought by folding INCR+EXPIRE into Lua, and
//     it can only be observed against a server with real concurrency.
//   - **NOSCRIPT recovery.** go-redis's Script.Run issues EVALSHA and
//     falls back to EVAL when the server has never seen the SHA. Every
//     Redis restart, failover or SCRIPT FLUSH empties that cache in
//     production. miniredis cannot model the miss. If the fallback
//     ever stopped working, every rate-limit check after a Redis
//     restart would error — fail-open for DefaultDwellTime, then
//     fail-CLOSED 503 across the whole API.
//
// internal/ratelimit is a fixed-window INCR+EXPIRE counter, NOT a
// token bucket: nothing here refills, and the assertions below are
// written against post-increment counts and window boundaries only.
//
// Nominal runtime: ~15s on a warm Docker cache (one container shared
// by every subtest), dominated by the two real-time expiry waits.

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// startPlainRedis is startRedis without the `--save 1 1` flag that
// test exists to weaponise: these tests want an ordinary server, and a
// BGSAVE firing after every write during the concurrency subtest would
// add timing noise to the one assertion that depends on real
// contention.
func startPlainRedis(ctx context.Context, t *testing.T) *redis.Client {
	t.Helper()
	ctr, err := testcontainers.Run(ctx,
		"redis:7.4-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("docker unavailable, skipping integration test: %v", err)
		}
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("container mapped port: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("%s:%s", host, port.Port())})
	t.Cleanup(func() { _ = rdb.Close() })
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
// namespace REL-05 closed, arrived at from the other direction.
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

// testFixedWindowUnsetKeyRace is the EXPIRE race REL-05 exists for,
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
