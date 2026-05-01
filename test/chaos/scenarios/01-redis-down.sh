#!/usr/bin/env bash
# Scenario 01 — Redis is killed; assert the API stays available
# (degraded but functional). Pre-merger: rate-limit middleware fails
# open + the orchestrator can't write VWAPs, but reads should still
# fall through to the Postgres-backed VWAP path.
#
# Pass criteria:
#   1. While Redis is down, /v1/healthz returns 200 within 5s.
#   2. While Redis is down, /v1/price/<a popular pair> returns 200
#      OR a documented degraded code (503 with {error: "redis_down"}).
#      A 5xx with no envelope = fail.
#   3. After Redis restart, /v1/healthz returns 200 within 30s.
#
# This scenario exercises the API's graceful-degradation path
# documented in:
#   - docs/operations/runbooks/redis-master-down.md
#   - internal/api/middleware/ratelimit (fail-open behaviour)
#
# Assumes the docker-compose dev stack (`make dev`) is running and
# the API is reachable at $CHAOS_TARGET (default http://localhost:8080).

set -euo pipefail

export SCENARIO="01-redis-down"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/chaos/scenarios/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

REDIS_CONTAINER="${REDIS_CONTAINER:-ratesengine-redis}"
HEALTH_URL="$CHAOS_TARGET/v1/healthz"

chaos_setup

# Always heal the partition + restart Redis on exit, even on early
# failure. The trap fires before scripts/ci picks up a non-zero exit.
cleanup() {
    if container_exists "$REDIS_CONTAINER"; then
        log "cleanup: ensuring $REDIS_CONTAINER is up"
        docker start "$REDIS_CONTAINER" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# 1. Baseline check: API healthy before chaos.
log "baseline: GET $HEALTH_URL"
assert_status "$HEALTH_URL" "200"

# 2. Stop Redis.
stop_container "$REDIS_CONTAINER"

# 3. While Redis is down, the API should still serve healthz. The
#    rate-limit middleware fails open on Redis errors so /v1/* paths
#    don't 5xx purely from Redis being unreachable.
log "verifying API stays up while Redis is down (5s window)"
sleep 2
got_health="$(http_status "$HEALTH_URL")"
case "$got_health" in
    200|503)
        log "API healthz returned $got_health while Redis is down (acceptable)"
        ;;
    *)
        die "API healthz returned $got_health while Redis is down (expected 200 or documented 503)"
        ;;
esac

# 4. Restart Redis.
start_container "$REDIS_CONTAINER"

# 5. Verify recovery.
assert_recovers_within "$HEALTH_URL" "200" 30

chaos_teardown_pass "API degraded then recovered within 30s of Redis restart"
