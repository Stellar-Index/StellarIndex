#!/usr/bin/env bash
# data-freshness-test.sh — fixture tests for the ClickHouse leg of the
# data-freshness watchdog the archival-node role installs as
# /usr/local/sbin/data-freshness.sh (runbook Wave L, #319).
#
# The script is configs/ansible/roles/archival-node/files/data-freshness.sh.
# It runs under `set -euo pipefail`, builds the whole node_exporter textfile
# in $TMP, and only at the very end does `chmod 0644 "$TMP"; mv "$TMP" "$OUT"`.
# Its LAST producer is a ClickHouse HTTP probe on :8123 — the one query that
# is not Postgres, and the one daemon that can be down while every gauge
# above it is perfectly computable.
#
# The bug this pins: `SF_AGE=$(curl … | tr …)` takes curl's status under
# `pipefail`, so a CH outage aborted the script at that line. The EXIT trap
# then deleted $TMP, `mv` never ran, and node_exporter kept re-serving the
# PREVIOUS data_freshness.prom byte-for-byte. Every gauge FROZE at its last
# value instead of going absent — so `stellarindex_data_freshness_stale`
# stayed 0 for genuinely stale sources AND the watchdog's own meta-alert
# (`absent_over_time(stellarindex_data_freshness_stale[45m])`) could not
# fire, because nothing was absent. One ClickHouse outage silenced the whole
# "never get behind" layer.
#
# What must hold:
#   1. the probe region is the LAST thing before the atomic swap (so an
#      abort inside it costs the entire tick) and the script really is
#      `set -euo pipefail` — the premise of the whole test;
#   2. curl transport failure (CH down) → the region exits 0 and emits no
#      sep41_supply sample, leaving the rest of the textfile intact;
#   3. curl HTTP failure (`-f`, CH 5xx) → same;
#   4. a non-numeric body that arrives with exit 0 emits NOTHING — one
#      unparseable sample makes node_exporter reject the whole file;
#   5. a healthy answer still emits both gauges, with stale=0 under the
#      3600 s threshold and stale=1 over it.
#
# The region under test is extracted from the SHIPPED script (same idiom as
# envfile-loader-test.sh, which extracts load_env_file() from it) so this is
# never a hand-copied twin. curl is stubbed on PATH; no network, no
# ClickHouse, no Postgres.
#
# Run: bash scripts/ci/data-freshness-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/configs/ansible/roles/archival-node/files/data-freshness.sh"
[[ -r "$SRC" ]] || { echo "data-freshness-test: missing $SRC" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── 1. the premise: strict mode + probe sits before the atomic swap ──
echo "data-freshness-test: shipped-script premises"
STRICT="$(grep -m1 -E '^set -euo pipefail$' "$SRC" || true)"
if [[ -n "$STRICT" ]]; then
  ok "script runs under 'set -euo pipefail'"
else
  bad "script no longer declares 'set -euo pipefail' — this test's premise moved"
fi
probe_ln="$(grep -n -m1 -E '^SF_AGE' "$SRC" | cut -d: -f1)"
# shellcheck disable=SC2016  # the literal $TMP/$OUT are the patterns being searched for
chmod_ln="$(grep -n -m1 -E '^chmod 0644 "\$TMP"$' "$SRC" | cut -d: -f1)"
# shellcheck disable=SC2016  # ditto
mv_ln="$(grep -n -m1 -E '^mv "\$TMP" "\$OUT"$' "$SRC" | cut -d: -f1)"
if [[ -n "$probe_ln" && -n "$chmod_ln" && -n "$mv_ln" && $probe_ln -lt $chmod_ln && $chmod_ln -lt $mv_ln ]]; then
  ok "ClickHouse probe precedes chmod 0644 + the atomic mv (an abort there costs the whole tick)"
else
  bad "probe/chmod/mv layout moved: probe=$probe_ln chmod=$chmod_ln mv=$mv_ln"
fi

# ─── extract the probe region from the shipped bytes ─────────────────
PROBE="$WORK/probe.sh"
{
  printf '%s\n' "${STRICT:-set -euo pipefail}"
  # shellcheck disable=SC2016  # emitted into the harness verbatim; $1 must not expand here
  printf 'TMP="$1"\n'
  awk '/^SF_AGE/ { p = 1 } /^chmod 0644 "\$TMP"$/ { p = 0 } p' "$SRC"
} > "$PROBE"
if grep -q 'stellar.supply_flows' "$PROBE" && grep -q 'curl' "$PROBE" \
   && grep -q 'sep41_supply' "$PROBE"; then
  ok "extracted region carries the curl probe and both sep41_supply emits"
else
  bad "extraction produced no usable probe region — the markers drifted"
fi

# ─── curl stub ───────────────────────────────────────────────────────
mkdir -p "$WORK/bin"
cat > "$WORK/bin/curl" <<'SH'
#!/usr/bin/env bash
# Stub: answers ${CURL_OUT} on stdout and exits ${CURL_RC}. Mirrors curl's
# real split — a transport failure (7) and an -f HTTP failure (22) both
# write only to stderr.
if [[ "${CURL_RC:-0}" == 0 ]]; then
  printf '%s\n' "${CURL_OUT:-}"
else
  echo "curl: (${CURL_RC}) stubbed failure" >&2
fi
exit "${CURL_RC:-0}"
SH
chmod +x "$WORK/bin/curl"
export PATH="$WORK/bin:$PATH"

# run <rc> <body> → rc in $RC, stderr in $ERR, textfile content in $PROM
run() {
  PROM="$WORK/out.$RANDOM.prom"
  printf 'stellarindex_data_freshness_stale{domain="oracle",source="coingecko"} 0\n' > "$PROM"
  ERR="$(CURL_RC="$1" CURL_OUT="$2" bash "$PROBE" "$PROM" 2>&1 >/dev/null)"
  RC=$?
}
sep41_lines() { grep -c 'sep41_supply' "$PROM"; }
gauge() { grep -E "^$1\{domain=\"sep41_supply\",source=\"supply_flows\"\} " "$PROM" | awk '{ print $2 }'; }

# ─── 2. ClickHouse unreachable → the tick survives ───────────────────
echo "data-freshness-test: ClickHouse unreachable (curl rc 7)"
run 7 ""
if [[ $RC -eq 0 ]]; then ok "probe region exits 0 — the script reaches its atomic swap"; else bad "rc $RC (want 0): a CH outage still aborts the whole watchdog"; fi
if [[ "$(sep41_lines)" == 0 ]]; then ok "no sep41_supply sample emitted"; else bad "emitted: $(grep sep41_supply "$PROM")"; fi
if grep -q 'stellarindex_data_freshness_stale{domain="oracle"' "$PROM"; then ok "the Postgres-derived gauges already in the textfile survive"; else bad "textfile lost its earlier content"; fi
if [[ "$ERR" == *"ClickHouse supply_flows probe failed"* ]]; then ok "the skip is announced on stderr (journal-visible)"; else bad "silent skip; stderr was: $ERR"; fi

# ─── 3. ClickHouse 5xx → same, via -f ────────────────────────────────
echo "data-freshness-test: ClickHouse HTTP failure (curl rc 22)"
run 22 ""
if [[ $RC -eq 0 ]]; then ok "probe region exits 0"; else bad "rc $RC (want 0)"; fi
if [[ "$(sep41_lines)" == 0 ]]; then ok "no sep41_supply sample emitted"; else bad "emitted: $(grep sep41_supply "$PROM")"; fi

# ─── 4. non-numeric body on exit 0 → nothing reaches the textfile ────
echo "data-freshness-test: non-numeric body"
run 0 "Code:60.DB::Exception:Table stellar.supply_flows does not exist"
if [[ $RC -eq 0 ]]; then ok "probe region exits 0"; else bad "rc $RC (want 0)"; fi
if [[ "$(sep41_lines)" == 0 ]]; then ok "garbage body emits no sample (whole-file parse stays valid)"; else bad "emitted: $(grep sep41_supply "$PROM")"; fi

# ─── 5. healthy answers still produce both gauges ────────────────────
echo "data-freshness-test: healthy ClickHouse"
run 0 "42"
if [[ $RC -eq 0 && "$(gauge stellarindex_data_freshness_age_seconds)" == 42 && "$(gauge stellarindex_data_freshness_stale)" == 0 ]]; then
  ok "age 42 s → age_seconds=42, stale=0"
else
  bad "rc=$RC file: $(cat "$PROM")"
fi
run 0 "7200"
if [[ $RC -eq 0 && "$(gauge stellarindex_data_freshness_age_seconds)" == 7200 && "$(gauge stellarindex_data_freshness_stale)" == 1 ]]; then
  ok "age 7200 s → age_seconds=7200, stale=1 (over the 3600 s threshold)"
else
  bad "rc=$RC file: $(cat "$PROM")"
fi

# ─── 6. the FX staleness budget must MIRROR the serving code ─────────
#
# The alert threshold and the tolerance the serving path applies are two
# copies of one number in two languages, with nothing tying them
# together. They drifted: the SQL said 48h while
# `aggregate.composite_reference.fx_max_age_hours` said 76h, so
# `stellarindex_data_source_stale{source="massive"}` fired EVERY weekend
# against a healthy feed (#370). `massive` publishes a business-day
# snapshot and FX markets close — Fri 00:00 → Mon 00:00 is 72h, so 48h
# could not survive a normal weekend.
#
# An alert stricter than the tolerance the code actually uses reports a
# fault the system does not have. This pins them together so the next
# change to either has to change both.
echo "data-freshness-test: FX budget mirrors the serving config"

SRC_SH="$(dirname "$0")/../../configs/ansible/roles/archival-node/files/data-freshness.sh"
CFG_GO="$(dirname "$0")/../../internal/config/config.go"

# The fx-domain threshold, in seconds, as the emitter's SQL declares it.
fx_thr="$(grep -E "^ *SELECT 'fx', source," -A0 "$SRC_SH" | grep -oE '[0-9]{4,}' | tail -1)"
# The serving budget, in hours, from the config default tag.
fx_hours="$(grep -oE 'fx_max_age_hours[^`]*default:"[0-9]+"' "$CFG_GO" | grep -oE 'default:"[0-9]+"' | grep -oE '[0-9]+')"

if [[ -z "$fx_thr" || -z "$fx_hours" ]]; then
  bad "could not read both numbers (sql='$fx_thr' config='${fx_hours}h') — the guard must fail loudly rather than silently pass when its anchors move"
elif [[ "$fx_thr" -eq $((fx_hours * 3600)) ]]; then
  ok "fx threshold ${fx_thr}s == fx_max_age_hours ${fx_hours}h"
else
  bad "fx threshold ${fx_thr}s != fx_max_age_hours ${fx_hours}h ($((fx_hours * 3600))s). An alert budget that disagrees with the serving budget either fires on healthy data or hides a real stall."
fi

# And it must be able to span a weekend market close at all — the
# property that actually broke, independent of the exact number.
if [[ -n "$fx_thr" && "$fx_thr" -gt $((72 * 3600)) ]]; then
  ok "fx threshold spans a 72h weekend close"
else
  bad "fx threshold ${fx_thr}s does not exceed a 72h weekend; a business-day FX feed will false-fire every Sunday"
fi

printf 'data-freshness-test: %d passed, %d failed\n' "$pass" "$fail"
[[ $fail -eq 0 ]]
