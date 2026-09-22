#!/usr/bin/env bash
# r1-smoke-test.sh — regression test for scripts/dev/r1-smoke.sh's
# issue-reference hygiene (RSWP-092).
#
# GitHub issue #1131 is real but unrelated to security.txt (it's the
# fail-open dwell-window ticket) — a dangling-reference sweep flagged
# r1-smoke.sh for citing #1131 as "the PR that ships security.txt",
# which never existed under that number. The /.well-known/security.txt
# route (internal/api/v1/server.go's handleSecurityTxt) is already live
# on this branch, so the fix promotes the check out of the "queued for
# promotion" block instead of leaving a wrong citation behind.
#
# Run: bash scripts/dev/r1-smoke-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SMOKE="scripts/dev/r1-smoke.sh"

pass=0
fail=0
ok()  { echo "  ok   $1"; pass=$((pass + 1)); }
bad() { echo "  FAIL $1"; fail=$((fail + 1)); }

if grep -q '1131' "$SMOKE"; then
  bad "no dangling #1131 citation remains (GitHub #1131 is the unrelated dwell-window issue, not security.txt)"
else
  ok "no dangling #1131 citation remains"
fi

# RSWP-094: #1134 was cited as the PR behind the cursor-guard behaviour
# pins, but GitHub #1134 is an unrelated open issue (xdrjson decode-arm
# coverage), not a PR, and never shipped this behaviour.
if grep -q '1134' "$SMOKE"; then
  bad "no dangling #1134 citation remains (GitHub #1134 is the unrelated xdrjson decode-arm issue)"
else
  ok "no dangling #1134 citation remains"
fi

# The route already ships (internal/api/v1/server.go registers GET
# /.well-known/security.txt) so the smoke must assert it live, not sit
# behind a commented-out "queued for promotion" line.
if grep -Eq '^check "security\.txt"[[:space:]]+"/\.well-known/security\.txt"' "$SMOKE"; then
  ok "security.txt is checked live (uncommented)"
else
  bad "security.txt is checked live (uncommented)"
fi

if grep -q '/.well-known/security.txt.*queued\|# .*/.well-known/security.txt 200' "$SMOKE"; then
  bad "security.txt is no longer listed as a pending promotion"
else
  ok "security.txt is no longer listed as a pending promotion"
fi

# RSWP-107: the markets ?asset= behaviour-pin lines cited "(#1189)" as the
# shipping PR, but GitHub #1189 is now an unrelated open issue (the
# extract-wasm-from-galexie found-before-write bug), not the PR that
# shipped the /v1/markets ?asset= filter (that was PR #1189 at merge time
# under an issue tracker that has since been renumbered/reused).
if grep -q '1189' "$SMOKE"; then
  bad "no dangling #1189 citation remains (GitHub #1189 is the unrelated extract-wasm-from-galexie issue)"
else
  ok "no dangling #1189 citation remains"
fi

# RSWP-108: the "queued for promotion" block cited (#1135, #1162,
# #1164, #1168, #1172, #1207, #1189, #1190) as the PRs/issues behind
# each pinned behaviour. Every one of those numbers now resolves to an
# unrelated live GitHub issue (confirmed via `gh issue view`), not the
# change it was cited for — the same dangling-citation class as the
# #1131/#1134 checks above. #1135 also appears earlier in the file as
# unrelated motivating context for expect_status's dual-status design
# (not a "PR that shipped this" claim) and is intentionally excluded
# from this loop.
for n in 1162 1164 1168 1172 1207 1189 1190; do
  if grep -q "#${n}\b" "$SMOKE"; then
    bad "no dangling #${n} citation remains (resolves to an unrelated GitHub issue)"
  else
    ok "no dangling #${n} citation remains"
  fi
done

echo "r1-smoke-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
