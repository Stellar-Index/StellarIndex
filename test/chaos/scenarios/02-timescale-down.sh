#!/usr/bin/env bash
# Scenario 02 — Timescale is killed; assert the API fails LOUDLY
# (no silent stale data) and recovers cleanly when storage returns.
#
# Storage is the source-of-truth — when it's gone, /v1/price MUST
# either:
#   a) Serve a still-fresh cached value (Redis hit + recent write)
#      with no claim of authority, OR
#   b) Return 5xx with a structured envelope.
#
# A silent fall-through to "0.000" or empty data would be the
# nightmare scenario. This scenario verifies that doesn't happen.
#
# Pass criteria:
#   1. While Timescale is down, /v1/healthz returns 200 OR 503 (the
#      latter is correct when readyz checks DB connectivity).
#   2. While Timescale is down, /v1/markets returns either a 200
#      (Redis cache hit) or 5xx — NOT 200 with empty data.
#   3. After Timescale restart, /v1/healthz returns 200 within 60s.
#
# Runbook: docs/operations/runbooks/timescale-primary-down.md
# (covers production HA case; this scenario verifies the dev stack's
# behaviour without HA — fail-loud is the contract).

set -euo pipefail

export SCENARIO="02-timescale-down"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/chaos/scenarios/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

TIMESCALE_CONTAINER="${TIMESCALE_CONTAINER:-stellarindex-timescale}"
HEALTH_URL="$CHAOS_TARGET/v1/healthz"

chaos_setup

cleanup() {
    if container_exists "$TIMESCALE_CONTAINER"; then
        log "cleanup: ensuring $TIMESCALE_CONTAINER is up"
        docker start "$TIMESCALE_CONTAINER" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# 1. Baseline.
assert_status "$HEALTH_URL" "200"

# 2. Stop Timescale.
stop_container "$TIMESCALE_CONTAINER"

# 3. Wait for the API's connection-pool conn-max-idle to expire so
#    the next request actually hits the dead DB. Conservative window.
log "waiting 8s for API conn pool idle expiry"
sleep 8

# 4. While Timescale is down, healthz should be 200 (process alive)
#    and readyz should return 503. Markets endpoint should not return
#    a fake-empty payload.
got_health="$(http_status "$HEALTH_URL")"
case "$got_health" in
    200|503)
        log "API healthz returned $got_health while Timescale is down (acceptable)"
        ;;
    *)
        die "API healthz returned $got_health while Timescale is down (expected 200 or documented 503)"
        ;;
esac

# Hit a path that we KNOW reaches the DB. /v1/markets (per
# internal/api/v1/markets.go) does a DistinctPairs query.
markets_status="$(http_status "$CHAOS_TARGET/v1/markets")"
case "$markets_status" in
    5*)
        log "API /v1/markets correctly 5xx while Timescale is down ($markets_status)"
        ;;
    200)
        log "API /v1/markets returned 200 — verifying it's not a fake-empty payload"
        body="$(curl -fsS --max-time 5 "$CHAOS_TARGET/v1/markets" || true)"
        if echo "$body" | grep -qE '"data":\s*\[\]' ; then
            die "API /v1/markets returned 200 with empty data while DB is down — should be 5xx"
        fi
        log "non-empty body — Redis-fed cache path looks healthy"
        ;;
    *)
        die "API /v1/markets returned $markets_status while Timescale is down (unexpected)"
        ;;
esac

# 5. Restart Timescale.
start_container "$TIMESCALE_CONTAINER"

# 6. Verify recovery (longer deadline — Postgres takes ~10-30s to
#    accept connections after start, especially with the migration
#    extension load).
assert_recovers_within "$HEALTH_URL" "200" 60

chaos_teardown_pass "API failed loudly while DB was down; recovered within 60s of restart"
