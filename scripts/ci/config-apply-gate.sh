#!/usr/bin/env bash
# config-apply-gate.sh — deploy.yml post-deploy forcing function.
#
# deploy-binary.yml swaps BINARIES ONLY: it does NOT render the ansible
# stellarindex.toml, sync Prometheus rules, install systemd units, or apply DB
# schema. When a release changed any of those, the feature they gate ships DEAD
# and SILENT unless an operator applies the config too
# (docs/operations/deploy-config-apply.md).
#
# SURFACES is the role's WHOLE config-bearing tree, not just templates/: tasks/
# carries INLINE `content:` blocks, role scripts live in files/, defaults/main.yml
# and inventory host_vars feed every template (galexie_ledgers_per_file ->
# galexie.toml.j2), handlers/ decide what restarts, and the role COPIES files
# from outside configs/ansible: configs/healthchecks/*,
# scripts/ops/{config-assertions,ch-schema-snapshot,restore-drill}.sh and
# scripts/dev/r1-smoke.sh. Deliberately NOT listed: playbooks/ and
# tasks/deploy-one-binary.yml (the deploy runs them, so a change is applied, not
# dead), migrations/ (deploy-binary.yml migrates) and bin/.
#
# The gate diffs the deploying version against a baseline over those surfaces.
# Baseline = the version live on the host when the caller knows it (3rd arg, e.g.
# the host's /var/lib/stellarindex/deployed-versions/<binary> sidecar), else the
# previous release tag by ancestry. Ancestry is WRONG for a skip-ahead deploy
# (host on v0.45.0, deploying v0.47.2 diffs only v0.47.1..v0.47.2) and for a
# rollback, so the baseline used is always printed. If any surface changed and
# config_acknowledged=true was not passed, it FAILS: a loud, NON-destructive
# forcing function (the binaries are already deployed; red just says "config
# still needs applying"). Acknowledging asserts the operator applied it per the
# runbook.
#
# Fail-closed: an unresolvable version/baseline or a failing `git diff` is an
# ERROR, not "no changes"; a gate that cannot see the diff has no basis for green.
#
# THREE OUTCOMES per changed surface, so a comment-only diff needs no operator
# decision and a DDL diff is never waved through as "probably comments":
#
#   comment-only   every added and removed line is blank or a comment in that
#                  file's syntax, so NOTHING the host renders or executes
#                  differs. Passed by the gate; no operator action. A file type
#                  with no known comment convention is substantive. (The host
#                  copy still differs TEXTUALLY until applied; the weekly
#                  ansible-drift job reports that.)
#   applied        substantive, but applied AND VERIFIED on this run by an
#                  automated step that fails closed; passed via [applied].
#   substantive    everything else. Blocks unless config_acknowledged=true,
#                  which asserts it was applied and VERIFIED landed.
#
# The gate reads git, not the host, so it verifies nothing itself: "applied" is
# evidence its CALLER produced. deploy.yml passes only two surfaces:
# configs/prometheus/rules.r1/ (apply-rules.sh polls /api/v1/rules) and any
# deploy/clickhouse/*.sql whose diff adds only whole new CREATE statements, each
# object then confirmed in system.tables on the target. A systemd unit, an
# ansible template or a ClickHouse diff that ALTERs an existing object has no
# such check and is never auto-passed (see --ddl-objects).
#
# Usage: config-apply-gate.sh <version-tag> [acknowledged] [baseline-tag] [applied] [refuted] [unanswered]
#   <version-tag>   the deploying tag, e.g. v0.43.0
#   [acknowledged]  "true" if the operator passed config_acknowledged=true
#   [baseline-tag]  the version live on the host, if known; defaults to the
#                   previous release tag by ancestry
#   [applied]       space-separated surface prefixes this run applied AND
#                   verified live
#   [refuted]       exact paths this run PROVED unapplied by asking the host;
#                   [acknowledged] cannot clear these
#   [unanswered]    exact paths this run tried to check on a host that runs
#                   ClickHouse but did not answer; [acknowledged] cannot clear
#                   these either
#
# Two helper modes expose the classification so there is ONE copy of it
# (scripts/dev/preflight-deploy.sh and deploy.yml both use them rather than
# re-deriving an opinion the gate would contradict):
#
#   config-apply-gate.sh --payload <baseline> <version> <path>
#       print the substantive (non-blank, non-comment) diff lines for one path.
#       EMPTY output means comment-only. Always exit 0.
#
#   config-apply-gate.sh --ddl-objects <baseline> <version> <path>
#       print the `db.object` names a ClickHouse DDL diff CREATES, and exit 0,
#       ONLY when the whole diff is additive whole statements: every hunk begins
#       with a CREATE TABLE/MATERIALIZED VIEW/VIEW/DICTIONARY, no substantive
#       line was removed, and no other DDL/DML verb appears. Otherwise print
#       nothing and exit 1, since object existence would not prove the diff
#       applied. A column added inside an existing `CREATE TABLE IF NOT EXISTS`
#       is refused: the object exists either way and re-running would not add it.
#
# [applied] lets automation retire surfaces from this gate ONE AT A TIME as they
# become self-applying. Today only configs/prometheus/rules.r1/, applied by
# configs/prometheus/apply-rules.sh: it validates with promtool, installs
# atomically, reconciles deletions, reloads, then POLLS /api/v1/rules until every
# expected alert is loaded and healthy, restoring a backup if not.
#
# The bar for adding to the list is that last clause: the applier must VERIFY the
# surface is live and fail if not. A step that merely copies a file has not
# earned an exemption (a copied alert can watch nothing while a deleted one keeps
# firing). The caller passes a surface ONLY when the apply succeeded on this run;
# a pre-declared list would clear the gate for a step that never ran.
set -uo pipefail

# ── Diff classification ──────────────────────────────────────────────
#
# Shared with scripts/dev/preflight-deploy.sh and deploy.yml via --payload /
# --ddl-objects, so the preview, the evidence step and the verdict cannot disagree.

# comment_marker <path> — the line-comment token for that file type, or empty if
# unknown. Empty is DELIBERATELY conservative: the failure to avoid is a
# rubber-stamped acknowledgement, so an unrecognised type goes to a human.
comment_marker() {
  case "$1" in
    *.sql) printf -- '--' ;;
    *.j2|*.yml|*.yaml|*.sh|*.service|*.timer|*.conf|*.cfg|*.ini|*.toml|*.py|*.rules) printf '#' ;;
    *.go|*.ts|*.js) printf '//' ;;
    *) printf '' ;;
  esac
}

# diff_body <baseline> <version> <path> — the diff's CONTENT lines only, one per
# line, with "@" marking each hunk boundary. Header is dropped by position, not
# by `^---`, which would also eat a REMOVED line starting `--` (a removed SQL
# comment, a removed YAML `---` separator).
diff_body() {
  git diff -U0 --no-color --src-prefix=a/ --dst-prefix=b/ "$1" "$2" -- "$3" \
    | awk '
        /^@@/ { in_hunk = 1; print "@"; next }
        !in_hunk { next }
        /^\\ No newline/ { next }
        /^[+-]/ { print }
      '
}

# surface_payload <baseline> <version> <path> — substantive diff lines; EMPTY
# means comment-only.
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

# ddl_objects <baseline> <version> <path> — see --ddl-objects above. Exit 1 with
# no output whenever object existence would not prove the diff applied.
ddl_objects() {
  # A cut-over DDL declares its CREATEd objects transient (si-cutover-object):
  # they exist only until the operator renames them onto the canonical name, so
  # absence is the FINISHED state as much as the never-run one. Existence proves
  # nothing; the surface stays with the operator's acknowledgement.
  local body
  body=$(git show "$2:$3" 2>/dev/null || true)
  if grep -q '^-- si-cutover-object:' <<<"$body"; then
    return 1
  fi
  diff_body "$1" "$2" "$3" | awk '
    function substantive(s) { return (s ~ /^[[:space:]]*$/ || s ~ /^[[:space:]]*--/) ? 0 : 1 }
    /^@$/ { at_hunk_start = 1; next }
    /^-/  { if (substantive(substr($0, 2))) unverifiable = 1; next }
    /^\+/ {
      line = substr($0, 2)
      if (!substantive(line)) next
      upper = toupper(line)
      creates = (upper ~ /^CREATE (OR REPLACE )?(TABLE|MATERIALIZED VIEW|VIEW|DICTIONARY) (IF NOT EXISTS )?[A-Za-z0-9_]+\.[A-Za-z0-9_]+/)
      # Each hunk must OPEN a new statement; one starting mid-body is an edit
      # inside an existing object, which existence cannot tell from unapplied.
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
REFUTED="${5:-}"
UNANSWERED="${6:-}"

# Config surfaces a binary deploy does NOT apply. Directory prefixes are git
# pathspecs. Keep in lockstep with config-apply-gate-test.sh (one fixture each).
SURFACES=(
  'configs/ansible/roles/'
  'configs/ansible/inventory/'
  'configs/healthchecks/'
  'configs/prometheus/rules.r1/'
  # r1's scrape config and single-host Loki/promtail: installed by hand per
  # their READMEs.
  'configs/prometheus/prometheus.r1.yml'
  'configs/loki/'
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
  # Ancestry fallback: the previous release tag is ASSUMED live on the host.
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

# Subtract surfaces this deploy applied AND verified. They stay in SURFACES (the
# diff still SEES them, and a run that did not apply them still gates); they are
# only removed from what the operator must acknowledge. Matching is an anchored
# PATH-PREFIX test: a substring match would let a loose token ("rules") also
# exempt deploy/monitoring/rules/, widening the exemption to a surface nothing
# applied.
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

# Separate comment-only surfaces from substantive ones. Runs AFTER the [applied]
# subtraction, so an applied-and-verified surface is reported as applied, not
# re-judged on its text. Classification is per FILE; one substantive file keeps
# the gate red.
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

# Surfaces this run PROVED unapplied by ASKING THE HOST (deploy.yml's ch_ddl step
# found every object the file creates absent from system.tables).
#
# Without this list the gate cannot tell "nobody asked" from "asked, and the
# answer was no", and a missing table behind a LEFT JOIN 500s its endpoint until
# applied by hand. config_acknowledged=true asserts a surface is applied; where
# the host answered otherwise in this same run, the run's own evidence refutes it.
# Unasked surfaces behave as before, so this NARROWS the acknowledgement only
# where it is still an open question.
#
# Matching is exact-path (the evidence step emits whole paths), the mirror of the
# [applied] anchored prefix test: a loose match would refuse surfaces nobody asked
# about.
# changed_in <space-separated paths> → the CHANGED paths it names, one per line.
changed_in() {
  [ -n "$1" ] || return 0
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    for r in $1; do
      [ "$path" = "$r" ] || continue
      printf '%s\n' "$path"
      break
    done
  done <<EOF
$CHANGED
EOF
}

REFUTED_HERE="$(changed_in "$REFUTED")"

if [ -n "$REFUTED_HERE" ]; then
  n=$(printf '%s\n' "$REFUTED_HERE" | grep -c . || true)
  {
    echo "## ⛔ Config-apply PROVED outstanding — the host was asked"
    echo ""
    echo "This deploy asked the host which ClickHouse objects exist, and **${n}** changed"
    echo "surface(s) create objects it does **not** have. \`config_acknowledged\` cannot"
    echo "clear these: an acknowledgement asserts a surface is applied, and the host"
    echo "answered otherwise in this same run."
    echo ""
    echo '```'
    printf '%s\n' "$REFUTED_HERE"
    echo '```'
  } >>"$SUMMARY" 2>/dev/null || true
  echo "::error::${n} config surface(s) were PROVED unapplied on ${VERSION}'s target during this run — every object they create is absent from system.tables. config_acknowledged does NOT clear these: an acknowledgement asserts a surface is applied, and the host answered otherwise in this same run. Apply them per docs/operations/deploy-config-apply.md, confirm the objects exist, then re-run the deploy. The binaries are already live, and a feature whose schema half is missing does not degrade quietly — it fails: v0.91.0 shipped with stellar.asset_month_usd_prices absent and every cohort request answered 500."
  printf '%s\n' "$REFUTED_HERE" | sed 's/^/    /'
  exit 1
fi

# Surfaces the same step tried to check on a ClickHouse host that did not answer.
# Silence could hide a refutation, so the assertion is held to the same bar.
UNANSWERED_HERE="$(changed_in "$UNANSWERED")"
if [ -n "$UNANSWERED_HERE" ]; then
  n=$(printf '%s\n' "$UNANSWERED_HERE" | grep -c . || true)
  {
    echo "## ⛔ Config-apply UNVERIFIED — the host was asked and did not answer"
    echo ""
    echo "This deploy tried to ask the host which ClickHouse objects exist and got no"
    echo "answer, so **${n}** changed surface(s) could be missing. \`config_acknowledged\`"
    echo "cannot clear these while the host's own evidence is outstanding."
    echo ""
    echo '```'
    printf '%s\n' "$UNANSWERED_HERE"
    echo '```'
  } >>"$SUMMARY" 2>/dev/null || true
  echo "::error::${n} config surface(s) could NOT be checked on ${VERSION}'s target: the host runs ClickHouse but did not answer which objects exist. config_acknowledged does NOT clear these — an unanswered check could be hiding objects the host lacks. Make clickhouse-server reachable on the host, then re-run the deploy so the check can answer."
  printf '%s\n' "$UNANSWERED_HERE" | sed 's/^/    /'
  exit 1
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
