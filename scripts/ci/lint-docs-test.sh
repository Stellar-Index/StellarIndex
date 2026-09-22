#!/usr/bin/env bash
# lint-docs-test.sh — prove lint-docs.sh's ADR/README index cross-check
# (§8) can actually fail: an ADR shipping with no row in
# docs/adr/README.md's Index table must be caught, not silently pass.
# Also covers §15 (incident post-mortem follow-up forcing function):
# an aged incident's unresolved action item only gates CI when it is
# a `- [ ]` checkbox — a plain prose bullet silently escapes the
# 30-day forcing function forever (K089/F168).
# This does not attempt to cover every section of lint-docs.sh — only
# the checks these fixtures drive.
#
# The fixtures are deliberately UNTRACKED; the git index is never touched.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-docs.sh"
FIX="docs/adr/0099-zz-lint-docs-fixture.md"
FIX2="internal/incidents/data/2020-01-01-zz-lint-docs-fixture.md"
PASS=0; FAIL=0
# shellcheck disable=SC2317,SC2329  # invoked indirectly by the EXIT trap
cleanup() { rm -f "$FIX" "$FIX2"; }
trap cleanup EXIT

check() { # <name> <expected ok|red>
  local name="$1" want="$2" rc
  bash "$GATE" >/dev/null 2>&1; rc=$?
  if { [ "$want" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$want" = red ] && [ "$rc" -gt 0 ]; }; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$name" "$rc" "$want"; FAIL=$((FAIL+1))
  fi
}

check "clean tree passes" ok

cat > "$FIX" <<'EOF'
---
adr: 0099
title: Fixture ADR for lint-docs self-test
status: Accepted
date: 2026-09-21
supersedes: []
superseded_by: null
---

# ADR-0099: fixture (never added to docs/adr/README.md)
EOF
check "an ADR with no docs/adr/README.md Index row is caught" red

rm -f "$FIX"
check "clean tree passes again" ok

# §4 stale-reference check: CHANGELOG.md must not carry the dangling
# "PR #1042" citation back in (RSWP-068 — #1042 resolves to a real but
# unrelated issue, not the PR the changelog implies).
echo "(PR #1042)" >> CHANGELOG.md
check "a reintroduced 'PR #1042' citation in CHANGELOG.md is caught" red
git checkout -- CHANGELOG.md
check "clean tree passes again after revert" ok

# RSWP-086: CHANGELOG's r1-smoke.sh budget-bump entry cited a PR number
# (#1108) that never identified the actual PR; #1108 now resolves to a
# real, unrelated issue, not a 404.
echo "(#1108)" >> CHANGELOG.md
check "a reintroduced '#1108' citation in CHANGELOG.md is caught" red
git checkout -- CHANGELOG.md
# §4 stale-reference check: coverage-matrix.md must not carry the dangling
# "#1271" citation back in (RSWP-144 — #1271 resolves to a real but
# unrelated open issue, not the PR the doc implies).
echo "(#1271)" >> docs/architecture/coverage-matrix.md
check "a reintroduced '#1271' citation in coverage-matrix.md is caught" red
git checkout -- docs/architecture/coverage-matrix.md
# §4 stale-reference check: docs/remediation-2026-07-01/STATUS.md must not
# carry the dangling "#1347" citation back in (RSWP-146 — #1347 resolves to
# a real but unrelated issue about retiring a data source, not the
# go-stellar-sdk v0.6 bump it was cited against).
echo "(#1347)" >> docs/remediation-2026-07-01/STATUS.md
check "a reintroduced '#1347' citation in STATUS.md is caught" red
# RSWP-147: docs/remediation-2026-07-01/STATUS.md must not carry the
# dangling "#1353" citation back in — #1353 now resolves to an
# unrelated auto-filed ci-health-bot issue, not the checkout v7 PR.
echo "#1353" >> docs/remediation-2026-07-01/STATUS.md
check "a reintroduced '#1353' citation in remediation STATUS.md is caught" red
git checkout -- docs/remediation-2026-07-01/STATUS.md
check "clean tree passes again after revert" ok

# §4 stale-reference check: docs/architecture/coverage-matrix.md must not
# carry the "R-013 → #1265" citation back in (RSWP-141 — #1265 now
# resolves to an unrelated resolveTip completeness-clamp finding, not the
# chart truncated/data_starts_at PR the R-013 row implies).
echo "R-013 → #1265" >> docs/architecture/coverage-matrix.md
check "a reintroduced 'R-013 -> #1265' citation is caught" red
git checkout -- docs/architecture/coverage-matrix.md
check "clean tree passes again after revert" ok

cat > "$FIX2" <<'EOF'
---
title: "[SEV-2] fixture — aged incident, plain prose action item"
date: 2026-09-02
severity: SEV-2
status: resolved
started_at: 2026-09-02T00:00:00Z
resolved_at: 2026-09-03T00:00:00Z
affected_components:
  - api
postmortem:
---

# fixture

## Post-exposure remediation

- **No targeted API-key rotation or customer notification.** Tracked
  as a postmortem action item, but not written as a checkbox.
EOF
check "an aged incident's unresolved action item written as plain prose (no checkbox) escapes the 30-day forcing function" ok

cat > "$FIX2" <<'EOF'
---
title: "[SEV-2] fixture — aged incident, unchecked checkbox action item"
date: 2026-09-02
severity: SEV-2
status: resolved
started_at: 2026-09-02T00:00:00Z
resolved_at: 2026-09-03T00:00:00Z
affected_components:
  - api
postmortem:
---

# fixture

## Post-exposure remediation

- [ ] **No targeted API-key rotation or customer notification.**
      Tracked as a postmortem action item.
EOF
check "an aged incident's unresolved action item written as a '- [ ]' checkbox is caught" red

rm -f "$FIX2"
check "clean tree passes a third time" ok

echo
echo "lint-docs-test: $PASS passed, $FAIL failed"
exit "$FAIL"
