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

# Each rule tree is linted against the scrape config it ships beside, so
# every fixture root needs both. The fixture jobs mirror the real naming
# split: underscores in the multi-host template, hyphens on r1.
seed_scrape_configs() { # <root>
  mkdir -p "$1/configs/ansible/roles/prometheus/templates"
  cat > "$1/configs/ansible/roles/prometheus/templates/prometheus.yml.j2" <<'J2'
scrape_configs:
  - job_name: "stellarindex_api"
{% if 'stellarindex_aggregator' in groups %}
  - job_name: "stellarindex_aggregator"
{% endif %}
J2
  cat > "$1/configs/prometheus/prometheus.r1.yml" <<'R1'
scrape_configs:
  - job_name: stellarindex-api
  - job_name: stellarindex-aggregator
R1
}
seed_scrape_configs "$ROOT"

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
  if grep -q "references '$2'" <<<"$OUT"; then
    echo "FAIL: $1 — '$2' flagged DEAD-REF but it should be accounted for" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $1"; pass=$((pass + 1))
}
expect_present() { # <name> <token>
  if ! grep -q "references '$2'" <<<"$OUT"; then
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

# 6. Producer -> alert direction (advisory, T456). A metric emitted with
# a real Name: literal but referenced by NO rule expr must be reported
# as UNALERTED, without affecting the pass/fail exit status. Uses its
# own clean fixture tree (steps 1-5 already carry real DEAD-REFs, which
# would confound a gate-exit-code assertion here).
CLEAN="$TMP/clean-repo"
mkdir -p "$CLEAN/scripts/ci" \
         "$CLEAN/deploy/monitoring/rules" \
         "$CLEAN/configs/prometheus/rules.r1" \
         "$CLEAN/internal" \
         "$CLEAN/configs/healthchecks"
cp "$SRC" "$CLEAN/scripts/ci/lint-metric-refs.sh"
seed_scrape_configs "$CLEAN"
cat > "$CLEAN/deploy/monitoring/rules/fixture.yml" <<'YML'
groups:
  - name: fixture
    rules:
      - alert: Unrelated
        expr: up == 1
YML
cat > "$CLEAN/internal/unalerted.go" <<'GO'
package internal

var _ = struct{ Name string }{
	Name: "stellarindex_fixture_unalerted_total",
}
GO
CLEAN_OUT="$(bash "$CLEAN/scripts/ci/lint-metric-refs.sh" 2>&1)"
CLEAN_STATUS=$?
if ! grep -q "UNALERTED (advisory): 'stellarindex_fixture_unalerted_total'" <<<"$CLEAN_OUT"; then
  echo "FAIL: emitted-but-unalerted metric should be reported as UNALERTED advisory" >&2
  printf '%s\n' "$CLEAN_OUT" | sed 's/^/    /' >&2
  fail=$((fail + 1))
else
  echo "ok: emitted-but-unalerted metric reported as UNALERTED advisory"; pass=$((pass + 1))
fi
if [ "$CLEAN_STATUS" -ne 0 ]; then
  echo "FAIL: an UNALERTED advisory finding must not fail the gate (exit=$CLEAN_STATUS)" >&2
  printf '%s\n' "$CLEAN_OUT" | sed 's/^/    /' >&2
  fail=$((fail + 1))
else
  echo "ok: UNALERTED advisory does not fail the gate"; pass=$((pass + 1))
fi

# 7. A metric that IS alerted must not be reported as UNALERTED, proving
# the ALERTED_SET built once from RULE_DIRS actually catches a real
# reference (not just an absence of false positives from test 6).
ALERTED="$TMP/alerted-repo"
mkdir -p "$ALERTED/scripts/ci" \
         "$ALERTED/deploy/monitoring/rules" \
         "$ALERTED/configs/prometheus/rules.r1" \
         "$ALERTED/internal" \
         "$ALERTED/configs/healthchecks"
cp "$SRC" "$ALERTED/scripts/ci/lint-metric-refs.sh"
seed_scrape_configs "$ALERTED"
cat > "$ALERTED/deploy/monitoring/rules/fixture.yml" <<'YML'
groups:
  - name: fixture
    rules:
      - alert: FixtureAlerted
        expr: stellarindex_fixture_alerted_total > 0
YML
cat > "$ALERTED/internal/alerted.go" <<'GO'
package internal

var _ = struct{ Name string }{
	Name: "stellarindex_fixture_alerted_total",
}
GO
ALERTED_OUT="$(bash "$ALERTED/scripts/ci/lint-metric-refs.sh" 2>&1)"
if grep -q "UNALERTED (advisory): 'stellarindex_fixture_alerted_total'" <<<"$ALERTED_OUT"; then
  echo "FAIL: a metric referenced by a rule expr must not be reported UNALERTED" >&2
  printf '%s\n' "$ALERTED_OUT" | sed 's/^/    /' >&2
  fail=$((fail + 1))
else
  echo "ok: an alerted metric is not reported UNALERTED"; pass=$((pass + 1))
fi

# 8. job= selectors bind to the scrape config of the SAME tree. A
# multi-host rule carrying r1's hyphenated job name matches no series
# there and is silently dead; an absent_over_time(up{job=...}) over an
# undefined job fires forever.
JOBS="$TMP/jobs-repo"
mkdir -p "$JOBS/scripts/ci" \
         "$JOBS/deploy/monitoring/rules" \
         "$JOBS/configs/prometheus/rules.r1" \
         "$JOBS/internal" \
         "$JOBS/configs/healthchecks"
cp "$SRC" "$JOBS/scripts/ci/lint-metric-refs.sh"
seed_scrape_configs "$JOBS"
cat > "$JOBS/internal/jobs.go" <<'GO'
package internal

var _ = struct{ Name string }{
	Name: "stellarindex_fixture_job_last_success_unix",
}
GO
cat > "$JOBS/deploy/monitoring/rules/fixture.yml" <<'YML'
groups:
  - name: fixture
    rules:
      # a job="ghost_in_comment" named only in a comment is not a selector
      - alert: HyphenOnMultiHost
        expr: (time() - stellarindex_fixture_job_last_success_unix{job="stellarindex-aggregator"}) > 1800
      - alert: UnderscoreOnMultiHost
        expr: (time() - stellarindex_fixture_job_last_success_unix{job="stellarindex_aggregator"}) > 1800
      - alert: RegexWithUndefinedJob
        expr: |
          up{job=~"stellarindex_api|minio_fixture_missing"} == 0
          or absent_over_time(up{job!="negated_is_not_a_reference"}[5m]) == 1
          or up{ops_job="ch-backfill-label-not-job"} == 0
        annotations:
          description: stellarindex_fixture_job_last_success_unix{job="annotation_not_expr"}
YML
cat > "$JOBS/configs/prometheus/rules.r1/fixture.yml" <<'YML'
groups:
  - name: fixture
    rules:
      - alert: HyphenOnR1
        expr: (time() - stellarindex_fixture_job_last_success_unix{job="stellarindex-aggregator"}) > 1800
      - alert: UnderscoreOnR1
        expr: (time() - stellarindex_fixture_job_last_success_unix{job="stellarindex_aggregator"}) > 1800
YML
JOBS_OUT="$(bash "$JOBS/scripts/ci/lint-metric-refs.sh" 2>&1)"
JOBS_STATUS=$?
expect_job() { # <name> <tree-file> <job> <present|absent>
  local hit=0
  grep -qF "DEAD-JOB: $2 selects job=\"$3\"" <<<"$JOBS_OUT" && hit=1
  if { [ "$4" = present ] && [ "$hit" -eq 0 ]; } || { [ "$4" = absent ] && [ "$hit" -eq 1 ]; }; then
    echo "FAIL: $1 — expected DEAD-JOB for '$3' in $2 to be $4" >&2
    printf '%s\n' "$JOBS_OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $1"; pass=$((pass + 1))
}
M=deploy/monitoring/rules/fixture.yml
R=configs/prometheus/rules.r1/fixture.yml
expect_job 'r1 hyphenated job on the multi-host tree is dead' "$M" stellarindex-aggregator present
expect_job 'multi-host job defined under a jinja guard resolves' "$M" stellarindex_aggregator absent
expect_job 'an undefined alternative inside job=~ is dead' "$M" minio_fixture_missing present
expect_job 'a defined alternative inside job=~ resolves' "$M" stellarindex_api absent
expect_job 'a job named only in a comment is not a selector' "$M" ghost_in_comment absent
expect_job 'a negated job matcher is not a reference' "$M" negated_is_not_a_reference absent
expect_job 'an ops_job label is not a job selector' "$M" ch-backfill-label-not-job absent
expect_job 'annotation text is not an expr' "$M" annotation_not_expr absent
expect_job 'r1 hyphenated job on the r1 tree resolves' "$R" stellarindex-aggregator absent
expect_job 'multi-host underscored job on the r1 tree is dead' "$R" stellarindex_aggregator present
if [ "$JOBS_STATUS" -eq 0 ]; then
  echo "FAIL: a dead job selector must fail the gate (exit=0)" >&2
  fail=$((fail + 1))
else
  echo "ok: a dead job selector fails the gate"; pass=$((pass + 1))
fi

echo "----"
echo "lint-metric-refs-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
