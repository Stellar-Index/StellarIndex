#!/bin/bash
# galexie-archive-contiguity — partition-level gap scan of the ADR-0016
# R1 durable mirror (the DR keystone the multi-region plan's off-site
# copy will be pulled from).
#
# WHY: the 2026-08-21 DR review found the ratified HA plan claiming a
# FULL-history archive while the bucket actually holds the genesis
# partition [0,63999] plus [ARCHIVE_FROM=49984000 → tip] — the middle
# is a DELIBERATE capacity trim (recoverable only from
# aws-public-blockchain). That mismatch sat unnoticed because nothing
# recurring asserted the archive's SHAPE: tip-lag (#31) proves the
# newest edge advances, archive-fill proves the pipe runs, but a
# partition silently deleted (bad trim cutoff, fat-fingered mc rm,
# MinIO heal failure) or an overlap from a botched re-fill would only
# surface at restore time — the worst possible moment for a DR asset.
# This scan is the promised recurring gap-check: the EXPECTED coverage
# is declared, and anything else pages.
#
# Mechanics: one `mc ls` of the bucket's top-level partition dirs
# (`<reverse-hex>--<start>-<end>/`, 64k ledgers each; ~220 rows —
# cheap). Sort by start ledger, walk the ranges, and count every
# discontinuity that is NOT the declared trim window. Overlaps count
# as unexpected too (a re-fill writing a partition that straddles an
# existing one is corruption, not coverage). Chunk-level holes INSIDE
# a partition are out of scope here — that is restore-drill territory
# (deploy/monitoring/rules/restore-drill.yml); this guards the
# coarse shape cheaply every hour.
#
# The declared trim is overridable via /etc/default/stellarindex-galexie
# style env (EXPECTED_TRIM="<gap_start>-<gap_end>", set EXPECTED_TRIM=""
# once the middle range is backfilled and the archive really is
# full-history — the scan then requires strict contiguity).
#
# The scan also reports ITS OWN health — galexie_archive_scan_ok,
# _scan_listing_lines and _scan_last_run_unix, written on every run —
# because its consumer stellarindex_galexie_archive_gap is severity
# `page` and a failed bucket read used to produce a verdict that was
# indistinguishable from a healthy one. See the read below, and
# stellarindex_galexie_archive_scan_degraded in both rule trees.
# Fixtures: scripts/ci/galexie-archive-contiguity-test.sh

set -uo pipefail

TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
OUT="$TEXTFILE_DIR/galexie_archive_contiguity.prom"
TMP="$OUT.tmp.$$"
trap 'rm -f "$TMP"' EXIT

BUCKET="${BUCKET:-local/galexie-archive}"
# Ledger range NOT expected in the archive (the ADR capacity trim):
# first missing ledger, last missing ledger. Empty string = expect
# strict contiguity from first partition to tip.
# NOTE ${VAR-default}, not ${VAR:-default}: an explicitly EMPTY
# EXPECTED_TRIM is the documented "strict full-history" mode and must
# not fall back to the default trim.
EXPECTED_TRIM="${EXPECTED_TRIM-64000-49983999}"

# Buffer the listing before parsing (mc ls + early-exiting pipe reader
# → SIGPIPE aborts under pipefail; see galexie-archive-tip-lag.sh).
#
# THE READ'S SUCCESS AND ITS CONTENT ARE SEPARATE NUMBERS. This line
# ended in `|| true`, which threw mc's exit status away, so a read that
# died PARTWAY through the listing handed the parser a truncated but
# perfectly contiguous prefix — bad=0, unexpected_gaps 0, a clean
# DR verdict derived from a failed read. Reproduced 2026-09-05 against
# these exact bytes: a listing cut after 4 of 11 partitions wrote
# `galexie_archive_unexpected_gaps 0`, byte-identical to the healthy
# answer, and stellarindex_galexie_archive_gap (severity `page`) is the
# ONLY recurring guard on this bucket's shape.
listing=""
read_rc=0
listing=$(mc ls "$BUCKET/" 2>/dev/null) || read_rc=$?

# Raw rows the listing returned, published alongside the status. It is
# what separates "the mirror is empty" (0) from "mc answered in a shape
# this scan can no longer parse" (>0) when the partition count is zero;
# both reach the same page below, and the responder cannot otherwise
# tell a deleted archive from a parser that stopped matching.
listing_lines=0
if [ -n "$listing" ]; then
  listing_lines=$(printf '%s\n' "$listing" | wc -l | tr -d ' ')
fi

# Left empty when the read failed, and then NOT emitted at all. The
# alternative is publishing a number nothing measured: a truncated read
# fabricates "no gaps" and a dead read fabricates a DR-corruption page
# that sends the responder to the wrong runbook. galexie_archive_scan_ok
# is what speaks for a run that could not look.
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
    # A SUCCESSFUL read that yielded no partitions — the bucket is empty,
    # or mc answered in a shape the parser no longer matches. Both are DR
    # events and both keep the shape that FIRES, exactly as shipped;
    # what changed is only that a FAILED read no longer lands here.
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

# ── The scan's own health, written on EVERY run ──────────────────────
#
#   _scan_ok            1 iff `mc ls` exited 0. Zero means the four
#                       series above are absent because nothing was
#                       measured, not because the archive is fine.
#   _scan_listing_lines rows that read returned (see above).
#   _scan_last_run_unix when this file was last WRITTEN. Deliberately
#                       not _last_success_unix: it stamps the RUN, so a
#                       run whose read failed still refreshes it and
#                       _scan_ok is what says the data is bad. It is
#                       also the only thing that catches the scan not
#                       running at all — node_exporter keeps serving the
#                       LAST file it saw, so a dead timer leaves
#                       fresh-looking scrapes of a frozen zero-gaps
#                       verdict. stellarindex_galexie_archive_scan_degraded
#                       reads all three.
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
EOF
mv "$TMP" "$OUT"
