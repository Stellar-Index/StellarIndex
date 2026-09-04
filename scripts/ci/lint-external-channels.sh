#!/usr/bin/env bash
# lint-external-channels.sh — a shipped file may not point at a channel
# that is switched off.
#
# GitHub repository settings and DNS names live outside the repo, so no
# other gate can see them, and three of them shipped as promises while
# switched off: SUPPORT.md linked GitHub Discussions on a repo with
# Discussions disabled, SECURITY.md offered the Security-tab reporting
# button on a repo with private vulnerability reporting disabled — so
# BOTH documented disclosure routes were dead at once, leaving a
# researcher with a real finding nowhere to go — and the OpenAPI spec
# carried a Staging server for a hostname with no DNS record, which
# every client importing the spec inherited.
#
# docs/operations/external-channels.md carries each channel's state and
# the literal strings that must not appear while it is `disabled`. That
# keeps the switch and the wording in one file: turning a channel on is
# a one-line flip, after which the same commit may restore the text.
#
# Exit code is the number of violations, so cron and the other gates
# consume it the same way lint-doc-links.sh is consumed.
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

# Rows are `| `id` | `state` | where | `pat`, `pat` |`. Only rows whose
# first cell is a backticked id are data; the header and its separator
# are not. Emits `id<TAB>state<TAB>pattern`, one line per pattern.
#
# Patterns are trimmed to nothing when the cell holds only whitespace.
# They were not, once: a blank-looking cell yielded a pattern of three
# spaces, which matches almost every line of every file in the tree —
# 56 MB of "violations" and no usable signal.
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

# Validate the manifest before trusting it. A typo'd state silently
# means "not disabled", which is the failure mode this gate exists to
# prevent, so it is an error rather than a skip.
while IFS=$'\t' read -r id state pat; do
  case "$state" in
    enabled) ;;
    disabled)
      if [ -z "$pat" ]; then
        fail "$MANIFEST: channel '$id' is disabled but names no forbidden string"
        violations=$((violations + 1))
      elif [ "${#pat}" -lt "$MIN_PATTERN_LEN" ]; then
        # A forbidden string identifies one channel — a URL or a button
        # label, never a fragment. Anything shorter is a typo, and a
        # typo here matches half the tree instead of failing.
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

# Stop here on a malformed manifest rather than scanning with it. The
# scan below is only as meaningful as the rows driving it, and a bad row
# buries its own diagnostic under whatever it happens to match.
if [ "$violations" -gt 0 ]; then
  echo >&2
  echo "lint-external-channels: $MANIFEST is malformed — fix the rows above, then re-run." >&2
  exit "$violations"
fi

awk -F'\t' '$2 == "disabled" && $3 != ""' "$TMP/rows" > "$TMP/disabled"
cut -f3 "$TMP/disabled" | sort -u > "$TMP/patterns"

if [ ! -s "$TMP/patterns" ]; then
  # Every channel is on. Nothing to forbid — report it rather than
  # printing a pass that looks like a scan.
  echo "lint-external-channels: no channel is switched off — nothing forbidden"
  exit "$violations"
fi

# Tracked files PLUS untracked, non-ignored ones — the same file set
# lint-doc-links uses, and for the same reason: a tracked-only scan is
# blind to exactly the files a change is adding.
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

  # One exemption, and it is definitional: the manifest names every
  # forbidden string in order to forbid it. Nothing else is exempt —
  # including CHANGELOG.md, which today quotes neither string and can
  # record a channel's history without pasting a dead URL into it.
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
