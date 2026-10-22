#!/usr/bin/env bash
# pg-chunk-scan — decode every compressed TimescaleDB chunk and publish the
# count that cannot be read, to pg_chunk_scan.prom.
#
# WHY. On r1 merge_chunks left 4 compressed chunks unreadable.
# count(*) answers from chunk metadata, so it reported nothing wrong; only a
# read of the compressed_data columns fails. This reads them.
#
# Cost: a chunk is decoded once per (compressed chunk id, relfilenode); a
# rewrite gets a new relfilenode and is decoded again. Verified keys live in
# $STATE. An unreadable chunk is never recorded, so it stays counted until it is
# repaired. PG_CHUNK_SCAN_FULL=1 (or a missing state file) decodes everything.
#
# A run stops starting chunks after PG_CHUNK_SCAN_BUDGET seconds (a full r1
# pass is ~5.5h, past the unit's 4h timeout) and keeps what it verified; the
# next night continues. Last run's unreadable chunks are decoded first, so a
# partial run cannot drop them from the count.
#
# Seams for scripts/ci/pg-chunk-scan-test.sh: TEXTFILE_DIR, PG_CHUNK_SCAN_STATE,
# PG_CHUNK_SCAN_PSQL. The unit sets none of them.
set -uo pipefail
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
STATE="${PG_CHUNK_SCAN_STATE:-/var/lib/pg-chunk-scan/verified}"
PSQL="${PG_CHUNK_SCAN_PSQL:-psql -X -q -d stellarindex -v ON_ERROR_STOP=1}"
FULL="${PG_CHUNK_SCAN_FULL:-0}"
BUDGET="${PG_CHUNK_SCAN_BUDGET:-10800}"
BADF="$STATE.bad"
OUT="$DIR/pg_chunk_scan.prom"
mkdir -p "$DIR" "$(dirname "$STATE")"
[ -s "$STATE" ] || FULL=1
skip='{}'
if [ "$FULL" != 1 ]; then
  skip="{$(paste -sd, "$STATE")}"
fi
first="{$(paste -sd, "$BADF" 2>/dev/null)}"

RAW=$(mktemp "$DIR/.pg_chunk_scan.raw.XXXXXX")
TMP=$(mktemp "$DIR/.pg_chunk_scan.prom.XXXXXX")
trap 'rm -f "$RAW" "$TMP"' EXIT

# Notices carry the per-chunk verdicts; psql's own errors land beside them and
# fail the run through ON_ERROR_STOP.
rc=0
$PSQL -v skip="$skip" -v first="$first" -v budget="$BUDGET" -f - >"$RAW" 2>&1 <<'SQL' || rc=$?
SET pgchunkscan.skip = :'skip';
SET pgchunkscan.first = :'first';
SET pgchunkscan.budget = :'budget';
DO $$
DECLARE ch record; cols text; n bigint; key text;
  skip text[] := current_setting('pgchunkscan.skip')::text[];
  first text[] := current_setting('pgchunkscan.first')::text[];
  deadline timestamptz := clock_timestamp() + make_interval(secs => current_setting('pgchunkscan.budget')::int);
BEGIN
 FOR ch IN
  SELECT h.table_name AS ht, c.table_name AS chunk,
         format('%I.%I', cc.schema_name, cc.table_name) AS cc,
         cc.id || ':' || pg_relation_filenode(format('%I.%I', cc.schema_name, cc.table_name)::regclass) AS key
    FROM _timescaledb_catalog.chunk c
    JOIN _timescaledb_catalog.hypertable h ON h.id = c.hypertable_id
    JOIN _timescaledb_catalog.chunk cc ON cc.id = c.compressed_chunk_id
   ORDER BY (cc.id || ':' || pg_relation_filenode(format('%I.%I', cc.schema_name, cc.table_name)::regclass)) = ANY(first) DESC, c.id
 LOOP
  CONTINUE WHEN ch.key = ANY(skip);
  IF clock_timestamp() > deadline THEN
   RAISE NOTICE 'SCAN|partial';
   EXIT;
  END IF;
  SELECT string_agg(format('(SELECT count(*) FROM %s t, LATERAL _timescaledb_functions.compressed_data_info(t.%I) i)', ch.cc, attname), '+')
    INTO cols
    FROM pg_attribute
   WHERE attrelid = ch.cc::regclass AND attnum > 0 AND NOT attisdropped
     AND atttypid = '_timescaledb_internal.compressed_data'::regtype;
  IF cols IS NULL THEN CONTINUE; END IF;
  BEGIN
   EXECUTE 'SELECT ' || cols INTO n;
   RAISE NOTICE 'CHUNK|ok|%|%|%', ch.key, ch.ht, ch.chunk;
  EXCEPTION WHEN OTHERS THEN
   RAISE NOTICE 'CHUNK|bad|%|%|%', ch.key, ch.ht, ch.chunk;
  END;
 END LOOP;
END $$;
SQL

now=$(date +%s)
if [ "$rc" -ne 0 ]; then
  echo "pg-chunk-scan: psql exited $rc:" >&2
  # Not just ERROR lines: a connection or auth failure has none.
  grep -v 'NOTICE:  CHUNK|' "$RAW" | tail -n 20 >&2
  # Keep the previous verdicts; publish only the failure so the stale alert
  # and the unit-failed alert both see it.
  {
    echo "# HELP stellarindex_pg_chunk_scan_success 1 if the last pg-chunk-scan run completed."
    echo "# TYPE stellarindex_pg_chunk_scan_success gauge"
    echo "stellarindex_pg_chunk_scan_success 0"
    grep -E '^stellarindex_pg_(compressed_chunk|chunk_scan_last_success)' "$OUT" 2>/dev/null
  } >"$TMP"
  chmod 0644 "$TMP"
  mv "$TMP" "$OUT"
  exit 1
fi

# psql prefixes script notices with "psql:<file>:<line>: ".
V=$(sed -n 's/.*NOTICE:  CHUNK|/CHUNK|/p' "$RAW")
ok_keys=$(printf '%s\n' "$V" | grep '^CHUNK|ok|' | cut -d'|' -f3)
bad=$(printf '%s\n' "$V" | grep '^CHUNK|bad|' | cut -d'|' -f4,5 | sort -u)
scanned=$(printf '%s\n' "$V" | grep -c '^CHUNK|')
nbad=0; [ -n "$bad" ] && nbad=$(printf '%s\n' "$bad" | wc -l | tr -d ' ')
partial=0; grep -q 'NOTICE:  SCAN|partial' "$RAW" && partial=1
printf '%s\n' "$V" | grep '^CHUNK|bad|' | cut -d'|' -f3 | sort -u >"$BADF"

# Verified keys: a completed full run replaces the set (dropped chunks fall
# out); any other run adds to it.
{
  [ "$FULL" = 1 ] && [ "$partial" = 0 ] || cat "$STATE" 2>/dev/null
  [ -n "$ok_keys" ] && printf '%s\n' "$ok_keys"
} | sort -u >"$STATE.new" && mv "$STATE.new" "$STATE"

{
  echo "# HELP stellarindex_pg_compressed_chunks_unreadable Compressed chunks whose compressed_data cannot be decoded."
  echo "# TYPE stellarindex_pg_compressed_chunks_unreadable gauge"
  echo "stellarindex_pg_compressed_chunks_unreadable $nbad"
  echo "# HELP stellarindex_pg_compressed_chunk_unreadable 1 per unreadable compressed chunk."
  echo "# TYPE stellarindex_pg_compressed_chunk_unreadable gauge"
  if [ -n "$bad" ]; then
    printf '%s\n' "$bad" | while IFS='|' read -r ht chunk; do
      echo "stellarindex_pg_compressed_chunk_unreadable{hypertable=\"$ht\",chunk=\"$chunk\"} 1"
    done
  fi
  echo "# HELP stellarindex_pg_compressed_chunks_scanned Compressed chunks decoded by the last run."
  echo "# TYPE stellarindex_pg_compressed_chunks_scanned gauge"
  echo "stellarindex_pg_compressed_chunks_scanned $scanned"
  echo "# HELP stellarindex_pg_chunk_scan_partial 1 if the last run hit its time budget before every chunk was decoded."
  echo "# TYPE stellarindex_pg_chunk_scan_partial gauge"
  echo "stellarindex_pg_chunk_scan_partial $partial"
  echo "# HELP stellarindex_pg_chunk_scan_success 1 if the last pg-chunk-scan run completed."
  echo "# TYPE stellarindex_pg_chunk_scan_success gauge"
  echo "stellarindex_pg_chunk_scan_success 1"
  echo "# HELP stellarindex_pg_chunk_scan_last_success_timestamp_seconds Unix time of the last completed run."
  echo "# TYPE stellarindex_pg_chunk_scan_last_success_timestamp_seconds gauge"
  echo "stellarindex_pg_chunk_scan_last_success_timestamp_seconds $now"
} >"$TMP"
chmod 0644 "$TMP"
mv "$TMP" "$OUT"
echo "pg-chunk-scan: scanned=$scanned unreadable=$nbad full=$FULL partial=$partial"
