#!/usr/bin/env bash
# protocol-upgrade-drill-test.sh — proves the drill FAILs on a green/live
# mismatch, an HTTP error, an old protocol and refuses an empty golden list,
# and PASSes on identical stacks that differ only in as_of.
#
# curl is stubbed via PATH: each URL maps to a fixture file in $STUBDIR.
# Run: bash scripts/ops/protocol-upgrade-drill-test.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
command -v python3 >/dev/null 2>&1 || { echo "python3 required" >&2; exit 2; }
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in -o|-w|--max-time) [ "$1" = -o ] && out="$2"; shift 2 ;; -s) shift ;; *) url="$1"; shift ;; esac
done
f="${url//\//_}"; f="${f//:/_}"
if [ -r "$STUBDIR/$f" ]; then cp "$STUBDIR/$f" "$out"; printf 200; else : > "$out"; printf 404; fi
STUB
chmod +x "$TMP/bin/curl"
export PATH="$TMP/bin:$PATH"
export STUBDIR="$TMP/fx"; mkdir -p "$STUBDIR"
fx() { local f="${1//\//_}"; f="${f//:/_}"; printf '%s' "$2" > "$STUBDIR/$f"; }
hdr() { printf '{"data":{"sequence":100,"protocol_version":%s,"tx_count":%s},"as_of":"%s","flags":{"stale":false}}' "$1" "$2" "$3"; }
for side in live green; do
  fx "http://$side/v1/ledgers/100/transactions?limit=1000" '{"data":[]}'
  fx "http://$side/v1/ledgers/100/operations?limit=2000" '{"data":[]}'
done
fx http://live/v1/ledgers/100 "$(hdr 29 3 t1)"
fx http://green/v1/ledgers/100 "$(hdr 29 3 t2)"
printf '# golden\n100\n' > "$TMP/g.txt"; : > "$TMP/empty.txt"

# shellcheck source=scripts/ops/protocol-upgrade-drill.sh
. scripts/ops/protocol-upgrade-drill.sh
export LIVE_URL=http://live/v1 GREEN_URL=http://green/v1
pass=0; fail=0
chk() { if [ "$2" = "$3" ]; then echo "  ok   $1"; pass=$((pass+1)); else echo "  FAIL $1 (want $3 got $2)"; fail=$((fail+1)); fi; }

drill_main "$TMP/g.txt" >/dev/null; chk "identical stacks pass" $? 0
DRILL_MIN_PROTOCOL=29 drill_main "$TMP/g.txt" >/dev/null; chk "protocol floor met" $? 0
DRILL_MIN_PROTOCOL=30 drill_main "$TMP/g.txt" >/dev/null; chk "old protocol fails" $? 1
fx http://green/v1/ledgers/100 "$(hdr 29 4 t2)"
drill_main "$TMP/g.txt" >/dev/null; chk "mismatch fails" $? 1
rm -f "$STUBDIR/http___green_v1_ledgers_100"
drill_main "$TMP/g.txt" >/dev/null; chk "green 404 fails" $? 1
drill_main "$TMP/empty.txt" >/dev/null 2>&1; chk "empty list refused" $? 2
GREEN_URL="" drill_main "$TMP/g.txt" >/dev/null 2>&1; chk "missing base refused" $? 2
for side in live green; do
  fx "http://$side/v1/ledgers/100" "$(hdr 29 3 t1)"
done
fx "http://green/v1/ledgers/100/transactions?limit=1000" '{"data":{"items":[],"truncated":true}}'
fx "http://live/v1/ledgers/100/transactions?limit=1000" '{"data":{"items":[],"truncated":true}}'
drill_main "$TMP/g.txt" >/dev/null; chk "truncated list fails" $? 1
fx "http://live/v1/ledgers/100/transactions?limit=1000" '{"data":[]}'
fx "http://green/v1/ledgers/100/transactions?limit=1000" '{"data":[]}'
drill_main "$TMP/g.txt" >/dev/null; chk "untruncated recovers" $? 0
rm -f "$STUBDIR/http___live_v1_ledgers_100" "$STUBDIR/http___green_v1_ledgers_100"
drill_main "$TMP/g.txt" >/dev/null; chk "both sides erroring fails" $? 1
echo "pass=$pass fail=$fail"; exit "$fail"
