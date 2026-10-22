#!/usr/bin/env bash
# Self-test for lint-chunk-functions.sh: the 2026-10-21 merge_chunks call and
# a drop_chunks call are caught; a DO NOT RUN warning, markdown prose and the
# reversible compress/decompress pair stay quiet.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
lint="$here/lint-chunk-functions.sh"
fail=0
run() { # run <want-exit> <label> <file> <content>
  local tmp; tmp=$(mktemp -d)
  (cd "$tmp" && env -u GIT_DIR -u GIT_INDEX_FILE -u GIT_WORK_TREE git init -q . && mkdir -p "$(dirname "$3")" && printf '%s\n' "$4" >"$3" \
    && env -u GIT_DIR -u GIT_INDEX_FILE -u GIT_WORK_TREE git add "$3")
  local rc=0
  env -u GIT_DIR -u GIT_INDEX_FILE -u GIT_WORK_TREE LINT_ROOT="$tmp" "$lint" >/dev/null 2>&1 </dev/null || rc=$?
  if [ "$rc" -ne "$1" ]; then echo "FAIL: $2 (exit $rc, want $1)"; fail=1; else echo "ok: $2"; fi
  rm -rf "$tmp"
}
run 1 "merge_chunks in SQL"     repair.sql "CALL merge_chunks('_timescaledb_internal._hyper_1_1_chunk', '_timescaledb_internal._hyper_1_2_chunk');"
run 1 "drop_chunks in Go"       x.go       'const q = `SELECT drop_chunks('"'"'trades'"'"', older_than => now() - interval '"'"'1 year'"'"')`'
run 1 "merge_chunks in a script" ops.sh    'psql -c "SELECT merge_chunks (a, b)"'
run 0 "DO NOT RUN warning"      w.sql      "-- DO NOT RUN: CALL merge_chunks(a, b); corrupts compressed chunks"
run 0 "markdown prose"          doc.md     "Never call merge_chunks(a, b) on a compressed chunk."
run 0 "compress/decompress"     c.go       "const q = \`SELECT decompress_chunk(\$1); SELECT compress_chunk(\$1)\`"
run 1 "drop_chunks in a new migration" migrations/0300_x.up.sql "SELECT drop_chunks('trades', older_than => now());"
run 0 "drop_chunks in a test"   y_test.go  "exec(\"SELECT drop_chunks('prices_1m', older_than => now())\")"
run 0 "add_retention_policy"    r.sql      "SELECT add_retention_policy('prices_1m', INTERVAL '90 days');"
exit "$fail"
