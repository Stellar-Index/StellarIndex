#!/usr/bin/env bash
# Apply alertmanager.r1.yml to R1.
#
# Reads webhook URLs from /etc/default/alertmanager-secrets, injects them with a
# Python YAML walker (an empty URL leaves the receiver a no-op stub instead of
# breaking the file), validates with amtool, installs to
# /etc/prometheus/alertmanager.yml, then reloads alertmanager.
#
# Required env vars (in /etc/default/alertmanager-secrets):
#   HEALTHCHECKS_DEADMANSSWITCH_URL - e.g. https://hc-ping.com/<uuid>
#   DISCORD_WEBHOOK_URL_PAGES       - e.g. https://discord.com/api/webhooks/<id>/<token>
#   DISCORD_WEBHOOK_URL_ALERTS      - e.g. https://discord.com/api/webhooks/<id>/<token>
#
# Optional env var:
#   HEALTHCHECKS_ALERT_DELIVERY_URL - a SECOND Healthchecks check (not the
#     deadmansswitch one) carrying stellarindex_alertmanager_notifications_failing
#     out of band, so a dead Discord receiver cannot swallow the alert that
#     reports it. Optional so provisioning a check never blocks an urgent apply,
#     but never silent: every run prints whether it is wired.
#
# An empty URL makes the renderer drop that receiver's *_configs block, leaving
# a stub that delivers to nobody, like `silent`. That keeps the config valid but
# is never legitimate to INSTALL unasked, so applying with an empty URL is a
# hard error. A warning is not enough: once, a secrets file supplied none of the
# three URLs and every delivery path (page, ticket, default AND the deadman's
# switch) was a black hole for 31 days. The config reloaded successfully, so
# alertmanager_config_last_reload_successful stayed 1 and
# stellarindex_alertmanager_config_bad never fired; the reporter was the broken
# part.
#
# Deliberately running one channel must be said per receiver:
#   ALERTMANAGER_ALLOW_EMPTY=pages bash apply.sh
# Pointing both Discord URLs at the same webhook beats waiving one.

set -euo pipefail

# --check-only: render + amtool-validate, then stop before install/reload. CI
# (monitoring-rules job) uses it to validate both render branches: a malformed
# edit that broke the block-stripper's indentation assumptions once produced a
# silently receiver-less Alertmanager, found only at the next hand apply.
#
# --render-only <path>: render to <path> and stop BEFORE amtool, so
# scripts/ci/check-alertmanager-parity.sh can compare this rendering with the
# Ansible template's using THIS renderer (a parity gate on a reimplementation
# only proves the two copies agree). Runs in the `ansible` CI job, which has
# jinja2 but not amtool; --check-only remains the validating mode.
CHECK_ONLY=false
RENDER_ONLY=false
RENDER_OUT=""
case "${1:-}" in
  --check-only)
    CHECK_ONLY=true
    ;;
  --render-only)
    RENDER_ONLY=true
    CHECK_ONLY=true # suppress the install-time fail-closed guard below
    RENDER_OUT="${2:-}"
    if [ -z "$RENDER_OUT" ]; then
      echo "error: --render-only needs an output path" >&2
      exit 1
    fi
    ;;
esac

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
SOURCE="${SCRIPT_DIR}/alertmanager.r1.yml"
ENV_FILE="${ALERTMANAGER_SECRETS:-/etc/default/alertmanager-secrets}"
TARGET="${TARGET:-/etc/prometheus/alertmanager.yml}"

if [ ! -f "$SOURCE" ]; then
  echo "error: source config not found: $SOURCE" >&2
  exit 1
fi

# shellcheck disable=SC1090
[ -f "$ENV_FILE" ] && . "$ENV_FILE"

# ── fail closed on an unset delivery URL ───────────────────────────
# Keyed by the waiver name an operator would type, so the error message
# and ALERTMANAGER_ALLOW_EMPTY use the same vocabulary.
am_url_for() {
  case "$1" in
    deadmansswitch)   printf '%s' "${HEALTHCHECKS_DEADMANSSWITCH_URL:-}" ;;
    pages)            printf '%s' "${DISCORD_WEBHOOK_URL_PAGES:-}" ;;
    alerts)           printf '%s' "${DISCORD_WEBHOOK_URL_ALERTS:-}" ;;
    delivery-failure) printf '%s' "${HEALTHCHECKS_ALERT_DELIVERY_URL:-}" ;;
    informational)    printf '%s' "${DISCORD_WEBHOOK_URL_INFORMATIONAL:-}" ;;
  esac
}
am_var_for() {
  case "$1" in
    deadmansswitch)   printf 'HEALTHCHECKS_DEADMANSSWITCH_URL' ;;
    pages)            printf 'DISCORD_WEBHOOK_URL_PAGES' ;;
    alerts)           printf 'DISCORD_WEBHOOK_URL_ALERTS' ;;
    delivery-failure) printf 'HEALTHCHECKS_ALERT_DELIVERY_URL' ;;
    informational)    printf 'DISCORD_WEBHOOK_URL_INFORMATIONAL' ;;
  esac
}
AM_RECEIVERS="deadmansswitch pages alerts"
# Optional: absence is reported, not refused. Requiring delivery-failure would
# make the apply that RESTORES a broken chat channel fail on a check nobody has
# created yet, the worst moment for a new precondition.
AM_OPTIONAL_RECEIVERS="delivery-failure informational"

# --check-only is a RENDERER test: CI drives it with every URL empty to exercise
# the block-stripper. The fail-closed guard is install-time policy, so it is
# skipped in that mode.
if [ "$CHECK_ONLY" = false ]; then
  ALLOW="${ALERTMANAGER_ALLOW_EMPTY:-}"
  missing=""
  for r in $AM_RECEIVERS; do
    [ -n "$(am_url_for "$r")" ] && continue
    case ",${ALLOW//[[:space:]]/}," in
      *",$r,"*)
        echo "alertmanager: WAIVED — receiver '$r' will deliver to nobody (ALERTMANAGER_ALLOW_EMPTY)" >&2
        continue ;;
    esac
    missing="$missing $r"
  done
  if [ -n "$missing" ]; then
    {
      echo "error: refusing to install a config whose receivers deliver to nobody."
      for r in $missing; do
        echo "  receiver '$r' has no URL — set $(am_var_for "$r") in $ENV_FILE"
      done
      echo
      echo "An empty URL is not a no-op: the receiver's *_configs block is dropped and"
      echo "it becomes a black hole identical to 'silent'. This is what caused the"
      echo "2026-07-29 → 2026-08-29 alerting outage, deadman's switch included."
      echo "If a receiver is meant to be dark, name it: ALERTMANAGER_ALLOW_EMPTY=${missing// /,}"
    } >&2
    exit 1
  fi

  # Optional receivers are never refused but never silent: an out-of-band
  # transport nobody notices is missing is the same failure as one never added.
  for r in $AM_OPTIONAL_RECEIVERS; do
    if [ -n "$(am_url_for "$r")" ]; then
      echo "alertmanager: optional receiver '$r' is wired ($(am_var_for "$r"))"
    else
      echo "alertmanager: optional receiver '$r' is DARK — $(am_var_for "$r") is unset," >&2
      echo "              so a refused chat delivery has no out-of-band alarm." >&2
    fi
  done
fi

RENDERED="$(mktemp)"
trap 'rm -f "$RENDERED"' EXIT

HEALTHCHECKS_URL="${HEALTHCHECKS_DEADMANSSWITCH_URL:-}" \
HEALTHCHECKS_DELIVERY_URL="${HEALTHCHECKS_ALERT_DELIVERY_URL:-}" \
DISCORD_PAGES_URL="${DISCORD_WEBHOOK_URL_PAGES:-}" \
DISCORD_ALERTS_URL="${DISCORD_WEBHOOK_URL_ALERTS:-}" \
  DISCORD_INFORMATIONAL_URL="${DISCORD_WEBHOOK_URL_INFORMATIONAL:-}" \
SOURCE="$SOURCE" \
RENDERED="$RENDERED" \
python3 - <<'PY'
import os

src = open(os.environ["SOURCE"]).read()

subs = {
    "${HEALTHCHECKS_DEADMANSSWITCH_URL}":  os.environ.get("HEALTHCHECKS_URL", "").strip(),
    "${HEALTHCHECKS_ALERT_DELIVERY_URL}":  os.environ.get("HEALTHCHECKS_DELIVERY_URL", "").strip(),
    "${DISCORD_WEBHOOK_URL_PAGES}":        os.environ.get("DISCORD_PAGES_URL", "").strip(),
    "${DISCORD_WEBHOOK_URL_ALERTS}":       os.environ.get("DISCORD_ALERTS_URL", "").strip(),
    "${DISCORD_WEBHOOK_URL_INFORMATIONAL}": os.environ.get("DISCORD_INFORMATIONAL_URL", "").strip(),
}


def strip_configs_block_for_marker(text, marker):
    """Remove the `*_configs:` block that contains `marker`, leaving
    the bare `- name: <receiver>` as a valid no-op stub. Line-based +
    marker-specific so one empty URL never collateral-strips another
    receiver's identically-keyed block (two discord_configs blocks)."""
    lines = text.split("\n")
    idx = next((i for i, l in enumerate(lines) if marker in l), None)
    if idx is None:
        return text
    # Walk up to the `*_configs:` header line for this marker.
    hdr = idx
    while hdr >= 0 and not lines[hdr].strip().endswith("_configs:"):
        hdr -= 1
    if hdr < 0:
        return text
    header_indent = len(lines[hdr]) - len(lines[hdr].lstrip())
    # The block body is every following line indented deeper than the
    # header (blank lines included); stop at the next line indented at
    # or above the header (the next receiver / top-level key).
    end = hdr + 1
    while end < len(lines):
        stripped = lines[end].strip()
        if stripped == "":
            end += 1
            continue
        indent = len(lines[end]) - len(lines[end].lstrip())
        if indent > header_indent:
            end += 1
        else:
            break
    del lines[hdr:end]
    return "\n".join(lines)


for marker, url in subs.items():
    if url:
        src = src.replace(marker, url)
    else:
        src = strip_configs_block_for_marker(src, marker)

open(os.environ["RENDERED"], "w").write(src)
PY

if [ "$RENDER_ONLY" = true ]; then
  cat "$RENDERED" > "$RENDER_OUT"
  exit 0
fi

if ! amtool check-config "$RENDERED"; then
  echo "error: alertmanager config failed validation" >&2
  exit 1
fi

if [ "$CHECK_ONLY" = true ]; then
  echo "alertmanager: config renders + validates OK (check-only, not applied)"
  exit 0
fi

# ── prove the credentials are LIVE, not merely non-empty ──
# amtool validates syntax, not whether a webhook was revoked; a stale URL fails
# as silently as an empty one. Both probes are read-only: a Healthchecks ping
# records a heartbeat (as Alertmanager does every minute) and a Discord webhook
# GET returns the webhook object without posting.
# ALERTMANAGER_SKIP_PROBE=1 for an offline or air-gapped apply.
if [ "${ALERTMANAGER_SKIP_PROBE:-0}" != "1" ]; then
  probe_failed=""
  for r in $AM_RECEIVERS $AM_OPTIONAL_RECEIVERS; do
    url="$(am_url_for "$r")"
    [ -z "$url" ] && continue
    if curl -fsS --max-time 10 -o /dev/null "$url"; then
      echo "alertmanager: probe OK — $r"
    else
      echo "alertmanager: probe FAILED — $r ($(am_var_for "$r") is set but did not answer 2xx)" >&2
      probe_failed="$probe_failed $r"
    fi
  done
  if [ -n "$probe_failed" ]; then
    echo "error: refusing to install — these receivers have a URL that does not work:$probe_failed" >&2
    echo "       a revoked or mistyped webhook fails as silently as an empty one." >&2
    echo "       Set ALERTMANAGER_SKIP_PROBE=1 only if you know the host cannot reach them." >&2
    exit 1
  fi
fi

# 0640 so the rendered config, which embeds the Discord webhook and Healthchecks
# deadman URLs (bearer capabilities), is not world-readable. Group is the
# alertmanager service group (prometheus on r1); override via AM_GROUP.
install -m 0640 -o root -g "${AM_GROUP:-prometheus}" "$RENDERED" "$TARGET"
systemctl reload prometheus-alertmanager
echo "alertmanager: applied $TARGET, reload OK"

# ── publish which optional receivers were installed dark ──
# Lets an alert carry the DARK line that scrolls past above. Describes the
# installed config, so only an apply rewrites it.
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
AM_PROM="$TEXTFILE_DIR/alertmanager_receivers.prom"
write_receiver_gauge() {
  local tmp r dark
  tmp="$(mktemp "${AM_PROM}.XXXXXX")" || return 1
  {
    echo '# HELP stellarindex_alertmanager_optional_receiver_dark 1 when configs/alertmanager/apply.sh installed an optional receiver with no URL, so it delivers to nobody.'
    echo '# TYPE stellarindex_alertmanager_optional_receiver_dark gauge'
    for r in $AM_OPTIONAL_RECEIVERS; do
      dark=1
      [ -n "$(am_url_for "$r")" ] && dark=0
      echo "stellarindex_alertmanager_optional_receiver_dark{receiver=\"$r\"} $dark"
    done
  } >"$tmp" && chmod 0644 "$tmp" && mv "$tmp" "$AM_PROM" && return 0
  rm -f "$tmp"
  return 1
}
# The config is already live, so a failed write must not fail the apply.
if ! write_receiver_gauge; then
  echo "alertmanager: WARNING — could not write $AM_PROM; dark optional receivers are unalarmed." >&2
fi

# ── assert what Alertmanager actually LOADED still delivers ──
# A successful reload is not evidence of delivery (the 31-day outage reloaded
# fine every time). Read the running config back and require the delivery
# blocks in it.
AM_API="${AM_API:-http://localhost:9093}"
for _ in 1 2 3 4 5; do
  loaded="$(curl -fsS --max-time 5 "$AM_API/api/v2/status" 2>/dev/null \
            | python3 -c 'import sys,json; print(json.load(sys.stdin)["config"]["original"])' 2>/dev/null)" && break
  sleep 1
  loaded=""
done
if [ -z "$loaded" ]; then
  echo "alertmanager: WARNING — could not read back the running config from $AM_API;" >&2
  echo "              the file is installed but delivery is UNVERIFIED." >&2
else
  want_webhook=0; want_discord=0
  for r in $AM_RECEIVERS $AM_OPTIONAL_RECEIVERS; do
    [ -z "$(am_url_for "$r")" ] && continue
    case "$r" in
      deadmansswitch|delivery-failure) want_webhook=$((want_webhook + 1)) ;;
      *)                               want_discord=$((want_discord + 1)) ;;
    esac
  done
  got_webhook=$(printf '%s' "$loaded" | grep -c 'webhook_configs:' || true)
  got_discord=$(printf '%s' "$loaded" | grep -c 'discord_configs:' || true)
  bad=""
  # Counted, not "at least one": with two webhook receivers, one surviving block
  # would satisfy a `-lt 1` test while the other had been stripped.
  [ "$got_webhook" -lt "$want_webhook" ] && bad="$bad webhook($got_webhook/$want_webhook)"
  [ "$got_discord" -lt "$want_discord" ] && bad="$bad discord($got_discord/$want_discord)"
  if [ -n "$bad" ]; then
    echo "error: the RUNNING config is missing delivery blocks:$bad" >&2
    echo "       alerts would be accepted and dropped. Investigate before walking away." >&2
    exit 1
  fi
  echo "alertmanager: running config verified — webhook_configs=$got_webhook discord_configs=$got_discord"
fi
