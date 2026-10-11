#!/usr/bin/env bash
# check-fleet-release-drift.sh — test-net release-drift tripwire
# (launch-plan 1.7).
#
# One day after testnet and futurenet were caught up to r1 they were a
# release and three migrations behind again (v0.57.0 / migrations head
# 0150 against r1's v0.58.0 / 0153). Nothing could have said so:
# deploy.yml deploys ONE region per dispatch and reports that region only,
# r1's Prometheus scrapes r1 only, and the test-net VMs are NAT-only
# (192.168.122.x) behind the KVM host — no series exists that a rule
# could compare. The drift itself was routine; that it was invisible was
# the gap.
#
# This script is the decision core for two callers:
#   - .github/workflows/fleet-release-drift.yml — daily; reads each
#     host's public /v1/version and opens/updates ONE tracking issue when
#     a test net has lacked an r1 release for longer than the grace window.
#   - .github/workflows/deploy.yml — after every deploy, with grace 0, so the
#     catch-up commands appear in the run that created the drift.
#
# Reads GET /v1/version (static, readiness-independent): it proves which
# release a host RUNS, nothing more. An HTTP error, a redirect or a body
# without .data.version is UNREADABLE → DEGRADED, never "in step". Redirects
# are never followed: the bare explorer names 301 to r1 and would read as
# "in step" forever. The version is what the host SAYS — a tripwire against
# accidental drift, not a control against a lying host; its bytes are untrusted.
#
# Migrations are derived from the tags (`git ls-tree <tag> migrations/`), not
# observed; a `migrations_skip=true` deploy is invisible here. Counts print `?`
# without tags; the verdict does not depend on them.
#
# Grace is anchored per follower on the OLDEST released tag it lacks, never on
# r1's newest tag (that re-granted a window on every release). An undatable
# anchor means the lag COUNTS — fail closed. Describe labels compare equal to
# their base; any other suffix (-rc.1) sorts below its release. Components are
# capped at three digits so the packed integer key cannot collide.
#
# A pre-release REFERENCE is fail-closed: nothing released dates the lag, so
# every follower behind is DRIFT with no grace. Pre-releases of one base pack
# to one key, so under such a reference only the IDENTICAL string is in step
# (rc.2 beside rc.1 once read "in step" and closed the ticket).
#
# Host bytes reach no message unsanitised (version alphabet or one printable
# line, 120 bytes max), so a forged `verdict=` line cannot be smuggled into
# GITHUB_OUTPUT, whose parser keeps the LAST key.
#
# Inputs (optional): FLEET_REFERENCE, FLEET_REFERENCE_VERSION (skip the read;
# deploy.yml passes its tag), FLEET_FOLLOWERS, FLEET_GRACE_HOURS (default 24),
# FLEET_RELEASE_BINARIES (default from region-binaries.tsv); tests:
# FLEET_FIXTURE_DIR, FLEET_GIT_DIR, FLEET_NOW. GITHUB_OUTPUT gets verdict=,
# reference_version=, behind_count=, <name>_version=/_state=/_lag=.
#
# Exit: 0 in step / ahead / inside grace; 1 DRIFT; 2 DEGRADED (unreadable
# host or no followers) — never read as "no drift".
set -euo pipefail

REFERENCE="${FLEET_REFERENCE:-r1=https://api.stellarindex.io}"
FOLLOWERS="${FLEET_FOLLOWERS:-testnet=https://api.testnet.stellarindex.io futurenet=https://api.futurenet.stellarindex.io}"
GRACE_HOURS="${FLEET_GRACE_HOURS:-24}"
RELEASE_BINARIES="${FLEET_RELEASE_BINARIES:-}"
REGION_MANIFEST="$(dirname "$0")/../dev/region-binaries.tsv"
FIXTURE_DIR="${FLEET_FIXTURE_DIR:-}"
GIT_DIR_OPT="${FLEET_GIT_DIR:-.}"
NOW="${FLEET_NOW:-$(date -u +%s)}"
CURL_MAX_TIME=20

if ! [[ "$GRACE_HOURS" =~ ^[0-9]+$ ]]; then
  echo "fleet-release-drift: FLEET_GRACE_HOURS='$GRACE_HOURS' is not a whole number of hours" >&2
  exit 2
fi

# A released tag, or a `git describe` label on one (vX.Y.Z-N-gSHA,
# vX.Y.Z-dirty, vX.Y.Z-N-gSHA-dirty): all sit on base vX.Y.Z. Any other suffix
# (-rc.1) is a pre-release and sorts BELOW the release it precedes.
VERSION_RE='^v([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})(-[0-9A-Za-z.-]{1,64})?$'
DESCRIBE_SUFFIX_RE='^(-[0-9]+-g[0-9a-f]+(-dirty)?|-dirty)$'
RELEASE_TAG_RE='^v[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$'
SANITISED_MAX=120

# is_prerelease <version> → 0 when the suffix is neither empty nor a describe
# label (v0.60.0-rc.1); 1 for a release or a describe label.
is_prerelease() {
  [[ "$1" =~ $VERSION_RE ]] || return 1
  local suffix="${BASH_REMATCH[4]}"
  [ -n "$suffix" ] || return 1
  if [[ "$suffix" =~ $DESCRIBE_SUFFIX_RE ]]; then return 1; fi
  return 0
}

# version_key <version> → integer usable with -lt/-gt; release and
# describe-labelled builds on a base score higher than a pre-release of it.
version_key() {
  local v="$1"
  [[ "$v" =~ $VERSION_RE ]] || return 1
  # 10# on every captured component: a host serving v0.08.0 (which the pattern
  # accepts) otherwise reaches bash arithmetic as the octal literal 08 —
  # "value too great for base", which `set -e` turns into a dead run mid-loop.
  local base=$(( (10#${BASH_REMATCH[1]} * 1000 + 10#${BASH_REMATCH[2]}) * 1000 + 10#${BASH_REMATCH[3]} ))
  if is_prerelease "$v"; then
    echo $(( base * 2 ))
  else
    echo $(( base * 2 + 1 ))
  fi
}

# base_tag <version> → the vX.Y.Z the build was cut from.
base_tag() {
  [[ "$1" =~ $VERSION_RE ]] || return 1
  echo "v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}"
}

# one_line <text> → host-supplied free text (a curl error, a response body) as
# ONE line of printable ASCII, backticks dropped, runs of blanks squeezed, at
# most SANITISED_MAX bytes. Byte-wise (LC_ALL=C) so invalid UTF-8 cannot abort tr.
one_line() {
  printf '%s' "$1" \
    | LC_ALL=C tr -c '\040-\176' ' ' \
    | LC_ALL=C tr -d '`' \
    | LC_ALL=C tr -s ' ' \
    | LC_ALL=C cut -c1-"$SANITISED_MAX"
}

# version_text <text> → a served "version" that failed VERSION_RE, rendered in
# the version alphabet: every byte outside [0-9A-Za-z.-] shown as `?`, at most
# SANITISED_MAX bytes.
version_text() {
  printf '%s' "$1" \
    | LC_ALL=C tr -c 'A-Za-z0-9.-' '?' \
    | LC_ALL=C cut -c1-"$SANITISED_MAX"
}

# byte_count <text> → its length in bytes.
byte_count() {
  printf '%s' "$1" | LC_ALL=C wc -c | tr -d ' '
}

# read_version <name> <url> → the served version on stdout, or a one-line
# reason on stdout with exit 1. Retries are bounded (3 tries × 20s) so a wedged
# host costs ~70s, far inside the job timeout.
read_version() {
  local name="$1" url="$2" body="" ver=""
  if [ -n "$FIXTURE_DIR" ]; then
    if [ ! -f "$FIXTURE_DIR/$name.json" ]; then
      printf 'no response\n'; return 1
    fi
    body="$(cat "$FIXTURE_DIR/$name.json")"
  else
    if ! body="$(curl -sS --fail-with-body --max-time "$CURL_MAX_TIME" --retry 2 --retry-delay 5 \
        -A 'stellarindex-fleet-release-drift' -H 'Accept: application/json' \
        "$url/v1/version" 2>&1)"; then
      printf 'GET %s/v1/version failed: %s\n' "$url" "$(one_line "$body")"; return 1
    fi
  fi
  ver="$(printf '%s' "$body" | jq -r '.data.version // empty' 2>/dev/null || true)"
  if [ -z "$ver" ]; then
    printf 'no .data.version in the response (%s)\n' "$(one_line "${body:0:80}")"; return 1
  fi
  if ! [[ "$ver" =~ $VERSION_RE ]]; then
    printf "served version '%s' (%s bytes) is not a release tag\n" "$(version_text "$ver")" "$(byte_count "$ver")"; return 1
  fi
  printf '%s\n' "$ver"
}

# ── Released tags in the checkout: name, version key, date ───────────
# Loaded once; pre-release tags are skipped. A lightweight tag dates from its
# commit, so a late tag on an old commit makes the ticket come earlier, never
# later — the fail-closed side.
git_ok=0
if git -C "$GIT_DIR_OPT" rev-parse --git-dir >/dev/null 2>&1; then git_ok=1; fi

rel_tags=(); rel_keys=(); rel_dates=(); rel_n=0
if [ "$git_ok" = 1 ]; then
  while read -r t d1 d2 d3; do
    [[ "$t" =~ $RELEASE_TAG_RE ]] || continue
    k="$(version_key "$t")" || continue
    d=""
    for c in "$d1" "$d2" "$d3"; do
      if [[ "$c" =~ ^[0-9]+$ ]]; then d="$c"; break; fi
    done
    rel_tags+=("$t"); rel_keys+=("$k"); rel_dates+=("$d"); rel_n=$((rel_n + 1))
  done < <(git -C "$GIT_DIR_OPT" for-each-ref \
             --format='%(refname:short) %(taggerdate:unix) %(*committerdate:unix) %(committerdate:unix)' \
             'refs/tags/v[0-9]*' 2>/dev/null || true)
fi

# tag_date <vX.Y.Z> → its epoch, or empty when the tag is not in the checkout.
tag_date() {
  local i
  for ((i = 0; i < rel_n; i++)); do
    if [ "${rel_tags[$i]}" = "$1" ]; then printf '%s' "${rel_dates[$i]}"; return 0; fi
  done
  return 0
}

# migrations_head <tag> → the highest migration number at that tag, or empty.
migrations_head() {
  [ "$git_ok" = 1 ] || return 0
  git -C "$GIT_DIR_OPT" ls-tree -r --name-only "refs/tags/$1" -- migrations/ 2>/dev/null \
    | grep -oE '^migrations/[0-9]+' | sed 's#migrations/##' | sort -n | tail -n1 || true
}

# migrations_between <from-tag> <to-tag> → how many migration numbers at
# <to-tag> exceed the head at <from-tag>, or empty when either is unknown.
migrations_between() {
  local from_head to_list
  from_head="$(migrations_head "$1")"
  [ -n "$from_head" ] || return 0
  to_list="$(git -C "$GIT_DIR_OPT" ls-tree -r --name-only "refs/tags/$2" -- migrations/ 2>/dev/null \
    | grep -oE '^migrations/[0-9]+' | sed 's#migrations/##' | sort -un || true)"
  [ -n "$to_list" ] || return 0
  printf '%s\n' "$to_list" | awk -v h="$((10#$from_head))" '($1 + 0) > h { n++ } END { print n + 0 }'
}

# releases_between <from-key> <to-key> → number of released tags in (from, to]
# by version, or empty when the checkout has no tags.
releases_between() {
  [ "$rel_n" -gt 0 ] || return 0
  local i n=0
  for ((i = 0; i < rel_n; i++)); do
    if [ "${rel_keys[$i]}" -gt "$1" ] && [ "${rel_keys[$i]}" -le "$2" ]; then n=$((n + 1)); fi
  done
  echo "$n"
}

# lag_anchor <follower-key> <reference-key> → "<tag> <epoch>" of the OLDEST
# released tag the follower lacks (earliest date over the released tags in
# (follower, reference] by version), or nothing when no dated released tag.
lag_anchor() {
  local i best_tag="" best_date=""
  for ((i = 0; i < rel_n; i++)); do
    [ "${rel_keys[$i]}" -gt "$1" ] || continue
    [ "${rel_keys[$i]}" -le "$2" ] || continue
    [ -n "${rel_dates[$i]}" ] || continue
    if [ -z "$best_date" ] || [ "${rel_dates[$i]}" -lt "$best_date" ]; then
      best_tag="${rel_tags[$i]}"; best_date="${rel_dates[$i]}"
    fi
  done
  [ -n "$best_tag" ] && printf '%s %s' "$best_tag" "$best_date"
  return 0
}

# ── Reference ────────────────────────────────────────────────────────
ref_name="${REFERENCE%%=*}"
ref_url="${REFERENCE#*=}"
ref_version=""
ref_source=""
degraded=0

if [ -n "${FLEET_REFERENCE_VERSION:-}" ]; then
  if ! [[ "$FLEET_REFERENCE_VERSION" =~ $VERSION_RE ]]; then
    echo "fleet-release-drift: FLEET_REFERENCE_VERSION='$FLEET_REFERENCE_VERSION' is not a release tag" >&2
    exit 2
  fi
  ref_version="$FLEET_REFERENCE_VERSION"
  ref_source="supplied"
elif ref_version="$(read_version "$ref_name" "$ref_url")"; then
  ref_source="served"
else
  printf 'fleet-release-drift: reference %s unreadable — %s\n' "$ref_name" "$ref_version"
  degraded=1
  ref_version=""
fi

ref_base=""; ref_key=""; ref_head=""; ref_epoch=""; ref_prerelease=0; age_note=""
if [ -n "$ref_version" ]; then
  ref_base="$(base_tag "$ref_version")"
  ref_key="$(version_key "$ref_version")"
  ref_head="$(migrations_head "$ref_base")"
  if is_prerelease "$ref_version"; then
    # Fail closed (see the header): no released tag dates what the followers
    # lack, so no grace is measured for any of them.
    ref_prerelease=1
    age_note="pre-release of $ref_base, which is not a released tag; grace not applied"
  else
    ref_epoch="$(tag_date "$ref_base")"
    if [ -n "$ref_epoch" ]; then
      age_note="tagged $(( (NOW - ref_epoch) / 3600 ))h ago"
    else
      age_note="release age unknown — tag $ref_base not in this checkout; grace not applied"
    fi
  fi
  echo "fleet-release-drift: reference $ref_name $ref_version ($ref_source; migrations head ${ref_head:-?}; $age_note)"
fi

# ── Followers ────────────────────────────────────────────────────────
instep_names=()      # followers on the reference's base
ahead_notes=()       # "<name> <version>" per follower ahead
behind_names=()      # every follower behind, in grace or not
behind_count=0
drift_names=()       # behind past the grace
drift_count=0
drift_notes=""
grace_note=""        # the longest in-grace lag, for the verdict line
grace_lag=-1
follower_n=0         # followers the list named, readable or not
outputs=()
[ -n "$ref_version" ] && outputs+=("reference_version=$ref_version")

for pair in $FOLLOWERS; do
  follower_n=$((follower_n + 1))
  name="${pair%%=*}"
  url="${pair#*=}"
  if ! ver="$(read_version "$name" "$url")"; then
    printf 'fleet-release-drift: %s unreadable — %s\n' "$name" "$ver"
    outputs+=("${name}_state=unreadable")
    degraded=1
    continue
  fi
  outputs+=("${name}_version=$ver")
  if [ -z "$ref_version" ]; then
    # Nothing to compare against; the state is unknown, not in step.
    printf 'fleet-release-drift: %-9s %s  (reference unreadable — not compared)\n' "$name" "$ver"
    outputs+=("${name}_state=uncompared")
    continue
  fi
  key="$(version_key "$ver")"
  # Relation to the reference. Against a RELEASE reference the version keys
  # order the fleet.
  if [ "$ref_prerelease" = 1 ]; then
    if [ "$ver" = "$ref_version" ]; then relation=in-step; else relation=behind; fi
  elif [ "$key" -eq "$ref_key" ]; then
    relation=in-step
  elif [ "$key" -gt "$ref_key" ]; then
    relation=ahead
  else
    relation=behind
  fi
  if [ "$relation" = in-step ]; then
    printf 'fleet-release-drift: %-9s %s  in step\n' "$name" "$ver"
    outputs+=("${name}_state=in-step")
    instep_names+=("$name")
  elif [ "$relation" = ahead ]; then
    printf 'fleet-release-drift: %-9s %s  ahead of %s\n' "$name" "$ver" "$ref_name"
    outputs+=("${name}_state=ahead")
    ahead_notes+=("$name $ver")
  else
    base="$(base_tag "$ver")"
    rel="$(releases_between "$key" "$ref_key")"
    mig="$(migrations_between "$base" "$ref_base")"
    head="$(migrations_head "$base")"
    # The grace is measured from the OLDEST release this follower lacks. No
    # dated anchor → the grace cannot be applied → the lag counts.
    lag_note=""; state="behind"
    if [ "$ref_prerelease" = 1 ]; then
      # Fail closed: nothing released dates the lag (header).
      rel=""
      if [ "$key" -gt "$ref_key" ]; then
        lag_note="not on $ref_version — grace not applied ($ref_name serves a pre-release; only the identical version is in step)"
      else
        lag_note="lacking $ref_base for ?h — grace not applied ($ref_name serves a pre-release of it, not a released tag)"
      fi
    elif [ -z "$ref_epoch" ]; then
      lag_note="lacking $ref_base for ?h — grace not applied (tag not in this checkout)"
    else
      anchor="$(lag_anchor "$key" "$ref_key")"
      if [ -z "$anchor" ]; then
        # Unreachable by construction — a dated release reference is itself
        # inside (follower, reference] — but never apply a grace that has no
        # date behind it.
        lag_note="lacking $ref_base for ?h — grace not applied (no dated released tag between $ver and $ref_version)"
      else
        anchor_tag="${anchor%% *}"; anchor_epoch="${anchor#* }"
        lag_h=$(( (NOW - anchor_epoch) / 3600 ))
        lag_note="lacking $anchor_tag for ${lag_h}h"
        if [ "$lag_h" -lt "$GRACE_HOURS" ]; then
          state="behind-in-grace"
          if [ "$lag_h" -gt "$grace_lag" ]; then grace_lag="$lag_h"; grace_note="$name $lag_note"; fi
        fi
      fi
    fi
    printf 'fleet-release-drift: %-9s %s  BEHIND by %s release(s), %s migration(s) (head %s, %s %s); %s\n' \
      "$name" "$ver" "${rel:-?}" "${mig:-?}" "${head:-?}" "$ref_name" "${ref_head:-?}" "$lag_note"
    outputs+=("${name}_state=$state" "${name}_lag=$lag_note")
    behind_names+=("$name")
    behind_count=$((behind_count + 1))
    if [ "$state" = behind ]; then
      drift_names+=("$name")
      drift_count=$((drift_count + 1))
      drift_notes="${drift_notes:+$drift_notes; }$name $lag_note"
    fi
  fi
done

# ── Verdict ──────────────────────────────────────────────────────────
verdict=""
rc=0

# Past the grace, the catch-up list names EVERY follower behind: the one still
# inside its window is behind too, and the operator dispatching anyway saves a
# run.
if [ "$drift_count" -gt 0 ]; then
  echo "fleet-release-drift: catch up with:"
  for n in "${behind_names[@]}"; do
    bins="$RELEASE_BINARIES"
    [ -n "$bins" ] || bins="$(awk -F'\t' -v r="$n" '$1 == r { print $5 }' "$REGION_MANIFEST" 2>/dev/null)"
    if [ -n "$bins" ]; then
      echo "  gh workflow run deploy.yml -f region=$n -f version=$ref_version -f binaries=$bins"
    else
      echo "  $n: no row in scripts/dev/region-binaries.tsv; add one, then scripts/dev/preflight-deploy.sh --region $n --version $ref_version --refresh-manifest"
    fi
  done
fi

if [ "$degraded" = 1 ]; then
  verdict="degraded"; rc=2
  echo "fleet-release-drift: DEGRADED — could not read every host; the comparison is incomplete and does not mean the fleet is in step."
elif [ "$follower_n" -eq 0 ]; then
  # Nothing was compared, so nothing may be called in step: an empty follower
  # list would otherwise fall through to the in-step arm below, exit 0, and
  # let the workflow close the tracking issue on a fleet no one looked at.
  verdict="no-followers"; rc=2
  echo "fleet-release-drift: NO FOLLOWERS — the follower list named no host, so nothing was compared; an empty fleet is not an in-step fleet."
elif [ "$drift_count" -gt 0 ]; then
  verdict="drift"; rc=1
  echo "fleet-release-drift: DRIFT — $drift_count host(s) behind $ref_name $ref_version for longer than the ${GRACE_HOURS}h grace ($drift_notes)."
elif [ "$behind_count" -gt 0 ]; then
  verdict="grace"; rc=0
  echo "fleet-release-drift: $behind_count host(s) behind $ref_name $ref_version, within the ${GRACE_HOURS}h grace ($grace_note) — a deploy in progress, not drift yet."
else
  # Nothing behind: every follower is on the reference's base or ahead of it.
  # A follower ahead is said to be ahead — not "in step".
  verdict="in-step"; rc=0
  summary=""
  if [ "${#instep_names[@]}" -gt 0 ]; then
    names="$(printf '%s, ' "${instep_names[@]}")"
    summary="${names%, } in step with $ref_name $ref_version"
  fi
  if [ "${#ahead_notes[@]}" -gt 0 ]; then
    names="$(printf '%s, ' "${ahead_notes[@]}")"
    summary="${summary:+$summary; }${names%, } ahead of the reference"
  fi
  echo "fleet-release-drift: OK — ${summary:-no follower to compare}."
fi

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "verdict=$verdict"
    echo "behind_count=$behind_count"
    # Guarded on the COUNT, which is safe on an empty array everywhere.
    if [ "${#outputs[@]}" -gt 0 ]; then
      printf '%s\n' "${outputs[@]}"
    fi
  } >> "$GITHUB_OUTPUT"
fi

exit "$rc"
