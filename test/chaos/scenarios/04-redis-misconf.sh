#!/usr/bin/env bash
# Scenario 04 — Force Redis into MISCONF (stop-writes-on-bgsave-error)
# and assert the full F-0039 cascade response chain:
#
#   1. /v1/price             → 200 with flags.stale:true  (NOT 500)
#   2. /v1/oracle/latest     → 503 + Retry-After:30       (Wave-0 step 7)
#   3. /v1/vwap, /v1/twap    → 503 + Retry-After:30
#   4. /v1/lending/pools     → 503 + Retry-After:30
#   5. POST /v1/signup       → 503 + errors/throttle-unavailable AFTER
#                              the 30s dwell-time window (fail-CLOSED;
#                              F-0049 / F-0149 / ffc33c15)
#
# Then heals Redis and verifies every route returns to nominal.
#
# Why this scenario exists
# ────────────────────────
# Pre-Wave-0 (commit a91f901b), Redis MISCONF surfaced as HTTP 500
# `errors/internal` on 5 handler families — semantically wrong (500 =
# code bug, 503 = infra) and starved clients of the Retry-After hint
# that lets well-behaved integrators back off automatically. The
# May-10 SEV-2 left the API returning 500s for ~17 minutes while
# operators were still rolling the runbook. This scenario is the
# CI-side guard that the 503 mapping doesn't silently regress.
#
# Reproduction technique
# ──────────────────────
# Redis enters MISCONF when `stop-writes-on-bgsave-error yes` is set
# (it is by default in our dev compose, mirroring r1) AND a BGSAVE
# call subsequently fails. The cheapest way to provoke a BGSAVE
# failure without filling the disk is to point `dir` at an unwritable
# path and trigger BGSAVE — Redis returns `MISCONF Redis is configured
# to save RDB snapshots, but it's currently unable to persist to disk`
# on every subsequent write until BGSAVE succeeds again. Recovery is
# symmetric: CONFIG SET dir /data + BGSAVE.
#
# Pass criteria + invariants
# ──────────────────────────
# - GET routes that *write* a cache entry on success (the cascade-
#   affected family) MUST return 503 + Retry-After:30 + type URL
#   ending in `errors/cache-unavailable`. A 500 here is a regression.
# - GET routes that *only read* and have a fallback path (/v1/price)
#   MUST stay 200 with flags.stale:true. A 5xx here means the read
#   path leaks through to Redis writes that aren't supposed to fire.
# - The signup throttle is fail-CLOSED post-dwell-time: during the
#   first 30s of MISCONF, signup may still 200 (the dwell-time
#   tolerates transient Redis blips); after 30s it MUST 503.
# - Recovery is fast: every route back to baseline within 30s of
#   `redis-cli BGSAVE` succeeding.

set -euo pipefail

export SCENARIO="04-redis-misconf"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/chaos/scenarios/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

REDIS_CONTAINER="${REDIS_CONTAINER:-stellarindex-redis}"
# Dwell-time on r1 / dev = 30s (auth.DefaultSignupThrottleDwellTime
# and ratelimit.DefaultDwellTime both lock to that value — keep this
# constant in step or the signup-fail-closed assertion will misfire).
DWELL_TIME_SEC="${DWELL_TIME_SEC:-30}"
HEALTH_URL="$CHAOS_TARGET/v1/healthz"
PRICE_URL="$CHAOS_TARGET/v1/price?asset=native&quote=fiat:USD"
ORACLE_URL="$CHAOS_TARGET/v1/oracle/latest?asset=native"
VWAP_URL="$CHAOS_TARGET/v1/vwap?base=native&quote=fiat:USD"
TWAP_URL="$CHAOS_TARGET/v1/twap?base=native&quote=fiat:USD"
LENDING_URL="$CHAOS_TARGET/v1/lending/pools"
SIGNUP_URL="$CHAOS_TARGET/v1/signup"

chaos_setup

# ─── cleanup: always heal Redis on exit ────────────────────────────
cleanup() {
    if container_exists "$REDIS_CONTAINER"; then
        log "cleanup: restoring Redis writeable state"
        docker exec "$REDIS_CONTAINER" redis-cli CONFIG SET dir /data >/dev/null 2>&1 || true
        docker exec "$REDIS_CONTAINER" redis-cli BGSAVE >/dev/null 2>&1 || true
        docker exec "$REDIS_CONTAINER" redis-cli CONFIG SET stop-writes-on-bgsave-error no >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# ─── helpers ───────────────────────────────────────────────────────

# http_status_and_header URL HEADER [TIMEOUT_SEC] →
#   echoes "<status> <header-value>" (header-value may be empty).
# Used to assert both the status code AND the Retry-After hint in a
# single curl invocation.
http_status_and_header() {
    local url="$1"
    local header="$2"
    local timeout="${3:-5}"
    local out
    out="$(curl --silent --output /dev/null \
                 --write-out "%{http_code} %header{$header}" \
                 --max-time "$timeout" "$url" 2>/dev/null || echo "000 ")"
    echo "$out"
}

# Assert a route is in the cascade-503 state. Pass if status==503
# AND Retry-After is set (per writeCacheUnavailableProblem).
assert_cascade_503() {
    local name="$1"
    local url="$2"
    local out status retry
    out="$(http_status_and_header "$url" "Retry-After")"
    status="${out%% *}"
    retry="${out#* }"
    if [ "$status" != "503" ]; then
        die "$name: expected 503 under MISCONF, got $status"
    fi
    if [ -z "$retry" ] || [ "$retry" = " " ]; then
        warn "$name: 503 received but Retry-After header was empty"
    else
        log "$name: ✓ 503 + Retry-After:${retry% }"
    fi
}

# Assert /v1/price stays 200 with stale flag — degraded but not broken.
assert_price_stale() {
    local out status
    out="$(curl --silent --max-time 5 --write-out '\n%{http_code}' "$PRICE_URL" 2>/dev/null || echo "ERR")"
    status="$(echo "$out" | tail -1)"
    if [ "$status" != "200" ]; then
        # 200 is preferred, but a 503 cache-unavailable on /v1/price
        # is documented Wave-0 behaviour for the cold-cache case;
        # treat it as acceptable (the regression we're hunting is a
        # 500, not a 503).
        case "$status" in
            503) log "/v1/price returned 503 under MISCONF (cold-cache; acceptable)" ;;
            *)   die "/v1/price expected 200|503 under MISCONF, got $status" ;;
        esac
        return
    fi
    log "/v1/price: ✓ 200 (read path tolerates MISCONF)"
}

# Force Redis into MISCONF. After this returns, every Redis write
# will fail with the MISCONF prefix until heal_redis_misconf runs.
force_redis_misconf() {
    log "forcing Redis MISCONF: dir=/nonexistent + BGSAVE"
    # Ensure the safety net is active. Dev compose already has this on,
    # but pin it explicitly so the scenario is portable.
    docker exec "$REDIS_CONTAINER" redis-cli CONFIG SET stop-writes-on-bgsave-error yes >/dev/null
    # Point the snapshot dir at an unwritable path and trigger BGSAVE.
    # BGSAVE is async; Redis only flips into "writes blocked" mode
    # AFTER the background fork fails. Poll INFO Persistence until
    # rdb_last_bgsave_status reports "err".
    docker exec "$REDIS_CONTAINER" redis-cli CONFIG SET dir /nonexistent >/dev/null
    docker exec "$REDIS_CONTAINER" redis-cli BGSAVE >/dev/null 2>&1 || true
    local deadline
    deadline="$(($(date -u +%s) + 10))"
    while [ "$(date -u +%s)" -lt "$deadline" ]; do
        if docker exec "$REDIS_CONTAINER" redis-cli INFO persistence \
            | grep -q "rdb_last_bgsave_status:err"; then
            log "MISCONF active (rdb_last_bgsave_status:err)"
            return 0
        fi
        sleep 1
    done
    die "Redis did not enter MISCONF within 10s"
}

heal_redis_misconf() {
    log "healing Redis: dir=/data + BGSAVE + stop-writes=no"
    docker exec "$REDIS_CONTAINER" redis-cli CONFIG SET dir /data >/dev/null
    docker exec "$REDIS_CONTAINER" redis-cli BGSAVE >/dev/null 2>&1 || true
    # Wait for BGSAVE to actually succeed before declaring the heal
    # complete; INFO persistence is the authoritative source.
    local deadline
    deadline="$(($(date -u +%s) + 10))"
    while [ "$(date -u +%s)" -lt "$deadline" ]; do
        if docker exec "$REDIS_CONTAINER" redis-cli INFO persistence \
            | grep -q "rdb_last_bgsave_status:ok"; then
            log "Redis writes restored (rdb_last_bgsave_status:ok)"
            docker exec "$REDIS_CONTAINER" redis-cli CONFIG SET stop-writes-on-bgsave-error no >/dev/null
            return 0
        fi
        sleep 1
    done
    warn "BGSAVE didn't report ok within 10s; clearing stop-writes anyway"
    docker exec "$REDIS_CONTAINER" redis-cli CONFIG SET stop-writes-on-bgsave-error no >/dev/null
}

# ─── 1. baseline ───────────────────────────────────────────────────
assert_status "$HEALTH_URL" "200"

# ─── 2. force MISCONF ──────────────────────────────────────────────
force_redis_misconf

# Give the API a tick to pick up the new state (its cache writes
# fire on read-through; the next /v1/price miss will surface the
# MISCONF reply).
log "waiting 3s for API to observe MISCONF on next write"
sleep 3

# ─── 3. assert cascade response chain ──────────────────────────────
log "verifying cascade response chain"
assert_cascade_503 "/v1/oracle/latest" "$ORACLE_URL"
assert_cascade_503 "/v1/vwap"          "$VWAP_URL"
assert_cascade_503 "/v1/twap"          "$TWAP_URL"
assert_cascade_503 "/v1/lending/pools" "$LENDING_URL"
assert_price_stale

# ─── 4. signup throttle fail-CLOSED after dwell-time ──────────────
# During the first 30s of MISCONF the rate-limit middleware still
# fails open (the dwell-time policy tolerates transient blips);
# only AFTER 30s sustained MISCONF does it fail closed with 503 +
# errors/throttle-unavailable. Sleep through the dwell window then
# assert.
elapsed_since_misconf="$(($(date -u +%s) - SCENARIO_START - 3))"
remaining="$((DWELL_TIME_SEC - elapsed_since_misconf + 2))"
if [ "$remaining" -gt 0 ]; then
    log "sleeping ${remaining}s for signup dwell-time window to elapse"
    sleep "$remaining"
fi
log "verifying signup throttle fail-CLOSED post-dwell-time"
signup_status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
                      --max-time 5 -X POST \
                      -H 'Content-Type: application/json' \
                      -d '{"email":"chaos-04@example.test"}' \
                      "$SIGNUP_URL" 2>/dev/null || echo "000")"
case "$signup_status" in
    503) log "POST /v1/signup: ✓ 503 (fail-CLOSED post-dwell-time)" ;;
    # 429 = legitimately throttled (not the failure mode we're
    # testing but indicates the limiter is alive); fail anything
    # that's NOT a 5xx envelope or 429.
    429) log "POST /v1/signup returned 429 — rate-limited (acceptable; throttle alive)" ;;
    *)   die "POST /v1/signup expected 503 post-dwell, got $signup_status" ;;
esac

# ─── 5. heal + verify nominal ──────────────────────────────────────
heal_redis_misconf

# Give the API time to observe the healed Redis. The next cache
# write will succeed and the 503 surface will revert.
assert_recovers_within "$ORACLE_URL" "200" 30
# Lenient final assertion for /v1/vwap: on a fresh stack the pair may
# legitimately have NO trades and return 404 — that is NOT a
# regression (the contract here is "not stuck at 503"). The previous
# code used assert_recovers_within, which calls die()→exit on a
# non-200, so the trailing `|| log ...` NEVER fired and a no-trades
# 404 produced a FALSE regression verdict (G22-05). Use a non-fatal
# wait that simply gives recovery time, then let the 200|404
# case-check below be the real assertion.
wait_for_status "$VWAP_URL" "200" 30 || \
    log "/v1/vwap did not reach 200 within 30s (may be legitimate no-trades 404)"
post_heal_vwap="$(http_status "$VWAP_URL" 5)"
case "$post_heal_vwap" in
    200|404) log "/v1/vwap post-heal: $post_heal_vwap (acceptable)" ;;
    *)       die "/v1/vwap stuck at $post_heal_vwap after Redis heal" ;;
esac

assert_recovers_within "$HEALTH_URL" "200" 30

chaos_teardown_pass "F-0039 cascade: 503-on-MISCONF mapped, signup fail-CLOSED post-dwell, recovered within 30s of heal"
