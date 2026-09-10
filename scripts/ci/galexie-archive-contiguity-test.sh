#!/usr/bin/env bash
# galexie-archive-contiguity-test.sh — fixture tests for
# configs/ansible/roles/archival-node/files/galexie-archive-contiguity.sh,
# the hourly partition-level gap scan of the ADR-0016 durable mirror.
#
# THE DEFECT THESE PIN. The scan read the bucket with
#   listing=$(mc ls "$BUCKET/" 2>/dev/null) || true
# which discarded mc's exit status, so a read that died PARTWAY through
# the listing handed the parser a truncated but perfectly contiguous
# prefix. The walk found no holes, the file said
# `galexie_archive_unexpected_gaps 0`, and that is byte-identical to the
# healthy answer. Its consumer, stellarindex_galexie_archive_gap, is
# severity `page` and is the only recurring guard on this bucket's
# shape; the sibling stellarindex_galexie_archive_contiguity_silent only
# covers the series going ABSENT, which a scan that keeps writing a
# clean zero never does.
#
# What must hold:
#   1. a healthy listing is parsed correctly — count, first, last, and
#      the declared capacity trim recognised as the one permitted hole —
#      and the scan reports ok 1 with the rows it read;
#   2. the guard still works: a deleted partition and an overlapping
#      re-fill both raise unexpected_gaps, and strict mode (no declared
#      trim) treats the trim hole as unexpected;
#   3. a read that FAILED publishes NO partition verdict at all —
#      neither the fabricated zero of a truncated read nor the
#      fabricated DR-corruption page of a dead one — and reports
#      scan_ok 0;
#   4. a read that SUCCEEDED and returned nothing keeps the shape that
#      fires, exactly as it shipped: an empty durable mirror is a real
#      DR event, and it must stay distinguishable from a failed read;
#   5. every run stamps last_run_unix, so "producing bad data" and "not
#      running" stay different signals — node_exporter re-serves the
#      last file it saw forever otherwise;
#   6. the output is valid Prometheus text and no temp file survives.
#
# The SHIPPED script is executed. `mc` is stubbed on PATH and
# TEXTFILE_DIR/BUCKET are redirected, so there is no MinIO, no systemd
# and no write outside $TMPDIR.
#
# Run: bash scripts/ci/galexie-archive-contiguity-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCAN="$PWD/configs/ansible/roles/archival-node/files/galexie-archive-contiguity.sh"
[[ -r "$SCAN" ]] || { echo "galexie-archive-contiguity-test: missing $SCAN" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── stub ───────────────────────────────────────────────────────────
#
# Real shape, one row per top-level partition dir:
#   [2026-09-05 10:00:00 UTC]     0B <reverse-hex>--<start>-<end>/
# The genesis partition plus a five-partition tail from ARCHIVE_FROM,
# i.e. the declared coverage on r1 in miniature.
mkdir -p "$TMP/bin" "$TMP/textfile"
cat > "$TMP/bin/mc" <<'SH'
#!/usr/bin/env bash
row() { printf '[2026-09-05 10:00:00 UTC]     0B %08x--%s-%s/\n' "$3" "$1" "$2"; }
genesis() { row 0 63999 4294967295; }
tail_from() {  # tail_from <first-index> <last-index>, 64k partitions from 49984000
  local i s e
  for i in $(seq "$1" "$2"); do
    s=$((49984000 + i * 64000)); e=$((s + 63999))
    row "$s" "$e" $((781 - i))
  done
}
case "${MODE:-full}" in
  full)       genesis; tail_from 0 4; exit 0 ;;
  gap)        genesis; tail_from 0 1; tail_from 3 4; exit 0 ;;   # partition 2 deleted
  overlap)    genesis; tail_from 0 4; row 50176000 50239999 776; exit 0 ;;
  truncated)  # the read dies partway; the prefix it emitted is contiguous
              genesis; tail_from 0 1
              echo "mc: <ERROR> Unable to list. connection reset by peer" >&2
              exit 1 ;;
  dead)       echo "mc: <ERROR> Unable to initialize new alias. no such host" >&2
              exit 1 ;;
  empty)      exit 0 ;;
  unparseable) # mc answered, in a shape the partition regex no longer matches
              echo "[2026-09-05 10:00:00 UTC]     0B ledgers/0/63999/"
              echo "[2026-09-05 10:00:00 UTC]     0B ledgers/49984000/50047999/"
              exit 0 ;;
esac
echo "mc stub: unknown MODE ${MODE:-}" >&2
exit 9
SH
chmod +x "$TMP/bin/mc"
export PATH="$TMP/bin:$PATH"
export TEXTFILE_DIR="$TMP/textfile"
export BUCKET="stub/galexie-archive"
PROM="$TEXTFILE_DIR/galexie_archive_contiguity.prom"

# run <mode> [expected-trim] — one scan; sets $RC.
run() {
  rm -f "$PROM"
  if [ "$#" -ge 2 ]; then
    MODE="$1" EXPECTED_TRIM="$2" bash "$SCAN"
  else
    MODE="$1" bash "$SCAN"
  fi
  RC=$?
}

metric() { awk -v want="$1" '$1 == want { print $2 }' "$PROM"; }

expect() { # expect <metric> <wanted> <label>
  local got
  got=$(metric "$1")
  if [[ "$got" == "$2" ]]; then ok "$3"; else bad "$3 (want '$2', got '${got:-<absent>}')"; fi
}

absent() { # absent <metric> <label>
  local got
  got=$(metric "$1")
  if [[ -z "$got" ]]; then ok "$2"; else bad "$2 (want <absent>, got '$got')"; fi
}

stamped() { # stamped <label>
  local s
  s=$(metric galexie_archive_scan_last_run_unix)
  if [[ "$s" =~ ^1[0-9]{9}$ ]]; then ok "$1"; else bad "$1 (got '${s:-<absent>}')"; fi
}

# ─── 1. a healthy listing ───────────────────────────────────────────
run full
if [[ "$RC" -eq 0 ]]; then ok "a healthy scan exits 0"; else bad "a healthy scan exits 0 (got $RC)"; fi
expect galexie_archive_partition_count 6 "a healthy scan counts every partition"
expect galexie_archive_first_ledger 0 "a healthy scan reports the genesis start"
expect galexie_archive_last_ledger 50303999 "a healthy scan reports the newest end"
expect galexie_archive_unexpected_gaps 0 "the declared capacity trim is not an unexpected gap"
expect galexie_archive_scan_ok 1 "a healthy scan reports scan_ok 1"
expect galexie_archive_scan_listing_lines 6 "a healthy scan reports the rows it read"
stamped "a healthy scan stamps last_run_unix"

mode=$(stat -c %a "$PROM" 2>/dev/null || stat -f %Lp "$PROM")
if [[ "$mode" == "644" ]]; then ok "textfile is 0644"; else bad "textfile is 0644 (got $mode)"; fi
leftovers=$(find "$TEXTFILE_DIR" -type f ! -name 'galexie_archive_contiguity.prom' | wc -l | tr -d ' ')
if [[ "$leftovers" == "0" ]]; then ok "no temp file survives"; else bad "no temp file survives ($leftovers left)"; fi

# ─── 2. the guard still works ───────────────────────────────────────
run gap
expect galexie_archive_unexpected_gaps 1 "a deleted partition raises one unexpected gap"
expect galexie_archive_partition_count 5 "a deleted partition lowers the count"
expect galexie_archive_scan_ok 1 "a real gap is reported by a scan that read fine"

run overlap
expect galexie_archive_unexpected_gaps 1 "an overlapping re-fill raises one unexpected gap"

run full ""
expect galexie_archive_unexpected_gaps 1 "strict mode counts the trim hole as unexpected"

# ─── 3. the read failed ─────────────────────────────────────────────
#
# The shape that made a failure indistinguishable from health: mc dies
# partway, the prefix it emitted is contiguous, and the walk certifies a
# clean DR mirror from a read that never finished.
run truncated
if [[ "$RC" -eq 0 ]]; then
    ok "a truncated read still writes the file"
else
    bad "a truncated read still writes the file (rc $RC)"
fi
absent galexie_archive_unexpected_gaps "a truncated read publishes NO gap verdict"
absent galexie_archive_partition_count "a truncated read publishes NO partition count"
absent galexie_archive_last_ledger "a truncated read publishes NO coverage upper bound"
expect galexie_archive_scan_ok 0 "a truncated read reports scan_ok 0"
expect galexie_archive_scan_listing_lines 3 "a truncated read reports the rows it did get"
stamped "a truncated read still stamps last_run_unix"

# A dead read used to publish unexpected_gaps 1, which pages with a
# DR-corruption claim about a bucket nobody managed to look at.
run dead
absent galexie_archive_unexpected_gaps "a dead read publishes NO gap verdict"
expect galexie_archive_scan_ok 0 "a dead read reports scan_ok 0"
expect galexie_archive_scan_listing_lines 0 "a dead read reports zero rows read"
stamped "a dead read still stamps last_run_unix"

# ─── 4. the read succeeded and returned nothing ─────────────────────
#
# An empty durable mirror is a real DR event and keeps the shape that
# fires, exactly as shipped. scan_ok is what separates it from the two
# cases above.
run empty
expect galexie_archive_unexpected_gaps 1 "an empty bucket still fires"
expect galexie_archive_partition_count 0 "an empty bucket reports zero partitions"
expect galexie_archive_scan_ok 1 "an empty bucket is a successful read"
expect galexie_archive_scan_listing_lines 0 "an empty bucket reports zero rows read"

# A listing the parser cannot match reaches the same page, and
# listing_lines is what tells the responder which of the two it is.
run unparseable
expect galexie_archive_unexpected_gaps 1 "an unparseable listing still fires"
expect galexie_archive_scan_ok 1 "an unparseable listing is a successful read"
expect galexie_archive_scan_listing_lines 2 "an unparseable listing reports the rows it read"

# ─── 5. the file parses as Prometheus text ──────────────────────────
#
# One malformed line makes node_exporter reject the WHOLE file, taking
# every family with it — the blindness the health series exist to
# report, arriving by a different door. The failed-read runs are
# included because they emit the health block with no partition block
# above it.
#
# `promtool check metrics` also LINTS names, and the shipped
# galexie_archive_partition_count trips its `_count`-suffix rule. That
# series has been live on r1 since 2026-08 and node_exporter parses and
# serves it; renaming it would break the existing series and is not what
# this fix is about. So exactly that one complaint is tolerated, and
# only that one — anything else fails.
KNOWN_LINT='galexie_archive_partition_count non-histogram and non-summary metrics should not have "_count" suffix'
prom_lint() { # prom_lint <label>
  local raw="$TMP/promtool.out" rest
  promtool check metrics < "$PROM" > "$raw" 2>&1
  rest=$(grep -vF -- "$KNOWN_LINT" "$raw")
  if [[ -z "${rest//[[:space:]]/}" ]]; then ok "$1"; else bad "$1 ($rest)"; fi
}
if command -v promtool >/dev/null 2>&1; then
  for m in full truncated dead empty; do
    run "$m"
    prom_lint "output parses as Prometheus text (mode=$m)"
  done
else
  echo "  note promtool not installed — skipping the text-format assertions" >&2
fi

# ─── 6. the partition walk answers SHORT ────────────────────────────
#
# THE DEFECT (2026-09-10 class, pre-existing). The walk's result was
# consumed by a bare
#   read -r count first last unexpected <<< "$result"
# with nothing between it and the heredoc that writes those four
# variables as metric values. `read` does not fail loudly on a short
# line: it assigns what it has and leaves the rest EMPTY, so awk dying
# mid-run (OOM), awk being absent, or a future edit to that END print
# renders `galexie_archive_last_ledger` with NO VALUE FIELD.
# node_exporter rejects the whole file over one such line, which takes
# the scan's own health gauges — galexie_archive_scan_ok,
# _scan_listing_lines, _scan_last_run_unix, the three series that exist
# to say the verdict is untrustworthy — down with the verdict itself.
# The blindness this producer was hardened against in 2026-09-05,
# arriving through a different door.
#
# The stub fails ONLY the walk (it is the one awk invocation carrying
# `-v ts=`), so the publication guard's own awk still runs. A stub that
# broke both would prove nothing about the guard.
echo "galexie-archive-contiguity-test: the partition walk answers short"
REAL_AWK="$(command -v awk)"
mkdir -p "$TMP/shortawk"
cat > "$TMP/shortawk/awk" <<SH
#!/usr/bin/env bash
for a in "\$@"; do
  case "\$a" in ts=*) exit 3 ;; esac
done
exec "$REAL_AWK" "\$@"
SH
chmod +x "$TMP/shortawk/awk"

rm -f "$PROM"
ERR="$(MODE=full PATH="$TMP/shortawk:$PATH" bash "$SCAN" 2>&1 >/dev/null)"
RC=$?
if python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM" >/dev/null 2>&1; then
  ok "a short walk still publishes VALID exposition (node_exporter keeps the file)"
else
  bad "a short walk published a file node_exporter would reject whole:
$(python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM" 2>&1)"
fi
absent galexie_archive_partition_count "a short walk publishes NO partition count"
absent galexie_archive_first_ledger    "a short walk publishes NO first ledger"
absent galexie_archive_last_ledger     "a short walk publishes NO coverage upper bound"
absent galexie_archive_unexpected_gaps "a short walk publishes NO gap verdict"
expect galexie_archive_unparseable_lines 4 "the four withheld lines are counted in the tally"
expect galexie_archive_scan_ok 1 "the read itself succeeded, and still says so"
expect galexie_archive_scan_listing_lines 6 "the rows read are still reported"
stamped "a short walk still stamps last_run_unix"
if [[ "$RC" -eq 1 ]]; then
  ok "a withheld line exits non-zero (the unit goes 'failed' under stellarindex_systemd_unit_failed)"
else
  bad "a withheld line was silent to systemd (rc $RC)"
fi
if [[ "$ERR" == *"not four non-negative integers"* && "$ERR" == *"withholding unparseable exposition line: galexie_archive_last_ledger"* ]]; then
  ok "the cause and each withheld line are named in the journal"
else
  bad "the short walk was not announced: $ERR"
fi
leftovers=$(find "$TEXTFILE_DIR" -type f ! -name 'galexie_archive_contiguity.prom' | wc -l | tr -d ' ')
if [[ "$leftovers" == "0" ]]; then
  ok "no temp file survives a short walk"
else
  bad "no temp file survives a short walk ($leftovers left)"
fi

# ─── 7. the validator itself cannot run ─────────────────────────────
#
# Nothing can be claimed about bytes that were never parsed, and
# publishing them unexamined IS the defect — so this refuses, and the
# previous file staying put is the cost. It is not silent: the
# `time() - galexie_archive_scan_last_run_unix > 10800` arm of
# stellarindex_galexie_archive_scan_degraded reads exactly a file that
# has stopped being republished, and the non-zero exit fails the unit.
echo "galexie-archive-contiguity-test: a validator that cannot run"
mkdir -p "$TMP/noawk"
printf '#!/usr/bin/env bash\nexit 1\n' > "$TMP/noawk/awk"
chmod +x "$TMP/noawk/awk"
PREVIOUS='galexie_archive_unexpected_gaps 0
galexie_archive_scan_ok 1
galexie_archive_scan_last_run_unix 1757000000'
printf '%s\n' "$PREVIOUS" > "$PROM"
ERR="$(MODE=full PATH="$TMP/noawk:$PATH" bash "$SCAN" 2>&1 >/dev/null)"
RC=$?
if [[ "$RC" -eq 1 ]] && [[ "$(cat "$PROM")" == "$PREVIOUS" ]] \
   && [[ "$ERR" == *"refusing to publish unvalidated bytes"* ]]; then
  ok "a validator that cannot run refuses, leaves the previous file byte-for-byte, and says so"
else
  bad "rc=$RC err='$ERR' published='$(cat "$PROM")'"
fi
leftovers=$(find "$TEXTFILE_DIR" -type f ! -name 'galexie_archive_contiguity.prom' | wc -l | tr -d ' ')
if [[ "$leftovers" == "0" ]]; then
  ok "a refusal leaves no temp file behind"
else
  bad "a refusal left $leftovers temp file(s)"
fi

# ─── 8. every mode publishes exposition the repo's own parser accepts ─
# promtool is optional on a developer box; this parser is not, so the
# grammar assertion runs everywhere.
echo "galexie-archive-contiguity-test: every mode parses"
for m in full gap overlap truncated dead empty unparseable; do
  run "$m"
  if python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM" >/dev/null 2>&1; then
    ok "mode=$m publishes valid exposition"
  else
    bad "mode=$m published a file node_exporter would reject whole:
$(python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM" 2>&1)"
  fi
done

printf 'galexie-archive-contiguity-test: %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
