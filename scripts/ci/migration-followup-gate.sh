#!/usr/bin/env bash
# migration-followup-gate.sh — deploy.yml pre-playbook forcing function.
#
# Some migrations blank data a deployment then has to rebuild by hand: an
# unqualified DELETE before a projector-replay (0164 cctp/rozo), or a
# continuous aggregate recreated WITH NO DATA before its refresh. The
# migration cannot run that rebuild itself, and until someone does, reads
# of that data serve empty. Each such up.sql carries one header line per
# command, enforced by pass 10 of scripts/ci/lint-migrations.sh:
#
#   -- REQUIRED-FOLLOWUP: <command>
#
# This gate lists those lines from every up.sql the deploying version ADDS
# over the baseline, and FAILS unless the operator passed
# followups_acknowledged=true. It runs before the playbook, so nothing
# blanks until someone has said they will run the rebuild; the commands are
# also written to the step summary.
#
# Baseline: the version live on the host when the caller knows it (3rd
# arg), else the previous release tag by ancestry — the same rule as
# config-apply-gate.sh. A skip-ahead deploy therefore lists the markers of
# every skipped release, and a rollback adds no migrations and passes.
#
# Fail-closed: an unresolvable version/baseline or a failing git command
# is an ERROR, never "no follow-ups".
#
# Usage: migration-followup-gate.sh <version-tag> [acknowledged] [baseline-tag]
set -uo pipefail

VERSION="${1:?deploying version tag, e.g. v0.43.0}"
ACK="${2:-false}"
BASELINE="${3:-}"
MARKER='-- REQUIRED-FOLLOWUP: '

if ! git rev-parse -q --verify "${VERSION}^{commit}" >/dev/null; then
  echo "::error::migration follow-up gate: deploying version '${VERSION}' does not resolve to a commit in this checkout — cannot list its migrations; refusing to pass (fail-closed)."
  exit 1
fi

if [ -n "$BASELINE" ]; then
  if ! git rev-parse -q --verify "${BASELINE}^{commit}" >/dev/null; then
    echo "::error::migration follow-up gate: host baseline '${BASELINE}' does not resolve to a commit in this checkout — cannot list the migrations this deploy adds; refusing to pass (fail-closed)."
    exit 1
  fi
  PREV="$BASELINE"
  echo "baseline: ${PREV} (host-reported live version)"
else
  PREV="$(git describe --tags --abbrev=0 "${VERSION}^" 2>/dev/null || true)"
  if [ -z "$PREV" ]; then
    echo "note: no release tag before ${VERSION}; skipping the migration follow-up gate (first release / shallow clone)."
    exit 0
  fi
  echo "baseline: ${PREV} (previous release tag by ancestry)"
fi

if ! ADDED="$(git diff --name-only --diff-filter=A "$PREV" "$VERSION" -- 'migrations/*.up.sql')"; then
  echo "::error::migration follow-up gate: git diff ${PREV}..${VERSION} failed — cannot list the migrations this deploy adds; refusing to pass (fail-closed)."
  exit 1
fi

FOLLOWUPS=""
while IFS= read -r path; do
  [ -n "$path" ] || continue
  if ! body="$(git show "${VERSION}:${path}")"; then
    echo "::error::migration follow-up gate: cannot read ${path} at ${VERSION}; refusing to pass (fail-closed)."
    exit 1
  fi
  while IFS= read -r line; do
    case "$line" in
      "$MARKER"*) FOLLOWUPS="${FOLLOWUPS}${path##*/}: ${line#"$MARKER"}"$'\n' ;;
    esac
  done <<<"$body"
done <<<"$ADDED"

SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/stdout}"
n_added=$(printf '%s' "$ADDED" | grep -c . || true)

if [ -z "$FOLLOWUPS" ]; then
  echo "✓ none of the ${n_added} migration(s) ${PREV}..${VERSION} adds declares a required follow-up."
  exit 0
fi

n=$(printf '%s' "$FOLLOWUPS" | grep -c . || true)
{
  echo "## ⚠️ Migration follow-up required — run these after the deploy applies its migrations"
  echo ""
  echo "Release **${VERSION}** adds migrations over **${PREV}** that leave data empty until"
  echo "these commands run. Run each on the host, in order, once the deploy finishes:"
  echo ""
  echo "Do not run a \`projected-rebuild\` for a source these migrations empty while they apply:"
  echo "it can checkpoint a window before the rows go, and \`-resume\` then skips that window."
  echo ""
  echo '```'
  printf '%s' "$FOLLOWUPS"
  echo '```'
} >>"$SUMMARY" 2>/dev/null || true

if [ "$ACK" = "true" ]; then
  echo "::notice::migration follow-ups acknowledged for ${VERSION} — operator asserts the ${n} command(s) below will run once the migrations are applied. This gate reads git, not the host: it cannot confirm they ran."
  printf '%s' "$FOLLOWUPS" | sed 's/^/    /'
  exit 0
fi

echo "::error::Release ${VERSION} adds ${n} required migration follow-up(s) over ${PREV}: the data they name serves empty from the moment the migration applies until each command runs. Nothing has been deployed yet. Re-run the deploy with -f followups_acknowledged=true once you are ready to run them straight after it, and with no projected-rebuild of an emptied source running (it can checkpoint a window before the rows go, and -resume then skips it):"
printf '%s' "$FOLLOWUPS" | sed 's/^/    /'
exit 1
