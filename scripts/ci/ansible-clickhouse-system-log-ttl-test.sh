#!/usr/bin/env bash
# ansible-clickhouse-system-log-ttl-test.sh — pins ClickHouse system-log retention.
#   - the real template, rendered with the role defaults, is well-formed XML and gives
#     every log in clickhouse_system_log_retention_days a DELETE ttl of its day count;
#   - every log the brief names is covered (a dropped entry is a silent unbounded table);
#   - the leftover matcher accepts <allowlisted base>_<digits> and nothing else;
#   - --apply drops only those names, in `system`, and arms the force flag only for an
#     oversize table, always removing it afterwards.
# Hermetic: local ansible render + a stub clickhouse-client, all in a mktemp dir.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="${ROLE:-$PWD/configs/ansible/roles/archival-node}"
SCRIPT="$ROLE/files/ch-system-log-leftovers.sh"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

for tool in ansible-playbook python3; do
  if ! command -v "$tool" >/dev/null; then
    echo "ansible-clickhouse-system-log-ttl-test: FAIL — $tool not on PATH (must not pass vacuously)" >&2
    exit 1
  fi
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/fixture.yml" <<YML
---
- hosts: localhost
  connection: local
  gather_facts: false
  vars_files:
    - "$ROLE/defaults/main.yml"
  tasks:
    - name: render the role template
      ansible.builtin.template:
        src: "$ROLE/templates/clickhouse-system-log-ttl.xml.j2"
        dest: "$TMP/ttl.xml"
        mode: "0644"
    - name: dump the retention map
      ansible.builtin.copy:
        content: "{{ clickhouse_system_log_retention_days | to_json }}"
        dest: "$TMP/days.json"
        mode: "0644"
YML
if ! out="$(cd "$TMP" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
    ansible-playbook -i localhost, fixture.yml 2>&1)"; then
  bad "rendering the template failed: $(tail -5 <<<"$out")"
else
  res="$(python3 -I - "$TMP/ttl.xml" "$TMP/days.json" <<'PY'
import json, sys
import xml.etree.ElementTree as ET
root = ET.parse(sys.argv[1]).getroot()
days = json.load(open(sys.argv[2]))
required = {"query_log", "part_log", "text_log", "trace_log", "processors_profile_log",
            "query_views_log", "metric_log", "asynchronous_metric_log", "query_metric_log",
            "background_schedule_pool_log", "error_log"}
errs = []
if root.tag != "clickhouse":
    errs.append("root is " + root.tag)
for name in sorted(required - set(days)):
    errs.append(name + " missing from retention map")
for name, n in days.items():
    el = root.find(name)
    ttl = el.find("ttl") if el is not None else None
    want = "event_date + INTERVAL %d DAY DELETE" % n
    if ttl is None or (ttl.text or "").strip() != want:
        errs.append("%s: ttl is %r, want %r" % (name, None if ttl is None else ttl.text, want))
    if el is not None and el.find("engine") is not None:
        errs.append(name + ": engine beside ttl is not allowed")
for el in root:
    if el.tag not in days:
        errs.append("unexpected section " + el.tag)
print("\n".join(errs))
PY
)"
  if [ -z "$res" ]; then
    ok "every log has a well-formed DELETE ttl matching its retention variable"
  else
    bad "rendered XML wrong: $res"
  fi
fi

BASES=(query_log part_log metric_log)
check_match() { # expect(0|1) name
  "$SCRIPT" --match "$2" "${BASES[@]}"
  local rc=$?
  if [ "$rc" -eq "$1" ]; then ok "match $2 -> $1"; else bad "match $2 gave $rc, want $1"; fi
}
check_match 0 query_log_0
check_match 0 part_log_12
check_match 1 query_log
check_match 1 query_log_backup
check_match 1 metric_log_0x
check_match 1 xquery_log_1
check_match 1 other_log_1

mkdir "$TMP/flags"
cat > "$TMP/ch" <<'STUB'
#!/usr/bin/env bash
q=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-q" ]; then q="$2"; fi
  shift
done
case "$q" in
  *"FROM system.tables"*)
    # includes names the server-side filter would not return, to prove the bash re-check
    printf 'query_log_0\t100\nmetric_log_3\t999999\npart_log_x\t5\nquery_log\t7\nquery_log_1\t\\N\n' ;;
  *"FROM system.server_settings"*) echo 1000 ;;
  DROP*)
    echo "$q" >> "$STUB_LOG"
    if [ -e "$STUB_FLAGS/force_drop_table" ]; then echo "FLAG-ARMED $q" >> "$STUB_LOG"; fi ;;
esac
STUB
chmod +x "$TMP/ch"
: > "$TMP/log"
out="$(STUB_LOG="$TMP/log" STUB_FLAGS="$TMP/flags" CLICKHOUSE_CLIENT="$TMP/ch" \
  CLICKHOUSE_FLAG_DIR="$TMP/flags" "$SCRIPT" --apply "${BASES[@]}" 2>&1)"
bt='`'
want_drops="$(printf 'DROP TABLE IF EXISTS system.%squery_log_0%s SYNC\nDROP TABLE IF EXISTS system.%smetric_log_3%s SYNC\nFLAG-ARMED DROP TABLE IF EXISTS system.%smetric_log_3%s SYNC\nDROP TABLE IF EXISTS system.%squery_log_1%s SYNC\n' "$bt" "$bt" "$bt" "$bt" "$bt" "$bt" "$bt" "$bt")"
got="$(cat "$TMP/log")"
want_drops="${want_drops%$'\n'}"
if [ "$(sort <<<"$got")" = "$(sort <<<"$want_drops")" ]; then
  ok "apply drops exactly the allowlisted leftovers, flag armed only for the oversize one"
else
  bad "apply statements wrong: $got"
fi
dropped="$(grep -c '^DROPPED ' <<<"$out")"
if [ "$dropped" = 3 ] && [ ! -e "$TMP/flags/force_drop_table" ]; then
  ok "three DROPPED lines printed and the force flag is gone"
else
  bad "output/flag wrong: $out"
fi

echo "ansible-clickhouse-system-log-ttl-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
