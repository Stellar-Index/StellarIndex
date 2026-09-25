#!/usr/bin/env bash
set -euo pipefail
# Self-test for T471/RLT-376 (audit-2026-09-18): a docs-only or
# internal/incidents/data/*.md-only change must NOT be excluded by
# ci.yml's own `paths-ignore`, because doing so skips the entire
# workflow — including the `doc-checks` job whose whole purpose is
# validating docs, and the only trigger covering the incidents
# `//go:embed data/*.md` corpus.
#
# Parses the real `paths-ignore` lists out of ci.yml (not a copy of
# them) and checks them against representative changed-file paths
# using the same glob semantics GitHub Actions uses for path filters.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="$repo_root/.github/workflows/ci.yml"

fail=0

check_trigger() {
  local trigger="$1"
  python3 - "$workflow" "$trigger" <<'PY'
import fnmatch
import sys

import yaml

with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)

trigger = sys.argv[2]
# PyYAML parses the bare `on:` key as the boolean True (YAML 1.1),
# not the string "on".
on_block = doc.get("on", doc.get(True))
patterns = on_block[trigger]["paths-ignore"]

# GitHub Actions path filters: '**' matches any number of path
# segments (including zero); translate to a regex-friendly glob that
# fnmatch can evaluate by mapping '**' to '*' after collapsing (good
# enough for the patterns actually in this file — no '?'/'[]' usage).
must_not_be_ignored = [
    "docs/operations/status-page-setup.md",
    "docs/adr/0040-example.md",
    "internal/incidents/data/2026-09-24-example.md",
    "web/status/README.md",
]

bad = []
for path in must_not_be_ignored:
    for pattern in patterns:
        if fnmatch.fnmatch(path, pattern):
            bad.append((path, pattern, trigger))

if bad:
    for path, pattern, trig in bad:
        print(f"FAIL: {trig} paths-ignore pattern '{pattern}' still excludes '{path}'")
    sys.exit(1)
print(f"OK: {trigger} paths-ignore does not exclude docs/incidents corpus paths")
PY
}

check_trigger pull_request || fail=1
check_trigger push || fail=1

exit $fail
