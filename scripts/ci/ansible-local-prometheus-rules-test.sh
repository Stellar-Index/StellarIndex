#!/usr/bin/env bash
# ansible-local-prometheus-rules-test.sh — pins the archival-node local
# Prometheus rule loading (local_prometheus_rules_enabled):
#   - every file in the default include list local_prometheus_rule_files exists
#     in configs/prometheus/rules.r1/;
#   - none is a file known to misfire on a test net, none applies absent() or
#     absent_over_time() to a metric outside ALLOWED_ABSENT (a staleness rule
#     on a component the host may not run fires forever), and none carries
#     severity page (a page turns the public /v1/status `overall` to degraded);
#   - the template renders rule_files only with the switch on, at the dir the
#     task installs into, and never an `alerting:` block (no delivery).
#
# Renders the real template with a local ansible-playbook run; no hosts.
# ROLE / RULES_DIR override the inputs so the failure paths can be exercised.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="${ROLE:-$PWD/configs/ansible/roles/archival-node}"
RULES_DIR="${RULES_DIR:-$PWD/configs/prometheus/rules.r1}"
RULES_GLOB="/etc/prometheus/rules.local/*.yml"

# Each misfires on a lean host; served-values.yml through
# stellarindex_served_value_check_stale (pubnet-only verify-served-values).
FORBIDDEN="served-values.yml aggregator.yml backup-offsite.yml cross-region.yml zfs-snapshots.yml restore-drill.yml"
# Metrics an included file may test with absent(): their producer runs on every
# archival node (data-freshness.timer is not network-gated).
ALLOWED_ABSENT="stellarindex_data_freshness_stale"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-local-prometheus-rules-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

render() { # $1 = true|false (local_prometheus_rules_enabled), $2 = output file
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
        src: "$ROLE/templates/local-prometheus.yml.j2"
        dest: "$2"
        mode: "0644"
    - name: write the default include list
      ansible.builtin.copy:
        content: "{{ local_prometheus_rule_files | join('\n') }}\n"
        dest: "$TMP/rule-files.txt"
        mode: "0644"
YML
  (cd "$TMP" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
    ansible-playbook -i localhost, fixture.yml -e region_id=ci -e local_prometheus_rules_enabled="$1" 2>&1)
}

if ! out="$(render false "$TMP/off.yml")"; then
  bad "rendering with rules off failed: $(tail -5 <<<"$out")"
fi
if ! out="$(render true "$TMP/on.yml")"; then
  bad "rendering with rules on failed: $(tail -5 <<<"$out")"
fi

files="$(grep -v '^$' "$TMP/rule-files.txt" 2>/dev/null || true)"
n="$(grep -c . <<<"$files" || true)"
if [ "$n" -gt 0 ]; then
  ok "default include list names $n file(s)"
else
  bad "default include list is empty or did not render"
fi

for f in $files; do
  if [ ! -f "$RULES_DIR/$f" ]; then
    bad "$f is in local_prometheus_rule_files but not in $RULES_DIR"
    continue
  fi
  case " $FORBIDDEN " in
    *" $f "*) bad "$f is in the default include list but misfires on a test net" ;;
  esac
  live="$(grep -v '^[[:space:]]*#' "$RULES_DIR/$f")"
  absent_metrics="$(grep -oE 'absent(_over_time)?\([[:space:]]*[A-Za-z_:][A-Za-z0-9_:]*' <<<"$live" \
    | sed -E 's/^absent(_over_time)?\([[:space:]]*//' | sort -u)"
  for m in $absent_metrics; do
    case " $ALLOWED_ABSENT " in
      *" $m "*) ;;
      *) bad "$f applies absent() to $m, not in ALLOWED_ABSENT; it fires forever where its producer does not run" ;;
    esac
  done
  if grep -qE '^[[:space:]]*severity:[[:space:]]*"?page"?' "$RULES_DIR/$f"; then
    bad "$f carries severity page; it would turn a test net's /v1/status overall to degraded"
  fi
done
[ "$fail" -eq 0 ] && [ "$n" -gt 0 ] && ok "every listed file exists, is not forbidden, has no disallowed absent() and no page rule"

for v in off on; do
  if [ -f "$TMP/$v.yml" ] && grep -qE '^[[:space:]]*alerting:' "$TMP/$v.yml"; then
    bad "template with rules $v renders an alerting: block; delivery is not configured on this path"
  fi
done
if [ -f "$TMP/on.yml" ] && ! grep -qE '^[[:space:]]*alerting:' "$TMP/on.yml" \
   && [ -f "$TMP/off.yml" ] && ! grep -qE '^[[:space:]]*alerting:' "$TMP/off.yml"; then
  ok "no alerting: block with rules on or off"
fi

if [ -f "$TMP/off.yml" ] && ! grep -q '^rule_files:' "$TMP/off.yml"; then
  ok "rules off: no rule_files"
else
  bad "rules off: rule_files rendered (or render missing)"
fi
if [ -f "$TMP/on.yml" ] && grep -q '^rule_files:' "$TMP/on.yml" && grep -qF -- "- $RULES_GLOB" "$TMP/on.yml"; then
  ok "rules on: rule_files loads $RULES_GLOB"
else
  bad "rules on: rule_files missing or not $RULES_GLOB"
fi
if grep -qF 'dest: "/etc/prometheus/rules.local/{{ item }}"' "$ROLE/tasks/23-local-prometheus.yml"; then
  ok "23-local-prometheus.yml installs into the dir the template loads"
else
  bad "23-local-prometheus.yml does not copy rules to /etc/prometheus/rules.local/ — template and task disagree"
fi

if command -v promtool >/dev/null; then
  for v in off on; do
    if [ -f "$TMP/$v.yml" ] && promtool check config --syntax-only "$TMP/$v.yml" >/dev/null 2>&1; then
      ok "promtool accepts the rules-$v render"
    else
      bad "promtool rejects the rules-$v render: $(promtool check config --syntax-only "$TMP/$v.yml" 2>&1 | tail -3)"
    fi
  done
  # shellcheck disable=SC2086 # $files is a whitespace list of bare names
  if [ -n "$files" ] && (cd "$RULES_DIR" && promtool check rules $files >/dev/null 2>&1); then
    ok "promtool accepts every listed rule file"
  else
    bad "promtool rejects a listed rule file"
  fi
else
  echo "  skip — promtool not on PATH; render and rule files not promtool-checked"
fi

echo "ansible-local-prometheus-rules-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
