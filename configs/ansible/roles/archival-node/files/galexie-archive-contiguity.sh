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
listing=$(mc ls "$BUCKET/" 2>/dev/null) || true

# One "start end" pair per partition dir, sorted numerically by start.
ranges=$(printf '%s\n' "$listing" \
  | grep -oE -- '--[0-9]+-[0-9]+/' \
  | sed -E 's/^--([0-9]+)-([0-9]+)\/$/\1 \2/' \
  | sort -n -k1,1)

if [ -z "$ranges" ]; then
  # Empty/unlistable bucket: emit a shape that FIRES (partition_count 0
  # + the absent-metric alert would otherwise be the only net).
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

mkdir -p "$TEXTFILE_DIR"
cat > "$TMP" <<EOF
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
mv "$TMP" "$OUT"
