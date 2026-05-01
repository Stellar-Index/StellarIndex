#!/usr/bin/env bash
# Shared helpers for chaos scenarios.
#
# Sourced by every scenarios/NN-*.sh. Provides:
#   - log + warn + die output helpers (timestamped, scenario-tagged)
#   - assert_*           — pass/fail predicates that bail with a
#                          named failure
#   - http_status        — fetch HTTP status code with timeout
#   - wait_for_status    — poll a URL until status code matches
#                          (or timeout)
#   - chaos_target_check — refuse to run against any target whose
#                          host looks production
#   - report_* helpers   — append structured rows to the run's
#                          markdown report under reports/

set -euo pipefail

# ─── Config / env ──────────────────────────────────────────────────

# CHAOS_TARGET is the API base URL the scenario hits to verify
# behaviour. Defaults to the local docker-compose dev stack — the
# only safe surface for chaos by default.
CHAOS_TARGET="${CHAOS_TARGET:-http://localhost:8080}"

# CHAOS_REPORT_DIR is where each scenario appends its row to a
# shared run report.  Defaulted to a deterministic path so a CI run
# can `cat` it after the scenario(s) finish.
CHAOS_REPORT_DIR="${CHAOS_REPORT_DIR:-test/chaos/reports}"
CHAOS_REPORT_FILE="${CHAOS_REPORT_FILE:-${CHAOS_REPORT_DIR}/chaos-run-$(date -u +%Y%m%d-%H%M%S).md}"

# CHAOS_DOCKER_PROJECT is the docker-compose project name used by
# `make dev`. Matches `name:` in deploy/docker-compose/dev.yaml.
CHAOS_DOCKER_PROJECT="${CHAOS_DOCKER_PROJECT:-ratesengine-dev}"

# Scenario name; each script sets this before sourcing common.sh
SCENARIO="${SCENARIO:-unknown}"

# ─── Production-safety guard ───────────────────────────────────────

chaos_target_check() {
    case "$CHAOS_TARGET" in
        *production*|*api.ratesengine.net*|*prod.*)
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

# http_status URL [TIMEOUT_SEC] → echoes 3-digit HTTP status. 000 on
# connect/timeout failure (curl's convention).
http_status() {
    local url="$1"
    local timeout="${2:-3}"
    curl --silent --output /dev/null --write-out '%{http_code}' \
         --max-time "$timeout" "$url" || echo "000"
}

# wait_for_status URL EXPECT_CODE [DEADLINE_SEC] [POLL_SEC]
#
# Polls until the URL returns EXPECT_CODE or the deadline expires.
# Returns 0 on match, 1 on timeout. Useful for "API is back up" or
# "API is now down" assertions.
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
    docker ps --filter "name=$1" --format '{{.Names}}' | grep -q "^$1$"
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

# Block ALL traffic to the named container by attaching a netem chaos
# sidecar via pumba. Falls back to docker network disconnect when
# pumba isn't available.
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
        kill "$pid" 2>/dev/null || true
        rm -f "/tmp/chaos-pumba-$name.pid"
        log "pumba pause cleared for $name"
        return
    fi
    if [ -f "/tmp/chaos-disconnected-$name.nets" ]; then
        while read -r net; do
            docker network connect "$net" "$name" >/dev/null 2>&1 || true
        done < "/tmp/chaos-disconnected-$name.nets"
        rm -f "/tmp/chaos-disconnected-$name.nets"
        log "network reconnected for $name"
    fi
}

# ─── Setup hooks per scenario ──────────────────────────────────────

# Standard prologue scenarios run; each scenario can override
# CHAOS_TARGET / SCENARIO before sourcing this file.
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
