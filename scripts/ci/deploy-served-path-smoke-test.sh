#!/usr/bin/env bash
# deploy-served-path-smoke-test.sh — deploy.yml's served-path smoke must
# assert readiness on /v1/readyz and run a real read on every region
# (GH-910).
#
# /v1/healthz's body is a constant "ok", so a smoke that probes it cannot
# fail while the process is up; /v1/readyz carries the Postgres and
# schema-head checkers (503 on a critical failure, "degraded" on a
# non-critical one). And every real read used to sit behind
# `if [ "$REGION" = "r1" ]`, leaving testnet/futurenet with only the
# tautology. Behavioural: extracts the step's real `run:` script and
# executes it per region against stub curl/ssh on PATH — no network.
#
# Run: bash scripts/ci/deploy-served-path-smoke-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
WORKFLOW="${WORKFLOW:-.github/workflows/deploy.yml}"
[[ -r "$WORKFLOW" ]] || { echo "deploy-served-path-smoke-test: missing $WORKFLOW" >&2; exit 2; }

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

python3 - "$WORKFLOW" >"$work/step.sh" <<'PY' || { echo "deploy-served-path-smoke-test: could not find the deploy job's 'Served-path smoke' step" >&2; exit 2; }
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
for step in wf["jobs"]["deploy"].get("steps", []):
    if step.get("name") == "Served-path smoke":
        print(step["run"])
        sys.exit(0)
sys.exit(1)
PY

mkdir -p "$work/bin"
# Stub curl: logs the URL, writes the body for its path to the -o file,
# prints the HTTP code for -w. READYZ_CODE / READYZ_STATUS shape /v1/readyz.
cat >"$work/bin/curl" <<'SH'
#!/usr/bin/env bash
out="" url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -w|-A|--max-time|--retry|--retry-delay) shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
echo "$url" >>"$CURL_LOG"
code=200
case "$url" in
  */v1/readyz)
    code="${READYZ_CODE:-200}"
    body="{\"data\":{\"status\":\"${READYZ_STATUS:-ok}\",\"checks\":[{\"name\":\"postgres\",\"ok\":true}]}}" ;;
  */v1/healthz) body='{"data":{"status":"ok"}}' ;;
  *) body='{"data":[]}' ;;
esac
printf '%s' "$body" >"$out"
printf '%s' "$code"
SH
cat >"$work/bin/ssh" <<'SH'
#!/usr/bin/env bash
printf 'stellarindex_binary_version_skew 0\nstellarindex_binary_version_probe_success 1\n'
SH
chmod +x "$work/bin/curl" "$work/bin/ssh"

# run_step <region> [VAR=value ...]: runs the extracted step; sets $rc and
# leaves the requested URLs in $work/<region>.log.
run_step() {
  local region="$1"
  shift
  : >"$work/$region.log"
  (cd "$work" && env PATH="$work/bin:$PATH" CURL_LOG="$work/$region.log" \
    REGION="$region" DEPLOY_HOST=stub DEPLOY_USER=stub "$@" \
    bash -euo pipefail step.sh >"$work/$region.out" 2>&1)
  rc=$?
}

for region in r1 testnet futurenet; do
  run_step "$region"
  if [ "$rc" -eq 0 ]; then ok "$region: smoke passes on a ready API"; else bad "$region: smoke failed on a ready API (rc=$rc): $(tail -3 "$work/$region.out")"; fi
  if grep -q '/v1/readyz$' "$work/$region.log"; then ok "$region: probes /v1/readyz"; else bad "$region: never probes /v1/readyz — readiness is not asserted"; fi
  if grep -q '/v1/healthz$' "$work/$region.log"; then bad "$region: still probes /v1/healthz, whose body is a constant"; else ok "$region: does not rely on /v1/healthz"; fi
  if grep -q '/v1/assets?limit=100$' "$work/$region.log"; then ok "$region: runs a real /v1/assets read"; else bad "$region: no real read — only the readiness probe runs on $region"; fi
done
if grep -q '/v1/assets?limit=500$' "$work/r1.log"; then ok "r1: keeps the limit=500 sitemap read"; else bad "r1: lost the limit=500 sitemap read"; fi

run_step testnet READYZ_STATUS=degraded
if [ "$rc" -ne 0 ]; then ok "readyz 200 with status=degraded fails the smoke"; else bad "readyz status=degraded passed the smoke"; fi
run_step testnet READYZ_CODE=503 READYZ_STATUS=unready
if [ "$rc" -ne 0 ]; then ok "readyz 503 (critical checker down) fails the smoke"; else bad "readyz 503 passed the smoke"; fi

echo "deploy-served-path-smoke-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
