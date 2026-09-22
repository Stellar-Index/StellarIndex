#!/usr/bin/env bash
# reconcile-supply-vs-horizon-test.sh — proves that a 5xx / non-JSON
# response from OUR OWN API (not Horizon) no longer aborts the whole
# reconciliation run (F083).
#
# reconcile-supply-vs-horizon.sh runs under `set -euo pipefail`. The
# Horizon fetch at the top of the loop uses `curl -sf`, so an HTTP >=400
# from Horizon is a curl failure the script already handles explicitly
# (INCONCLUSIVE(horizon)). The "ours" fetch used to use plain `curl -s`:
# an HTTP 5xx from our own API is NOT a curl failure, the body is
# non-JSON, `jq -r` fails to parse it, and pipefail propagates that
# failure through the `ours=$(curl ... | jq ...)` assignment — which
# `set -e` treats as the whole script failing, before it ever reaches
# the `[ -z "$ours" ]` SKIP(no-data) branch or prints the documented
# summary line. One flaky 500 from our own API silently killed the
# reconciliation instead of skipping the affected asset.
#
# curl is stubbed on PATH so this needs no network. The stub honors the
# real `-f` semantics (HTTP failure -> non-zero exit 22, nothing on
# stdout; no `-f` -> the error body is handed to the caller on stdout
# with exit 0) — exactly what distinguishes the fixed "ours" fetch from
# the old one, so this exercises the SHIPPED script's actual curl
# invocation rather than a synthetic stand-in.
#
# Run: bash scripts/ops/reconcile-supply-vs-horizon-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/scripts/ops/reconcile-supply-vs-horizon.sh"
[[ -r "$SRC" ]] || { echo "reconcile-supply-vs-horizon-test: missing $SRC" >&2; exit 2; }

command -v jq >/dev/null 2>&1 || { echo "reconcile-supply-vs-horizon-test: jq required" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── curl stub: routes on URL, honors real -f semantics ──────────────
# Horizon always answers a valid, reconciling record (Horizon is not
# what this test is about). Our own API always answers an HTML 5xx page
# for the "ours" endpoint — whether that becomes a curl failure (exit
# 22, no stdout) or a swallowed 200-shaped body depends on whether the
# invocation actually passed `-f`, i.e. on the script's OWN code, not on
# a test-side knob.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/curl" <<'SH'
#!/usr/bin/env bash
url=""
strict=0
for a in "$@"; do
  case "$a" in
    http*://*) url="$a" ;;
    -*f*) strict=1 ;;
  esac
done
case "$url" in
  */assets\?*)
    printf '%s' "$HORIZON_BODY"
    exit 0
    ;;
  */v1/assets/*)
    if [[ "$strict" == 1 ]]; then
      exit 22
    fi
    printf '%s' "$API_5XX_BODY"
    exit 0
    ;;
esac
echo "stub curl: unrecognized url: $url" >&2
exit 1
SH
chmod +x "$WORK/bin/curl"
export PATH="$WORK/bin:$PATH"

export HORIZON_BODY='{"_embedded":{"records":[{"balances":{"authorized":"1000.0000000"},"claimable_balances_amount":"0.0000000","liquidity_pools_amount":"0.0000000","contracts_amount":"0.0000000"}]}}'
export API_5XX_BODY='<html><body>Internal Server Error</body></html>'

echo "reconcile-supply-vs-horizon-test: our own API 500s for every asset"
OUT="$WORK/out.txt"
ERR="$WORK/err.txt"
bash "$SRC" >"$OUT" 2>"$ERR"
rc=$?

if [[ $rc -eq 0 ]]; then
  ok "a 5xx from our own API does not abort the run (rc=0)"
else
  bad "rc=$rc (want 0): a 5xx from our own API still aborts the whole reconciliation — stderr: $(cat "$ERR")"
fi

if grep -qE '^AQUA .*SKIP\(no-data\)' "$OUT"; then
  ok "the affected asset is reported SKIP(no-data) instead of crashing the run"
else
  bad "no SKIP(no-data) line for AQUA; output was:\n$(cat "$OUT")"
fi

if grep -q '^assets=8 outside_tolerance=0 inconclusive=0$' "$OUT"; then
  ok "the documented summary line is printed and the run completes for all 8 assets"
else
  bad "summary line missing or wrong; output was:\n$(cat "$OUT")"
fi

echo
echo "reconcile-supply-vs-horizon-test: pass=$pass fail=$fail"
[[ $fail -eq 0 ]]
