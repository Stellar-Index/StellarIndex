#!/usr/bin/env bash
# check-directory-pin.sh — decision core for .github/workflows/directory-pin-reminder.yml.
#
# configs/ansible/roles/archival-node/defaults/main.yml pins the stellar-expert
# public-directory archive at ONE commit (directory_sync_url + directory_sync_sha256).
# A reviewed pin feeds the scam gate, so it must never track a moving branch, but
# nothing prompted a bump: the pin went 45 days stale before
# stellarindex_directory_sync_snapshot_stale was about to fire. This reports
# when the pin trails upstream by more than THRESHOLD_DAYS while upstream has
# newer commits.
#
# Usage:
#   check-directory-pin.sh parse                  prints repo= and pin= from the defaults file
#   check-directory-pin.sh verdict                prints verdict=STALE|CURRENT plus the facts
#
# Inputs (env):
#   DEFAULTS_FILE     defaults/main.yml (default: the archival-node role's)
#   PIN_COMMIT_JSON   GitHub API commit object for the pinned commit      (verdict)
#   HEAD_COMMIT_JSON  GitHub API commit object for upstream's default head (verdict)
#   COMPARE_JSON      GitHub API compare object pin...head                 (verdict)
#   THRESHOLD_DAYS    default 21
#   BODY_OUT          when set and the verdict is STALE, the issue body is written here
# Only API metadata is read; no upstream content is downloaded or executed.
set -euo pipefail

DEFAULTS_FILE="${DEFAULTS_FILE:-configs/ansible/roles/archival-node/defaults/main.yml}"
THRESHOLD_DAYS="${THRESHOLD_DAYS:-21}"
RUNBOOK="docs/operations/runbooks/api.md#stellarindex_directory_sync_snapshot_stale"

die() { echo "check-directory-pin: $*" >&2; exit 2; }

parse() {
  local line
  line="$(grep -E '^directory_sync_url:' "$DEFAULTS_FILE" || true)"
  [ -n "$line" ] || die "no directory_sync_url in $DEFAULTS_FILE"
  repo="$(printf '%s' "$line" | sed -nE 's#.*github\.com/([^/]+/[^/]+)/archive/.*#\1#p')"
  pin="$(printf '%s' "$line" | sed -nE 's#.*/archive/([0-9a-f]{40})\.tar\.gz.*#\1#p')"
  [ -n "$repo" ] || die "directory_sync_url is not a github.com/<owner>/<repo>/archive/ URL"
  [ -n "$pin" ] || die "directory_sync_url is not pinned to a 40-hex commit (a moving branch must never feed the scam gate)"
}

# shellcheck disable=SC2016 # backticks in the printf formats are literal markdown
verdict() {
  parse
  local f
  for f in PIN_COMMIT_JSON HEAD_COMMIT_JSON COMPARE_JSON; do
    [ -f "${!f:-}" ] || die "$f must name a file"
  done
  local pin_date head_date head ahead
  pin_date="$(jq -er '.commit.committer.date' "$PIN_COMMIT_JSON")" || die "pin commit has no committer date"
  head_date="$(jq -er '.commit.committer.date' "$HEAD_COMMIT_JSON")" || die "head commit has no committer date"
  head="$(jq -er '.sha' "$HEAD_COMMIT_JSON")" || die "head commit has no sha"
  ahead="$(jq -er '.ahead_by' "$COMPARE_JSON")" || die "compare has no ahead_by"
  local days
  days="$(jq -nr --arg a "$pin_date" --arg b "$head_date" \
    '((($b | fromdateiso8601) - ($a | fromdateiso8601)) / 86400) | floor')" || die "unparseable commit dates"

  local v=CURRENT
  if [ "$days" -gt "$THRESHOLD_DAYS" ] && [ "$ahead" -gt 0 ]; then v=STALE; fi
  printf 'verdict=%s\nrepo=%s\npin=%s\npin_date=%s\nhead=%s\nhead_date=%s\ndays_behind=%s\nahead_by=%s\n' \
    "$v" "$repo" "$pin" "$pin_date" "$head" "$head_date" "$days" "$ahead"

  if [ "$v" = STALE ] && [ -n "${BODY_OUT:-}" ]; then
    local url="https://github.com/${repo}/archive/${head}.tar.gz"
    {
      printf 'The directory-sync pin trails upstream by **%s days** (threshold %s) and upstream has %s newer commit(s).\n\n' \
        "$days" "$THRESHOLD_DAYS" "$ahead"
      printf '| | commit | date |\n| --- | --- | --- |\n'
      printf '| pinned | `%s` | %s |\n' "$pin" "$pin_date"
      printf '| upstream head | `%s` | %s |\n\n' "$head" "$head_date"
      printf '### Bump\n\nIn `%s` change:\n\n' "$DEFAULTS_FILE"
      printf '```yaml\ndirectory_sync_url: "%s"\ndirectory_sync_sha256: "<sha256 of that tarball>"\n```\n\n' "$url"
      printf 'Compute the digest after review: `curl -sL %s | shasum -a 256`\n\n' "$url"
      printf '### Review before bumping\n\n'
      printf -- '- entries added or removed since the pin\n'
      printf -- '- existing entries newly scam-flagged, or un-flagged\n'
      printf -- '- renamed entries\n'
      printf -- '- tag vocabulary changes (the scam gate reads tags)\n\n'
      printf 'Runbook: [%s](%s)\n\n' "$RUNBOOK" "$RUNBOOK"
      printf 'Always pin a reviewed commit, never a branch.\n'
    } > "$BODY_OUT"
  fi
}

case "${1:-}" in
  parse) parse; printf 'repo=%s\npin=%s\n' "$repo" "$pin" ;;
  verdict) verdict ;;
  *) die "usage: $0 parse|verdict" ;;
esac
