#!/usr/bin/env bash
# config-apply-gate.sh — deploy.yml post-deploy forcing function.
#
# deploy-binary.yml swaps BINARIES ONLY: it does NOT render the ansible
# stellarindex.toml, sync Prometheus rules, install systemd units, or apply
# DB schema. So when a release changed any of those surfaces, the feature
# they gate ships DEAD and SILENT unless an operator applies the config too
# (the 2026-08-25 declared-peg + rules.d incidents — see
# docs/operations/deploy-config-apply.md).
#
# The surfaces below are the role's WHOLE config-bearing tree, not just
# templates/: on 2026-08-28 a new node_exporter probe + its systemd units
# shipped as INLINE `content:` blocks inside tasks/10-observability.yml
# (and role scripts live in files/), so a release that touched only
# tasks/ or files/ would have passed this gate and deployed the feature
# dead. A near-miss, caught by hand — hence the two extra entries.
#
# 2026-08-28 (audit deploy-ansible-gate-4): the list was still narrower
# than what the role actually renders. roles/…/defaults/main.yml feeds
# every template (galexie_ledgers_per_file → galexie.toml.j2), inventory
# host_vars do the same, handlers/ decide what restarts, and the role
# COPIES files from outside configs/ansible entirely: configs/healthchecks/*
# (17-stellarindex-healthchecks.yml), scripts/ops/config-assertions.sh
# (15-log-discipline.yml), scripts/ops/{ch-schema-snapshot,restore-drill}.sh
# and scripts/dev/r1-smoke.sh. A defaults-only or healthchecks-only
# release passed this gate. Deliberately NOT listed: playbooks/ and
# tasks/deploy-one-binary.yml (the deploy itself runs them — a change
# there is applied, not dead), migrations/ (deploy-binary.yml migrates)
# and bin/.
#
# This gate diffs the deploying version against a baseline over the config
# surfaces. The baseline is the version live on the host when the caller
# knows it (3rd arg — e.g. read from the host's
# /var/lib/stellarindex/deployed-versions/<binary> sidecar), else the
# previous release tag by ancestry. Ancestry is WRONG for a skip-ahead
# deploy (host on v0.45.0, deploying v0.47.2 diffs only v0.47.1..v0.47.2)
# and for a rollback, so the baseline actually used is always printed.
# If any surface changed and the operator did NOT pass
# config_acknowledged=true, it FAILS — a loud, NON-destructive forcing
# function (the binaries are already deployed by an earlier step; a red
# gate just says "config still needs applying", which is accurate and
# actionable). Acknowledging asserts the operator will apply / has applied
# the config per the runbook.
#
# Fail-closed: an unresolvable version/baseline or a failing `git diff`
# is an ERROR, not "no changes" — a gate that cannot see the diff has no
# basis for a green.
#
# THREE OUTCOMES PER CHANGED SURFACE, not two. Until 2026-09-07 this gate
# knew only "changed" and "unchanged", so every diff was an operator
# decision. Two of the day's four failed deploys were spent discovering
# that a diff was comment-only (deploy/clickhouse/*.sql, twice), and the
# rule people then generalised from that — "comment-only, so acknowledge"
# — is FALSE: v0.61.1..v0.62.0 added `CREATE TABLE
# stellar.account_creators_ops` to account_creators_rollup.sql and
# tier1_schema.sql. That acknowledgement was correct only because the DDL
# had already been applied by hand and the objects confirmed present.
# So each changed surface lands in exactly one of:
#
#   comment-only   every added and removed line is blank or a comment in
#                  that file's syntax, so NOTHING the host renders or
#                  executes differs. Passed by this gate itself; no
#                  operator action. Conservative by construction: a file
#                  type with no known comment convention is substantive,
#                  and any non-comment payload on either side is
#                  substantive. (The host's copy still differs TEXTUALLY
#                  until the config is applied; the weekly ansible-drift
#                  job is what reports that.)
#   applied        substantive, and applied AND VERIFIED on this run by
#                  an automated step that fails closed — passed via
#                  [applied]. See the bar below.
#   substantive    everything else. Blocks unless the operator passes
#                  config_acknowledged=true, which asserts they have
#                  applied it and VERIFIED it landed.
#
# What this gate can and cannot verify itself: nothing. It reads git, not
# the host. "applied" is evidence its CALLER produced —
# .github/workflows/deploy.yml verifies two surfaces and passes only
# those: configs/prometheus/rules.r1/ (apply-rules.sh polls /api/v1/rules)
# and any deploy/clickhouse/*.sql whose diff adds only whole new CREATE
# statements, each of whose objects is then confirmed present in
# system.tables on the target. A systemd unit, an ansible template or a
# ClickHouse diff that ALTERs an existing object has no such check and is
# never auto-passed — see the --ddl-objects mode below for exactly which
# shapes qualify.
#
# Usage: config-apply-gate.sh <version-tag> [acknowledged] [baseline-tag] [applied]
#   <version-tag>   the deploying tag, e.g. v0.43.0
#   [acknowledged]  "true" if the operator passed config_acknowledged=true
#   [baseline-tag]  the version live on the host, if known; defaults to
#                   the previous release tag by ancestry
#   [applied]       space-separated surface prefixes this deploy already
#                   applied AND VERIFIED automatically — see below
#
# Two helper modes expose the classification to callers so there is ONE
# copy of it (scripts/dev/preflight-deploy.sh and deploy.yml both use
# them rather than re-deriving a second opinion the gate would contradict):
#
#   config-apply-gate.sh --payload <baseline> <version> <path>
#       print the substantive (non-blank, non-comment) diff lines for one
#       path. EMPTY output means comment-only. Always exit 0.
#
#   config-apply-gate.sh --ddl-objects <baseline> <version> <path>
#       print the `db.object` names a ClickHouse DDL diff CREATES, and
#       exit 0, ONLY when that file's whole diff is additive whole
#       statements: every hunk begins with a CREATE
#       TABLE/MATERIALIZED VIEW/VIEW/DICTIONARY, no substantive line was
#       removed, and no other DDL/DML verb appears. Otherwise print
#       nothing and exit 1 — object existence would not prove such a diff
#       applied. A column added inside an existing `CREATE TABLE IF NOT
#       EXISTS` is the case this refuses: the object exists either way,
#       and re-running the file would not add the column.
#
# The [applied] argument exists so automation can retire surfaces from
# this gate ONE AT A TIME as they become self-applying, without weakening
# it for the rest. Today the only such surface is
# configs/prometheus/rules.r1/, applied by configs/prometheus/apply-rules.sh
# in the deploy job — which validates with promtool, installs atomically,
# reconciles deletions, reloads, and then POLLS /api/v1/rules until every
# expected alert is loaded and healthy, restoring a backup if not.
#
# The bar for adding to this list is that last clause: the applier must
# VERIFY the surface is live and fail if it is not. A step that merely
# copies a file has not earned an exemption — it reproduces the exact
# 2026-09-01 failure this gate exists to catch, where a merged ClickHouse
# alert watched nothing and a deleted alert kept firing. The caller must
# also pass a surface here ONLY when the apply actually succeeded on this
# run; a pre-declared list would clear the gate for a step that never ran.
set -uo pipefail

# ── Diff classification ──────────────────────────────────────────────
#
# Shared with scripts/dev/preflight-deploy.sh and .github/workflows/deploy.yml
# through the --payload / --ddl-objects modes, so the local preview, the
# workflow's evidence step and this gate's verdict can never disagree.

# comment_marker <path> — the line-comment token for that file type, or
# empty when the convention is not known. Empty is DELIBERATELY the
# conservative answer: the failure to avoid is a rubber-stamped
# acknowledgement, so an unrecognised type falls to a human reading the
# diff.
comment_marker() {
  case "$1" in
    *.sql) printf -- '--' ;;
    *.j2|*.yml|*.yaml|*.sh|*.service|*.timer|*.conf|*.cfg|*.ini|*.toml|*.py|*.rules) printf '#' ;;
    *.go|*.ts|*.js) printf '//' ;;
    *) printf '' ;;
  esac
}

# diff_body <baseline> <version> <path> — the diff's CONTENT lines only,
# one per line, with "@" marking each hunk boundary.
#
# Everything before the first @@ is header, so it is dropped by position
# rather than by pattern. Matching `^---` instead (the obvious spelling)
# also eats a REMOVED line that begins with `--`: every removed SQL
# comment, and a removed YAML `---` document separator.
diff_body() {
  git diff -U0 --no-color --src-prefix=a/ --dst-prefix=b/ "$1" "$2" -- "$3" \
    | awk '
        /^@@/ { in_hunk = 1; print "@"; next }
        !in_hunk { next }
        /^\\ No newline/ { next }
        /^[+-]/ { print }
      '
}

# surface_payload <baseline> <version> <path> — the substantive diff
# lines. EMPTY means comment-only.
surface_payload() {
  local marker
  marker="$(comment_marker "$3")"
  if [ -z "$marker" ]; then
    printf '%s\n' "<no comment convention is known for this file type — substantive by default>"
    return 0
  fi
  diff_body "$1" "$2" "$3" \
    | sed -E '/^@$/d; s/^[+-]//' \
    | grep -vE "^[[:space:]]*(${marker}|\$)"
  return 0
}

# ddl_objects <baseline> <version> <path> — see the --ddl-objects usage
# above. Exit 1 (printing nothing) whenever object existence would not
# prove this diff applied.
ddl_objects() {
  diff_body "$1" "$2" "$3" | awk '
    function substantive(s) { return (s ~ /^[[:space:]]*$/ || s ~ /^[[:space:]]*--/) ? 0 : 1 }
    /^@$/ { at_hunk_start = 1; next }
    /^-/  { if (substantive(substr($0, 2))) unverifiable = 1; next }
    /^\+/ {
      line = substr($0, 2)
      if (!substantive(line)) next
      upper = toupper(line)
      creates = (upper ~ /^CREATE (OR REPLACE )?(TABLE|MATERIALIZED VIEW|VIEW|DICTIONARY) (IF NOT EXISTS )?[A-Za-z0-9_]+\.[A-Za-z0-9_]+/)
      # Each hunk must OPEN a new statement. A hunk that starts mid-body
      # is a column/engine/setting edit inside an object that already
      # exists, which existence cannot distinguish from unapplied.
      if (at_hunk_start && !creates) unverifiable = 1
      at_hunk_start = 0
      if (creates) {
        n = split(line, token, /[[:space:](]+/)
        for (i = 1; i <= n; i++) {
          if (token[i] ~ /^[A-Za-z0-9_]+\.[A-Za-z0-9_]+$/) { name[++found] = token[i]; break }
        }
        next
      }
      if (upper ~ /^(ALTER|DROP|RENAME|TRUNCATE|INSERT|DELETE|UPDATE|OPTIMIZE|EXCHANGE|ATTACH|DETACH|SYSTEM|GRANT|REVOKE|CREATE)([[:space:]]|$)/) unverifiable = 1
    }
    END {
      if (unverifiable || found == 0) exit 1
      for (i = 1; i <= found; i++) print name[i]
    }
  '
}

case "${1:-}" in
  --payload)
    [ "$#" -eq 4 ] || { echo "usage: config-apply-gate.sh --payload <baseline> <version> <path>" >&2; exit 2; }
    surface_payload "$2" "$3" "$4"
    exit 0
    ;;
  --ddl-objects)
    [ "$#" -eq 4 ] || { echo "usage: config-apply-gate.sh --ddl-objects <baseline> <version> <path>" >&2; exit 2; }
    ddl_objects "$2" "$3" "$4"
    exit $?
    ;;
esac

VERSION="${1:?deploying version tag, e.g. v0.43.0}"
ACK="${2:-false}"
BASELINE="${3:-}"
APPLIED="${4:-}"

# Config surfaces a binary deploy does NOT apply. Directory prefixes are
# git pathspecs: the trailing slash matches everything beneath them.
# Keep in lockstep with config-apply-gate-test.sh (one fixture per entry).
SURFACES=(
  'configs/ansible/roles/'
  'configs/ansible/inventory/'
  'configs/healthchecks/'
  'configs/prometheus/rules.r1/'
  'configs/alertmanager/'
  'deploy/monitoring/rules/'
  'deploy/systemd/'
  'deploy/clickhouse/'
  # Repo scripts the archival-node role copies onto the host verbatim.
  'scripts/ops/config-assertions.sh'
  'scripts/ops/ch-schema-snapshot.sh'
  'scripts/ops/restore-drill.sh'
  'scripts/dev/r1-smoke.sh'
)

if ! git rev-parse -q --verify "${VERSION}^{commit}" >/dev/null; then
  echo "::error::config-apply gate: deploying version '${VERSION}' does not resolve to a commit in this checkout — cannot diff config surfaces; refusing to pass (fail-closed). Fetch tags (fetch-depth: 0) or check the tag name."
  exit 1
fi

if [ -n "$BASELINE" ]; then
  if ! git rev-parse -q --verify "${BASELINE}^{commit}" >/dev/null; then
    echo "::error::config-apply gate: host baseline '${BASELINE}' does not resolve to a commit in this checkout — cannot diff config surfaces; refusing to pass (fail-closed)."
    exit 1
  fi
  PREV="$BASELINE"
  echo "baseline: ${PREV} (host-reported live version)"
else
  # Ancestry fallback: the previous release tag = the config baseline
  # ASSUMED live on the host.
  PREV="$(git describe --tags --abbrev=0 "${VERSION}^" 2>/dev/null || true)"
  if [ -z "$PREV" ]; then
    echo "note: no release tag before ${VERSION}; skipping config-drift gate (first release / shallow clone)."
    exit 0
  fi
  echo "baseline: ${PREV} (previous release tag by ancestry; for a skip-ahead or rollback deploy pass the host's live version as the 3rd argument)"
fi

if ! CHANGED="$(git diff --name-only "$PREV" "$VERSION" -- "${SURFACES[@]}")"; then
  echo "::error::config-apply gate: git diff ${PREV}..${VERSION} failed — cannot determine which config surfaces changed; refusing to pass (fail-closed)."
  exit 1
fi

SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/stdout}"

# Subtract surfaces this deploy applied AND verified automatically. They
# stay in SURFACES above (so the diff still SEES them, and a run that did
# not apply them still gates on them) — they are only removed from what
# the operator is asked to acknowledge.
# Matching is an anchored PATH-PREFIX test, not a substring search. A
# substring match would let a loose token ("rules") silently exempt
# deploy/monitoring/rules/ as well as configs/prometheus/rules.r1/ —
# quietly widening the exemption to a surface nothing applied, which is
# the failure mode this whole gate exists to prevent.
AUTO_APPLIED=""
if [ -n "$APPLIED" ] && [ -n "$CHANGED" ]; then
  remaining=""
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    exempt=""
    for prefix in $APPLIED; do
      case "$path" in
        "$prefix"*) exempt=1; break ;;
      esac
    done
    if [ -n "$exempt" ]; then
      AUTO_APPLIED="${AUTO_APPLIED}${path}"$'\n'
    else
      remaining="${remaining}${path}"$'\n'
    fi
  done <<EOF
$CHANGED
EOF
  CHANGED="$(printf '%s' "$remaining")"
fi

if [ -n "$AUTO_APPLIED" ]; then
  n=$(printf '%s' "$AUTO_APPLIED" | grep -c . || true)
  echo "✓ ${n} config surface(s) were applied and VERIFIED automatically by this deploy:"
  printf '%s' "$AUTO_APPLIED" | sed 's/^/    /'
  {
    echo "- ✓ config-apply gate: **${n}** surface(s) applied *and verified live* automatically by this deploy (not left to the operator)."
  } >>"$SUMMARY" 2>/dev/null || true
fi

# Separate the comment-only surfaces from the substantive ones. This runs
# AFTER the [applied] subtraction, so a surface this deploy applied and
# verified is reported as applied rather than re-judged on its text.
#
# Classification is per FILE and the whole set must clear: one substantive
# file keeps the gate red for the release.
COMMENT_ONLY=""
if [ -n "$CHANGED" ]; then
  remaining=""
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    if [ -z "$(surface_payload "$PREV" "$VERSION" "$path")" ]; then
      COMMENT_ONLY="${COMMENT_ONLY}${path}"$'\n'
    else
      remaining="${remaining}${path}"$'\n'
    fi
  done <<EOF
$CHANGED
EOF
  CHANGED="$(printf '%s' "$remaining")"
fi

if [ -n "$COMMENT_ONLY" ]; then
  n=$(printf '%s' "$COMMENT_ONLY" | grep -c . || true)
  echo "✓ ${n} changed config surface(s) are COMMENT-ONLY — every added and removed line is blank or a comment in that file's syntax, so nothing the host renders or executes differs:"
  printf '%s' "$COMMENT_ONLY" | sed 's/^/    /'
  echo "  (they still differ TEXTUALLY from the host copies until the config is applied; the weekly ansible-drift job is what reports that.)"
  {
    echo "- ✓ config-apply gate: **${n}** changed surface(s) are comment-only — no rendered behaviour differs, no operator action."
  } >>"$SUMMARY" 2>/dev/null || true
fi

if [ -z "$CHANGED" ]; then
  if [ -n "$AUTO_APPLIED" ] || [ -n "$COMMENT_ONLY" ]; then
    echo "✓ every changed config surface between ${PREV} and ${VERSION} is comment-only or was applied and verified automatically — nothing left for the operator."
    echo "- ✓ config-apply gate: every changed surface was comment-only or auto-applied and verified; no operator action outstanding." >>"$SUMMARY" 2>/dev/null || true
  else
    echo "✓ no config-surface changes between ${PREV} and ${VERSION} — the binary deploy is complete."
    echo "- ✓ config-apply gate: no config-surface changes between \`${PREV}\` and \`${VERSION}\`." >>"$SUMMARY" 2>/dev/null || true
  fi
  exit 0
fi

{
  echo "## ⚠️ Config-apply required — a binary deploy does NOT apply these"
  echo ""
  echo "Release **${VERSION}** changed these config/schema surfaces SUBSTANTIVELY since **${PREV}**"
  echo "(comment-only surfaces and surfaces this deploy applied and verified are already cleared above)."
  echo "\`deploy-binary.yml\` swaps binaries only. Apply them per"
  echo "\`docs/operations/deploy-config-apply.md\` and **verify each landed on the host**"
  echo "(grep the live surface — never trust \"deploy OK\"):"
  echo ""
  echo '```'
  echo "$CHANGED"
  echo '```'
} >>"$SUMMARY" 2>/dev/null || true

if [ "$ACK" = "true" ]; then
  echo "::notice::config-apply acknowledged for ${VERSION} — operator asserts the $(echo "$CHANGED" | grep -c . ) SUBSTANTIVE config surface(s) above are/will be applied AND verified per the runbook. This gate reads git, not the host: it cannot confirm that assertion."
  echo "" >>"$SUMMARY" 2>/dev/null || true
  echo "_Operator passed \`config_acknowledged=true\` — gate satisfied._" >>"$SUMMARY" 2>/dev/null || true
  exit 0
fi

echo "::error::Release ${VERSION} changed $(echo "$CHANGED" | grep -c . ) config surface(s) SUBSTANTIVELY — a binary deploy does NOT apply them, and neither comment-only classification nor an automated verifier cleared them. Apply them per docs/operations/deploy-config-apply.md, VERIFY each landed on the host, then re-run deploy with -f config_acknowledged=true (or pass it now if you have already applied AND verified them). Acknowledging without verifying is what ships a schema change dead: v0.61.1..v0.62.0 looked like the two comment-only ClickHouse diffs before it and was not. Binaries are already deployed; this gate only flags the outstanding config-apply."
exit 1
