#!/usr/bin/env bash
# restore-drill-test.sh — pins the restore drill's invocation contract.
#
# THE DEFECT THIS EXISTS FOR (found 2026-07-25):
# scripts/ops/restore-drill.sh's optional ClickHouse re-derive stage
# invoked `stellarindex-ops ch-backfill … -database drill_scratch`. That
# flag has never existed. ch-backfill parses with flag.ContinueOnError,
# so the invocation died at PARSE time, the stage recorded a generic
# "ch-backfill sample failed", and `| tail -5` swallowed the reason. The
# ADR-0043 §2.2 re-derive drill — the thing that turns "the lake is
# re-derivable" from a claim into a measured fact — therefore never ran
# once between shipping and this test.
#
# Nothing caught it because it is a drift between a SHELL script and a
# GO flag set, and no gate looked across that seam. This test is that
# gate: every flag restore-drill.sh passes to ch-backfill must be
# declared in internal/ops/chops/ch_backfill.go's FlagSet.
#
# Static, root-free, second-fast. The deployed box gets the same check at
# runtime from restore-drill.sh's own CH preflight (which reads
# `ch-backfill -help`, so it also catches a stale binary on disk); this
# catches it in CI before it ships.
#
# Section 5 (2026-09-04) pins a second seam of the same class: the
# textfile metric is per-repo (a `repo` label on every series, one file
# per repo), the off-site unit runs the script with DRILL_REPO=2, and the
# off-site staleness rule in BOTH trees selects repo="2". Drift in any one
# of the three leaves the off-site alert selecting a series nothing
# writes — dead by construction, exactly like a mis-spelled flag.
#
# Run: bash scripts/ops/restore-drill-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

DRILL="scripts/ops/restore-drill.sh"
CH_BACKFILL_GO="internal/ops/chops/ch_backfill.go"
OFFSITE_UNIT="configs/ansible/roles/archival-node/templates/systemd/restore-drill-offsite.service.j2"
RULE_TREES=(deploy/monitoring/rules/restore-drill.yml configs/prometheus/rules.r1/restore-drill.yml)

pass=0
fail=0
ok()   { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

for f in "$DRILL" "$CH_BACKFILL_GO" "$OFFSITE_UNIT" "${RULE_TREES[@]}"; do
  [[ -r "$f" ]] || { echo "restore-drill-test: missing $f" >&2; exit 2; }
done

# ─── the flag sets ──────────────────────────────────────────────────
# Declared: every fs.<Type>("name", …) in ch-backfill's FlagSet.
declared="$(grep -oE 'fs\.(String|Bool|Int|Uint|Int64|Uint64|Duration|Float64)\("[a-z0-9-]+"' "$CH_BACKFILL_GO" \
  | sed -E 's/.*\("([a-z0-9-]+)"/\1/' | sort -u)"
if [[ -z "$declared" ]]; then
  echo "restore-drill-test: parsed ZERO flags out of $CH_BACKFILL_GO — the extraction" >&2
  echo "  broke (renamed FlagSet? different helper?), and a vacuous pass here is exactly" >&2
  echo "  the failure mode this test exists to prevent." >&2
  exit 2
fi

# The drill's REAL ch-backfill invocation — the one that runs the
# re-derive, not the `-help` preflight probe, not prose in a comment or a
# note(). Anchored on the binary immediately preceding the subcommand, so
# it matches both the current `"$OPS_BIN" ch-backfill` form and the
# pre-fix inline `/usr/local/bin/stellarindex-ops ch-backfill` one. The
# invocation spans continuation lines, so capture through the line that
# redirects stderr.
ch_invocation() {
  awk '
    /^[[:space:]]*#/ { next }                                  # comments are prose
    !inv && /(stellarindex-ops|OPS_BIN"?)[[:space:]]+ch-backfill/ && !/-help/ { inv = 1 }
    inv { print; if ($0 ~ /2>&1/) inv = 0 }
  ' "$DRILL"
}

# Passed: the `-flag` tokens in that invocation.
passed="$(ch_invocation | grep -oE '(^|[[:space:]])-[a-z][a-z0-9-]*' | tr -d ' ' | sed 's/^-//' | sort -u)"
if [[ -z "$passed" ]]; then
  echo "restore-drill-test: found no ch-backfill invocation in $DRILL — either the drill" >&2
  echo "  stage was removed or this test's extraction drifted; refusing to pass vacuously." >&2
  exit 2
fi

# ─── 1. every passed flag is declared ───────────────────────────────
while IFS= read -r flag; do
  [[ -z "$flag" ]] && continue
  if grep -qxF "$flag" <<<"$declared"; then
    ok "restore-drill passes -$flag, which ch-backfill declares"
  else
    bad "restore-drill passes -$flag, which ch-backfill DOES NOT declare — flag.ContinueOnError
       rejects the whole invocation at parse time and the CH re-derive stage can never run
       (the 2026-07-25 '-database drill_scratch' defect). Declared flags: $(tr '\n' ' ' <<<"$declared")"
  fi
done <<<"$passed"

# ─── 2. the stage must read history from an explicit bucket ─────────
# ch-backfill's no-seam default is the (trimmed) live bucket, which
# cannot hold a window ~1M ledgers below the tip — the 5179250a
# wrong-bucket class. The drill must not rely on that default.
if grep -qxF "bucket" <<<"$passed"; then
  ok "the re-derive stage passes -bucket explicitly (does not inherit the live default)"
else
  bad "the re-derive stage does not pass -bucket — ch-backfill's no-seam default is the
       trimmed live bucket, which cannot hold the drill's historic window"
fi

# ─── 3. the stage must not write to the live lake ───────────────────
# There is no scratch-database mode (clickhouse.Open pins `stellar`), so
# a writing drill writes to production. -dry-run is the ADR-0043 §2.2
# compliant shape on this host.
if grep -qxF "dry-run" <<<"$passed"; then
  ok "the re-derive stage runs -dry-run (non-destructive, per the script's own header)"
else
  bad "the re-derive stage does not pass -dry-run — with no scratch-database mode available
       it would re-derive into the LIVE lake, which is not a non-destructive drill"
fi

# ─── 4. the failure output must not be truncated ────────────────────
# `| tail -5` on the invocation is what hid the parse error for weeks.
if grep -q 'tail -' <<<"$(ch_invocation)"; then
  bad "the ch-backfill invocation pipes through 'tail -' — truncating the stage's output is
       how the original defect stayed invisible; print it in full"
else
  ok "the ch-backfill invocation does not truncate its output"
fi

# ─── 5. the metric is per-repo, and the three seams agree ───────────
# (a) every series the drill emits carries the repo label. The label is
# built once (`lbl`) and appended to each metric name; a metric line
# emitted without it is a series no per-repo rule can select.
label_def="$(grep -E '^[[:space:]]*local lbl=' "$DRILL" || true)"
if grep -q 'repo=' <<<"$label_def"; then
  ok "the drill builds a repo label for its textfile series"
else
  bad "the drill no longer builds a \`lbl\` repo label — the per-repo rules select repo=\"…\"
       and an un-labelled series satisfies neither"
fi
emitted="$(grep -vE '^[[:space:]]*#' "$DRILL" | grep -oE 'echo "stellarindex_restore_drill_[a-z_]+[^ ]*' || true)"
if [[ -z "$emitted" ]]; then
  bad "found no metric emit lines in $DRILL — the extraction drifted; refusing to pass vacuously"
fi
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  name="${line#echo \"}"; name="${name%%[\$\{ ]*}"
  if [[ "$line" == *'${lbl}'* ]]; then
    ok "$name is emitted with the repo label"
  else
    bad "$name is emitted WITHOUT the repo label — stellarindex_restore_drill_offsite_stale
       selects repo=\"2\" and stellarindex_restore_drill_stale repo=~\"1|\"; an un-labelled
       series is read by the on-box rule alone, whichever repo wrote it"
  fi
done <<<"$emitted"

# (b) one file per repo, so the two timers cannot rewrite each other's
# verdict. repo1 keeps the historical name; every other repo gets its own.
if grep -q 'restore_drill_repo${DRILL_REPO}.prom' "$DRILL"; then
  ok "repos other than 1 write their own textfile (restore_drill_repo<N>.prom)"
else
  bad "the drill writes one textfile for every repo — a clean repo2 run would erase a failed
       repo1 verdict (the file is rewritten whole on every run)"
fi

# (c) the off-site unit runs THIS script for repo2 …
if grep -q '^Environment=DRILL_REPO=2$' "$OFFSITE_UNIT"; then
  ok "restore-drill-offsite.service sets DRILL_REPO=2"
else
  bad "$OFFSITE_UNIT does not set Environment=DRILL_REPO=2 — the off-site drill would drill repo1"
fi

# … and the off-site rule, in both trees, reads the series that repo writes.
for rules in "${RULE_TREES[@]}"; do
  offsite_expr="$(awk '
    /- alert: stellarindex_restore_drill_offsite_stale$/ { inalert = 1; next }
    inalert && /^ *- alert:/ { exit }
    inalert { print }
  ' "$rules")"
  if grep -q 'stellarindex_restore_drill_last_success_unix{repo="2"}' <<<"$offsite_expr"; then
    ok "$rules: stellarindex_restore_drill_offsite_stale selects last_success_unix{repo=\"2\"}"
  else
    bad "$rules: stellarindex_restore_drill_offsite_stale does not select
       stellarindex_restore_drill_last_success_unix{repo=\"2\"} — the series the DRILL_REPO=2 run writes"
  fi
  onbox_expr="$(awk '
    /- alert: stellarindex_restore_drill_stale$/ { inalert = 1; next }
    inalert && /^ *- alert:/ { exit }
    inalert { print }
  ' "$rules")"
  # An ALLOW-LIST, not `repo!="2"`. A negative matcher lets any OTHER
  # repo satisfy the absent branch: with a repo3 series fresh (the script
  # takes any positive-integer DRILL_REPO) and repo1 never drilled, the
  # ticket stayed silent — the masking inversion the scoping exists to
  # fix, one repo over. The empty alternative keeps the pre-label series.
  if grep -q 'stellarindex_restore_drill_last_success_unix{repo=~"1|"}' <<<"$onbox_expr"; then
    ok "$rules: stellarindex_restore_drill_stale selects last_success_unix{repo=~\"1|\"} (repo1 + pre-label)"
  else
    bad "$rules: stellarindex_restore_drill_stale does not select repo=~\"1|\" — with a negative or
       absent matcher its absent_over_time branch stays silent for a never-drilled repo1 while
       some other repo has a sample"
  fi
done

echo "restore-drill-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]] || exit 1
