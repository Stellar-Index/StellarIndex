#!/usr/bin/env bash
set -euo pipefail
# Self-test for the resolve-tag SemVer validation step in release.yml
# (Q228/RNC25, audit-2026-09-18): a multi-line tag whose FIRST line is
# a valid SemVer must be rejected outright, not accepted because a
# per-line grep -Eq matched line 1 and ignored the rest.
#
# Extracts the real `run:` script from the workflow (not a copy of
# it) and executes it under bash, so this proves the shipped step,
# not a stand-in for it.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="$repo_root/.github/workflows/release.yml"

extract_step() {
  python3 - "$workflow" <<'PY'
import sys
import yaml

with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)
for step in doc["jobs"]["resolve-tag"]["steps"]:
    if step.get("id") == "out":
        print(step["run"])
        break
PY
}

script="$(extract_step)"
if [ -z "$script" ]; then
  echo "FAIL: could not extract resolve-tag's 'out' step from $workflow"
  exit 1
fi

fail=0

# Malicious case: line 1 is a valid SemVer, line 2 is arbitrary
# GITHUB_OUTPUT-injected content. Must be rejected (non-zero exit),
# never silently accepted with the extra line riding along.
out_file="$(mktemp)"
set +e
EVENT_NAME=push INPUT_TAG='' REF_NAME=$'v1.0.0\nmalicious=should-not-appear' \
  GITHUB_OUTPUT="$out_file" bash -c "$script" >/tmp/release-tag-test.out 2>&1
status=$?
set -e
if [ "$status" -eq 0 ]; then
  echo "FAIL: multi-line tag with a valid first line was ACCEPTED"
  cat /tmp/release-tag-test.out
  fail=1
else
  echo "OK: multi-line tag rejected (exit $status)"
fi
if grep -q 'malicious' "$out_file" 2>/dev/null; then
  echo "FAIL: injected content reached GITHUB_OUTPUT"
  fail=1
fi
rm -f "$out_file"

# Sanity: an ordinary valid tag still resolves correctly.
out_file="$(mktemp)"
EVENT_NAME=push INPUT_TAG='' REF_NAME='v1.2.3' GITHUB_OUTPUT="$out_file" \
  bash -c "$script" >/tmp/release-tag-test.out 2>&1
if grep -qx 'v1.2.3' "$out_file"; then
  echo "OK: valid tag v1.2.3 still resolves"
else
  echo "FAIL: valid tag v1.2.3 did not resolve correctly"
  cat /tmp/release-tag-test.out
  cat "$out_file"
  fail=1
fi
rm -f "$out_file" /tmp/release-tag-test.out

exit $fail
