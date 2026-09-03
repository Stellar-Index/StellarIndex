#!/usr/bin/env bash
# Apply alertmanager.r1.yml to R1.
#
# Reads webhook URLs from /etc/default/alertmanager-secrets, injects
# them into the config (using a Python YAML walker so empty URLs
# leave the receiver as a no-op stub instead of breaking the file),
# validates with amtool, atomic-installs to
# /etc/prometheus/alertmanager.yml, then reloads alertmanager.
#
# Required env vars (in /etc/default/alertmanager-secrets):
#   HEALTHCHECKS_DEADMANSSWITCH_URL — e.g. https://hc-ping.com/<uuid>
#   DISCORD_WEBHOOK_URL_PAGES       — e.g. https://discord.com/api/webhooks/<id>/<token>
#   DISCORD_WEBHOOK_URL_ALERTS      — e.g. https://discord.com/api/webhooks/<id>/<token>
#
# An empty URL makes the renderer drop that receiver's *_configs block,
# leaving a no-op stub: the receiver accepts alerts and delivers them to
# nobody, exactly like `silent`. That is a legitimate rendering branch
# (it keeps the config valid), but it is NEVER a legitimate thing to
# INSTALL unasked — so applying with an empty URL is a hard error.
#
# Why it is fatal rather than a warning: between 2026-07-29 06:24 and
# 2026-08-29 22:14 the secrets file supplied none of the three URLs.
# Every delivery path — page, ticket, default AND the deadman's switch —
# was a black hole for 31 days. The config reloaded SUCCESSFULLY the
# whole time, so alertmanager_config_last_reload_successful stayed 1 and
# stellarindex_alertmanager_config_bad never fired. Nothing in the system
# could report it, because the thing that reports things was the thing
# that was broken. A warning would have scrolled past; this exits 1.
#
# Deliberately running one channel (e.g. no separate pages webhook) is
# still possible, but it has to be said out loud, per receiver:
#   ALERTMANAGER_ALLOW_EMPTY=pages bash apply.sh
# Point both Discord URLs at the same webhook if you only want one
# channel — that is the better answer than waiving one.

set -euo pipefail

# --check-only: render + amtool-validate, then stop before the
# install/reload. Used by CI (monitoring-rules job) to validate both
# render branches at PR time — pre-gate, a malformed edit that broke
# the block-stripper's indentation assumptions produced a silently
# receiver-less Alertmanager, discovered only at the next hand apply.
CHECK_ONLY=false
if [ "${1:-}" = "--check-only" ]; then
  CHECK_ONLY=true
fi

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
    deadmansswitch) printf '%s' "${HEALTHCHECKS_DEADMANSSWITCH_URL:-}" ;;
    pages)          printf '%s' "${DISCORD_WEBHOOK_URL_PAGES:-}" ;;
    alerts)         printf '%s' "${DISCORD_WEBHOOK_URL_ALERTS:-}" ;;
  esac
}
am_var_for() {
  case "$1" in
    deadmansswitch) printf 'HEALTHCHECKS_DEADMANSSWITCH_URL' ;;
    pages)          printf 'DISCORD_WEBHOOK_URL_PAGES' ;;
    alerts)         printf 'DISCORD_WEBHOOK_URL_ALERTS' ;;
  esac
}
AM_RECEIVERS="deadmansswitch pages alerts"

# --check-only is a RENDERER test: CI drives it with every URL empty on
# purpose, to exercise the block-stripper. The fail-closed guard is an
# INSTALL-time policy, so it is deliberately not applied in that mode.
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
fi

RENDERED="$(mktemp)"
trap 'rm -f "$RENDERED"' EXIT

HEALTHCHECKS_URL="${HEALTHCHECKS_DEADMANSSWITCH_URL:-}" \
DISCORD_PAGES_URL="${DISCORD_WEBHOOK_URL_PAGES:-}" \
DISCORD_ALERTS_URL="${DISCORD_WEBHOOK_URL_ALERTS:-}" \
SOURCE="$SOURCE" \
RENDERED="$RENDERED" \
python3 - <<'PY'
import os

src = open(os.environ["SOURCE"]).read()

subs = {
    "${HEALTHCHECKS_DEADMANSSWITCH_URL}": os.environ.get("HEALTHCHECKS_URL", "").strip(),
    "${DISCORD_WEBHOOK_URL_PAGES}":       os.environ.get("DISCORD_PAGES_URL", "").strip(),
    "${DISCORD_WEBHOOK_URL_ALERTS}":      os.environ.get("DISCORD_ALERTS_URL", "").strip(),
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

if ! amtool check-config "$RENDERED"; then
  echo "error: alertmanager config failed validation" >&2
  exit 1
fi

if [ "$CHECK_ONLY" = true ]; then
  echo "alertmanager: config renders + validates OK (check-only, not applied)"
  exit 0
fi

# ── prove the credentials are LIVE, not merely non-empty ───────────
# amtool validates syntax; it cannot tell a current webhook from a
# revoked one. A stale-but-well-formed URL fails exactly like an empty
# one — silently, at the moment it is needed. Both probes are read-only:
# a Healthchecks ping URL records a heartbeat (the same thing
# Alertmanager does every minute), and a GET on a Discord webhook
# returns the webhook object without posting a message.
# ALERTMANAGER_SKIP_PROBE=1 for an offline or air-gapped apply.
if [ "${ALERTMANAGER_SKIP_PROBE:-0}" != "1" ]; then
  probe_failed=""
  for r in $AM_RECEIVERS; do
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

# CS-121: 0640 (not 0644) so the rendered config — which embeds the Discord
# webhook URLs + the Healthchecks deadman URL (bearer capabilities) — is not
# world-readable. Group is the alertmanager service user's group (prometheus on
# r1) so the service can still read it; override via AM_GROUP if it differs.
install -m 0640 -o root -g "${AM_GROUP:-prometheus}" "$RENDERED" "$TARGET"
systemctl reload prometheus-alertmanager
echo "alertmanager: applied $TARGET, reload OK"

# ── assert what Alertmanager actually LOADED still delivers ────────
# A successful reload is not evidence of delivery: the 31-day outage
# reloaded successfully every time. Read the running config back and
# require the delivery blocks to be present in it. This is the step
# that turns "the file I wrote looks right" into "the process I just
# reloaded will fan out".
AM_API="${AM_API:-http://localhost:9093}"
for attempt in 1 2 3 4 5; do
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
  for r in $AM_RECEIVERS; do
    [ -z "$(am_url_for "$r")" ] && continue
    case "$r" in deadmansswitch) want_webhook=1 ;; *) want_discord=$((want_discord + 1)) ;; esac
  done
  got_webhook=$(printf '%s' "$loaded" | grep -c 'webhook_configs:' || true)
  got_discord=$(printf '%s' "$loaded" | grep -c 'discord_configs:' || true)
  bad=""
  [ "$want_webhook" -gt 0 ] && [ "$got_webhook" -lt 1 ] && bad="$bad deadmansswitch"
  [ "$got_discord" -lt "$want_discord" ] && bad="$bad discord($got_discord/$want_discord)"
  if [ -n "$bad" ]; then
    echo "error: the RUNNING config is missing delivery blocks:$bad" >&2
    echo "       alerts would be accepted and dropped. Investigate before walking away." >&2
    exit 1
  fi
  echo "alertmanager: running config verified — webhook_configs=$got_webhook discord_configs=$got_discord"
fi
