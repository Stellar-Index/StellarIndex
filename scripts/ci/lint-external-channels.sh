#!/usr/bin/env bash
# lint-external-channels.sh — a shipped file may not point at a channel that
# is switched off.
#
# GitHub repository settings and DNS names live outside the repo, so no other
# gate sees them, and three shipped as promises while off: SUPPORT.md linked
# Discussions on a repo with Discussions disabled; SECURITY.md offered the
# Security-tab reporting button with private vulnerability reporting disabled
# (so BOTH disclosure routes were dead at once); the OpenAPI spec carried a
# Staging server for a hostname with no DNS record, inherited by every client.
#
# docs/operations/external-channels.md holds each channel's state and the
# literal strings that must not appear while it is `disabled`, so the switch
# and the wording live in one file: enabling a channel is a one-line flip,
# after which the same commit may restore the text.
#
# Exit code is the number of violations (consumed like lint-doc-links.sh).
#
# Env overrides (used by the self-test): CHANNELS_MANIFEST.
#
# Env overrides (used by the self-test): CHANNELS_MANIFEST.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

MANIFEST="${CHANNELS_MANIFEST:-docs/operations/external-channels.md}"
MIN_PATTERN_LEN=8

fail() { echo "  FAIL $*" >&2; }

if [ ! -f "$MANIFEST" ]; then
  fail "manifest '$MANIFEST' not found — without it this gate would pass"
  fail "vacuously while every dead channel stayed published"
  exit 1
fi

TMP=$(mktemp -d)
# shellcheck disable=SC2329  # invoked indirectly by the EXIT trap
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# Rows are `| `id` | `state` | where | `pat`, `pat` |`. Only rows whose first
# cell is a backticked id are data. Emits `id<TAB>state<TAB>pattern`, one line
# per pattern. A whitespace-only pattern cell is trimmed to nothing: it once
# yielded a three-space pattern matching almost every line (56 MB of
# "violations").
awk -F'|' '
  function trim(v) { sub(/^[ \t]+/, "", v); sub(/[ \t]+$/, "", v); return v }
  /^\| *`[^`]+` *\|/ {
    id = $2; state = $3; pats = $5
    gsub(/[` ]/, "", id); gsub(/[` ]/, "", state)
    n = split(pats, parts, ",")
    emitted = 0
    for (i = 1; i <= n; i++) {
      p = trim(parts[i])
      sub(/^`/, "", p); sub(/`$/, "", p)
      p = trim(p)
      if (p != "") { print id "\t" state "\t" p; emitted++ }
    }
    # A row that yielded no pattern still has to be seen, or a disabled
    # channel could be silenced by emptying its last column.
    if (emitted == 0) print id "\t" state "\t"
  }
' "$MANIFEST" > "$TMP/rows"

if [ ! -s "$TMP/rows" ]; then
  fail "$MANIFEST declared zero channel rows — refusing to pass vacuously"
  fail "(did the state table's shape drift?)"
  exit 1
fi

violations=0

# Validate the manifest before trusting it: a typo'd state silently means "not
# disabled", the failure this gate exists to prevent, so it is an error.
while IFS=$'\t' read -r id state pat; do
  case "$state" in
    enabled) ;;
    disabled)
      if [ -z "$pat" ]; then
        fail "$MANIFEST: channel '$id' is disabled but names no forbidden string"
        violations=$((violations + 1))
      elif [ "${#pat}" -lt "$MIN_PATTERN_LEN" ]; then
        # A forbidden string identifies one channel (URL or button label),
        # never a fragment; shorter is a typo that matches half the tree.
        fail "$MANIFEST: channel '$id' forbids '$pat', under $MIN_PATTERN_LEN characters — too broad to name a channel"
        violations=$((violations + 1))
      fi
      ;;
    *)
      fail "$MANIFEST: channel '$id' has state '$state' — must be 'enabled' or 'disabled'"
      violations=$((violations + 1))
      ;;
  esac
done < "$TMP/rows"

# Stop on a malformed manifest: a bad row buries its own diagnostic under
# whatever it happens to match.
if [ "$violations" -gt 0 ]; then
  echo >&2
  echo "lint-external-channels: $MANIFEST is malformed — fix the rows above, then re-run." >&2
  exit "$violations"
fi

awk -F'\t' '$2 == "disabled" && $3 != ""' "$TMP/rows" > "$TMP/disabled"
cut -f3 "$TMP/disabled" | sort -u > "$TMP/patterns"

if [ ! -s "$TMP/patterns" ]; then
  # Every channel is on: report it rather than print a pass that looks like a scan.
  echo "lint-external-channels: no channel is switched off — nothing forbidden"
  exit "$violations"
fi

# Tracked plus untracked non-ignored files (as lint-doc-links): a tracked-only
# scan is blind to the files a change is adding.
{ git ls-files -z; git ls-files -z --others --exclude-standard; } > "$TMP/files"
if [ ! -s "$TMP/files" ]; then
  fail "git listed zero files — refusing to pass vacuously (not a work tree?)"
  exit $((violations + 1))
fi

# -I skips binaries; -H forces the filename prefix even on a one-file batch.
xargs -0 grep -InHF -f "$TMP/patterns" -- < "$TMP/files" > "$TMP/hits" 2>/dev/null

while IFS= read -r hit; do
  file=${hit%%:*}
  rest=${hit#*:}
  lineno=${rest%%:*}
  text=${rest#*:}

  # One exemption: the manifest names every forbidden string in order to
  # forbid it. Nothing else is exempt, CHANGELOG.md included.
  case "$file" in
    "$MANIFEST") continue ;;
  esac

  while IFS=$'\t' read -r id _ pat; do
    case "$text" in
      *"$pat"*)
        fail "$file:$lineno references '$pat' — channel '$id' is switched off in $MANIFEST"
        violations=$((violations + 1))
        ;;
    esac
  done < "$TMP/disabled"
done < "$TMP/hits"

echo
if [ "$violations" -gt 0 ]; then
  echo "lint-external-channels: $violations reference(s) to a switched-off channel." >&2
  echo "Either drop the reference, or switch the channel on and flip its row in" >&2
  echo "$MANIFEST to 'enabled' in the same commit." >&2
else
  echo "lint-external-channels: OK — no file promises a channel recorded as switched off"
fi
exit "$violations"
