#!/usr/bin/env bash
# Self-test for check-alertmanager-parity.sh.
#
# A parity gate is worth exactly the divergences it can still see. This
# one is green on the repo as it stands, and a green gate that cannot go
# red is indistinguishable from no gate at all — the vacuous-guard shape
# that let the 31-day alerting outage pass a fully-passing check. So each
# case below MUTATES a fixture copy of the Ansible template and requires
# the gate to fail, naming the thing that moved.
#
# Case 1 reproduces the real 2026-09-08 regression (#501): informational
# routed to `chat-informational` in one file and to `silent` — a receiver
# that delivers to nobody — in the other.
#
# Case 3 is the one worth keeping honest. It diverges ONLY on the dark
# branch, so a gate that compared just the all-URLs-set rendering would
# pass it, and the difference would surface on the day someone left a
# webhook unset — precisely when nothing is left to report it.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="scripts/ci/check-alertmanager-parity.sh"
J2_REL="configs/ansible/roles/prometheus/templates/alertmanager.yml.j2"

FAILED=0
ok() { echo "  ok — $1"; }
bad() {
  echo "  FAIL — $1" >&2
  FAILED=1
}

# Substring tests use `case` rather than a pipe into `grep -q`: under
# pipefail an early-exit consumer SIGPIPEs the producer and the pipeline
# status stops meaning what it looks like it means
# (scripts/ci/lint-shell-sigpipe.sh).
contains() { # <haystack> <needle>
  case "$1" in
    *"$2"*) return 0 ;;
    *) return 1 ;;
  esac
}

# The gate skips (0) when no python3 with jinja2 + PyYAML exists. Detect
# that here rather than reading every red case as a pass.
PROBE=$(bash "$GATE" 2>&1)
if contains "$PROBE" 'SKIP'; then
  echo "check-alertmanager-parity-test: SKIP (gate skipped: no python3 with jinja2 + PyYAML)"
  exit 0
fi

# ── fixture: a tree holding only what the gate reads ───────────────
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

new_fixture() { # <dir>
  local d="$1"
  rm -rf "$d"
  mkdir -p "$d/configs/alertmanager" "$d/$(dirname "$J2_REL")"
  cp configs/alertmanager/apply.sh configs/alertmanager/alertmanager.r1.yml \
    "$d/configs/alertmanager/"
  cp "$J2_REL" "$d/$J2_REL"
}

expect_red() { # <label> <dir> <substring the message must contain>
  local out rc
  out=$(AM_PARITY_ROOT="$2" bash "$GATE" 2>&1)
  rc=$?
  if [ "$rc" -eq 0 ]; then
    bad "$1: gate PASSED a divergence it must catch"
    return
  fi
  if ! contains "$out" "$3"; then
    bad "$1: gate failed, but the message never mentions '$3'"
    printf '%s\n' "$out" | sed 's/^/      /' >&2
    return
  fi
  ok "$1"
}

# ── 0. the repo as it stands must be GREEN ─────────────────────────
if bash "$GATE" >/dev/null 2>&1; then
  ok "the committed pair renders identically on both branches"
else
  bad "the committed pair does NOT render identically — the gate is red on main"
  bash "$GATE" 2>&1 | sed 's/^/      /' >&2
fi

# ── 1. the real regression: informational routed somewhere else ────
F="$TMPD/route"
new_fixture "$F"
perl -0pi -e 's/(- matchers: \[severity = "informational"\]\n      receiver: )chat-informational/${1}silent/' \
  "$F/$J2_REL"
if grep -q 'receiver: silent' "$F/$J2_REL"; then
  # The receiver LIST is unchanged — chat-informational still exists in
  # both, it is simply unreachable on one path. That is the nastiest
  # shape of this bug and the reason the whole `route` tree is compared
  # rather than only the receivers.
  expect_red "a diverged informational route is caught" "$F" \
    "differs between the two apply paths"
else
  bad "fixture 1 did not mutate — the route text moved, update this test"
fi

# ── 2. a Discord payload bound that drifted on one path only ───────
# The header of both files calls the Go templates byte-identical. This
# is what makes that claim enforceable: a description cap of 3000 on one
# path renders an embed the other cannot, which is the 2026-09-07 outage.
F="$TMPD/bound"
new_fixture "$F"
perl -0pi -e 's/%\.300s/%.3000s/' "$F/$J2_REL"
if grep -q '%.3000s' "$F/$J2_REL"; then
  expect_red "a diverged payload bound is caught" "$F" "differs"
else
  bad "fixture 2 did not mutate — the printf bound moved, update this test"
fi

# ── 3. a divergence visible ONLY in the dark (all-URLs-empty) branch ─
# Drop the `{% if %}` guard around the informational webhook so the
# receiver renders its discord_configs block unconditionally. With every
# URL set both paths still agree; with none set the template emits a
# block the r1 renderer strips. A wired-only gate would pass this.
F="$TMPD/dark"
new_fixture "$F"
perl -0pi -e 's/\{% if alertmanager_discord_webhook_url_informational %\}\n//' "$F/$J2_REL"
perl -0pi -e 's/\{% endraw %\}\n\{% else %\}\n(    #[^\n]*\n)+\{% endif %\}\n\n  # ─── silent/{% endraw %}\n\n  # ─── silent/' \
  "$F/$J2_REL"
if grep -q 'alertmanager_discord_webhook_url_informational %}' "$F/$J2_REL"; then
  bad "fixture 3 did not mutate — the informational {% if %} moved, update this test"
else
  expect_red "a dark-branch-only divergence is caught" "$F" "[dark]"
  # And prove it really is dark-only: if the wired branch also diverged,
  # this case would pass for the wrong reason and the dark rendering
  # would still be uncovered.
  dark_out=$(AM_PARITY_ROOT="$F" bash "$GATE" 2>&1)
  if contains "$dark_out" "[wired]"; then
    bad "case 3 also diverges when wired — it no longer proves the dark branch is checked"
  else
    ok "case 3 diverges on the dark branch ONLY (a wired-only gate would miss it)"
  fi
fi

# ── 4. the gate refuses to run against a tree missing a file ───────
F="$TMPD/missing"
new_fixture "$F"
rm -f "$F/$J2_REL"
if out=$(AM_PARITY_ROOT="$F" bash "$GATE" 2>&1); then
  bad "a missing template did not fail the gate"
elif contains "$out" "missing"; then
  ok "a missing input is a failure, not a silent pass"
else
  bad "a missing template failed the gate, but the message does not name it"
fi

if [ "$FAILED" -ne 0 ]; then
  echo "check-alertmanager-parity-test: FAILED" >&2
  exit 1
fi
echo "check-alertmanager-parity-test: all cases passed"
