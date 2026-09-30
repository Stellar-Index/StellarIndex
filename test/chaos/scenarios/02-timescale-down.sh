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
#   2. While Timescale is down, a /v1/markets variant that no cache
#      holds (?limit=97) returns 5xx — the handler cannot answer it
#      without Postgres, so a 200 there is a silent fall-through.
#   3. While Timescale is down, the default /v1/markets returns either
#      a 200 (cache hit) or 5xx — and a 200 with empty data fails
#      unless the pre-outage answer was empty too (a stack with no
#      trades legitimately caches `data: []`).
#   4. After Timescale restart, /v1/healthz returns 200 within 60s.
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
MARKETS_URL="$CHAOS_TARGET/v1/markets"
# Both markets caches (Redis list cache, in-process SWR) key on
# (cursor, limit, order) and store only successes; 97 is outside the
# prewarm set, so this request must reach Postgres.
MARKETS_UNCACHED_URL="$CHAOS_TARGET/v1/markets?limit=97"

markets_data_empty() {
    grep -qE '"data":[[:space:]]*\[\]' <<<"$1"
}

chaos_setup

cleanup() {
    if container_exists "$TIMESCALE_CONTAINER"; then
        log "cleanup: ensuring $TIMESCALE_CONTAINER is up"
        # Best-effort: this runs from the EXIT trap and must not itself
        # abort (that would mask the scenario's real pass/fail exit
        # code) — but a failed restart leaves Timescale down for the
        # next scenario/dev session, so warn loudly instead of
        # swallowing it.
        if ! docker start "$TIMESCALE_CONTAINER" >/dev/null 2>&1; then
            warn "cleanup: failed to restart $TIMESCALE_CONTAINER — it may still be down; check manually (docker start $TIMESCALE_CONTAINER)"
        fi
    fi
}
trap cleanup EXIT

# 1. Baseline, including whether /v1/markets is empty before the outage.
assert_status "$HEALTH_URL" "200"
pre_markets_body="$(curl -fsS --max-time 15 "$MARKETS_URL")" \
    || die "baseline GET $MARKETS_URL failed before the outage"
pre_markets_empty=0
if markets_data_empty "$pre_markets_body"; then
    pre_markets_empty=1
    log "baseline /v1/markets has empty data (no trades on this stack)"
fi

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
uncached_status="$(http_status "$MARKETS_UNCACHED_URL" 15)"
case "$uncached_status" in
    5*)
        log "API /v1/markets?limit=97 (uncached) correctly 5xx while Timescale is down ($uncached_status)"
        ;;
    *)
        die "API /v1/markets?limit=97 (uncached) returned $uncached_status while Timescale is down — should be 5xx"
        ;;
esac

markets_out="$(curl --silent --max-time 15 --write-out '\n%{http_code}' "$MARKETS_URL" || true)"
markets_status="${markets_out##*$'\n'}"
body="${markets_out%$'\n'*}"
case "$markets_status" in
    5*)
        log "API /v1/markets correctly 5xx while Timescale is down ($markets_status)"
        ;;
    200)
        if ! markets_data_empty "$body"; then
            log "API /v1/markets returned 200 with non-empty data — cache path looks healthy"
        elif [ "$pre_markets_empty" -eq 1 ]; then
            log "API /v1/markets returned 200 with empty data — same as the pre-outage answer (cache hit)"
        else
            die "API /v1/markets returned 200 with empty data while DB is down (non-empty before) — should be 5xx or the cached rows"
        fi
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
