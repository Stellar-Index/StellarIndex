#!/usr/bin/env bash
# lint-docs-test.sh — prove the lint-docs.sh checks these fixtures drive
# can actually fail, and that the deliberate exemptions stay exempt.
# This does not attempt to cover every section of lint-docs.sh.
#
# Every fixture is applied at once and the lint runs ONCE; each case then
# asserts its own error line is present (or absent). Asserting the message,
# not just a red exit, stops one case's red from masking another's
# regression — and one run instead of one per case keeps this ~1 min.
#
# New fixtures are untracked; edited tracked files are backed up and
# restored byte-for-byte, so uncommitted work survives. The index is never touched.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-docs.sh"
PHOENIX_FAKE_XLM_SAC="CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
ADR_NOROW="docs/adr/0099-zz-lint-docs-fixture.md"
ADR_STALE="docs/adr/0098-zz-lint-docs-fixture.md"
INC_PROSE="internal/incidents/data/2020-01-01-zz-lint-docs-prose-fixture.md"
INC_BOX="internal/incidents/data/2020-01-02-zz-lint-docs-checkbox-fixture.md"
DESIGN="docs/design/zz-lint-docs-fixture.md"
OPS_STALE="docs/operations/zz-lint-docs-stale-fixture.md"
OPS_WARN="docs/operations/zz-lint-docs-warn-fixture.md"
UNTRACKED_README="docs/zz-lint-docs-fixture/README.md"
RUNBOOK="docs/operations/runbooks/aggregator-class-drop-spike.md"
SPEC="openapi/stellar-index.v1.yaml"
EXPLORER_README="web/explorer/README.md"
EDITED=(CHANGELOG.md docs/architecture/coverage-matrix.md docs/remediation-2026-07-01/STATUS.md
        docs/adr/README.md docs/protocols/README.md "$RUNBOOK" "$SPEC" "$EXPLORER_README")
BACKUP=$(mktemp -d); OUT=$(mktemp)
PASS=0; FAIL=0

# shellcheck disable=SC2317,SC2329  # invoked indirectly by the EXIT trap
cleanup() {
  local f
  for f in "${EDITED[@]}"; do
    if [ -f "$BACKUP/$f" ]; then cp -p "$BACKUP/$f" "$f"; fi
  done
  rm -f "$ADR_NOROW" "$ADR_STALE" "$INC_PROSE" "$INC_BOX" "$DESIGN" "$OPS_STALE" "$OPS_WARN"
  rm -rf "$(dirname "$UNTRACKED_README")" "$BACKUP" "$OUT"
}
trap cleanup EXIT

result() { # <name> <status: 0 = ok>
  if [ "$2" -eq 0 ]; then printf '  ok   %s\n' "$1"; PASS=$((PASS+1))
  else printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); fi
}
present() { grep -qE -- "$2" "$OUT"; result "$1" $?; }  # <name> <ERE>
absent() { ! grep -qE -- "$2" "$OUT"; result "$1" $?; } # <name> <ERE>
# The listed line must sit under THIS pattern's header: one fixture line
# can match several patterns, so the line alone would let one mask another.
stale() { # <name> <pattern as printed in the header> <listed-line ERE>
  HDR="Stale reference to '$2' in" RE="$3" awk '
    /ERROR:/ { cur = index($0, ENVIRON["HDR"]) > 0; next }
    cur && $0 ~ ENVIRON["RE"] { found = 1 }
    END { exit !found }' "$OUT"
  result "$1" $?
}
tree_state() { git status --porcelain=v1 -uall; git diff; }
days_ago() { date -u -r $(( $(date -u +%s) - $1 * 86400 )) +%F 2>/dev/null || date -u -d "@$(( $(date -u +%s) - $1 * 86400 ))" +%F; }
incident() { # <path> <bullet prefix>
  cat > "$1" <<EOF
---
title: "[SEV-2] fixture — aged incident"
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

$2 **No targeted API-key rotation or customer notification.**
  Tracked as a postmortem action item.
EOF
}

bash "$GATE" >/dev/null 2>&1; result "clean tree passes" $?
START_STATE=$(tree_state)
for f in "${EDITED[@]}"; do mkdir -p "$BACKUP/$(dirname "$f")"; cp -p "$f" "$BACKUP/$f"; done

# §8: an ADR shipping with no row in docs/adr/README.md's Index table.
printf -- '---\nadr: 0099\ntitle: Fixture ADR for lint-docs self-test\nstatus: Accepted\ndate: 2026-09-21\nsupersedes: []\nsuperseded_by: null\n---\n\n# ADR-0099: fixture (never added to docs/adr/README.md)\n' > "$ADR_NOROW"

# T557: docs/adr is out of §6's freshness find-root — an ADR is an immutable
# record gated only by §8, so a stale last_verified on one must not be aged.
# It gets a valid index row so §8 passes and only §6 is under test.
printf -- '---\nadr: 0098\ntitle: Fixture ADR for lint-docs self-test\nstatus: Accepted\nlast_verified: 2020-01-01\ndate: 2026-09-21\nsupersedes: []\nsuperseded_by: null\n---\n\n# ADR-0098: fixture (deliberately stale last_verified)\n' > "$ADR_STALE"
printf '| [0098](0098-zz-lint-docs-fixture.md) | Accepted | Fixture ADR for lint-docs self-test | 2026-09-21 |\n' >> docs/adr/README.md

# §4 stale references. Each number now resolves to a real but unrelated
# issue/PR (RSWP-068 #1042, RSWP-135 #1254, RSWP-086 #1108, RSWP-144 #1271,
# RSWP-141 R-013→#1265, RSWP-146 #1347, RSWP-147 #1353, RSWP-149 #1369,
# RSWP-128 #1231, RSWP-151 dependabot #1371/#1372, RSWP-139 #1263) or never
# existed (RSWP-127 #1230). #1263 is a bare pattern so the 2026-05-11
# PR-list header alone, without "R-008", is caught. docs/design/ is scanned too.
printf '(PR #1042)\n(PR #1254)\n(PR #1230)\n(#1108)\n(PR #1231)\nsupersedes dependabot #1371/#1372\n' >> CHANGELOG.md
printf '(#1271)\nR-013 → #1265\n(PRs #1261, #1262, #1263, #1268, #1270)\n' >> docs/architecture/coverage-matrix.md
printf '(#1347)\n#1353\n#1369\n' >> docs/remediation-2026-07-01/STATUS.md
printf '# fixture design doc\n\nCites the dangling reference (PR #1042).\n' > "$DESIGN"

# §15 (K089/F168): an aged incident's action item gates CI only as a `- [ ]`
# checkbox; a plain prose bullet escapes the 30-day forcing function.
incident "$INC_PROSE" '-'
incident "$INC_BOX" '- [ ]'

# RLT-171: an unquoted comma in a flow-mapping `description:` splits the
# mapping into a bogus null-valued key; and internal_routes_re must not
# exempt a route the spec documents, so undocumenting it must be caught.
sed -e 's/asset:           { type: string, description: Reserve underlying token (C-strkey). }/asset: { type: string, description: Reserve underlying token, C-strkey. }/' \
    -e 's|^  /account/admin/lookup:$|  /account/admin/lookup-zzfixture:|' "$BACKUP/$SPEC" > "$SPEC"

# §11b: a continued heavy-job command without -write, in a runbook that
# had no wrapper line before.
# shellcheck disable=SC1003  # a literal trailing backslash, not an escape
printf '%s\n' 'sudo /usr/local/sbin/run-heavy-job.sh zz -- \' '  stellarindex-ops supply snapshot -asset native' >> "$RUNBOOK"

# §6: a living procedure past 180 days is red; past only 90 days it warns.
printf -- '---\nlast_verified: %s\n---\n\n# fixture\n' "$(days_ago 181)" > "$OPS_STALE"
printf -- '---\nlast_verified: %s\n---\n\n# fixture\n' "$(days_ago 100)" > "$OPS_WARN"

# §21: only TRACKED READMEs are scanned — an untracked copy (e.g. under an
# agent worktree) must not redden the lint, but a tracked one must.
mkdir -p "$(dirname "$UNTRACKED_README")"
echo "$PHOENIX_FAKE_XLM_SAC" > "$UNTRACKED_README"
echo "$PHOENIX_FAKE_XLM_SAC" >> docs/protocols/README.md

# Explorer docs: a script documented as the wrong command, an undeclared
# package, and a route with no src/app directory.
# shellcheck disable=SC2016  # literal Markdown backticks, not a substitution
printf '\n```sh\npnpm lint               # next lint\n```\n\nMDX via `@next/mdx`; account at `/account/*`.\n' >> "$EXPLORER_README"

bash "$GATE" > "$OUT" 2>&1; rc=$?
red=1; [ "$rc" -gt 0 ] && red=0
result "the fixture tree is red (rc=$rc)" "$red"
present "an ADR with no Index row is caught" "ADR '$ADR_NOROW' \(ADR-0099\) has no row"
absent  "a stale last_verified on an ADR is not aged by §6" "0098-zz-lint-docs-fixture"
stale "'PR #1042' in CHANGELOG.md is caught" 'PR #1042' '^ +CHANGELOG\.md:[0-9]+:\(PR #1042\)$'
stale "'(PR #1254)' in CHANGELOG.md is caught" '\(PR #1254\)' '^ +CHANGELOG\.md:[0-9]+:\(PR #1254\)$'
stale "'PR #1230' in CHANGELOG.md is caught" 'PR #1230' '^ +CHANGELOG\.md:[0-9]+:\(PR #1230\)$'
stale "'#1108' in CHANGELOG.md is caught" '#1108\b' '^ +CHANGELOG\.md:[0-9]+:\(#1108\)$'
stale "'PR #1231' in CHANGELOG.md is caught" 'PR #1231' '^ +CHANGELOG\.md:[0-9]+:\(PR #1231\)$'
stale "'dependabot #1371/#1372' in CHANGELOG.md is caught" 'dependabot #1371/#1372' '^ +CHANGELOG\.md:[0-9]+:supersedes dependabot'
stale "'#1271' in coverage-matrix.md is caught" '#1271\b' '^ +docs/architecture/coverage-matrix\.md:[0-9]+:\(#1271\)$'
stale "'R-013 → #1265' in coverage-matrix.md is caught" 'R-013.*#1265' '^ +docs/architecture/coverage-matrix\.md:[0-9]+:R-013 → #1265$'
stale "a bare '#1263' header citation in coverage-matrix.md is caught" '#1263\b' '^ +docs/architecture/coverage-matrix\.md:[0-9]+:\(PRs #1261'
stale "'#1347' in remediation STATUS.md is caught" '#1347\b' '^ +docs/remediation-2026-07-01/STATUS\.md:[0-9]+:\(#1347\)$'
stale "'#1353' in remediation STATUS.md is caught" '#1353' '^ +docs/remediation-2026-07-01/STATUS\.md:[0-9]+:#1353$'
stale "'#1369' in remediation STATUS.md is caught" '#1369' '^ +docs/remediation-2026-07-01/STATUS\.md:[0-9]+:#1369$'
stale "a dangling 'PR #1042' in docs/design/ is caught" 'PR #1042' "^ +$DESIGN:[0-9]+:"
absent  "an aged incident's prose action item escapes the forcing function" "$(basename "$INC_PROSE")"
present "an aged incident's '- [ ]' action item is caught" "incident '$INC_BOX' is older than 30 days"
present "an unquoted comma in a flow-mapping description is caught" "bogus null-valued key 'C-strkey\.'"
present "an undocumented /account/admin/lookup route is caught" "Route '/account/admin/lookup' is registered in handlers but missing"
present "a continued heavy-job 'supply snapshot' without -write is caught" "$RUNBOOK: heavy-job command runs write-gated 'supply snapshot' without -write"
present "a docs/operations page verified 181 days ago is caught" "ERROR: Doc '$OPS_STALE' is STALE"
present "a docs/operations page verified 100 days ago warns" "WARN: doc '$OPS_WARN'"
absent  "a docs/operations page verified 100 days ago is not an error" "ERROR: .*$OPS_WARN"
present "a tracked README republishing Phoenix's fake XLM SAC is caught" "docs/protocols/README\.md republishes '$PHOENIX_FAKE_XLM_SAC'"
absent  "an untracked README with the fake SAC is ignored" "$UNTRACKED_README"
present "an explorer README script documented as the wrong command is caught" "documents 'pnpm lint' as 'next lint' but package\.json runs 'eslint \.'"
present "an explorer README naming an undeclared package is caught" "names package '@next/mdx' but web/explorer/package\.json"
present "an explorer README naming a missing route is caught" "names route '/account/\*' but web/explorer/src/app/account/"

if [ "$FAIL" -gt 0 ]; then echo "--- lint output ---"; grep -E 'ERROR|WARN: doc .docs/operations/zz' "$OUT"; fi
cleanup; trap - EXIT
same=1; [ "$(tree_state)" = "$START_STATE" ] && same=0
result "tree restored to its starting state" "$same"

echo
echo "lint-docs-test: $PASS passed, $FAIL failed"
exit "$FAIL"
