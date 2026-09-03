#!/usr/bin/env bash
# deploy-baseline-test.sh — deploy.yml's config-apply-gate BASELINE step
# must refuse to guess (#427).
#
# The baseline is the version live on the host; the gate diffs the config
# surfaces between it and the deploying tag. Two ways that went wrong:
#
#   1. A FAILED READ warned and fell back to "the previous release tag by
#      ancestry", which on a multi-release catch-up means the gate diffs
#      <prev>..<version> — usually nothing — and passes. Both test-net
#      deploys on 2026-09-02 did exactly this (ProxyJump identity, #434)
#      across five releases that touched 9-11 gated files, and the gate
#      went green. A read failure must now FAIL the step.
#   2. stellarindex-migrate is the oldest sidecar by design (it gates no
#      config surface and is omitted from some deploys), so taking the
#      minimum across ALL binaries dragged the baseline to a range nobody
#      was deploying. It must be excluded from the minimum.
#
# A genuinely absent sidecar directory (first-ever deploy) is NOT a failed
# read and must still fall back, so the two cases are pinned separately.
#
# The step is extracted from the workflow and run against a fake `ssh` on
# PATH — no network, no host, no gh.
#
# Run: bash scripts/ci/deploy-baseline-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
WORKFLOW="${WORKFLOW:-.github/workflows/deploy.yml}"
[[ -r "$WORKFLOW" ]] || { echo "deploy-baseline-test: missing $WORKFLOW" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

python3 - "$WORKFLOW" "$TMP/baseline.sh" <<'PY' || { echo "deploy-baseline-test: could not extract the baseline step" >&2; exit 2; }
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
for job in wf["jobs"].values():
    for step in job.get("steps", []):
        if step.get("id") == "baseline":
            open(sys.argv[2], "w").write(step["run"])
            sys.exit(0)
sys.exit(1)
PY

# The step must actually contain the exclusion — a rewrite that drops it
# would otherwise pass every behavioural case below on a host whose
# migrate sidecar happens to be current.
if grep -q 'stellarindex-migrate' "$TMP/baseline.sh"; then
  ok "the step excludes stellarindex-migrate from the minimum"
else
  bad "the step no longer mentions stellarindex-migrate — the #427 exclusion is gone"
fi

mkdir -p "$TMP/bin"
export PATH="$TMP/bin:$PATH"

# fake_ssh <exit-code> <stdout>
fake_ssh() {
  cat > "$TMP/bin/ssh" <<EOF
#!/usr/bin/env bash
printf '%s' "\$(cat <<'OUT'
$2
OUT
)"
exit $1
EOF
  chmod +x "$TMP/bin/ssh"
}

# run_baseline → exit code; stdout+stderr in \$out, GITHUB_OUTPUT in \$gh
run_baseline() {
  : > "$TMP/gh_output"
  out="$(DEPLOY_HOST=h DEPLOY_USER=u DEPLOY_JUMP="" GITHUB_OUTPUT="$TMP/gh_output" \
         bash "$TMP/baseline.sh" 2>&1)"
  local rc=$?
  gh="$(cat "$TMP/gh_output")"
  return $rc
}

echo "deploy-baseline-test: failed read"

fake_ssh 255 ""
if run_baseline; then
  bad "an ssh failure (exit 255) still passed — the false-green fallback is back: $out"
else
  case "$out$gh" in
    *"::error::"*live=*) bad "ssh failure failed the step but still wrote a live= output" ;;
    *"::error::"*)       ok "an ssh failure FAILS the step instead of falling back" ;;
    *)                   bad "ssh failure produced no ::error:: annotation: $out" ;;
  esac
fi

echo "deploy-baseline-test: successful reads"

fake_ssh 0 "v0.57.0
v0.57.0
v0.57.0"
if run_baseline && [ "$gh" = "live=v0.57.0" ]; then
  ok "a uniform fleet reports its version"
else
  bad "uniform fleet: rc=$? output='$gh' log='$out'"
fi

fake_ssh 0 "v0.57.0
v0.44.7
v0.57.0"
if run_baseline && [ "$gh" = "live=v0.44.7" ]; then
  ok "a lagging binary drags the baseline down (the gate's whole point)"
else
  bad "lagging binary: output='$gh' log='$out'"
fi

# The fake ssh cannot honour the remote loop's migrate filter, so the
# exclusion is asserted structurally above; here we pin that a sidecar
# list WITHOUT migrate still yields the true minimum.
fake_ssh 0 "v0.57.0
v0.57.0"
if run_baseline && [ "$gh" = "live=v0.57.0" ]; then
  ok "a fleet current except migrate reports current (migrate excluded remotely)"
else
  bad "migrate-excluded fleet: output='$gh' log='$out'"
fi

echo "deploy-baseline-test: no sidecar (first deploy)"

fake_ssh 0 ""
if run_baseline && [ "$gh" = "live=" ]; then
  case "$out" in
    *"::warning::"*) ok "an absent sidecar warns and falls back (not a failed read)" ;;
    *) bad "absent sidecar fell back with no warning: $out" ;;
  esac
else
  bad "absent sidecar: rc=$? output='$gh' log='$out'"
fi

fake_ssh 0 "not-a-version
garbage"
if run_baseline && [ "$gh" = "live=" ]; then
  ok "unparseable sidecar content falls back rather than passing garbage on"
else
  bad "garbage sidecar: output='$gh' log='$out'"
fi


echo "deploy-baseline-test: the remote snippet itself"

# The fake ssh above never RUNS the remote snippet, so its own logic went
# untested — and that is exactly where the unreadable-sidecar hole lived.
# Extract it from the workflow with sed (awk/sed only; no nested heredocs)
# and execute it against real fixture directories.
sed -n "/^ *remote='/,/^ *exit 0'/p" "$WORKFLOW" \
  | sed "s/^ *remote='//; s/exit 0'$/exit 0/" > "$TMP/remote.sh"
if ! grep -q "deployed-versions" "$TMP/remote.sh"; then
  echo "deploy-baseline-test: could not extract the remote snippet from $WORKFLOW" >&2
  exit 2
fi

rout=""
run_remote() {
  rout="$(sed "s#d=/var/lib/stellarindex/deployed-versions#d=$1#" "$TMP/remote.sh" | bash 2>/dev/null)"
  return $?
}

mkdir -p "$TMP/sidecars"
printf 'v0.57.0' > "$TMP/sidecars/stellarindex-api"
printf 'v0.57.0' > "$TMP/sidecars/stellarindex-indexer"
printf 'v0.11.0' > "$TMP/sidecars/stellarindex-migrate"

run_remote "$TMP/sidecars"
if [ $? -eq 0 ] && [ "$(printf '%s' "$rout" | grep -c 'v0.57.0')" -eq 2 ] && ! printf '%s' "$rout" | grep -q 'v0.11.0'; then
  ok "remote snippet reads the sidecars and excludes migrate"
else
  bad "remote snippet output wrong: '$rout'"
fi

run_remote "$TMP/definitely-not-here"
if [ $? -eq 0 ] && [ -z "$rout" ]; then
  ok "absent directory exits 0 with no output (the first-deploy case)"
else
  bad "absent directory did not exit 0 with empty output: '$rout'"
fi

if [ "$(id -u)" -eq 0 ]; then
  ok "unreadable-sidecar case skipped (running as root, which can read anything)"
else
  chmod 000 "$TMP/sidecars/stellarindex-api" 2>/dev/null
  run_remote "$TMP/sidecars"
  rc=$?
  chmod 644 "$TMP/sidecars/stellarindex-api" 2>/dev/null
  if [ "$rc" -eq 3 ]; then
    ok "an UNREADABLE sidecar exits 3 rather than silently raising the baseline"
  else
    bad "unreadable sidecar returned $rc, want 3 — it would silently raise the baseline"
  fi
fi


echo "deploy-baseline-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
