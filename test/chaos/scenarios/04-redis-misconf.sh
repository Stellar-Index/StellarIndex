#!/usr/bin/env bash
# Scenario 04 - force Redis into MISCONF (stop-writes-on-bgsave-error) and
# assert the rate limiter's dwell-time policy end to end.
#
# Under MISCONF Redis serves reads and refuses writes. GET data routes only read
# Redis (cache writes are best-effort), but every rate-limited route makes one
# write, the rate-limit take, so the limiter's failure policy decides what
# clients see (internal/ratelimit/doc.go, "Failure mode — dwell-time fail-open
# inversion"):
#   (a) within the dwell window (api.rate_limit_dwell, default 30s) counted from
#       the FIRST failed take: fail OPEN, routes answer as before, never 500.
#   (b) past it: fail CLOSED, every rate-limited route (POST /v1/signup
#       included) answers 503 errors/throttle-unavailable + Retry-After.
# Then heals Redis and verifies every GET route returns to baseline.
#
# Why: a write-refusing Redis must surface as 503 + Retry-After (infra, back off),
# not 500 (code bug), and a sustained outage must not fail open indefinitely.
#
# The dwell clock is per bucket and stays armed until a full window of unbroken
# successes, so this relies on earlier scenarios probing only limiter-exempt
# health routes.
#
# Technique: MISCONF needs `stop-writes-on-bgsave-error yes` (dev compose default,
# mirrors r1) plus a failing BGSAVE. Make the snapshot dir unwritable (chmod 000
# via `docker exec`, root vs the `redis` user) and trigger BGSAVE. `CONFIG SET
# dir` is unusable: Redis 7 protects it. Recovery: restore the mode + BGSAVE.
# The open AOF keeps accepting appends; only new file creation (RDB temp) fails.

set -euo pipefail

export SCENARIO="04-redis-misconf"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/chaos/scenarios/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

REDIS_CONTAINER="${REDIS_CONTAINER:-stellarindex-redis}"
# Must match the API's api.rate_limit_dwell (default 30s).
DWELL_TIME_SEC="${DWELL_TIME_SEC:-30}"
HEALTH_URL="$CHAOS_TARGET/v1/healthz"
PRICE_URL="$CHAOS_TARGET/v1/price?asset=native&quote=fiat:USD"
ORACLE_URL="$CHAOS_TARGET/v1/oracle/latest?asset=native"
VWAP_URL="$CHAOS_TARGET/v1/vwap?base=native&quote=fiat:USD"
TWAP_URL="$CHAOS_TARGET/v1/twap?base=native&quote=fiat:USD"
LENDING_URL="$CHAOS_TARGET/v1/lending/pools"
SIGNUP_URL="$CHAOS_TARGET/v1/signup"
GET_ROUTES=(
    "/v1/price|$PRICE_URL"
    "/v1/oracle/latest|$ORACLE_URL"
    "/v1/vwap|$VWAP_URL"
    "/v1/twap|$TWAP_URL"
    "/v1/lending/pools|$LENDING_URL"
)
# Set by force_redis_misconf once the snapshot dir's mode is known, so
# the EXIT trap only restores a mode it actually changed.
REDIS_DIR=""
REDIS_DIR_MODE=""
PROBE_BODY="$(mktemp)"

chaos_setup

# ─── cleanup: always heal Redis on exit ────────────────────────────
cleanup() {
    rm -f "$PROBE_BODY"
    if container_exists "$REDIS_CONTAINER"; then
        log "cleanup: restoring Redis writeable state"
        # Best-effort: runs from the EXIT trap and must not abort (it would mask
        # the scenario's exit code), but a failure leaves Redis in MISCONF for later
        # scenarios, so warn per step instead of swallowing it.
        local heal_failed=0
        if [ -n "$REDIS_DIR_MODE" ]; then
            docker exec "$REDIS_CONTAINER" chmod "$REDIS_DIR_MODE" "$REDIS_DIR" >/dev/null 2>&1 \
                || { warn "cleanup: failed to restore mode $REDIS_DIR_MODE on $REDIS_DIR"; heal_failed=1; }
        fi
        docker exec "$REDIS_CONTAINER" redis-cli BGSAVE >/dev/null 2>&1 \
            || { warn "cleanup: BGSAVE failed while restoring Redis"; heal_failed=1; }
        redis_config_set stop-writes-on-bgsave-error no \
            || { warn "cleanup: failed to clear stop-writes-on-bgsave-error"; heal_failed=1; }
        if [ "$heal_failed" -eq 1 ]; then
            warn "cleanup: Redis may still be in MISCONF / read-only state — verify manually: docker exec $REDIS_CONTAINER redis-cli INFO persistence"
        fi
    fi
}
trap cleanup EXIT

# ─── helpers ───────────────────────────────────────────────────────

# probe METHOD URL [DATA] — one request; sets PROBE_STATUS and
# PROBE_RETRY_AFTER and leaves the body in $PROBE_BODY. curl already
# prints "000" on a connect/timeout failure, so its exit is swallowed.
probe() {
    local method="$1" url="$2" data="${3:-}" out
    local args=(--silent --max-time 5 --output "$PROBE_BODY"
                --write-out '%{http_code} %header{retry-after}' -X "$method")
    if [ -n "$data" ]; then
        args+=(-H 'Content-Type: application/json' -d "$data")
    fi
    out="$(curl "${args[@]}" "$url" 2>/dev/null || true)"
    PROBE_STATUS="${out%% *}"
    PROBE_RETRY_AFTER="${out#* }"
}

# assert_throttle_unavailable NAME METHOD URL [DATA] — the fail-CLOSED
# answer: 503, a Retry-After hint, and the throttle-unavailable type.
assert_throttle_unavailable() {
    local name="$1"
    probe "$2" "$3" "${4:-}"
    if [ "$PROBE_STATUS" != "503" ]; then
        die "$name: expected 503 throttle-unavailable after the ${DWELL_TIME_SEC}s dwell window, got $PROBE_STATUS"
    fi
    if [ -z "$PROBE_RETRY_AFTER" ]; then
        die "$name: 503 without a Retry-After header"
    fi
    if ! grep -q 'errors/throttle-unavailable' "$PROBE_BODY"; then
        die "$name: 503 without the errors/throttle-unavailable problem type: $(head -c 300 "$PROBE_BODY")"
    fi
    log "$name: ✓ 503 errors/throttle-unavailable + Retry-After:$PROBE_RETRY_AFTER"
}

# redis_config_set NAME VALUE — returns non-zero with Redis's reply on
# stderr unless it answers OK. redis-cli's exit status on an error reply
# is not dependable across versions, so a refused config is read from
# the reply itself.
redis_config_set() {
    local out
    out="$(docker exec "$REDIS_CONTAINER" redis-cli CONFIG SET "$1" "$2" 2>&1)" || true
    if [ "$out" != "OK" ]; then
        printf 'redis-cli CONFIG SET %s %s: %s\n' "$1" "$2" "$out" >&2
        return 1
    fi
}

# wait_for_bgsave_status ok|err — re-issues BGSAVE each tick (one may
# already be in flight and refuse the first) until rdb_last_bgsave_status
# matches or 10s pass.
wait_for_bgsave_status() {
    local want="$1"
    local deadline info
    deadline="$(($(date -u +%s) + 10))"
    while [ "$(date -u +%s)" -lt "$deadline" ]; do
        docker exec "$REDIS_CONTAINER" redis-cli BGSAVE >/dev/null 2>&1 || true
        sleep 1
        info="$(docker exec "$REDIS_CONTAINER" redis-cli INFO persistence 2>&1)" || true
        if grep -q "rdb_last_bgsave_status:$want" <<<"$info"; then
            return 0
        fi
    done
    return 1
}

# Force Redis into MISCONF. After this returns, every Redis write
# will fail with the MISCONF prefix until heal_redis_misconf runs.
force_redis_misconf() {
    local dir
    dir="$(docker exec "$REDIS_CONTAINER" redis-cli CONFIG GET dir | tail -1)"
    [ -n "$dir" ] || die "could not read Redis snapshot dir (CONFIG GET dir)"
    REDIS_DIR_MODE="$(docker exec "$REDIS_CONTAINER" stat -c %a "$dir")" \
        || die "could not stat Redis snapshot dir $dir"
    REDIS_DIR="$dir"
    log "forcing Redis MISCONF: chmod 000 $REDIS_DIR (was $REDIS_DIR_MODE) + BGSAVE"
    # Pin explicitly (dev compose already has it) so the scenario is portable.
    redis_config_set stop-writes-on-bgsave-error yes \
        || die "could not enable stop-writes-on-bgsave-error"
    docker exec "$REDIS_CONTAINER" chmod 000 "$REDIS_DIR" \
        || die "could not chmod 000 $REDIS_DIR in $REDIS_CONTAINER"
    # BGSAVE is async; writes block only after the background save fails.
    if wait_for_bgsave_status err; then
        log "MISCONF active (rdb_last_bgsave_status:err)"
        return 0
    fi
    die "Redis did not enter MISCONF within 10s"
}

heal_redis_misconf() {
    log "healing Redis: chmod $REDIS_DIR_MODE $REDIS_DIR + BGSAVE + stop-writes=no"
    docker exec "$REDIS_CONTAINER" chmod "$REDIS_DIR_MODE" "$REDIS_DIR" \
        || die "could not restore mode $REDIS_DIR_MODE on $REDIS_DIR"
    # INFO persistence is authoritative for whether the heal completed.
    if wait_for_bgsave_status ok; then
        log "Redis writes restored (rdb_last_bgsave_status:ok)"
    else
        warn "BGSAVE didn't report ok within 10s; clearing stop-writes anyway"
    fi
    redis_config_set stop-writes-on-bgsave-error no \
        || die "could not clear stop-writes-on-bgsave-error"
}

# ─── 1. baseline ───────────────────────────────────────────────────
# Each GET route's pre-MISCONF status is its nominal answer on this
# stack (an empty dev stack may legitimately 404 a pair with no trades).
assert_status "$HEALTH_URL" "200"
BASELINE=()
for i in "${!GET_ROUTES[@]}"; do
    name="${GET_ROUTES[$i]%%|*}"
    probe GET "${GET_ROUTES[$i]#*|}"
    case "$PROBE_STATUS" in
        000|5??) die "$name: baseline returned $PROBE_STATUS before any fault was injected" ;;
    esac
    BASELINE[i]="$PROBE_STATUS"
    log "$name: baseline $PROBE_STATUS"
done

# ─── 2. force MISCONF ──────────────────────────────────────────────
force_redis_misconf

# ─── 3. inside the dwell window: fail OPEN ─────────────────────────
# The first failed take below arms the limiter's dwell clock.
dwell_armed_at="$(date -u +%s)"
log "phase (a): limiter must fail OPEN for ${DWELL_TIME_SEC}s from the first refused take"
for i in "${!GET_ROUTES[@]}"; do
    name="${GET_ROUTES[$i]%%|*}"
    probe GET "${GET_ROUTES[$i]#*|}"
    if [ "$(($(date -u +%s) - dwell_armed_at))" -ge "$DWELL_TIME_SEC" ]; then
        die "$name: phase (a) overran the ${DWELL_TIME_SEC}s dwell window; its answer ($PROBE_STATUS) proves nothing"
    fi
    if [ "$PROBE_STATUS" != "${BASELINE[i]}" ]; then
        die "$name: expected baseline ${BASELINE[i]} inside the dwell window (fail-OPEN), got $PROBE_STATUS"
    fi
    log "$name: ✓ $PROBE_STATUS (fail-OPEN inside the dwell window)"
done

# ─── 4. past the dwell window: fail CLOSED ─────────────────────────
remaining="$((dwell_armed_at + DWELL_TIME_SEC + 2 - $(date -u +%s)))"
if [ "$remaining" -gt 0 ]; then
    log "sleeping ${remaining}s for the dwell window to elapse"
    sleep "$remaining"
fi
log "phase (b): every rate-limited route must fail CLOSED"
for route in "${GET_ROUTES[@]}"; do
    assert_throttle_unavailable "${route%%|*}" GET "${route#*|}"
done
# The limiter rejects before the handler runs, so this creates no signup.
assert_throttle_unavailable "POST /v1/signup" POST "$SIGNUP_URL" '{"email":"chaos-04@example.test"}'

# ─── 5. heal + verify nominal ──────────────────────────────────────
# The first successful take lets requests through; the dwell clock disarms only
# after a full window of unbroken successes.
heal_redis_misconf
for i in "${!GET_ROUTES[@]}"; do
    name="${GET_ROUTES[$i]%%|*}"
    url="${GET_ROUTES[$i]#*|}"
    if ! wait_for_status "$url" "${BASELINE[i]}" 30; then
        die "$name: did not return to baseline ${BASELINE[i]} within 30s of the Redis heal (last: $(http_status "$url" 5))"
    fi
    log "$name: ✓ back to baseline ${BASELINE[i]}"
done
assert_recovers_within "$HEALTH_URL" "200" 30

chaos_teardown_pass "MISCONF: limiter fail-OPEN inside the ${DWELL_TIME_SEC}s dwell window, 503 throttle-unavailable after it (signup included), recovered within 30s of heal"
