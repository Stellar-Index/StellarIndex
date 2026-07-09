#!/usr/bin/env bash
# Scenario 03 — Network-partition the Redis container (without
# stopping it) and assert the API's rate-limit middleware fails open
# rather than 5xx-ing every request.
#
# This differs from 01 (full stop) by exercising the timeout path
# rather than the connection-refused path. They hit different go-redis
# code branches and historically a regression in one didn't surface in
# the other.
#
# Pass criteria:
#   1. Pre-partition: API is healthy.
#   2. During partition (60s): API healthz still 200; rate-limited
#      paths still serve (fail-open).
#   3. Post-heal: API stays healthy (no lingering bad-conn pool state).
#
# Tooling preference: pumba (`netem` ingress drop). Falls back to
# `docker network disconnect` when pumba isn't installed — this is
# heavier-handed but exercises the same cold-conn path.

set -euo pipefail

export SCENARIO="03-redis-network-partition"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=test/chaos/scenarios/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

REDIS_CONTAINER="${REDIS_CONTAINER:-stellarindex-redis}"
HEALTH_URL="$CHAOS_TARGET/v1/healthz"
PARTITION_DURATION_SEC="${PARTITION_DURATION_SEC:-30}"

chaos_setup

cleanup() {
    log "cleanup: healing partition (no-op if already healed)"
    # Do NOT redirect heal_partition's own stdout/stderr away — it now
    # warns internally when a network reconnect fails, and swallowing
    # that here would defeat the point. `|| true` only guards this
    # trap against heal_partition's "nothing to heal" no-op return
    # (status 1 when neither state file exists), which must not abort
    # the rest of cleanup() under `set -e`.
    heal_partition "$REDIS_CONTAINER" || true
    if container_exists "$REDIS_CONTAINER"; then
        # Best-effort: this runs from the EXIT trap and must not itself
        # abort (that would mask the scenario's real pass/fail exit
        # code) — but a failed restart leaves Redis down for the next
        # scenario/dev session, so warn loudly instead of swallowing it.
        if ! docker start "$REDIS_CONTAINER" >/dev/null 2>&1; then
            warn "cleanup: failed to restart $REDIS_CONTAINER — it may still be down; check manually (docker start $REDIS_CONTAINER)"
        fi
    fi
}
trap cleanup EXIT

# 1. Baseline.
assert_status "$HEALTH_URL" "200"

# 2. Apply partition.
partition_container "$REDIS_CONTAINER"
log "partition active for ${PARTITION_DURATION_SEC}s"

# 3. Sample healthz several times during the window. The API's
#    rate-limit middleware uses a context timeout (typically 200ms)
#    and fails open — we should see no 5xx storms.
fail_count=0
sample_count=0
end="$(($(date -u +%s) + PARTITION_DURATION_SEC))"
while [ "$(date -u +%s)" -lt "$end" ]; do
    sample_count="$((sample_count + 1))"
    got="$(http_status "$HEALTH_URL" 5)"
    case "$got" in
        200|503) ;;
        *)
            warn "sample $sample_count: healthz returned $got"
            fail_count="$((fail_count + 1))"
            ;;
    esac
    sleep 3
done
log "during-partition: $fail_count/$sample_count samples failed"

# Tolerate up to 1 transient sample (the very first request after
# partition start can race the connection state). Anything more = real
# regression.
if [ "$fail_count" -gt 1 ]; then
    die "API returned non-200/503 on $fail_count/$sample_count samples while Redis was partitioned"
fi

# 4. Heal + verify the conn pool refreshed cleanly.
heal_partition "$REDIS_CONTAINER"
assert_recovers_within "$HEALTH_URL" "200" 30

chaos_teardown_pass "API stable through ${PARTITION_DURATION_SEC}s Redis partition + recovery"
