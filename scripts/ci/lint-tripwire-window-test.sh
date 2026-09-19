#!/usr/bin/env bash
# lint-tripwire-window-test.sh — fixtures for the `for:` == event-window gate
# (scripts/ci/lint-tripwire-window.py, audit Q261).
#
# The gate's in-process `--self-test` covers the expression shapes. This
# script covers what only a real invocation can: exit codes, directory
# handling, the self-accounting line, the wiring, and — the case that
# matters — that the gate goes RED on the SHIPPED rule files when one
# tripwire's `for:` is put back to its window. A gate never seen to fail on
# the real input is not known to work.
#
#   1. `--self-test` passes.
#   2. THE INCIDENT SHAPES, as fixture files: `increase(m[15m]) > 0` with
#      `for: 15m`, and the api.yml webhook twin `sum(rate(..[1h])) > 0` with
#      `for: 1h`, are each rejected by name; the `for: 0m` fix passes.
#   3. WAIVERS: a reasoned waiver passes and is COUNTED; a bare marker, and a
#      waiver left on a rule that no longer has the shape, both fail.
#   4. VACUITY: a missing directory and an empty one fail; a tree with no
#      alert rule at all exits 2 rather than reporting a pass over nothing.
#   5. THE REAL TREES pass, and report what they waived.
#   6. MUTATION OF THE REAL FILES: each shipped tree, with
#      stellarindex_ingestion_trade_buffer_drop's `for: 0m` put back to
#      `for: 15m`, fails and names that alert. Same for the webhook rule.
#   7. WIRING: lint-rule-structure.py runs the gate (self-test first), and
#      fails when the gate fails.
#
# Run: bash scripts/ci/lint-tripwire-window-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROOT="$PWD"
SRC="$ROOT/scripts/ci/lint-tripwire-window.py"
[ -f "$SRC" ] || { echo "lint-tripwire-window-test: gate not found at $SRC" >&2; exit 2; }
command -v python3 >/dev/null || { echo "lint-tripwire-window-test: python3 not found" >&2; exit 2; }

TMP="$(mktemp -d)" || exit 2
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }
expect_rc()   { if [ "$rc" -eq "$1" ]; then ok "$2 (rc=$rc)"; else bad "$2 (expected rc $1, got $rc)"; echo "$out" | sed -n '1,12s/^/      | /p'; fi; }
expect_hit()  { if grep -q -- "$1" <<<"$out"; then ok "$2"; else bad "$2 (missing: $1)"; echo "$out" | sed -n '1,12s/^/      | /p'; fi; }
expect_miss() { if grep -q -- "$1" <<<"$out"; then bad "$2 (unexpected: $1)"; echo "$out" | sed -n '1,12s/^/      | /p'; else ok "$2"; fi; }
run() { out="$(python3 -B "$SRC" "$@" 2>&1)"; rc=$?; }

# rule <dir> <file> <alert> <for> <comment-or-empty> <expr>
rule() {
  mkdir -p "$1"
  {
    echo "groups:"
    echo "  - name: fixture"
    echo "    rules:"
    [ -n "$5" ] && echo "      $5"
    echo "      - alert: $3"
    echo "        expr: |"
    echo "          $6"
    echo "        for: $4"
  } >"$1/$2"
}

echo "1. in-process self-test"
run --self-test
expect_rc 0 "--self-test passes"
expect_hit "self-test OK" "and says how many cases it ran"

echo "2. the incident shapes"
rule "$TMP/inc" a.yml fx_persist_drop 15m "" 'increase(fx_errors_total{kind="dropped"}[15m]) > 0'
run "$TMP/inc"
expect_rc 1 "increase[15m] > 0 with for: 15m is rejected"
expect_hit "fx_persist_drop" "the alert is named"
expect_hit "cannot fire on a single event" "the message says what is wrong"

rule "$TMP/rate" a.yml fx_webhook_exhausted 1h "" 'sum(rate(fx_attempts_total{outcome="exhausted"}[1h])) > 0'
run "$TMP/rate"
expect_rc 1 "the rate() twin under sum() with for: 1h is rejected"
expect_hit "fx_webhook_exhausted" "the alert is named"

rule "$TMP/fixed" a.yml fx_webhook_exhausted 0m "" 'sum(rate(fx_attempts_total{outcome="exhausted"}[1h])) > 0'
run "$TMP/fixed"
expect_rc 0 "the same rule at for: 0m passes"
expect_hit "1 alert rule(s) in 1 file(s)" "self-accounting line counts what was read"

echo "3. waivers"
rule "$TMP/waived" a.yml fx_sink_drops 10m \
  "# lint-tripwire-window:sustained: drops are normal in rare bursts, only continuous dropping tickets" \
  'increase(fx_dropped_total[10m]) > 0'
run "$TMP/waived"
expect_rc 0 "a reasoned waiver passes"
expect_hit "1 waived as sustained" "and is counted"

rule "$TMP/bare" a.yml fx_sink_drops 10m "# lint-tripwire-window:sustained:" 'increase(fx_dropped_total[10m]) > 0'
run "$TMP/bare"
expect_rc 1 "a bare marker with no reason waives nothing"
expect_hit "needs a reason" "and says why"

rule "$TMP/stale" a.yml fx_sink_drops 0m \
  "# lint-tripwire-window:sustained: drops are normal in rare bursts, only continuous dropping tickets" \
  'increase(fx_dropped_total[10m]) > 0'
run "$TMP/stale"
expect_rc 1 "a waiver on a rule without the shape is rejected"
expect_hit "stale waiver" "and is called stale"

echo "4. vacuity"
run "$TMP/does-not-exist"
expect_rc 1 "a missing directory fails"
mkdir -p "$TMP/empty"
run "$TMP/empty"
expect_rc 1 "a directory with no *.yml fails"
mkdir -p "$TMP/records"
printf 'groups:\n  - name: r\n    rules:\n      - record: fx:rate5m\n        expr: rate(fx_total[5m])\n' >"$TMP/records/r.yml"
run "$TMP/records"
expect_rc 2 "a tree with no alert rule exits 2, not 0"

echo "5. the shipped trees"
run
expect_rc 0 "deploy/monitoring/rules + configs/prometheus/rules.r1 pass"
expect_hit "over 2 dir(s)" "both trees were read"
expect_miss " 0 waived" "the sustained-signal waivers were seen"

echo "6. mutation of the shipped files"
# put_back <tree-dir> <file> <alert> <for> — copy the tree, restore one for:.
put_back() {
  local dst
  dst="$TMP/mut-$(basename "$1")-$3"
  mkdir -p "$dst"
  cp "$1"/*.yml "$dst/"
  python3 -B - "$dst/$2" "$3" "$4" <<'PY' || return 1
import re, sys
path, alert, for_ = sys.argv[1:4]
text = open(path, encoding="utf-8").read()
pat = re.compile(r"(- alert: %s\n(?:(?!\s+- alert:).*\n)*?\s+for: )0m\n" % re.escape(alert))
new, n = pat.subn(lambda m: m.group(1) + for_ + "\n", text)
if n != 1:
    sys.exit("fixture: expected exactly one `for: 0m` under %s, found %d" % (alert, n))
open(path, "w", encoding="utf-8").write(new)
PY
  echo "$dst"
}
for tree in deploy/monitoring/rules configs/prometheus/rules.r1; do
  if dst="$(put_back "$tree" ingestion.yml stellarindex_ingestion_trade_buffer_drop 15m)"; then
    run "$dst"
    expect_rc 1 "$tree: trade_buffer_drop put back to for: 15m is rejected"
    expect_hit "alert 'stellarindex_ingestion_trade_buffer_drop'" "$tree: and it is the alert named"
  else
    bad "$tree: could not build the trade_buffer_drop mutation"
  fi
  if dst="$(put_back "$tree" api.yml stellarindex_customer_webhook_delivery_exhausted 1h)"; then
    run "$dst"
    expect_rc 1 "$tree: webhook_delivery_exhausted put back to for: 1h is rejected"
    expect_hit "alert 'stellarindex_customer_webhook_delivery_exhausted'" "$tree: and it is the alert named"
  else
    bad "$tree: could not build the webhook mutation"
  fi
done

echo "7. wiring through lint-rule-structure.py"
out="$(python3 -B "$ROOT/scripts/ci/lint-rule-structure.py" 2>&1)"; rc=$?
expect_rc 0 "lint-rule-structure passes on the shipped trees"
expect_hit "lint-tripwire-window: self-test OK" "it ran the gate's self-test"
expect_hit "lint-tripwire-window: OK" "it ran the gate over the trees"
# A failing gate must fail the host lint. Run a copy of the host beside a
# stand-in gate that always exits 1.
mkdir -p "$TMP/wire/scripts/ci"
cp "$ROOT/scripts/ci/lint-rule-structure.py" "$TMP/wire/scripts/ci/"
printf 'import sys\nprint("stand-in gate: red")\nsys.exit(1)\n' >"$TMP/wire/scripts/ci/lint-tripwire-window.py"
out="$(python3 -B "$TMP/wire/scripts/ci/lint-rule-structure.py" 2>&1)"; rc=$?
expect_rc 1 "lint-rule-structure fails when the gate fails"
expect_hit "stand-in gate: red" "and shows the gate's output"
rm "$TMP/wire/scripts/ci/lint-tripwire-window.py"
out="$(python3 -B "$TMP/wire/scripts/ci/lint-rule-structure.py" 2>&1)"; rc=$?
expect_rc 1 "lint-rule-structure fails when the gate script is missing"

echo
echo "lint-tripwire-window-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
