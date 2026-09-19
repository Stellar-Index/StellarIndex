#!/usr/bin/env bash
# ansible-clickhouse-host-gate-test.sh — the archival-node role must not
# hard-fail on a host that has no ClickHouse (F128, audit-2026-09-02).
#
# WHAT WENT WRONG. Three CH-CONFIG task files — 20-clickhouse-serving-
# profile.yml, 21-clickhouse-drop-guard.yml, 22-clickhouse-exporter.yml —
# were imported by tasks/main.yml with NO `when:`, and all three of their
# own enable flags default TRUE. They are not installers: they write XML
# into /etc/clickhouse-server/{config.d,users.d} and shell out to
# clickhouse-client. So on a host without ClickHouse (the DR bring-up, a
# fresh r2/r3, a no-lake box) the FIRST of them aborted the whole role on
# its vault-password assert, taking every later task with it.
#
# WHY `run_clickhouse` IS THE WRONG GATE, and the trap this test exists to
# spring: run_clickhouse is FALSE on r1 by design (08-clickhouse.yml must
# never rebuild r1's hand-tended lake), and r1 is precisely the host those
# three files were written for. Gating them on run_clickhouse would be
# green here, green in CI, and would silently remove the destructive-DDL
# drop guard, the serving profile and the metrics endpoint from the one
# production lake. Arm B below fails on exactly that "fix".
#
# HOW IT TESTS. A real `ansible-playbook -c local` run of the role's OWN
# tasks/main.yml (not a transcription of it), tag-selected down to the
# three CH-config files, against three host shapes:
#
#   A. no ClickHouse, run_clickhouse false  → play SUCCEEDS, all three
#      files skipped (this is the arm that is RED before the fix);
#   B. ClickHouse config dir PRESENT, run_clickhouse false (the r1 shape)
#      → the tasks RUN, proving r1 is still reached; with no vault
#      password the serving-profile assert fails, which is both the
#      designed fail-closed behaviour and the evidence the gate opened;
#   C. no config dir but run_clickhouse true (a fresh host installing
#      ClickHouse on this same run) → the tasks RUN, same evidence.
#
# Nothing is written outside a temp dir: arm B/C abort on the assert,
# which is the first task in the first file.
#
# ROLE_TASKS overrides the tasks directory so the pre-fix tree can be
# replayed (the red proof); ROLE_DEFAULTS does the same for defaults.
#
# Needs ansible-playbook (the ci.yml ansible-check job installs it).
# Run: bash scripts/ci/ansible-clickhouse-host-gate-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 2

ROLE="$PWD/configs/ansible/roles/archival-node"
ROLE_TASKS="${ROLE_TASKS:-$ROLE/tasks}"
ROLE_DEFAULTS="${ROLE_DEFAULTS:-$ROLE/defaults/main.yml}"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

for f in "$ROLE_TASKS/main.yml" "$ROLE_TASKS/20-clickhouse-serving-profile.yml" \
         "$ROLE_TASKS/21-clickhouse-drop-guard.yml" \
         "$ROLE_TASKS/22-clickhouse-exporter.yml" "$ROLE_DEFAULTS"; do
  [ -r "$f" ] || { echo "ansible-clickhouse-host-gate-test: missing $f" >&2; exit 2; }
done

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-clickhouse-host-gate-test: FAIL — ansible-playbook not on PATH" \
       "(this test must not pass vacuously)" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ─── structural: the default probe path is the package's own dir ──────
echo "ansible-clickhouse-host-gate-test: role wiring"

if grep -q '^clickhouse_server_config_dir: *"/etc/clickhouse-server"' "$ROLE_DEFAULTS"; then
  ok "clickhouse_server_config_dir defaults to /etc/clickhouse-server"
else
  bad "clickhouse_server_config_dir does not default to /etc/clickhouse-server"
fi

# Every CH-CONFIG import must carry a `when:`. The behavioural arms below
# prove the condition is the RIGHT one; this catches a fourth such file
# being added later with no gate at all.
for f in 20-clickhouse-serving-profile.yml 21-clickhouse-drop-guard.yml \
         22-clickhouse-exporter.yml; do
  # The two lines after the import line: a `when:` must be one of them.
  ctx="$(grep -A2 "import_tasks: $f" "$ROLE_TASKS/main.yml")"
  if grep -q 'when:' <<<"$ctx"; then
    ok "main.yml gates the import of $f"
  else
    bad "main.yml imports $f with no when: — a host without ClickHouse hard-fails"
  fi
done

# ─── behavioural: run the role's own main.yml, three host shapes ──────
cat > "$TMP/inventory" <<'INV'
localhost ansible_connection=local
INV

cat > "$TMP/play.yml" <<PLAY
---
- name: archival-node ClickHouse-config gate
  hosts: localhost
  gather_facts: false
  vars_files:
    - $ROLE_DEFAULTS
  tasks:
    - name: the role's real orchestration file
      ansible.builtin.import_tasks: $ROLE_TASKS/main.yml
PLAY

# task_status <logfile> <task-name-substring> — the status ansible printed
# for that task ("ok" / "skipping" / "fatal"), or "" if the task never ran.
# The status is on the line AFTER the `TASK [...]` banner, possibly behind
# an interpreter-discovery WARNING, so match the status verbs explicitly.
# Reads the whole file (no early exit): a `| head`-style stop would EPIPE
# under pipefail (#475).
task_status() {
  awk -v pat="$2" '
    index($0, "TASK [") == 1 { intask = (index($0, pat) > 0); next }
    intask && /^(ok|skipping|changed|fatal|failed):/ {
      sub(/:.*/, "", $0); print; intask = 0
    }' "$1"
}

# run_play <logfile> <extra-var>... — tag-select the three CH-config files
# (plus the `always`-tagged detect tasks) and skip preflight, whose tasks
# are `always` and assume a real archival node.
run_play() {
  local log="$1"; shift
  ansible-playbook -i "$TMP/inventory" "$TMP/play.yml" \
    --tags clickhouse-serving-profile,clickhouse-drop-guard,clickhouse-exporter \
    --skip-tags preflight \
    "$@" > "$log" 2>&1
}

ABSENT="$TMP/no-clickhouse-on-this-host"
PRESENT="$TMP/etc-clickhouse-server"
mkdir -p "$PRESENT"

# ── A. no ClickHouse, run_clickhouse false: the role must survive ─────
run_play "$TMP/a.log" -e "clickhouse_server_config_dir=$ABSENT" -e run_clickhouse=false
rc=$?
if [ "$rc" -eq 0 ]; then
  ok "A: host without ClickHouse — role completes (rc 0)"
else
  bad "A: host without ClickHouse — role FAILED (rc $rc); see below"
  sed -n '1,40p' "$TMP/a.log" | sed 's/^/       /'
fi
# Non-vacuous: it must have completed by SKIPPING the CH work, not by
# quietly running it. The serving-profile assert is the task that aborted
# the role before the fix; the other two are the files that would have
# written drop-ins into a directory ClickHouse does not have here.
for task in "Assert ClickHouse serving-profile password is set" \
            "Pin ClickHouse max_table_size_to_drop" \
            "Enable the ClickHouse Prometheus endpoint"; do
  st="$(task_status "$TMP/a.log" "$task")"
  if [ "$st" = "skipping" ]; then
    ok "A: '$task' skipped, not executed"
  else
    bad "A: '$task' reported '${st:-<never ran>}', expected 'skipping'"
  fi
done

# ── B. r1's shape: ClickHouse installed, run_clickhouse false ─────────
# The gate MUST open here. Gating on run_clickhouse alone passes arm A
# and fails this one — it would strip r1's drop guard, serving profile
# and metrics endpoint.
run_play "$TMP/b.log" -e "clickhouse_server_config_dir=$PRESENT" -e run_clickhouse=false
rc=$?
st="$(task_status "$TMP/b.log" "Assert ClickHouse serving-profile password is set")"
if [ "$st" = "fatal" ] || [ "$st" = "failed" ]; then
  ok "B: ClickHouse present + run_clickhouse false (r1) — CH-config tasks still run"
else
  bad "B: ClickHouse present + run_clickhouse false (r1) — serving-profile assert reported" \
      "'${st:-<never ran>}'; r1 would lose the drop guard, the serving profile and the metrics endpoint"
fi
# And with no vault password that run is fail-closed, as designed.
if [ "$rc" -ne 0 ] && grep -q 'clickhouse_serving_password is not set' "$TMP/b.log"; then
  ok "B: missing vault password still fails closed (rc $rc)"
else
  bad "B: missing vault password did not fail closed (rc $rc)"
fi

# ── C. fresh host that is installing ClickHouse on this same run ──────
run_play "$TMP/c.log" -e "clickhouse_server_config_dir=$ABSENT" -e run_clickhouse=true
st="$(task_status "$TMP/c.log" "Assert ClickHouse serving-profile password is set")"
if [ "$st" = "fatal" ] || [ "$st" = "failed" ]; then
  ok "C: run_clickhouse true — CH-config tasks run even before the package lands"
else
  bad "C: run_clickhouse true — serving-profile assert reported '${st:-<never ran>}'"
fi

echo
echo "ansible-clickhouse-host-gate-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
