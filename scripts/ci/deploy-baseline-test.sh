#!/usr/bin/env bash
# deploy-baseline-test.sh — deploy.yml's config-apply-gate BASELINE step
# must refuse to guess.
#
# The baseline is the version live on the host; the gate diffs the config
# surfaces between it and the deploying tag. Two ways that went wrong:
#
#   1. A FAILED READ warned and fell back to "the previous release tag by
#      ancestry", which on a multi-release catch-up means the gate diffs
#      <prev>..<version> — usually nothing — and passes. Both test-net
#      deploys did exactly this (ProxyJump identity)
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
         REGION=testnet REGION_SET="${REGION_SET:-}" bash "$TMP/baseline.sh" 2>&1)"
  local rc=$?
  gh="$(grep '^live=' "$TMP/gh_output")"
  cfg="$(grep '^config=' "$TMP/gh_output")"
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

fake_ssh 0 $'stellarindex-api\tv0.57.0\nstellarindex-indexer\tv0.57.0\nstellarindex-ops\tv0.57.0'
if run_baseline && [ "$gh" = "live=v0.57.0" ]; then
  ok "a uniform fleet reports its version"
else
  bad "uniform fleet: rc=$? output='$gh' log='$out'"
fi

fake_ssh 0 $'stellarindex-api\tv0.57.0\nstellarindex-indexer\tv0.44.7\nstellarindex-ops\tv0.57.0'
if run_baseline && [ "$gh" = "live=v0.44.7" ]; then
  ok "a lagging binary drags the baseline down (the gate's whole point)"
else
  bad "lagging binary: output='$gh' log='$out'"
fi

# A sidecar left behind by a binary the region no longer ships (testnet's
# aggregator) must not drag the baseline down; without a manifest set it
# still counts, so the filter is the manifest set and nothing else.
fake_ssh 0 $'stellarindex-api\tv0.57.0\nstellarindex-aggregator\tv0.44.7\nstellarindex-indexer\tv0.57.0'
if REGION_SET="stellarindex-indexer stellarindex-api stellarindex-ops" run_baseline \
   && [ "$gh" = "live=v0.57.0" ] && grep -q '::notice::ignoring sidecar stellarindex-aggregator=v0.44.7' <<<"$out"; then
  ok "a sidecar outside the region's manifest set is ignored and named"
else
  bad "non-manifest sidecar: rc=$? output='$gh' log='$out'"
fi
if run_baseline && [ "$gh" = "live=v0.44.7" ]; then
  ok "without a manifest set every sidecar counts"
else
  bad "no manifest set: output='$gh' log='$out'"
fi

# The fake ssh cannot honour the remote loop's migrate filter, so the
# exclusion is asserted structurally above; here we pin that a sidecar
# list WITHOUT migrate still yields the true minimum.
fake_ssh 0 $'stellarindex-api\tv0.57.0\nstellarindex-indexer\tv0.57.0'
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

fake_ssh 0 $'stellarindex-api\tnot-a-version\nstellarindex-indexer\tgarbage'
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

if run_remote "$TMP/sidecars" && [ "$(printf '%s' "$rout" | grep -c 'v0.57.0')" -eq 2 ] && ! grep -q 'v0.11.0' <<<"$rout"; then
  ok "remote snippet reads the sidecars and excludes migrate"
else
  bad "remote snippet output wrong: '$rout'"
fi

if run_remote "$TMP/definitely-not-here" && [ -z "$rout" ]; then
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


echo "deploy-baseline-test: config owed after a red gate"

# The playbook moves every sidecar to the deploying version before the gate
# runs, so the sidecars alone make a re-run diff VERSION..VERSION and pass.
# The .config-applied marker is what a re-run must read instead.
fake_ssh 0 $'stellarindex-api\tv0.58.0\nstellarindex-indexer\tv0.58.0\n.config-applied\tv0.57.0'
if run_baseline && [ "$gh" = "live=v0.58.0" ] && [ "$cfg" = "config=v0.57.0" ]; then
  ok "binaries ahead of the applied config: the config baseline stays behind"
else
  bad "owed config: live='$gh' config='$cfg' log='$out'"
fi

fake_ssh 0 $'stellarindex-api\tv0.44.7\nstellarindex-indexer\tv0.58.0\n.config-applied\tv0.58.0'
if run_baseline && [ "$cfg" = "config=v0.44.7" ]; then
  ok "a lagging binary still drags the config baseline below the marker"
else
  bad "lagging binary under a current marker: config='$cfg' log='$out'"
fi

fake_ssh 0 $'stellarindex-api\tv0.58.0\n.config-applied\tgarbage'
if run_baseline; then
  bad "a marker that is not a version passed: config='$cfg' log='$out'"
else
  ok "a marker that is not a version FAILS the step rather than being ignored"
fi

# Two deploys against one stateful host: every ssh runs its remote command
# locally against a fixture directory.
step_script() {
  python3 - "$WORKFLOW" "$1" "$TMP/$1.sh" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
for job in wf["jobs"].values():
    for step in job.get("steps", []):
        if step.get("id") == sys.argv[2]:
            open(sys.argv[3], "w").write(step["run"])
            sys.exit(0)
sys.exit(1)
PY
}
step_script config_seed || { echo "deploy-baseline-test: no config_seed step" >&2; exit 2; }
step_script config_mark || { echo "deploy-baseline-test: no config_mark step" >&2; exit 2; }

HOSTDIR="$TMP/host"
mkdir -p "$HOSTDIR"
cat > "$TMP/bin/ssh" <<EOF
#!/usr/bin/env bash
for last; do :; done
printf '%s' "\$last" | sed 's#/var/lib/stellarindex/deployed-versions#$HOSTDIR#g; s#install -d -o root -g root#install -d#' > "$TMP/remote-cmd.sh"
exec bash "$TMP/remote-cmd.sh"
EOF
chmod +x "$TMP/bin/ssh"

run_step() { # run_step <id> <VERSION> <CONFIG_BASELINE>
  DEPLOY_HOST=h DEPLOY_USER=u DEPLOY_JUMP="" VERSION="$2" CONFIG_BASELINE="${3:-}" \
    bash "$TMP/$1.sh" >"$TMP/step.log" 2>&1
}
swap_binaries() {
  printf '%s' "$1" > "$HOSTDIR/stellarindex-api"
  printf '%s' "$1" > "$HOSTDIR/stellarindex-indexer"
}

swap_binaries v0.57.0
run_baseline
if [ "$cfg" = "config=v0.57.0" ] && run_step config_seed v0.58.0 v0.57.0 \
   && [ "$(cat "$HOSTDIR/.config-applied" 2>/dev/null)" = "v0.57.0" ]; then
  ok "first deploy records the baseline on the host before the binaries swap"
else
  bad "seed: config='$cfg' marker='$(cat "$HOSTDIR/.config-applied" 2>/dev/null)' log='$(cat "$TMP/step.log")'"
fi

swap_binaries v0.58.0 # the playbook ran; the gate then went red, so config_mark never ran
run_baseline
if [ "$gh" = "live=v0.58.0" ] && [ "$cfg" = "config=v0.57.0" ]; then
  ok "a re-run after a red gate still diffs from the owed baseline"
else
  bad "re-run after red: live='$gh' config='$cfg' log='$out'"
fi

swap_binaries v0.58.1 # the next release deploys over the unapplied config
run_step config_seed v0.58.1 v0.57.0
run_baseline
if [ "$cfg" = "config=v0.57.0" ] && [ "$(cat "$HOSTDIR/.config-applied")" = "v0.57.0" ]; then
  ok "the next release still diffs from the owed baseline, and the seed keeps the marker"
else
  bad "next release: config='$cfg' marker='$(cat "$HOSTDIR/.config-applied")'"
fi

if run_step config_mark v0.58.1 && run_baseline && [ "$cfg" = "config=v0.58.1" ]; then
  ok "a passed gate advances the marker to the deployed version"
else
  bad "mark: config='$cfg' log='$(cat "$TMP/step.log")'"
fi
if [ -z "$(find "$HOSTDIR" -name '.config-applied.*')" ]; then
  ok "no staging file is left beside the marker"
else
  bad "staging files left behind: $(ls -A "$HOSTDIR")"
fi

if grep -qF 'd/.config-applied" || exit 3' scripts/dev/preflight-deploy.sh; then
  ok "preflight-deploy.sh reads the same marker into its baseline"
else
  bad "preflight-deploy.sh no longer reads .config-applied — its plan would diff a narrower range than the gate"
fi

echo "deploy-baseline-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
