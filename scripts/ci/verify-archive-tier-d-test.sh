#!/usr/bin/env bash
# verify-archive-tier-d-test.sh — pins the archival-node role's Tier D
# fork-detection cron to arguments `stellarindex-ops verify-archive` can
# actually parse (issue #362, verified live on r1 2026-09-02).
#
# Why this exists. The task rendered `-to {{ stellarindex_live_seam_ledger
# | default(0) | int - 1 }}` unconditionally. `stellarindex_live_seam_ledger`
# defaults to 0 (roles/archival-node/defaults/main.yml), so every host
# that never set a seam — including r1 — got `-to -1` in
# /etc/cron.d/stellarindex-verify-archive-tier-d. `-to` is an fs.Uint
# (internal/ops/archive/verify_archive.go), so the flag parser rejects it
# with `invalid value "-1" for flag -to: parse error` before any work
# happens. The cron fired weekly for months and
# `journalctl -t stellarindex-tier-d --since -60d` was EMPTY: a
# consensus-level fork-detection control (ADR-0016 §7.4) that had never
# run once, and nothing in CI or on the host said so.
#
# What it checks, against the REAL task file, rendered by the REAL Jinja:
#   1. every numeric flag value the cron renders parses as an unsigned
#      integer — the exact rule Go's fs.Uint applies (this is the check
#      that would have caught `-to -1`);
#   2. `-to` is OMITTED when there is no seam (0 = "resolve the upper
#      bound from the peers' own tips", verify_archive.go), and is
#      `seam - 1` when there is one;
#   3. `-from` is never below 2 (ledger 1 has no predecessor);
#   4. the flags the cron passes still exist in verify_archive.go, and
#      `-to` is still an fs.Uint — if either changes, premise (2) needs
#      re-deriving rather than silently drifting;
#   5. (#1232) the walk runs under run-heavy-job.sh — lock, MemoryMax scope,
#      disk watchdog — with a lock name of its own (sharing tier A/B's
#      would let a long tier-A run swallow the weekly check), and starts
#      outside the nightly band of daily heavy-job timers (first start to
#      last start + 2h), derived from the role's templates.
#
# ROLE_TASKS / OPS_ARCHIVE_SRC point the gate at a fixture copy (used for
# the red-proof against the pre-fix task file).
# Run: bash scripts/ci/verify-archive-tier-d-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE_TASKS="${ROLE_TASKS:-$PWD/configs/ansible/roles/archival-node/tasks}"
OPS_ARCHIVE_SRC="${OPS_ARCHIVE_SRC:-$PWD/internal/ops/archive/verify_archive.go}"
TASK_FILE="$ROLE_TASKS/14-stellarindex-services.yml"
ROLE_SYSTEMD="${ROLE_SYSTEMD:-$PWD/configs/ansible/roles/archival-node/templates/systemd}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

# ── interpreter: same discovery order as lint-jinja-templates.sh ──────────
PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then
    PY="$cand"; break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  # ansible ships its own interpreter (shebang of the console script)
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then PY="$cand"; fi
fi
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "verify-archive-tier-d-test: FAIL — no python3 with jinja2+yaml (required in CI)" >&2
    exit 1
  fi
  echo "verify-archive-tier-d-test: SKIP (no python3 with jinja2+yaml locally; CI enforces)"
  exit 0
fi

# ── 1-3. render the real task under real Jinja and inspect the flags ──────
render_ok=0
"$PY" - "$TASK_FILE" <<'PY_RENDER_EOF' || render_ok=1
import re
import sys

import jinja2
import yaml

TASK_NAME = "Install Tier D verify-archive weekly cron"

with open(sys.argv[1], encoding="utf-8") as fh:
    tasks = yaml.safe_load(fh)

job = None
for task in tasks or []:
    if isinstance(task, dict) and task.get("name") == TASK_NAME:
        job = (task.get("ansible.builtin.cron") or {}).get("job")
if not job:
    print(f"  FAIL — task {TASK_NAME!r} (or its `job:`) not found; this gate must not pass vacuously")
    sys.exit(1)

# The four host shapes the role actually has to serve. r1 sets a hot floor
# and no seam (the shape that rendered `-to -1`); the seam'd shape is R2/R3
# once they are provisioned; the bare shape is a greenfield / test net.
CASES = [
    ("greenfield (no vars set)", {}, None),
    ("r1 (hot floor, no seam)", {"stellarindex_archive_hot_floor": 49984000}, None),
    ("explicit seam 0", {"stellarindex_live_seam_ledger": 0}, None),
    ("seam'd region",
     {"stellarindex_archive_hot_floor": 49984000, "stellarindex_live_seam_ledger": 63050000},
     63049999),
]

env = jinja2.Environment()
template = env.from_string(job)
failures = 0

for label, ctx, want_to in CASES:
    try:
        rendered = " ".join(template.render(**ctx).split())
    except jinja2.UndefinedError as exc:  # a var the role does not default
        print(f"  FAIL — {label}: template raised UndefinedError: {exc}")
        failures += 1
        continue

    # (1) every flag value Go will hand to strconv.ParseUint must be one.
    #     This is the check that fires on `-to -1`.
    for flag in ("-from", "-to", "-peer-samples"):
        for value in re.findall(rf"(?<!\S){re.escape(flag)}\s+(\S+)", rendered):
            if not re.fullmatch(r"[0-9]+", value):
                print(f"  FAIL — {label}: {flag} renders {value!r}, which "
                      f"`stellarindex-ops verify-archive` cannot parse")
                failures += 1
            else:
                print(f"  ok   — {label}: {flag} {value} parses as an unsigned integer")

    # (2) -to present iff there is a seam, and equal to seam-1.
    got_to = re.search(r"(?<!\S)-to\s+(\S+)", rendered)
    got_to = got_to.group(1) if got_to else None
    if want_to is None:
        if got_to is None:
            print(f"  ok   — {label}: -to omitted (0 = resolve the bound from the peers' tips)")
        else:
            print(f"  FAIL — {label}: -to rendered as {got_to!r}; with no seam it must be OMITTED")
            failures += 1
    elif got_to == str(want_to):
        print(f"  ok   — {label}: -to {got_to} (seam - 1)")
    else:
        print(f"  FAIL — {label}: -to is {got_to!r}, expected {want_to}")
        failures += 1

    # (3) -from is never below 2.
    got_from = re.search(r"(?<!\S)-from\s+(\S+)", rendered)
    if not got_from:
        print(f"  FAIL — {label}: no -from rendered")
        failures += 1
    elif not re.fullmatch(r"[0-9]+", got_from.group(1)) or int(got_from.group(1)) < 2:
        print(f"  FAIL — {label}: -from is {got_from.group(1)!r}; ledger 1 has no predecessor")
        failures += 1
    else:
        print(f"  ok   — {label}: -from {got_from.group(1)} >= 2")

sys.exit(1 if failures else 0)
PY_RENDER_EOF
if [ "$render_ok" -eq 0 ]; then
  ok "Tier D cron renders parseable flags for every host shape"
else
  bad "Tier D cron renders a flag verify-archive cannot parse (see above)"
fi

# ── 4. the premises this gate rests on still hold in the Go source ────────
if [ ! -r "$OPS_ARCHIVE_SRC" ]; then
  bad "verify_archive.go not readable at $OPS_ARCHIVE_SRC (gate cannot pass vacuously)"
else
  if grep -q 'to := fs.Uint("to"' "$OPS_ARCHIVE_SRC"; then
    ok "-to is still fs.Uint (negative values are unrepresentable, 0 = unbounded)"
  else
    bad "-to is no longer fs.Uint in verify_archive.go — re-derive the cron's -to semantics"
  fi
  for flag in from to tier peer-samples; do
    if grep -q "fs\.\(Uint\|Int\|String\)(\"$flag\"" "$OPS_ARCHIVE_SRC"; then
      ok "verify-archive still defines -$flag"
    else
      bad "verify-archive no longer defines -$flag, but the Tier D cron passes it"
    fi
  done
  # -to 0 must still resolve against the peers, or omitting it is wrong.
  if grep -q 'peerArchiveTip' "$OPS_ARCHIVE_SRC"; then
    ok "tier peers still resolves an unbounded -to from the peer archive tips"
  else
    bad "peerArchiveTip is gone — an omitted -to may no longer mean 'up to the peers' tips'"
  fi
fi

# ── 5. host protection: heavy-job wrapper, own lock, off the nightly band ─
heavy_ok=0
"$PY" - "$TASK_FILE" "$ROLE_SYSTEMD" <<'PY_HEAVY_EOF' || heavy_ok=1
import glob
import os
import re
import sys

import yaml

TASK_NAME = "Install Tier D verify-archive weekly cron"
WRAPPER = "/usr/local/sbin/run-heavy-job.sh"
MARGIN_MIN = 120

cron = None
for task in yaml.safe_load(open(sys.argv[1], encoding="utf-8")) or []:
    if isinstance(task, dict) and task.get("name") == TASK_NAME:
        cron = task.get("ansible.builtin.cron") or {}
if not cron or not cron.get("job"):
    print(f"  FAIL — task {TASK_NAME!r} not found; this gate must not pass vacuously")
    sys.exit(1)
failures = 0
job = " ".join(cron["job"].split())

# Lock names the tier A/B units take, and the daily heavy-job start times.
sibling_locks, starts = set(), []
for svc in glob.glob(os.path.join(sys.argv[2], "*.service.j2")):
    m = re.search(r"^ExecStart=" + re.escape(WRAPPER) + r"\s+(\S+)",
                  open(svc, encoding="utf-8").read(), re.M)
    if not m:
        continue
    if os.path.basename(svc).startswith("verify-archive-tier-"):
        sibling_locks.add(m.group(1))
    timer = svc.replace(".service.j2", ".timer.j2")
    if os.path.exists(timer):
        for hh, mm in re.findall(r"^OnCalendar=\*-\*-\* (\d\d):(\d\d)",
                                 open(timer, encoding="utf-8").read(), re.M):
            starts.append(int(hh) * 60 + int(mm))
if not sibling_locks or not starts:
    print("  FAIL — no tier A/B heavy-job lock or daily heavy timer found; gate would be vacuous")
    sys.exit(1)

m = re.search(re.escape(WRAPPER) + r"\s+(\S+)\s+/usr/local/bin/stellarindex-ops verify-archive", job)
if not m:
    print("  FAIL — Tier D runs stellarindex-ops verify-archive without run-heavy-job.sh: "
          "no singleton lock, no MemoryMax scope, no disk watchdog")
    failures += 1
elif m.group(1) in sibling_locks:
    print(f"  FAIL — Tier D takes lock {m.group(1)!r}, shared with tier A/B; a long tier-A "
          "run would skip the weekly fork check with exit 0")
    failures += 1
else:
    print(f"  ok   — Tier D runs under run-heavy-job.sh with its own lock {m.group(1)!r}")

try:
    at = int(str(cron.get("hour"))) * 60 + int(str(cron.get("minute")))
except ValueError:
    print(f"  FAIL — cron hour/minute {cron.get('hour')!r}:{cron.get('minute')!r} is not a single time")
    sys.exit(1)
lo, hi = min(starts), max(starts) + MARGIN_MIN


def hhmm(t):
    return f"{t // 60:02d}:{t % 60:02d}"


verdict = "inside" if lo <= at <= hi else "outside"
print(f"  {'FAIL' if verdict == 'inside' else 'ok  '} — Tier D starts {hhmm(at)}, "
      f"{verdict} the nightly heavy band {hhmm(lo)}-{hhmm(hi)}")
failures += verdict == "inside"
sys.exit(1 if failures else 0)
PY_HEAVY_EOF
if [ "$heavy_ok" -eq 0 ]; then
  ok "Tier D is host-protected by run-heavy-job.sh and scheduled off the heavy band"
else
  bad "Tier D bypasses run-heavy-job.sh, shares tier A/B's lock, or starts in the heavy band"
fi

# ── 6. testnet/futurenet get no peer region to cross-check — cron must be absent ─
network_ok=0
"$PY" - "$TASK_FILE" <<'PY_NETWORK_EOF' || network_ok=1
import sys

import jinja2
import yaml

TASK_NAME = "Install Tier D verify-archive weekly cron"

with open(sys.argv[1], encoding="utf-8") as fh:
    tasks = yaml.safe_load(fh)

cron = None
for task in tasks or []:
    if isinstance(task, dict) and task.get("name") == TASK_NAME:
        cron = task.get("ansible.builtin.cron") or {}
if not cron or "state" not in cron:
    print(f"  FAIL — task {TASK_NAME!r} has no `state:` — nothing gates it off a network "
          "with no peer region (testnet/futurenet)")
    sys.exit(1)

env = jinja2.Environment()
env.filters["bool"] = bool
template = env.from_string(cron["state"])
failures = 0

# verify_archive_tier_d_enabled (defaults/main.yml) mirrors verify_archive_tier_d_enabled's
# own network derivation: pubnet-only.
CASES = [
    ("pubnet (r1/r2/r3)", True, "present"),
    ("testnet", False, "absent"),
    ("futurenet", False, "absent"),
]
for label, enabled, want in CASES:
    got = template.render(verify_archive_tier_d_enabled=enabled)
    if got == want:
        print(f"  ok   — {label}: Tier D cron state={got}")
    else:
        print(f"  FAIL — {label}: Tier D cron state={got!r}, expected {want!r}")
        failures += 1

sys.exit(1 if failures else 0)
PY_NETWORK_EOF
if [ "$network_ok" -eq 0 ]; then
  ok "Tier D cron is absent on networks with no peer region to cross-check"
else
  bad "Tier D cron installs on a network with no peer region (testnet/futurenet)"
fi

echo
echo "verify-archive-tier-d-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
