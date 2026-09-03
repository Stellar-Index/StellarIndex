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

# Every gate CI invokes, in ANY job. Reading one job is how two gates added
# to `doc-checks` passed parity in 2026-09 without being looked at.
# Gates that cannot do useful work in a local pre-push run. Each is listed
# with WHY, so an exemption is a visible decision rather than an accident of
# which CI job a gate happened to land in. Anything not here must be in
# verify.sh.
#
#   govulncheck-gated.sh  reached via `make vuln`, which verify.sh runs behind
#                         a graceful-skip; this extractor only sees direct
#                         ./scripts/ci invocations, not ones through make.
#   integration-shard.sh  runs ONE slice of the Docker matrix by shard index;
#                         verify.sh runs the compile-only integration smoke,
#                         and `make test-integration` runs the whole suite.
#   coverage-floor.sh     hard-fails without the coverage profile CI produces
#                         as an artifact. Its SELF-TEST is in verify.sh and is
#                         what actually guards the logic.
#   fuzz-smoke.sh         30s per target is ~2 min of wall clock on a gate
#                         people run before every push. Its targets are also
#                         exercised as plain seed-corpus tests by `make test`.
LOCAL_EXEMPT="govulncheck-gated.sh integration-shard.sh coverage-floor.sh fuzz-smoke.sh"

ci_scripts="$(extract_invoked <"$CI_YML" || true)"

# NB: the `|| true` above matters — extract_invoked's grep exits non-zero when
# it matches NOTHING, and under `set -e` an unguarded `x="$(pipeline)"` would
# abort the script there, silently, before the non-vacuous guard below could
# report it. Swallow the no-match status here; the guard turns it into a fault.
if [ -z "$ci_scripts" ]; then
  echo "check-verify-parity: FAIL — no scripts/ci/*.sh invocations found anywhere in ${CI_YML}." >&2
  echo "  (Did the extraction pattern drift? This check must not pass vacuously.)" >&2
  exit 1
fi

verify_scripts="$(extract_invoked <"$VERIFY_SH" || true)"

missing=""
while IFS= read -r s; do
  [ -z "$s" ] && continue
  case " $LOCAL_EXEMPT " in *" $(basename "$s") "*) continue ;; esac
  if ! grep -qxF "$s" <<<"$verify_scripts"; then
    missing="${missing}${s}"$'\n'
  fi
done <<<"$ci_scripts"

if [ -n "$missing" ]; then
  echo "check-verify-parity: FAIL — CI runs gate scripts that ${VERIFY_SH} does NOT:" >&2
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

echo "check-verify-parity: OK — all $(printf '%s\n' "$ci_scripts" | grep -c .) CI gate scripts are mirrored in ${VERIFY_SH} or explicitly exempt."
