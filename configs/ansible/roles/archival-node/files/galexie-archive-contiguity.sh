#!/bin/bash
# galexie-archive-contiguity: partition-level gap scan of the ADR-0016 R1 durable mirror
# (the DR keystone the off-site copy is pulled from).
#
# WHY: the HA plan assumed a FULL-history archive, but the bucket holds the genesis
# partition [0,63999] plus [ARCHIVE_FROM=49984000 -> tip]; the middle is a DELIBERATE
# capacity trim (recoverable only from aws-public-blockchain). Tip-lag and archive-fill do
# not assert the archive's SHAPE, so a deleted partition (bad trim, stray `mc rm`, MinIO heal
# failure) or an overlap from a botched re-fill would surface only at restore time. The
# EXPECTED coverage is declared here and anything else pages.
#
# Mechanics: one `mc ls` of the top-level partition dirs (`<reverse-hex>--<start>-<end>/`,
# 64k ledgers each, ~220 rows). Sort by start, walk the ranges, count every discontinuity
# that is NOT the declared trim; overlaps count too (a straddling re-fill is corruption).
# Chunk-level holes inside a partition are restore-drill territory
# (deploy/monitoring/rules/restore-drill.yml).
#
# EXPECTED_TRIM="<gap_start>-<gap_end>" overrides the trim (via the
# /etc/default/stellarindex-galexie style env); set it empty once the middle is backfilled
# to require strict contiguity.
#
# The scan also reports its OWN health (galexie_archive_scan_ok, _scan_listing_lines,
# _scan_last_run_unix) on every run, because stellarindex_galexie_archive_gap is severity
# `page` and a failed bucket read must not look like a healthy verdict; see
# stellarindex_galexie_archive_scan_degraded in both rule trees.
# Fixtures: scripts/ci/galexie-archive-contiguity-test.sh

set -uo pipefail

TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
OUT="$TEXTFILE_DIR/galexie_archive_contiguity.prom"
TMP="$OUT.tmp.$$"
trap 'rm -f "$TMP" "$TMP.checked"' EXIT

BUCKET="${BUCKET:-local/galexie-archive}"
# Ledger range NOT expected in the archive (the ADR capacity trim): first and last missing
# ledger. Empty = expect strict contiguity from first partition to tip.
# ${VAR-default}, not ${VAR:-default}: an explicitly EMPTY EXPECTED_TRIM is the documented
# strict mode and must not fall back to the default trim.
EXPECTED_TRIM="${EXPECTED_TRIM-64000-49983999}"

# Buffer the listing before parsing (mc ls + early-exiting pipe reader -> SIGPIPE under
# pipefail; see galexie-archive-tip-lag.sh).
#
# The read's success and its content are separate numbers. Discarding mc's exit status
# (`|| true`) let a read that died PARTWAY hand the parser a truncated but contiguous
# prefix: bad=0, unexpected_gaps 0, byte-identical to a healthy answer, while
# stellarindex_galexie_archive_gap (page) is the ONLY recurring guard on this bucket's shape.
listing=""
read_rc=0
listing=$(mc ls "$BUCKET/" 2>/dev/null) || read_rc=$?

# Raw rows the listing returned, published with the status: with a zero partition count it
# separates "the mirror is empty" (0) from "mc answered in a shape this scan can no longer
# parse" (>0); both reach the same page, and the responder cannot otherwise tell them apart.
listing_lines=0
if [ -n "$listing" ]; then
  listing_lines=$(printf '%s\n' "$listing" | wc -l | tr -d ' ')
fi

# Left empty, and NOT emitted, when the read failed: a truncated read would fabricate "no
# gaps" and a dead read a DR-corruption page pointing at the wrong runbook.
# galexie_archive_scan_ok speaks for a run that could not look.
scan_ok=0
count=""
first=""
last=""
unexpected=""

if [ "$read_rc" -eq 0 ]; then
  scan_ok=1

  # One "start end" pair per partition dir, sorted numerically by start.
  ranges=$(printf '%s\n' "$listing" \
    | grep -oE -- '--[0-9]+-[0-9]+/' \
    | sed -E 's/^--([0-9]+)-([0-9]+)\/$/\1 \2/' \
    | sort -n -k1,1)

  if [ -z "$ranges" ]; then
    # A SUCCESSFUL read with no partitions (empty bucket, or mc output the parser no longer
    # matches) is a DR event and keeps the shape that FIRES; a FAILED read never lands here.
    count=0; first=0; last=0; unexpected=1
  else
    read -r trim_start trim_end <<< "$(printf '%s' "$EXPECTED_TRIM" | tr '-' ' ')"
    result=$(printf '%s\n' "$ranges" | awk -v ts="${trim_start:-}" -v te="${trim_end:-}" '
      NR==1 { first=$1; prev=$2; n=1; bad=0; next }
      {
        n++
        if ($1 <= prev) bad++                       # overlap/duplicate
        else if ($1 != prev+1) {                    # a hole
          if (!(ts != "" && prev+1 == ts+0 && $1-1 == te+0)) bad++
        }
        if ($2 > prev) prev=$2
      }
      END { print n, first, prev, bad }')
    read -r count first last unexpected <<< "$result"
    # awk's four fields ARE the partition block's values, and `read` cannot fail loudly: a SHORT
    # answer (awk killed, missing, or a future edit to that END print) leaves the tail variables
    # EMPTY and renders a sample with no value field, which makes node_exporter reject the WHOLE
    # file, health gauges included.
    #
    # The publication guard below withholds any such line, so this check repairs nothing: it names
    # the CAUSE in the journal, otherwise the operator sees withheld lines with no reason.
    for field in "$count" "$first" "$last" "$unexpected"; do
      case "$field" in
        '' | *[!0-9]*)
          echo "galexie-archive-contiguity: the partition walk answered '$result' — not four non-negative integers, so no partition verdict is publishable this run (the health gauges below still are)" >&2
          break
          ;;
      esac
    done
  fi
fi

mkdir -p "$TEXTFILE_DIR"
: > "$TMP"
if [ "$scan_ok" -eq 1 ]; then
  cat >> "$TMP" <<EOF
# HELP galexie_archive_partition_count Top-level 64k-ledger partitions present in galexie-archive.
# TYPE galexie_archive_partition_count gauge
galexie_archive_partition_count $count
# HELP galexie_archive_first_ledger First ledger covered by the archive (genesis partition start).
# TYPE galexie_archive_first_ledger gauge
galexie_archive_first_ledger $first
# HELP galexie_archive_last_ledger Last ledger covered by the newest archive partition (upper bound; the open tip partition may still be filling).
# TYPE galexie_archive_last_ledger gauge
galexie_archive_last_ledger $last
# HELP galexie_archive_unexpected_gaps Partition-level discontinuities or overlaps that are NOT the declared capacity trim. Any non-zero value is a DR-asset integrity failure.
# TYPE galexie_archive_unexpected_gaps gauge
galexie_archive_unexpected_gaps $unexpected
EOF
fi

# ── The scan's own health, written on EVERY run ──
#
#   _scan_ok            1 iff `mc ls` exited 0. Zero means the series above are absent
#                       because nothing was measured, not because the archive is fine.
#   _scan_listing_lines rows that read returned.
#   _scan_last_run_unix when this file was last WRITTEN (stamps the RUN, not success, so
#                       _scan_ok says the data is bad). Also the only catch for the scan not
#                       running: node_exporter keeps serving the LAST file, so a dead timer
#                       leaves fresh-looking scrapes of a frozen zero-gaps verdict.
#                       stellarindex_galexie_archive_scan_degraded reads all three.
cat >> "$TMP" <<EOF
# HELP galexie_archive_scan_ok Whether the bucket listing this scan is derived from exited 0 (1 = yes). 0 means the partition series above were not written.
# TYPE galexie_archive_scan_ok gauge
galexie_archive_scan_ok $scan_ok
# HELP galexie_archive_scan_listing_lines Rows the bucket listing returned on the last run, before partition parsing.
# TYPE galexie_archive_scan_listing_lines gauge
galexie_archive_scan_listing_lines $listing_lines
# HELP galexie_archive_scan_last_run_unix Unix time this scan last wrote its textfile, whether or not the bucket listing succeeded.
# TYPE galexie_archive_scan_last_run_unix gauge
galexie_archive_scan_last_run_unix $(date +%s)
# HELP galexie_archive_unparseable_lines Exposition lines this run rendered that are not valid Prometheus samples and were therefore WITHHELD from the published textfile. 0 every healthy run; above 0 means a value this scan composed is not a number and that one series is absent this tick.
# TYPE galexie_archive_unparseable_lines gauge
EOF

# ── Validate the rendered bytes before publishing ──
#
# Everything above reaches $TMP unexamined, and four of the seven values come from an awk
# walk over a listing this scan does not control. node_exporter rejects an unparseable
# textfile WHOLE, so one bad field would take the DR verdict AND the health gauges off the host.
#
# WITHHOLD offending lines rather than refuse to publish: a withheld verdict leaves
# galexie_archive_unexpected_gaps ABSENT, which stellarindex_galexie_archive_contiguity_silent
# already reads, whereas an unpublished file is re-served verbatim and freezes a stale
# zero-gaps DR certificate indistinguishable from a fresh one.
#
# Same grammar as data-freshness.sh and the timescale-jobs probe. Braces are [{] / [}]: in an
# ERE a bare brace opens an interval expression (escape undefined by POSIX), so the bracket
# form means the same under mawk, gawk and BWK awk. No apostrophe in the awk program: it is
# a single-quoted shell word.
CHECKED="$TMP.checked"
if ! DROPPED=$(awk -v keep="$CHECKED" '
  /^[ \t]*$/ || /^#/ { print > keep; next }
  {
    rest = $0
    sub(/^[a-zA-Z_:][a-zA-Z0-9_:]*([{][^}]*[}])?[ \t]+/, "", rest)
    if (rest != $0 && rest ~ /^[+-]?([0-9]+\.?[0-9]*([eE][+-]?[0-9]+)?|\.[0-9]+([eE][+-]?[0-9]+)?|Inf|NaN)([ \t]+[0-9]+)?[ \t]*$/) {
      print > keep
      next
    }
    printf "galexie-archive-contiguity: withholding unparseable exposition line: %s\n", $0 > "/dev/stderr"
    bad = bad + 1
  }
  END { print bad + 0 }
' "$TMP"); then
  rm -f "$CHECKED"
  echo "galexie-archive-contiguity: the exposition validator did not run — refusing to publish unvalidated bytes, since node_exporter drops an unparseable file whole and says so only in node_textfile_scrape_error. Previous galexie_archive_contiguity.prom left in place; its age is what the scan_last_run_unix arm of stellarindex_galexie_archive_scan_degraded reads." >&2
  exit 1
fi
# The tally becomes a metric value; a non-count means nothing was validated, so refuse
# rather than coerce to 0.
case "${DROPPED:-}" in
  '' | *[!0-9]*)
    rm -f "$CHECKED"
    echo "galexie-archive-contiguity: exposition validator returned a non-numeric tally ('${DROPPED:-}') — refusing to publish. Previous galexie_archive_contiguity.prom left in place." >&2
    exit 1
    ;;
esac
# Guarded: a failed unchecked `mv` would leave $TMP holding the UNVALIDATED render for the
# publish below to ship.
if ! mv "$CHECKED" "$TMP"; then
  rm -f "$CHECKED"
  echo "galexie-archive-contiguity: could not swap in the validated render — refusing to publish. Previous galexie_archive_contiguity.prom left in place." >&2
  exit 1
fi
printf 'galexie_archive_unparseable_lines %s\n' "$DROPPED" >> "$TMP"

# node_exporter runs unprivileged and SILENTLY skips an unreadable textfile; set the mode
# explicitly rather than inheriting umask.
chmod 644 "$TMP"
mv "$TMP" "$OUT"
trap - EXIT

# Published; now be loud. A withheld line is a defect here or in what mc answered. The good
# samples are already on disk, so a non-zero exit only fails the oneshot, which the
# stellarindex_systemd_unit_failed ticket already watches (the unit is deliberately not in
# scripts/ci/unit-failed-dedicated.baseline).
if [ "$DROPPED" -gt 0 ]; then
  echo "galexie-archive-contiguity: published galexie_archive_contiguity.prom with $DROPPED unparseable line(s) withheld — galexie_archive_unparseable_lines carries the count" >&2
  exit 1
fi
