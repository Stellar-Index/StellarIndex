#!/usr/bin/env bash
# pg-chunk-scan — decode every compressed TimescaleDB chunk and publish the
# count that cannot be read, to pg_chunk_scan.prom.
#
# WHY. On r1 merge_chunks left 4 compressed chunks unreadable.
# count(*) answers from chunk metadata, so it reported nothing wrong; only a
# read of the compressed_data columns fails. This reads them.
#
# Cost: a chunk is decoded once per (compressed chunk id, relfilenode, rows
# written to the compressed relation). A rewrite gets a new relfilenode; a
# recompression that writes batches in place moves the write count. A
# statistics reset moves every count and costs one more pass. Verified keys
# (key@writes) live in $STATE, appended as each chunk passes and pruned to
# the chunks that still exist. The unreadable and backend-killer lists keep
# the bare key, so a statistics reset after a crash cannot put a killer back
# in the queue. An unreadable chunk is never recorded, so it stays counted until it is
# repaired. PG_CHUNK_SCAN_FULL=1 (or a missing state file) decodes everything.
#
# Each chunk is decoded on its own connection and transaction: the scan holds a
# lock on one chunk at a time, a chunk dropped mid-run costs only itself, and a
# chunk that kills its backend is retried once and then named as unreadable.
#
# It also lists fully compressed chunks whose indexes exceed 1 GiB. Their heap
# is near-empty, so healthy indexes are tens of MB; r1 had 13 at 3-26 GB each
# (161 GB) after a writer deleted the heap rows without rebuilding them.
#
# A run stops starting chunks after PG_CHUNK_SCAN_BUDGET seconds (a full r1
# pass is ~5.5h, past the unit's 4h timeout); the next night continues. Last
# run's unreadable chunks are decoded first, so a partial run cannot drop them
# from the count.
#
# Seams for scripts/ci/pg-chunk-scan-test.sh: TEXTFILE_DIR, PG_CHUNK_SCAN_STATE,
# PG_CHUNK_SCAN_PSQL, PG_CHUNK_SCAN_RECOVER_WAIT. The unit sets none of them.
set -uo pipefail
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
STATE="${PG_CHUNK_SCAN_STATE:-/var/lib/pg-chunk-scan/verified}"
# A superuser session in the app-owned database: only pg_catalog is searched, whatever search_path the
# database owner set. Keeping public on the path would not do: a closer-typed function there still wins.
export PGOPTIONS="${PGOPTIONS:+$PGOPTIONS }-c search_path=pg_catalog"
PSQL="${PG_CHUNK_SCAN_PSQL:-psql -X -q -d stellarindex -v ON_ERROR_STOP=1}"
FULL="${PG_CHUNK_SCAN_FULL:-0}"
BUDGET="${PG_CHUNK_SCAN_BUDGET:-10800}"
RECOVER_WAIT="${PG_CHUNK_SCAN_RECOVER_WAIT:-180}"
BADF="$STATE.bad"
# Chunks that ended their backend when decoded. A backend death restarts the whole cluster, so such a chunk is never
# decoded again; it stays counted unreadable while it exists. Delete its line to have it tried once more.
CRASHF="$STATE.crash"
OUT="$DIR/pg_chunk_scan.prom"
mkdir -p "$DIR" "$(dirname "$STATE")"
[ -s "$STATE" ] || FULL=1
touch "$STATE" "$BADF" "$CRASHF"

W=$(mktemp -d "${TMPDIR:-/tmp}/pg-chunk-scan.XXXXXX")
TMP=$(mktemp "$DIR/.pg_chunk_scan.prom.XXXXXX")
trap 'rm -rf "$W" "$TMP"' EXIT
: >"$W/skip"; : >"$W/bad"; : >"$W/rescanned"; : >"$W/err"; : >"$W/crash"
[ "$FULL" = 1 ] || cp "$STATE" "$W/skip"

# Keeps the previous verdicts and publishes only the failure, so the stale
# alert and the unit-failed alert both see it.
fail_run() {
  echo "pg-chunk-scan: $1" >&2
  tail -n 20 "$W/err" >&2
  cut -d'|' -f1 "$W/bad" >>"$BADF"
  {
    echo "# HELP stellarindex_pg_chunk_scan_success 1 if the last pg-chunk-scan run completed."
    echo "# TYPE stellarindex_pg_chunk_scan_success gauge"
    echo "stellarindex_pg_chunk_scan_success 0"
    grep -E '^stellarindex_pg_(compressed_chunk|chunk_scan_last_success)' "$OUT" 2>/dev/null
  } >"$TMP"
  chmod 0644 "$TMP"
  mv "$TMP" "$OUT"
  exit 1
}

# One statement: C|key|hypertable|chunk|compressed relation|writes, then
# B|hypertable|chunk|index bytes||.
$PSQL -At -F'|' -f - >"$W/list" 2>"$W/err" <<'SQL' || fail_run "listing chunks: psql exited $?"
SELECT tag, f1, f2, f3, f4, f5 FROM (
  SELECT 'C' AS tag, c.id AS ord,
         cc.id || ':' || pg_relation_filenode(format('%I.%I', cc.schema_name, cc.table_name)::regclass) AS f1,
         h.table_name::text AS f2, c.table_name::text AS f3,
         format('%I.%I', cc.schema_name, cc.table_name) AS f4,
         coalesce((SELECT s.n_tup_ins + s.n_tup_upd + s.n_tup_del FROM pg_stat_all_tables s
                    WHERE s.relid = format('%I.%I', cc.schema_name, cc.table_name)::regclass), 0)::text AS f5
    FROM _timescaledb_catalog.chunk c
    JOIN _timescaledb_catalog.hypertable h ON h.id = c.hypertable_id
    JOIN _timescaledb_catalog.chunk cc ON cc.id = c.compressed_chunk_id
  UNION ALL
  SELECT 'B', c.id, h.table_name::text, c.table_name::text,
         pg_indexes_size(format('%I.%I', c.schema_name, c.table_name)::regclass)::text, '', ''
    FROM _timescaledb_catalog.chunk c
    JOIN _timescaledb_catalog.hypertable h ON h.id = c.hypertable_id
   WHERE c.compressed_chunk_id IS NOT NULL AND c.status & 8 = 0
     AND pg_indexes_size(format('%I.%I', c.schema_name, c.table_name)::regclass) > 1073741824
) l ORDER BY tag DESC, ord;
SQL

# To decode: every listed chunk not yet verified, last run's unreadable first.
# A known backend-killer is not decoded: it goes straight to the unreadable list.
awk -F'|' -v crashf="$CRASHF" '
  BEGIN { while ((getline l < crashf) > 0) c[l] }
  $1 == "C" && ($2 in c) { print $2 "|" $3 "|" $4 }
' "$W/list" >"$W/crashers"
cat "$W/crashers" >>"$W/bad"
cut -d'|' -f1 "$W/crashers" >>"$W/rescanned"
awk -F'|' -v badf="$BADF" -v skipf="$W/skip" -v crashf="$CRASHF" '
  BEGIN { while ((getline l < badf) > 0) b[l]; while ((getline l < skipf) > 0) v[l]; while ((getline l < crashf) > 0) c[l] }
  $1 == "C" && !(($2 "@" $6) in v) && !($2 in c) { print (($2 in b) ? 0 : 1) "|" $2 "|" $3 "|" $4 "|" $5 "|" $6 }
' "$W/list" | sort -s -t'|' -k1,1n | cut -d'|' -f2- >"$W/todo"

# Prints ok, skip, bad or lost. A caught error that says nothing about the
# chunk's bytes (dropped mid-run, lock wait, out of resources) is a skip.
decode() {
  local rc=0 v cc=${1//\\/\\\\}
  # Session settings ride in PGOPTIONS, which splits on unescaped spaces.
  PGOPTIONS="-c search_path=pg_catalog -c lock_timeout=30s -c pgchunkscan.cc=${cc// /\\ }" \
    $PSQL -f - >/dev/null 2>"$W/err" <<'SQL' || rc=$?
DO $$
DECLARE cc text := current_setting('pgchunkscan.cc'); cols text; n bigint;
BEGIN
  SELECT string_agg(format('(SELECT count(*) FROM %s t, LATERAL _timescaledb_functions.compressed_data_info(t.%I) i)', cc, attname), '+')
    INTO cols
    FROM pg_attribute
   WHERE attrelid = cc::regclass AND attnum > 0 AND NOT attisdropped
     AND atttypid = '_timescaledb_internal.compressed_data'::regtype;
  IF cols IS NOT NULL THEN EXECUTE 'SELECT ' || cols INTO n; END IF;
  RAISE NOTICE 'CHUNK|ok';
EXCEPTION
  WHEN undefined_table OR lock_not_available OR deadlock_detected OR insufficient_resources THEN
    RAISE NOTICE 'CHUNK|skip|% %', SQLSTATE, SQLERRM;
  WHEN OTHERS THEN
    RAISE NOTICE 'CHUNK|bad|% %', SQLSTATE, SQLERRM;
END $$;
SQL
  v=$(sed -n 's/.*NOTICE:  CHUNK|\([a-z]*\).*/\1/p' "$W/err")
  v=${v%%$'\n'*}
  case "$v" in
    ok | skip | bad) echo "$v" ;;
    # 3 is a script error on a live connection (a cancel, say): undecided.
    *) if [ "$rc" -eq 3 ]; then echo skip; else echo lost; fi ;;
  esac
}

server_back() {
  local waited=0
  until $PSQL -c 'SELECT 1' </dev/null >/dev/null 2>>"$W/err"; do
    [ "$waited" -ge "$RECOVER_WAIT" ] && return 1
    sleep 5
    waited=$((waited + 5))
  done
}

start=$(date +%s)
partial=0 scanned=0 skipped=0
while IFS='|' read -r key ht chunk cc writes; do
  if [ $(($(date +%s) - start)) -ge "$BUDGET" ]; then
    partial=1
    break
  fi
  v=$(decode "$cc")
  if [ "$v" = lost ]; then
    # The connection died: the server went away, or this chunk killed its
    # backend. A second death on a server that is back up is the chunk's.
    server_back || fail_run "server unreachable while decoding $ht/$chunk"
    v=$(decode "$cc")
    if [ "$v" = lost ]; then
      server_back || fail_run "server unreachable while decoding $ht/$chunk"
      echo "pg-chunk-scan: $ht/$chunk ends its backend when decoded; counted unreadable and not decoded again" >&2
      echo "$key" >>"$W/crash"
      v=bad
    fi
  fi
  case "$v" in
    ok) echo "$key@$writes" >>"$STATE"; scanned=$((scanned + 1)) ;;
    bad)
      echo "$key|$ht|$chunk" >>"$W/bad"; scanned=$((scanned + 1))
      echo "pg-chunk-scan: $ht/$chunk unreadable: $(grep -m1 'CHUNK|bad' "$W/err")" >&2
      ;;
    *) skipped=$((skipped + 1)); echo "pg-chunk-scan: $ht/$chunk undecided: $(tail -n 1 "$W/err")" >&2 ;;
  esac
  [ "$v" = skip ] || echo "$key" >>"$W/rescanned"
done <"$W/todo"

# An unreadable chunk this run did not reach (budget) stays counted while it
# still exists.
awk -F'|' -v donef="$W/rescanned" -v badf="$BADF" '
  BEGIN { while ((getline l < donef) > 0) d[l]; while ((getline l < badf) > 0) b[l] }
  $1 == "C" && ($2 in b) && !($2 in d) { print $2 "|" $3 "|" $4 }
' "$W/list" >>"$W/bad"
sort -u "$W/bad" -o "$W/bad"
cut -d'|' -f1 "$W/bad" >"$BADF.new" && mv "$BADF.new" "$BADF"
# Crashers that still exist carry over; one that was dropped or rewritten falls out with its key.
{ cut -d'|' -f1 "$W/crashers"; cat "$W/crash"; } | sort -u >"$CRASHF.new" && mv "$CRASHF.new" "$CRASHF"
bad=$(cut -d'|' -f2,3 "$W/bad")
nbad=$(wc -l <"$W/bad" | tr -d ' ')
bloat=$(sed -n 's/^B|//p' "$W/list")
nbloat=0; [ -n "$bloat" ] && nbloat=$(printf '%s\n' "$bloat" | wc -l | tr -d ' ')

# Verified keys of chunks that no longer exist fall out on every run.
awk -F'|' -v statef="$STATE" '
  BEGIN { while ((getline l < statef) > 0) s[l] }
  $1 == "C" && (($2 "@" $6) in s) && !seen[$2]++ { print $2 "@" $6 }
' "$W/list" >"$STATE.new" && mv "$STATE.new" "$STATE"
now=$(date +%s)

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
  echo "# HELP stellarindex_pg_compressed_chunks_index_bloated Fully compressed chunks whose indexes exceed 1 GiB."
  echo "# TYPE stellarindex_pg_compressed_chunks_index_bloated gauge"
  echo "stellarindex_pg_compressed_chunks_index_bloated $nbloat"
  echo "# HELP stellarindex_pg_compressed_chunk_index_bytes Index bytes of each fully compressed chunk over 1 GiB."
  echo "# TYPE stellarindex_pg_compressed_chunk_index_bytes gauge"
  if [ -n "$bloat" ]; then
    printf '%s\n' "$bloat" | while IFS='|' read -r ht chunk idx; do
      echo "stellarindex_pg_compressed_chunk_index_bytes{hypertable=\"$ht\",chunk=\"$chunk\"} $idx"
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
echo "pg-chunk-scan: scanned=$scanned unreadable=$nbad undecided=$skipped index_bloated=$nbloat full=$FULL partial=$partial"
