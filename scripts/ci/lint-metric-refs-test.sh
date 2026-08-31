#!/usr/bin/env bash
# lint-metric-refs-test.sh — fixtures for the F-1329 dead-alert guard.
#
# The load-bearing case (W8.15): a metric NAMED ONLY IN A COMMENT must
# not count as "emitted". Before the fix, is_emitted() did a plain
# `grep -rlF` over emitter files, so a `// TODO wire stellarindex_foo`
# Go comment or a `# HELP stellarindex_foo` .prom header made a dead
# reference look live — the exact "false sense of coverage" this gate
# exists to prevent. These fixtures pin that comment lines (Go `//`,
# shell/.prom `#`) do NOT satisfy is_emitted, while a real `Name:`
# literal / bare .prom metric line still does. They also pin that a
# metric named only inside a `#` comment within an `expr:` region is not
# enforced (strip_hash).
#
# The fixtures build a throwaway repo_root and run the REAL script
# against it (the script derives repo_root from its own path), so a
# regression in lint-metric-refs.sh reds this test.
#
# Run: bash scripts/ci/lint-metric-refs-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/scripts/ci/lint-metric-refs.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# Build a fixture repo tree. The script computes repo_root as
# <script>/../.., so placing it at $ROOT/scripts/ci/ makes $ROOT the root.
ROOT="$TMP/repo"
mkdir -p "$ROOT/scripts/ci" \
         "$ROOT/deploy/monitoring/rules" \
         "$ROOT/configs/prometheus/rules.r1" \
         "$ROOT/internal" \
         "$ROOT/configs/healthchecks"
cp "$SRC" "$ROOT/scripts/ci/lint-metric-refs.sh"

# Rule file: three refs — one backed by a real emitter, one backed only
# by comments, one that appears only inside a `#` comment in the expr.
cat > "$ROOT/deploy/monitoring/rules/fixture.yml" <<'YML'
groups:
  - name: fixture
    rules:
      - alert: RealBacked
        expr: increase(stellarindex_fixture_real_total[5m]) == 0
      - alert: CommentOnly
        expr: increase(stellarindex_fixture_commentonly_total[5m]) == 0
      - alert: ExprCommentRef
        expr: |
          # this expr mentions stellarindex_fixture_exprcomment_total in a
          # comment only; it must NOT be enforced as a live reference.
          increase(stellarindex_fixture_real_total[5m]) == 0
YML

# Go emitter: emits the "real" metric via a Name: literal, but mentions
# the "commentonly" metric ONLY in a // comment.
cat > "$ROOT/internal/metrics.go" <<'GO'
package internal

// stellarindex_fixture_commentonly_total is planned but NOT yet wired.
var _ = struct{ Name string }{
	Name: "stellarindex_fixture_real_total",
}
GO

run() { OUT="$(bash "$ROOT/scripts/ci/lint-metric-refs.sh" 2>&1)" || true; }

expect_absent() { # <name> <token>
  if printf '%s' "$OUT" | grep -q "references '$2'"; then
    echo "FAIL: $1 — '$2' flagged DEAD-REF but it should be accounted for" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $1"; pass=$((pass + 1))
}
expect_present() { # <name> <token>
  if ! printf '%s' "$OUT" | grep -q "references '$2'"; then
    echo "FAIL: $1 — '$2' NOT flagged DEAD-REF but it should be (comment-only is not an emitter)" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $1"; pass=$((pass + 1))
}

# 1. Comment-only Go mention does NOT satisfy is_emitted → DEAD-REF.
run
expect_present 'comment-only Go mention is dead' 'stellarindex_fixture_commentonly_total'
# 2. Real Name: literal still counts → not flagged.
expect_absent 'real Name: literal is live' 'stellarindex_fixture_real_total'
# 3. A token that appears only in a `#` comment within the expr region is
#    not enforced at all → never flagged.
expect_absent 'expr hash-comment token is ignored' 'stellarindex_fixture_exprcomment_total'

# 4. A .prom HELP/TYPE header (`# ...`) is a comment, not an emitter, but
#    the bare metric line IS. Add a .prom under an emitter path and a rule
#    referencing a header-only metric; it must stay dead.
cat > "$ROOT/deploy/monitoring/rules/prom-fixture.yml" <<'YML'
groups:
  - name: promfixture
    rules:
      - alert: PromHeaderOnly
        expr: stellarindex_fixture_promheader_total == 0
      - alert: PromRealLine
        expr: stellarindex_fixture_promline_total == 0
YML
cat > "$ROOT/configs/healthchecks/fixture.prom" <<'PROM'
# HELP stellarindex_fixture_promheader_total planned, header only
# TYPE stellarindex_fixture_promheader_total counter
stellarindex_fixture_promline_total 1
PROM
run
expect_present '.prom header-only metric is dead' 'stellarindex_fixture_promheader_total'
expect_absent '.prom bare metric line is live' 'stellarindex_fixture_promline_total'

# 5. An emitter that lives as an INLINE ansible `content:` block must
#    count as live. Several textfile-collector probes (timescale-jobs,
#    galexie-catchup, stellar-stack-version, binary-version) are written
#    that way, and because is_emitted() only grepped *.go/*.sh/*.prom,
#    every one of their REAL, SCRAPED metrics had to be parked in
#    KNOWN_INERT with a comment reading "NOT inert: the probe timer runs
#    every minute on r1". That made KNOWN_INERT mean two different
#    things — "no producer" and "producer invisible to this lint" —
#    which is the confusion the gate exists to prevent.
mkdir -p "$ROOT/configs/ansible/roles/archival-node/tasks"
cat > "$ROOT/deploy/monitoring/rules/ansible-fixture.yml" <<'YML'
groups:
  - name: ansiblefixture
    rules:
      - alert: AnsibleInlineEmitted
        expr: stellarindex_fixture_ansible_inline_total > 0
      - alert: AnsibleCommentOnly
        expr: stellarindex_fixture_ansible_comment_total > 0
YML
cat > "$ROOT/configs/ansible/roles/archival-node/tasks/probe.yml" <<'YML'
- name: fixture probe
  ansible.builtin.copy:
    dest: /usr/local/sbin/fixture-probe.sh
    content: |
      #!/bin/sh
      # stellarindex_fixture_ansible_comment_total is only mentioned here
      echo "stellarindex_fixture_ansible_inline_total 1" > /tmp/f.prom
YML
run
expect_absent 'ansible inline content: block counts as an emitter' 'stellarindex_fixture_ansible_inline_total'
expect_present 'a metric named only in an ansible COMMENT stays dead' 'stellarindex_fixture_ansible_comment_total'

echo "----"
echo "lint-metric-refs-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
