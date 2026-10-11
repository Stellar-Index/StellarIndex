#!/usr/bin/env bash
# Shared helpers for chaos scenarios, sourced by every scenarios/NN-*.sh:
# log/warn/die, assert_*, http_status, wait_for_status, chaos_target_check
# (refuses production-looking targets) and report_* (rows in reports/).

set -euo pipefail

# ─── Config / env ──────────────────────────────────────────────────

# API base URL the scenario probes. Defaults to the local dev compose stack,
# the only safe surface; port 3000 is the API's listen_addr default.
CHAOS_TARGET="${CHAOS_TARGET:-http://localhost:3000}"

# Shared run report; the default path is deterministic so CI can `cat` it.
CHAOS_REPORT_DIR="${CHAOS_REPORT_DIR:-test/chaos/reports}"
CHAOS_REPORT_FILE="${CHAOS_REPORT_FILE:-${CHAOS_REPORT_DIR}/chaos-run-$(date -u +%Y%m%d-%H%M%S).md}"

# Compose project name used by `make dev` (`name:` in deploy/docker-compose/dev.yaml).
CHAOS_DOCKER_PROJECT="${CHAOS_DOCKER_PROJECT:-stellarindex-dev}"

# Scenario name; each script sets this before sourcing common.sh
SCENARIO="${SCENARIO:-unknown}"

# ─── Production-safety guard ───────────────────────────────────────

chaos_target_check() {
    case "$CHAOS_TARGET" in
        *production*|*api.stellarindex.io*|*prod.*)
            die "CHAOS_TARGET=$CHAOS_TARGET looks like production. Refusing to run."
            ;;
    esac
    log "target ok: $CHAOS_TARGET"
}

# ─── Output ────────────────────────────────────────────────────────

log()  { printf '[%s] [%s] %s\n' "$(date -u +%H:%M:%S)" "$SCENARIO" "$*"; }
warn() { printf '[%s] [%s] WARN %s\n' "$(date -u +%H:%M:%S)" "$SCENARIO" "$*" >&2; }
die()  {
    printf '[%s] [%s] FAIL %s\n' "$(date -u +%H:%M:%S)" "$SCENARIO" "$*" >&2
    report_failure "$*"
    exit 1
}

# ─── Reporting ─────────────────────────────────────────────────────

report_init() {
    mkdir -p "$(dirname "$CHAOS_REPORT_FILE")"
    if [ ! -f "$CHAOS_REPORT_FILE" ]; then
        {
            echo "# Chaos run — $(date -u +'%Y-%m-%d %H:%M:%SZ')"
            echo ""
            echo "Target: \`${CHAOS_TARGET}\`"
            echo ""
            echo "| Scenario | Outcome | Duration | Notes |"
            echo "|---|---|---|---|"
        } > "$CHAOS_REPORT_FILE"
    fi
}

report_pass() {
    local notes="${1:-}"
    local elapsed="${SCENARIO_ELAPSED:-?}"
    printf '| %s | ✅ pass | %ss | %s |\n' "$SCENARIO" "$elapsed" "$notes" >> "$CHAOS_REPORT_FILE"
}

report_failure() {
    local notes="${1:-}"
    local elapsed="${SCENARIO_ELAPSED:-?}"
    printf '| %s | ❌ fail | %ss | %s |\n' "$SCENARIO" "$elapsed" "$notes" >> "$CHAOS_REPORT_FILE"
}

# ─── Time helpers ──────────────────────────────────────────────────

scenario_start() {
    SCENARIO_START="$(date -u +%s)"
}

scenario_end() {
    local now
    now="$(date -u +%s)"
    SCENARIO_ELAPSED="$((now - SCENARIO_START))"
}

# ─── HTTP helpers ──────────────────────────────────────────────────

# http_status URL [TIMEOUT_SEC] → 3-digit HTTP status; 000 on connect/timeout.
# curl already prints "000" on failure, so `|| echo "000"` would double it to
# "000000" and break every comparison; `|| true` swallows the exit instead.
http_status() {
    local url="$1"
    local timeout="${2:-3}"
    curl --silent --output /dev/null --write-out '%{http_code}' \
         --max-time "$timeout" "$url" || true
}

# wait_for_status URL EXPECT_CODE [DEADLINE_SEC] [POLL_SEC]
# Polls until URL returns EXPECT_CODE; 0 on match, 1 on timeout.
wait_for_status() {
    local url="$1"
    local expect="$2"
    local deadline="${3:-30}"
    local poll="${4:-1}"
    local end
    end="$(($(date -u +%s) + deadline))"
    while [ "$(date -u +%s)" -lt "$end" ]; do
        local got
        got="$(http_status "$url" 2)"
        if [ "$got" = "$expect" ]; then
            return 0
        fi
        sleep "$poll"
    done
    return 1
}

# ─── Asserts ───────────────────────────────────────────────────────

assert_status() {
    local url="$1"
    local expect="$2"
    local got
    got="$(http_status "$url")"
    if [ "$got" != "$expect" ]; then
        die "GET $url returned $got, expected $expect"
    fi
}

assert_recovers_within() {
    local url="$1"
    local expect="$2"
    local deadline="$3"
    if ! wait_for_status "$url" "$expect" "$deadline"; then
        die "GET $url did not return $expect within ${deadline}s"
    fi
    log "recovery: $url → $expect within ${deadline}s ✓"
}

# ─── Docker helpers ────────────────────────────────────────────────

docker_running() {
    docker info >/dev/null 2>&1 || die "docker is not running; chaos suite needs the dev stack"
}

container_exists() {
    local names
    names=$(docker ps --filter "name=$1" --format '{{.Names}}')
    grep -q "^$1$" <<<"$names"
}

stop_container() {
    local name="$1"
    if container_exists "$name"; then
        log "stopping container $name"
        docker stop "$name" >/dev/null
    else
        warn "container $name not running; skipping stop"
    fi
}

start_container() {
    local name="$1"
    log "starting container $name"
    docker start "$name" >/dev/null
}

# Block all traffic to the container via pumba; falls back to
# docker network disconnect when pumba is absent.
partition_container() {
    local name="$1"
    if command -v pumba >/dev/null 2>&1; then
        log "pumba: --duration 60s pause $name"
        # Detached; caller heals via heal_partition.
        pumba pause --duration 60s "$name" >/dev/null 2>&1 &
        echo $! > "/tmp/chaos-pumba-$name.pid"
        return
    fi
    log "pumba not present; using docker network disconnect"
    local nets
    nets="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$name")"
    for net in $nets; do
        docker network disconnect "$net" "$name" >/dev/null
        echo "$net" >> "/tmp/chaos-disconnected-$name.nets"
    done
}

heal_partition() {
    local name="$1"
    if [ -f "/tmp/chaos-pumba-$name.pid" ]; then
        local pid
        pid="$(cat "/tmp/chaos-pumba-$name.pid")"
        # Non-zero kill just means pumba's --duration already expired.
        kill "$pid" 2>/dev/null || true
        rm -f "/tmp/chaos-pumba-$name.pid"
        log "pumba pause cleared for $name"
        return
    fi
    if [ -f "/tmp/chaos-disconnected-$name.nets" ]; then
        # Unlike the pumba kill, a failed reconnect is NOT benign: the container
        # stays cut off for every later scenario. Track failures; claim
        # "reconnected" only if every network came back.
        local reconnect_failed=0
        while read -r net; do
            if ! docker network connect "$net" "$name" >/dev/null 2>&1; then
                warn "failed to reconnect $name to network $net — container may still be partitioned; reconnect manually: docker network connect $net $name"
                reconnect_failed=1
            fi
        done < "/tmp/chaos-disconnected-$name.nets"
        rm -f "/tmp/chaos-disconnected-$name.nets"
        if [ "$reconnect_failed" -eq 0 ]; then
            log "network reconnected for $name"
        else
            warn "network reconnect for $name completed with failures — see warnings above"
        fi
    fi
}

# ─── Setup hooks per scenario ──────────────────────────────────────

# Standard scenario prologue; scenarios may override CHAOS_TARGET / SCENARIO before sourcing.
chaos_setup() {
    chaos_target_check
    docker_running
    report_init
    scenario_start
    log "prologue complete"
}

chaos_teardown_pass() {
    scenario_end
    report_pass "$@"
    log "PASS in ${SCENARIO_ELAPSED}s"
}
