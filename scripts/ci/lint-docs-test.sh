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
FIX3="docs/design/zz-lint-docs-fixture.md"
PASS=0; FAIL=0
# shellcheck disable=SC2317,SC2329  # invoked indirectly by the EXIT trap
cleanup() { rm -f "$FIX" "$FIX2" "$FIX3"; }
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

# §4 stale-reference check: CHANGELOG.md must not carry the dangling
# "(PR #1254)" citation back in (RSWP-135 — #1254 resolves to a real but
# unrelated live issue, config-apply-gate's refuted-arm gap, not the PR
# that shipped the exchanges-chart error-state fix it named).
echo "(PR #1254)" >> CHANGELOG.md
check "a reintroduced '(PR #1254)' citation in CHANGELOG.md is caught" red
git checkout -- CHANGELOG.md
check "clean tree passes again after revert" ok

# RSWP-127: CHANGELOG's fx_quotes/migration-0028 runbook entry cited
# "(PR #1230)" but no such PR exists.
echo "(PR #1230)" >> CHANGELOG.md
check "a reintroduced 'PR #1230' citation in CHANGELOG.md is caught" red
git checkout -- CHANGELOG.md
check "clean tree passes again after revert" ok

# RSWP-128: CHANGELOG's /v1/coins/{slug} canonical asset_id entry cited
# "(PR #1231)" but #1231 is a real, currently-open, unrelated issue
# (${EXTRA_FLAGS} brace-form word-split), not that PR.
echo "(PR #1231)" >> CHANGELOG.md
check "a reintroduced 'PR #1231' citation in CHANGELOG.md is caught" red
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
# RSWP-149: docs/remediation-2026-07-01/STATUS.md must not carry the
# dangling "#1369" citation back in — #1369 now resolves to an
# unrelated, already-merged W3 slice PR, not the tooling-groups bump.
echo "#1369" >> docs/remediation-2026-07-01/STATUS.md
check "a reintroduced '#1369' citation in remediation STATUS.md is caught" red
git checkout -- docs/remediation-2026-07-01/STATUS.md
check "clean tree passes again after revert" ok

# RSWP-151: CHANGELOG.md must not carry the dangling "dependabot
# #1371/#1372" citation back in — #1371 and #1372 now resolve to real,
# unrelated live PRs (an open dependabot npm-bump PR and a closed
# audit-remediation PR), not the dependabot bumps this entry named.
echo "supersedes dependabot #1371/#1372" >> CHANGELOG.md
check "a reintroduced 'dependabot #1371/#1372' citation in CHANGELOG.md is caught" red
git checkout -- CHANGELOG.md
check "clean tree passes again after revert" ok

# §4 stale-reference check: docs/architecture/coverage-matrix.md must not
# carry the "R-013 → #1265" citation back in (RSWP-141 — #1265 now
# resolves to an unrelated resolveTip completeness-clamp finding, not the
# chart truncated/data_starts_at PR the R-013 row implies).
echo "R-013 → #1265" >> docs/architecture/coverage-matrix.md
check "a reintroduced 'R-013 -> #1265' citation is caught" red
git checkout -- docs/architecture/coverage-matrix.md
check "clean tree passes again after revert" ok

# §4 stale-reference check: docs/architecture/coverage-matrix.md must not
# carry the "#1263" citation back in (RSWP-139 — #1263 now resolves to an
# unrelated, currently-open projector cursor-commit finding, not the
# ATH/day-VWAP fix). Bare pattern, so a citation reappearing in the
# 2026-05-11 PR-list header ALONE (without the literal "R-008" alongside
# it) is caught too, not only an "R-008 → #1263" row.
echo "(PRs #1261, #1262, #1263, #1268, #1270)" >> docs/architecture/coverage-matrix.md
check "a reintroduced bare '#1263' header citation is caught" red
git checkout -- docs/architecture/coverage-matrix.md
check "clean tree passes again after revert" ok

# §4 stale-reference check must cover docs/design/, not just
# docs/architecture/ — a dangling PR citation in a design doc previously
# escaped the scan entirely because docs/design/ was omitted from the
# grep roots.
cat > "$FIX3" <<'EOF'
# fixture design doc

Cites the dangling reference (PR #1042).
EOF
check "a dangling 'PR #1042' citation in docs/design/ is caught" red
rm -f "$FIX3"
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

# RLT-171: an unquoted comma inside a flow-mapping `description:` splits
# the mapping at that comma — the trailing fragment becomes a bogus
# null-valued schema key and the description silently truncates.
sed -i.bak 's/asset:           { type: string, description: Reserve underlying token (C-strkey). }/asset: { type: string, description: Reserve underlying token, C-strkey. }/' \
  openapi/stellar-index.v1.yaml
check "a reintroduced unquoted comma in a flow-mapping description is caught" red
mv openapi/stellar-index.v1.yaml.bak openapi/stellar-index.v1.yaml
check "clean tree passes again after revert" ok

# RLT-171: internal_routes_re must not exempt a route that IS documented
# in the spec — that exemption doesn't keep the route out of the public
# docs, it only blinds this check to it. Undocument the staff look-up
# route and confirm the (former) exemption no longer hides the gap.
sed -i.bak 's|^  /account/admin/lookup:$|  /account/admin/lookup-zzfixture:|' \
  openapi/stellar-index.v1.yaml
check "an undocumented /account/admin/lookup route is now caught (exemption removed)" red
mv openapi/stellar-index.v1.yaml.bak openapi/stellar-index.v1.yaml
check "clean tree passes again after revert" ok

# T557: docs/adr is out of the §6 freshness find-root — an ADR is an
# immutable record whose only real gate is §8 (status/superseded_by/
# index-row), so an explicit last_verified in an ADR must not be aged
# by §6 (that would imply a freshness contract §8 doesn't actually give
# 0/51 ADRs, since ADRs never carry the field). Add a valid README
# index row so §8 passes and only §6's handling of the stale date is
# under test.
cat > "$FIX" <<'EOF'
---
adr: 0099
title: Fixture ADR for lint-docs self-test
status: Accepted
last_verified: 2020-01-01
date: 2026-09-21
supersedes: []
superseded_by: null
---

# ADR-0099: fixture (carries a deliberately stale last_verified)
EOF
printf '| [0099](0099-zz-lint-docs-fixture.md) | Accepted | Fixture ADR for lint-docs self-test | 2026-09-21 |\n' >> docs/adr/README.md
check "a stale last_verified on an ADR is not aged by §6 (docs/adr is out of its find-root)" ok
git checkout -- docs/adr/README.md
rm -f "$FIX"
check "clean tree passes a fourth time" ok

echo
echo "lint-docs-test: $PASS passed, $FAIL failed"
exit "$FAIL"
