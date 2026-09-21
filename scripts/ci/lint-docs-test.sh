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
