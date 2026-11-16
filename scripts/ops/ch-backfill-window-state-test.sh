#!/usr/bin/env bash
# ch-backfill-window-state-test.sh — the resume state of ch-full-backfill.sh
# and ch-supply-flows-seed.sh: a window is skipped only when it was recorded
# through its current end and the lake still holds it.
#
# Hermetic: clickhouse-client and the ops binary are shims.
#
#   bash scripts/ops/ch-backfill-window-state-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
OPS_DIR="$PWD/scripts/ops"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"
# LAKE=full answers a count with the whole window, empty with 0, down fails.
cat > "$TMP/bin/clickhouse-client" <<'STUB'
#!/usr/bin/env bash
q="${*: -1}"
case "${LAKE:-full}" in
  down) exit 1 ;;
  empty) echo 0 ;;
  *)
    if [[ "$q" =~ BETWEEN\ ([0-9]+)\ AND\ ([0-9]+) ]]; then
      lo=${BASH_REMATCH[1]}; hi=${BASH_REMATCH[2]}
      [ "$lo" -lt 2 ] && lo=2
      echo $((hi - lo + 1))
    else
      echo 2500000
    fi
    ;;
esac
STUB
# Records "<from> <to>" for each call.
cat > "$TMP/bin/fake-ops" <<'STUB'
#!/usr/bin/env bash
from=; to=
while [ $# -gt 0 ]; do
  case "$1" in -from) from=$2; shift 2 ;; -to) to=$2; shift 2 ;; *) shift ;; esac
done
echo "$from $to" >> "$OPS_LOG"
STUB
chmod +x "$TMP/bin/clickhouse-client" "$TMP/bin/fake-ops"

pass=0
fail=0
# run <script> <state> <to> [LAKE]: runs the driver; calls land in $CALLS.
run() {
  CALLS="$TMP/calls"; : > "$CALLS"
  PATH="$TMP/bin:$PATH" OPS="$TMP/bin/fake-ops" OPS_LOG="$CALLS" LAKE="${4:-full}" \
    FROM=1 TO="$3" STATE="$2" bash "$OPS_DIR/$1" > "$TMP/out" 2>&1
  RC=$?
}
# expect <label> <rc> <calls, ';'-separated>
expect() {
  local got
  got="$(paste -sd';' "$CALLS")"
  if [ "$RC" -eq "$2" ] && [ "$got" = "$3" ]; then
    printf '  ok   %s\n' "$1"; pass=$((pass + 1))
  else
    printf '  FAIL %s: rc=%s calls=[%s], want rc=%s calls=[%s]\n' "$1" "$RC" "$got" "$2" "$3"
    sed 's/^/       /' "$TMP/out" | tail -n 8
    fail=$((fail + 1))
  fi
}

for script in ch-full-backfill.sh ch-supply-flows-seed.sh; do
  echo "$script"
  st="$TMP/$script.state"

  run "$script" "$st" 1500000
  expect "first run walks both windows" 0 "1 1000000;1000001 1500000"

  run "$script" "$st" 1500000
  expect "same range again does nothing" 0 ""

  run "$script" "$st" 1900000
  expect "a later TO redoes the window it lengthened" 0 "1000001 1900000"

  run "$script" "$st" 1900000 empty
  expect "a state file that outlived the lake skips nothing" 0 "1 1000000;1000001 1900000"

  run "$script" "$st" 1900000 down
  expect "an unreadable lake stops the run" 1 ""

  printf '1\n1000001\n' > "$st.old"
  run "$script" "$st.old" 1900000
  expect "a bare start counts only when its successor is recorded" 0 "1000001 1900000"
done

echo
if [ "$fail" -ne 0 ]; then
  echo "ch-backfill-window-state-test: $fail of $((pass + fail)) case(s) failed"
  exit 1
fi
echo "ch-backfill-window-state-test: $pass case(s) passed"
