#!/usr/bin/env bash
# check-verify-parity.sh — verify.sh ↔ CI import-checks lint parity (W5-ci-6).
#
# scripts/dev/verify.sh is the canonical pre-push gate ("run this before
# every push"). But CI's `import-checks` job (.github/workflows/ci.yml) runs
# a BUNDLE of scripts/ci/*.sh gates, and nothing forced verify.sh to mirror
# it. A gate added to import-checks but not to verify.sh is invisible drift:
# `bash scripts/dev/verify.sh` goes green while the SAME commit reddens CI on
# the new gate — the exact "verify.sh green ≠ CI green" class verify.sh's own
# comments record biting twice (2026-07-06, 2026-07-25).
#
# This gate closes that hole DETERMINISTICALLY (no network, no gh): it
# extracts every scripts/ci/*.sh invoked by the import-checks job and fails
# if any is not also invoked by verify.sh. Adding a gate to CI now obligates
# adding it to verify.sh in the same change — or the local gate stops being a
# truthful pre-push signal.
#
# Env overrides (used by the self-probe / tests): CI_YML, VERIFY_SH.
set -euo pipefail

cd "$(dirname "$0")/../.."

CI_YML="${CI_YML:-.github/workflows/ci.yml}"
VERIFY_SH="${VERIFY_SH:-scripts/dev/verify.sh}"

for f in "$CI_YML" "$VERIFY_SH"; do
  if [ ! -f "$f" ]; then
    echo "check-verify-parity: FAIL — file not found: $f" >&2
    exit 1
  fi
done

# extract_invoked <file-or-block-on-stdin> — pull the scripts/ci/*.sh paths
# that are EXECUTED (prefixed by `./` or `bash `), not merely mentioned. The
# prefix requirement deliberately excludes ci.yml's CID-03 restore step, which
# lists gate scripts in a `for f in scripts/ci/… ` loop as git-show ARGUMENTS,
# never as commands to run.
extract_invoked() {
  grep -oE '(\./|bash +)scripts/ci/[A-Za-z0-9._-]+\.sh' \
    | sed -E 's#^(\./|bash +)##' | sort -u
}

# Isolate the import-checks job block: from its 2-space-indented job key to
# the next 2-space-indented job key (step/env keys sit at ≥4 spaces, so they
# never match). Then pull the invoked gate scripts out of it.
ci_scripts="$(
  awk '/^  [A-Za-z0-9_-]+:/ { inblock = ($0 ~ /^  import-checks:/) } inblock' "$CI_YML" \
    | extract_invoked || true
)"

# NB: the `|| true` above matters — extract_invoked's grep exits non-zero when
# it matches NOTHING, and under `set -e` an unguarded `x="$(pipeline)"` would
# abort the script there, silently, before the non-vacuous guard below could
# report it. Swallow the no-match status here; the guard turns it into a fault.
if [ -z "$ci_scripts" ]; then
  echo "check-verify-parity: FAIL — no scripts/ci/*.sh invocations found in the import-checks job of ${CI_YML}." >&2
  echo "  (Did the job rename, or did the extraction pattern drift? This check must not pass vacuously.)" >&2
  exit 1
fi

verify_scripts="$(extract_invoked <"$VERIFY_SH" || true)"

missing=""
while IFS= read -r s; do
  [ -z "$s" ] && continue
  if ! grep -qxF "$s" <<<"$verify_scripts"; then
    missing="${missing}${s}"$'\n'
  fi
done <<<"$ci_scripts"

if [ -n "$missing" ]; then
  echo "check-verify-parity: FAIL — CI's import-checks job runs gate scripts that ${VERIFY_SH} does NOT:" >&2
  while IFS= read -r s; do
    [ -n "$s" ] && echo "  - $s" >&2
  done <<<"$missing"
  cat >&2 <<EOF

verify.sh is the "run before every push" gate; a gate it skips is a green it
has no basis for (W5-ci-6). Add each script above to scripts/dev/verify.sh so
the local gate mirrors CI, then re-run. If a gate genuinely cannot do useful
work locally (needs a PR BASE_SHA, network, etc.), invoke it in verify.sh
anyway behind the same guard CI uses — it self-skips, keeping parity honest.
EOF
  exit 1
fi

echo "check-verify-parity: OK — all $(printf '%s\n' "$ci_scripts" | grep -c .) import-checks gate scripts are mirrored in ${VERIFY_SH}."
