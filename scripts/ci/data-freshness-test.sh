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
# It also pins the PUBLICATION GUARD (section 7), which is a different
# defect in the same file. Everything the script writes is composed
# inside SQL and redirected into the .prom unexamined, and node_exporter
# rejects an unparseable .prom WHOLE — so one NULL, one error string or
# one psql command tag would take all 162 samples this producer writes on
# r1 off the host at once, the watchdog's own meta-alert among them. The
# guard parses the rendered bytes before the atomic swap and withholds
# only the offending lines rather than refusing to publish, because an
# unpublished file here is re-served frozen and green (see 2 above).
# What section 7 asserts:
#   6. the validator is the last thing to touch the textfile;
#   7. a malformed value never reaches the published file, while every
#      other line of the same render does, and the withholding is loud
#      (stderr + a tally metric + a non-zero exit);
#   8. ordinary output is published BYTE-IDENTICALLY (cmp, not eyes);
#   9. a validator that cannot run, or that answers with something other
#      than a count, refuses rather than publishing unvalidated bytes.
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

# ─── 7. the publication guard ────────────────────────────────────────
#
# Everything above this point in the shipped script writes into $TMP
# UNEXAMINED: seven psql invocations whose stdout IS the exposition text,
# composed inside SQL. node_exporter rejects an unparseable .prom WHOLE,
# so one NULL, one error string or one command tag would take all 162
# samples this producer writes on r1 off the host together — including
# the series `absent_over_time(stellarindex_data_freshness_stale[45m])`
# reads, which is the only thing watching this watchdog.
#
# The guard parses the rendered bytes before the atomic swap. Its verdict
# differs from timescale-jobs-probe.sh on purpose: that probe refuses to
# publish, because an unpublished file ages into its own last-run alert;
# this one has no last-run gauge, so an unpublished file is re-served
# frozen and green forever (Wave L / #319). It therefore PUBLISHES and
# withholds only the offending lines, counts them in a metric, names them
# on stderr and exits non-zero.
#
# What must hold, in both directions:
#   a. the guard is the LAST thing to touch the textfile, so nothing
#      reaches node_exporter unvalidated;
#   b. a malformed value never reaches the published file, while every
#      other line in the same render does;
#   c. ordinary output is published BYTE-IDENTICALLY — the guard must be
#      a no-op on the happy path, proven with cmp rather than by eye;
#   d. a guard that cannot run, or that answers with something other than
#      a count, refuses rather than publishing unvalidated bytes or
#      writing a non-number of its own.
echo "data-freshness-test: publication guard — placement"

guard_ln="$(grep -n -m1 -E '^CHECKED=' "$SRC" | cut -d: -f1)"
if [[ -n "$guard_ln" && -n "$probe_ln" && -n "$chmod_ln" && $probe_ln -lt $guard_ln && $guard_ln -lt $chmod_ln ]]; then
  ok "the validator runs after the last producer and before the atomic swap"
else
  bad "validator placement moved: probe=$probe_ln guard=$guard_ln chmod=$chmod_ln"
fi

# Anything appended AFTER the validator has read the file reaches
# node_exporter unexamined, which is the entire defect. The one permitted
# append is the validator's own tally, which is a shell integer.
# shellcheck disable=SC2016  # the literal $TMP is the pattern being searched for
late="$(awk -v s="${guard_ln:-0}" 'NR > s && />> "\$TMP"/ && $0 !~ /unparseable_lines/ { print NR ": " $0 }' "$SRC")"
if [[ -z "$late" ]]; then
  ok "nothing appends to the textfile after validation"
else
  bad "unvalidated content is appended after the validator: $late"
fi

if grep -q '^  echo .# HELP stellarindex_data_freshness_unparseable_lines ' "$SRC" \
   && grep -q '^  echo .# TYPE stellarindex_data_freshness_unparseable_lines gauge.$' "$SRC"; then
  ok "the tally family declares its own HELP + TYPE"
else
  bad "stellarindex_data_freshness_unparseable_lines is emitted without a HELP/TYPE of its own"
fi

# ─── extract the publication path from the shipped bytes ─────────────
PUBLISH="$WORK/publish.sh"
{
  printf '%s\n' "${STRICT:-set -euo pipefail}"
  # shellcheck disable=SC2016  # emitted into the harness verbatim; $1/$2 must not expand here
  printf 'TMP="$1"\nOUT="$2"\n'
  awk '/^CHECKED=/ { p = 1 } p' "$SRC"
} > "$PUBLISH"
# shellcheck disable=SC2016  # the literal $TMP/$OUT are the patterns being searched for
if grep -q 'mv "$TMP" "$OUT"' "$PUBLISH" && grep -q 'awk -v keep=' "$PUBLISH"; then
  ok "extracted region carries the validator and the atomic mv"
else
  bad "publication-path extraction produced nothing usable — the markers drifted"
fi

# publish <rendered-body> → rc in $RC, stderr in $ERR, published file in $PUB.
# $PUB is pre-seeded with a PREVIOUS tick so a refusal to publish is
# distinguishable from a publish.
PREV="$WORK/previous.prom"
printf 'stellarindex_data_freshness_stale{domain="oracle",source="coingecko"} 0\n' > "$PREV"
publish() {
  PUB="$WORK/published.$RANDOM.prom"
  cp "$PREV" "$PUB"
  local rendered="$WORK/render.$RANDOM.prom"
  cp "$1" "$rendered"
  ERR="$(PATH="${STUB_PATH:-$PATH}" bash "$PUBLISH" "$rendered" "$PUB" 2>&1 >/dev/null)"
  RC=$?
}

# ─── 7a. the happy path is byte-identical ────────────────────────────
#
# Mirrors what this producer actually emits on r1 (measured 2026-09-10):
# the label keys in use are domain/source/view, no label value contains a
# quote or a backslash, every value is a whole number, and three families
# are bare names with no labels at all. Comment headers and a blank line
# are in the fixture because they are exposition too, and a guard that
# tidied them away would be changing the file it exists to protect.
echo "data-freshness-test: publication guard — the happy path is untouched"
BODY_OK="$WORK/body-ok.prom"
cat > "$BODY_OK" <<'PROM'
# HELP stellarindex_data_freshness_age_seconds Seconds since the newest row for a data domain/source.
# TYPE stellarindex_data_freshness_age_seconds gauge

stellarindex_data_freshness_age_seconds{domain="oracle",source="coingecko"} 142
stellarindex_data_freshness_stale{domain="fx",source="massive"} 0
stellarindex_completeness_incomplete{source="soroswap"} 0
stellarindex_completeness_watermark_lag_ledgers{source="soroswap"} 0
stellarindex_recognition_ok{source="phoenix"} 1
stellarindex_twap_history_missing{view="twap_1d"} 0
stellarindex_supply_assets_stale 0
stellarindex_supply_asset_max_age_seconds 282
stellarindex_recognition_unattributed_shapes 25419
PROM
publish "$BODY_OK"
EXPECT_OK="$WORK/expect-ok.prom"
cp "$BODY_OK" "$EXPECT_OK"
printf 'stellarindex_data_freshness_unparseable_lines 0\n' >> "$EXPECT_OK"
if [[ $RC -eq 0 ]] && cmp -s "$EXPECT_OK" "$PUB"; then
  ok "every rendered byte reaches the published file unchanged, plus a 0 tally (cmp)"
else
  bad "rc=$RC (want 0); the guard altered ordinary output: $(diff "$EXPECT_OK" "$PUB" 2>&1)"
fi
if [[ -z "$ERR" ]]; then
  ok "a clean render says nothing on stderr"
else
  bad "clean render wrote to stderr: $ERR"
fi

# ─── 7b. malformed values are withheld, the rest survives ────────────
#
# Four shapes, all of which the SQL above can produce and none of which
# the shell can see: a psql command tag as the value (r1 2026-09-10), a
# NULL rendering as an empty value field, a server error string that
# reached stdout, and a bare command tag on a line of its own.
echo "data-freshness-test: publication guard — malformed values are withheld"
BODY_BAD="$WORK/body-bad.prom"
{
  printf '# TYPE stellarindex_data_freshness_age_seconds gauge\n'
  printf 'stellarindex_data_freshness_age_seconds{domain="oracle",source="coingecko"} 142\n'
  printf 'stellarindex_completeness_watermark_lag_ledgers{source="soroswap"} SET\n'
  # A NULL anywhere in the concatenation yields an empty value field. The
  # trailing space is the whole point of the case, so it is written with
  # printf rather than in a heredoc no formatter can be trusted to leave
  # alone.
  printf 'stellarindex_supply_asset_max_age_seconds \n'
  printf 'stellarindex_supply_assets_stale 0\n'
  printf 'stellarindex_recognition_unattributed_shapes ERROR:  relation "completeness_snapshots" does not exist\n'
  printf 'SET\n'
  printf 'stellarindex_recognition_ok{source="phoenix"} 1\n'
} > "$BODY_BAD"
publish "$BODY_BAD"
EXPECT_BAD="$WORK/expect-bad.prom"
{
  printf '# TYPE stellarindex_data_freshness_age_seconds gauge\n'
  printf 'stellarindex_data_freshness_age_seconds{domain="oracle",source="coingecko"} 142\n'
  printf 'stellarindex_supply_assets_stale 0\n'
  printf 'stellarindex_recognition_ok{source="phoenix"} 1\n'
  printf 'stellarindex_data_freshness_unparseable_lines 4\n'
} > "$EXPECT_BAD"
if cmp -s "$EXPECT_BAD" "$PUB"; then
  ok "the 4 unparseable lines are withheld, the 3 good samples and the header survive verbatim, tally = 4"
else
  bad "published file is not the withheld-lines-removed render: $(diff "$EXPECT_BAD" "$PUB" 2>&1)"
fi
if [[ $RC -eq 1 ]]; then
  ok "a withheld line exits non-zero — the unit goes 'failed' under stellarindex_systemd_unit_failed"
else
  bad "rc=$RC (want 1): withholding a line was silent to systemd"
fi
if [[ "$ERR" == *'withholding unparseable exposition line: stellarindex_completeness_watermark_lag_ledgers{source="soroswap"} SET'* \
   && "$ERR" == *'withholding unparseable exposition line: SET'* \
   && "$ERR" == *'4 unparseable line(s) withheld'* ]]; then
  ok "each withheld line is named on stderr and the count is summarised (journal-visible)"
else
  bad "the withholding was not announced: $ERR"
fi

# ─── 7c. a guard that cannot answer refuses to publish ───────────────
#
# The one case where refusing IS right: if the validator did not run, no
# claim can be made about the bytes, and publishing them unexamined is
# precisely the defect. The previous file staying in place is the cost,
# and the non-zero exit is what makes that cost visible.
echo "data-freshness-test: publication guard — a validator that cannot answer"
mkdir -p "$WORK/nobin"
printf '#!/usr/bin/env bash\nexit 1\n' > "$WORK/nobin/awk"
chmod +x "$WORK/nobin/awk"
STUB_PATH="$WORK/nobin:$PATH" publish "$BODY_OK"
if [[ $RC -eq 1 ]] && cmp -s "$PREV" "$PUB" && [[ "$ERR" == *"refusing to publish unvalidated bytes"* ]]; then
  ok "validator failure → exit 1, previous file untouched, refusal on stderr"
else
  bad "rc=$RC err='$ERR' published='$(cat "$PUB")'"
fi

# And a validator that answers with something that is not a count has
# validated nothing — coercing it to 0 would write the guard's OWN
# unparseable value into the file it exists to keep parseable.
mkdir -p "$WORK/nobin2"
printf '#!/usr/bin/env bash\necho SET\nexit 0\n' > "$WORK/nobin2/awk"
chmod +x "$WORK/nobin2/awk"
STUB_PATH="$WORK/nobin2:$PATH" publish "$BODY_OK"
if [[ $RC -eq 1 ]] && cmp -s "$PREV" "$PUB" && [[ "$ERR" == *"non-numeric tally"* ]]; then
  ok "a non-numeric tally → exit 1, previous file untouched (never written as a metric value)"
else
  bad "rc=$RC err='$ERR' published='$(cat "$PUB")'"
fi

printf 'data-freshness-test: %d passed, %d failed\n' "$pass" "$fail"
[[ $fail -eq 0 ]]
