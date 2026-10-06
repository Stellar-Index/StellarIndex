#!/usr/bin/env bash
#
# pre-launch-check.sh — verify R1 is in production-ready shape
# before the public DNS cutover at api.stellarindex.io.
#
# Read-only — performs zero state changes. Each check reports
# pass / warn / fail; exit code is the number of FAIL findings
# (warns don't gate). Designed to be safe to run on any R1
# whether or not it's already serving public traffic.
#
# Run on R1 itself:
#   ssh root@r1 'bash -s' < scripts/ops/pre-launch-check.sh
# Or interactively:
#   ssh root@r1 'bash /opt/stellarindex/pre-launch-check.sh'
#
# Companion to docs/operations/pre-launch-hardening.md — each
# check maps to a numbered step in that runbook.

set -uo pipefail

# ANSI colour helpers — disabled when stdout isn't a TTY.
if [ -t 1 ]; then
  GREEN="$(printf '\033[32m')"; YELLOW="$(printf '\033[33m')"; RED="$(printf '\033[31m')"; DIM="$(printf '\033[2m')"; OFF="$(printf '\033[0m')"
else
  GREEN=""; YELLOW=""; RED=""; DIM=""; OFF=""
fi

FAILS=0
WARNS=0

pass() {
  printf "  %sok%s   %-44s %s%s%s\n" "$GREEN" "$OFF" "$1" "$DIM" "$2" "$OFF"
}
warn() {
  printf "  %sWARN%s %-44s %s%s%s\n" "$YELLOW" "$OFF" "$1" "$DIM" "$2" "$OFF"
  WARNS=$((WARNS + 1))
}
fail() {
  printf "  %sFAIL%s %-44s %s%s%s\n" "$RED" "$OFF" "$1" "$DIM" "$2" "$OFF"
  FAILS=$((FAILS + 1))
}

CONFIG="${STELLARINDEX_TOML:-/etc/stellarindex.toml}"
HC_ENV="${HEALTHCHECKS_ENV_FILE:-/etc/default/stellarindex-healthchecks}"
AM_ENV="${ALERTMANAGER_ENV_FILE:-/etc/default/alertmanager-secrets}"

echo "Pre-launch check — R1 $(hostname) — $(date -u +%FT%TZ)"
echo "Config: ${CONFIG}"
echo

# ── 1. API binds to loopback (or has trusted proxy CIDRs)
echo "  Network exposure"
if [ ! -f "$CONFIG" ]; then
  fail "config file present" "$CONFIG missing"
else
  # sigpipe-ok: $CONFIG is a TOML file of a few KiB and the grep matches at
  # most a handful of lines — well under the pipe buffer (#475).
  listen_addr="$(grep -E '^\s*listen_addr\s*=' "$CONFIG" | head -1 | sed -E 's/.*=\s*"([^"]+)".*/\1/' || true)"
  if [ -z "$listen_addr" ]; then
    listen_addr="0.0.0.0:3000  (default)"
  fi
  case "$listen_addr" in
    127.0.0.1:*|localhost:*|"::1:"*)
      pass "listen_addr is loopback" "$listen_addr"
      ;;
    *)
      # sigpipe-ok: $CONFIG is a TOML file of a few KiB and the grep matches at
      # most a handful of lines — well under the pipe buffer (#475).
      proxy_cidrs="$(grep -E '^\s*trusted_proxy_cidrs\s*=' "$CONFIG" | head -1 || true)"
      if [ -z "$proxy_cidrs" ] || grep -q '\[\s*\]' <<<"$proxy_cidrs"; then
        fail "listen_addr public + no trusted proxies" "$listen_addr"
      else
        warn "listen_addr public" "$listen_addr (proxy CIDRs configured)"
      fi
      ;;
  esac

  # Verify the running process matches.
  # sigpipe-ok: `ss -tlnp` lists listening sockets only — tens of lines,
  # orders of magnitude under the pipe buffer, so awk's early exit cannot
  # make ss block on a write (#475).
  # #1097: TASK_COMM_LEN is 16 bytes including the NUL, so the kernel
  # truncates "stellarindex-api" (16 chars) to "stellarindex-ap" in
  # /proc/<pid>/comm — which is what `ss -p` reads. The full name never
  # matched, so this check was unreachable: $actual was always empty and
  # step 1's only comparator against the RUNNING process was silently
  # skipped, with no output. Matching the truncated form the kernel
  # actually reports, and now run as root per this script's own header.
  actual="$(ss -tlnp 2>/dev/null | awk '/stellarindex-ap/ {print $4; exit}')"
  if [ -n "$actual" ]; then
    case "$actual" in
      127.0.0.1:*|"[::1]:"*)  pass "process bound to loopback" "$actual" ;;
      "*:"*|"0.0.0.0:"*|"[::]:"*)
        case "$listen_addr" in
          127.0.0.1:*|localhost:*|"::1:"*)
            warn "process bind != config" "$actual (config says $listen_addr; restart needed?)" ;;
          *)
            warn "process bound to all interfaces" "$actual" ;;
        esac
        ;;
      *)  pass "process bound" "$actual" ;;
    esac
  else
    fail "process bind check" "no listening stellarindex-api socket found via ss -tlnp — is the service running?"
  fi
fi
echo

# ── 2. CORS narrowed
echo "  CORS"
# sigpipe-ok: $CONFIG is a TOML file of a few KiB and the grep matches at
# most a handful of lines — well under the pipe buffer (#475).
allowed_origins="$(grep -E '^\s*allowed_origins\s*=' "$CONFIG" 2>/dev/null | head -1 || true)"
if [ -z "$allowed_origins" ] || grep -q '"\*"' <<<"$allowed_origins"; then
  fail "allowed_origins is wide open" '["*"] — narrow to your showcase + API hostnames'
else
  pass "allowed_origins narrowed" "${allowed_origins#"${allowed_origins%%[![:space:]]*}"}"
fi
echo

# ── 3. Healthchecks.io URLs
echo "  Healthchecks.io"
if [ ! -f "$HC_ENV" ]; then
  fail "HC env file missing" "$HC_ENV"
else
  # Quote-tolerant like section 4: pre-launch-hardening.md writes these single-quoted.
  for v in HEALTHCHECKS_URL_INDEXER HEALTHCHECKS_URL_AGGREGATOR HEALTHCHECKS_URL_API HEALTHCHECKS_URL_SMOKE HEALTHCHECKS_URL_SLA_PROBE; do
    if grep -qE "^$v=('|\")?https://" "$HC_ENV" 2>/dev/null; then
      pass "$v" "set"
    else
      fail "$v" "unset — see hardening doc step 5"
    fi
  done
fi
echo

# ── 4. Alertmanager secrets
echo "  Alertmanager"
if [ ! -f "$AM_ENV" ]; then
  fail "AM env file missing" "$AM_ENV"
else
  # The optional quote is load-bearing. Env files here write these
  # values single-quoted, and `^NAME=https://` cannot match
  # `NAME='https://...'` — so on r1 this reported all three as "unset —
  # alerts won't fan out" while alertmanager was running with six
  # receivers and eight webhook URLs loaded from that very file
  # (2026-09-10). A launch gate that cries wolf about paging is worse
  # than no gate: it is read once, disbelieved, and then it is worth
  # nothing when a URL really is missing.
  # Still warns on an empty value or a non-URL — the quote is tolerated,
  # the https:// requirement is not relaxed.
  for v in HEALTHCHECKS_DEADMANSSWITCH_URL DISCORD_WEBHOOK_URL_PAGES DISCORD_WEBHOOK_URL_ALERTS; do
    if grep -qE "^$v=('|\")?https://" "$AM_ENV" 2>/dev/null; then
      pass "$v" "set"
    else
      warn "$v" "unset — alerts won't fan out"
    fi
  done
fi
echo

# ── 5. Timers active
echo "  Timers"
for t in 'stellarindex-heartbeat@indexer.timer' \
         'stellarindex-heartbeat@aggregator.timer' \
         'stellarindex-heartbeat@api.timer' \
         'stellarindex-smoke.timer'; do
  if systemctl is-active --quiet "$t" 2>/dev/null; then
    pass "$t" "active"
  else
    fail "$t" "inactive"
  fi
done
echo

# ── 6. Core services
echo "  Services"
for s in stellarindex-indexer.service \
         stellarindex-aggregator.service \
         stellarindex-api.service \
         caddy.service \
         prometheus.service \
         prometheus-alertmanager.service; do
  if systemctl is-active --quiet "$s" 2>/dev/null; then
    pass "$s" "active"
  else
    fail "$s" "inactive"
  fi
done
echo

# ── 7. Caddy serving on :443
echo "  Caddy"
if grep -q ':443.*caddy' <<<"$(ss -tlnp 2>/dev/null)"; then
  pass "caddy listening on :443" ""
else
  fail "caddy not on :443" "TLS termination won't work"
fi
echo

# ── 8. API smoke
echo "  Smoke (loopback)"
if curl -fsS --max-time 5 http://localhost:3000/v1/healthz >/dev/null 2>&1; then
  pass "/v1/healthz" "200"
else
  fail "/v1/healthz" "loopback API not responding"
fi
if curl -fsS --max-time 5 http://localhost:3000/v1/status >/dev/null 2>&1; then
  pass "/v1/status" "200"
else
  fail "/v1/status" "not responding"
fi
echo

# ── 9. Boot warnings
echo "  Recent SECURITY warnings"
# #1097: -p warning filters on the journal's syslog PRIORITY, a property
# of the transport — systemd stamps every line from stderr at the
# default PRIORITY=6/info because no SyslogLevel is set anywhere in this
# repo's deploy config (grep -rn SyslogLevel deploy configs → 0) — not
# on the JSON payload's own "level" field the structured logger writes.
# -p warning selected nothing regardless of what was logged, so the four
# logger.Warn SECURITY: call sites in cmd/stellarindex-api/main.go were
# unreachable by this check. Grep the payload for the level instead, and
# read every boot line first so an empty journal (unit not found, wrong
# name, no boot logs yet) fails loudly rather than reading as zero
# warnings.
boot_log="$(journalctl -u stellarindex-api -b -o cat --no-pager 2>/dev/null)"
if [ -z "$boot_log" ]; then
  fail "no boot log read for stellarindex-api" "journalctl returned nothing — cannot assert on SECURITY warnings"
else
  sec_warns="$(printf '%s\n' "$boot_log" | grep -c '"level":"WARN".*SECURITY:' || true)"
  if [ "$sec_warns" -eq 0 ]; then
    pass "no SECURITY warnings since boot" ""
  else
    fail "SECURITY warnings present" "$sec_warns lines — journalctl -u stellarindex-api -b -o cat | grep SECURITY:"
  fi
fi
echo

# ── 10. USD volume pricing bars
# Valuation never gates `complete` (ADR-0033 is capture-only), so the bars
# gate go-live here. The API publishes meets_bar for external venues only;
# the firing usd-volume-coverage alerts also cover the on-chain bar.
echo "  USD volume pricing"
cov="$(curl -fsS --max-time 10 http://localhost:3000/v1/coverage 2>/dev/null)"
if [ -z "$cov" ]; then
  fail "/v1/coverage" "not responding — cannot read usd_volume_pricing"
elif ! command -v jq >/dev/null 2>&1; then
  fail "usd_volume_pricing" "jq is not installed — cannot parse /v1/coverage"
elif ! printf '%s' "$cov" | jq -e '.data | has("usd_volume_pricing")' >/dev/null 2>&1; then
  fail "usd_volume_pricing" "absent from /v1/coverage — API predates the axis"
elif printf '%s' "$cov" | jq -e '.data.usd_volume_pricing == null' >/dev/null 2>&1; then
  fail "usd_volume_pricing not computed" "no snapshot yet (null stays null while refreshes fail) — re-run after the API's first refresh"
elif ! printf '%s' "$cov" | jq -e '(.data.usd_volume_pricing.sources | type == "array") and (.data.usd_volume_pricing.sources | length > 0)' >/dev/null 2>&1; then
  fail "usd_volume_pricing.sources" "null, missing or empty — no venues to check"
else
  below="$(printf '%s' "$cov" | jq -r '.data.usd_volume_pricing.sources[]
    | select(.meets_bar == false)
    | "\(.source) priced_ratio=\(.priced_ratio // "none") bar=\(.bar) trades=\(.trades)"')"
  if [ -z "$below" ]; then
    pass "external venues meet the USD pricing bar" ""
  else
    while IFS= read -r line; do
      fail "usd_volume below bar: ${line%% *}" "${line#* }"
    done <<<"$below"
  fi
fi
PROM_URL="${PROMETHEUS_URL:-http://127.0.0.1:9090}"
firing="$(curl -fsS --max-time 10 -G "$PROM_URL/api/v1/query" \
  --data-urlencode 'query=ALERTS{component="usd-volume-coverage",alertstate="firing"}' 2>/dev/null)"
if ! printf '%s' "$firing" | jq -e '.status == "success"' >/dev/null 2>&1; then
  fail "usd-volume-coverage alerts" "cannot query Prometheus at $PROM_URL"
else
  names="$(printf '%s' "$firing" | jq -r '.data.result[].metric | "\(.alertname) \(.source // "")"')"
  if [ -z "$names" ]; then
    pass "no usd-volume-coverage alerts firing" ""
  else
    while IFS= read -r line; do
      detail=""
      [ "${line#* }" != "" ] && detail="source=${line#* }"
      fail "firing: ${line%% *}" "$detail"
    done <<<"$names"
  fi
fi
echo

# ── Summary
if [ "$FAILS" -eq 0 ] && [ "$WARNS" -eq 0 ]; then
  printf "%sAll checks passed — ready for DNS cutover.%s\n" "$GREEN" "$OFF"
elif [ "$FAILS" -eq 0 ]; then
  printf "%s%s warning(s); 0 failure(s) — review and proceed if intentional.%s\n" "$YELLOW" "$WARNS" "$OFF"
else
  printf "%s%s failure(s), %s warning(s) — fix before flipping DNS.%s\n" "$RED" "$FAILS" "$WARNS" "$OFF"
fi
echo
echo "Reference: docs/operations/pre-launch-hardening.md"

exit "$FAILS"
