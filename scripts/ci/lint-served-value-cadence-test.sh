#!/usr/bin/env bash
# lint-served-value-cadence-test.sh — fixture tests for the served-value
# scheduling gate (scripts/ci/lint-served-value-cadence.sh).
#
# The gate exists because the harness it guards shipped fully alerted and
# entirely unscheduled: three rules, a runbook saying "daily timer", and no
# unit, timer, ansible task or cron anywhere. A gate against that class is
# only worth its line count if it fails on the shapes that reproduce it, so
# its verdicts are pinned here rather than assumed:
#
#   - the repo's own tree passes;
#   - a complete fixture tree passes;
#   - a missing service or timer template is CAUGHT;
#   - a service that writes its .prom outside the collector directory, or
#     runs some other command, is CAUGHT;
#   - a task file that only STOPS, DISABLES and DELETES the units is
#     `grep -rl verify-served-values.service` is satisfied by a removal
#     block as readily as by an install block, and this role ships removal
#     blocks for three network-gated ops jobs;
#   - an install block WITH its paired removal block (the real file's
#     shape) still passes, so the assertion is precise and not merely
#     allergic to the word "absent";
#   - rendering the units without enabling the timer is CAUGHT;
#   - a network gate that is false on pubnet is CAUGHT — flipping that one
#     default unschedules the harness again with every other assertion
#     still green;
#   - a weekly timer against the 48h threshold is CAUGHT at both bounds;
#   - an empty task tree FAILS rather than passing vacuously.
#
# Run: bash scripts/ci/lint-served-value-cadence-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-served-value-cadence.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
check() { # check <desc> <want-exit> <root>
  local desc="$1" want="$2" root="$3" got
  bash "$LINT" "$root" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want $want)"
    fail=$((fail + 1))
  fi
}

UNITS=configs/ansible/roles/archival-node/templates/systemd
TASKS=configs/ansible/roles/archival-node/tasks
DEFAULTS=configs/ansible/roles/archival-node/defaults

install_block() {
  cat <<'YML'
- name: Install + enable verify-served-values
  tags: [ops-jobs]
  when: verify_served_values_enabled | bool
  block:
    - name: Install verify-served-values unit + timer
      ansible.builtin.template:
        src: "systemd/{{ item }}.j2"
        dest: "/etc/systemd/system/{{ item }}"
        mode: "0644"
      loop:
        - verify-served-values.service
        - verify-served-values.timer
      notify: Reload systemd

    - name: Enable + start verify-served-values.timer
      ansible.builtin.systemd:
        name: verify-served-values.timer
        state: started
        enabled: true
        daemon_reload: true
YML
}

removal_block() {
  cat <<'YML'
- name: Remove verify-served-values where its ground truth is not correct
  tags: [ops-jobs]
  when: not (verify_served_values_enabled | bool)
  block:
    - name: Stop + disable verify-served-values timer and service
      ansible.builtin.systemd:
        name: "{{ item }}"
        state: stopped
        enabled: false
      loop:
        - verify-served-values.timer
        - verify-served-values.service
      failed_when: false

    - name: Remove verify-served-values unit files
      ansible.builtin.file:
        path: "/etc/systemd/system/{{ item }}"
        state: absent
      loop:
        - verify-served-values.timer
        - verify-served-values.service
      notify: Reload systemd
YML
}

rules_file() { # rules_file <threshold-seconds>
  cat <<YML
groups:
  - name: data-freshness
    rules:
      - alert: stellarindex_served_value_check_stale
        expr: |
          (time() - stellarindex_served_value_last_run_unix) > $1
          or
          absent_over_time(stellarindex_served_value_last_run_unix[2d])
        for: 1h
        labels:
          severity: ticket
YML
}

mk_tree() { # mk_tree <root>
  local r="$TMP/$1"
  mkdir -p "$r/$UNITS" "$r/$TASKS" "$r/$DEFAULTS" \
           "$r/deploy/monitoring/rules" "$r/configs/prometheus/rules.r1"

  cat > "$r/$UNITS/verify-served-values.service.j2" <<'YML'
[Service]
Type=oneshot
Environment=TEXTFILE=/var/lib/node_exporter/textfile_collector/served_values.prom
ExecStart=/usr/local/bin/stellarindex-ops verify-served-values \
  -api ${API_BASE} \
  -textfile ${TEXTFILE} \
  -timeout ${RUN_TIMEOUT}
YML

  cat > "$r/$UNITS/verify-served-values.timer.j2" <<'YML'
[Timer]
OnCalendar=*-*-* 06:20:00 UTC
RandomizedDelaySec=300
Persistent=true
Unit=verify-served-values.service
YML

  { install_block; echo; removal_block; } > "$r/$TASKS/14-stellarindex-services.yml"

  printf '%s\n' \
    "verify_served_values_enabled: \"{{ (stellar_network | default('pubnet')) == 'pubnet' }}\"" \
    > "$r/$DEFAULTS/main.yml"

  rules_file 172800 > "$r/deploy/monitoring/rules/data-freshness.yml"
  rules_file 172800 > "$r/configs/prometheus/rules.r1/data-freshness.yml"
  echo "$r"
}

echo "lint-served-value-cadence-test: scheduling + calibration verdicts"

check "the repo's own tree passes" 0 "$PWD"

good="$(mk_tree good)"
check "a complete fixture tree passes" 0 "$good"

no_service="$(mk_tree no_service)"
rm "$no_service/$UNITS/verify-served-values.service.j2"
check "a missing service template is caught" 1 "$no_service"

no_timer="$(mk_tree no_timer)"
rm "$no_timer/$UNITS/verify-served-values.timer.j2"
check "a missing timer template is caught" 1 "$no_timer"

wrong_path="$(mk_tree wrong_path)"
sed 's|/var/lib/node_exporter/textfile_collector|/var/tmp|' \
  "$good/$UNITS/verify-served-values.service.j2" \
  > "$wrong_path/$UNITS/verify-served-values.service.j2"
check "a .prom written outside the collector directory is caught" 1 "$wrong_path"

wrong_cmd="$(mk_tree wrong_cmd)"
sed 's|stellarindex-ops verify-served-values|stellarindex-ops verify-archive|' \
  "$good/$UNITS/verify-served-values.service.j2" \
  > "$wrong_cmd/$UNITS/verify-served-values.service.j2"
check "a unit that runs some other subcommand is caught" 1 "$wrong_cmd"

# THE CASE THE PREVIOUS REVISION PASSED. Every mention of both unit names is
# here — twice each — and nothing installs anything.
removal_only="$(mk_tree removal_only)"
removal_block > "$removal_only/$TASKS/14-stellarindex-services.yml"
check "a task file that only REMOVES the units is caught" 1 "$removal_only"

# The install block present but under another tag: the documented apply
# (--tags ops-jobs) would never render it, so the harness stays unscheduled.
retagged="$(mk_tree retagged)"
sed 's/ops-jobs/some-other-tag/g' "$retagged/$TASKS/14-stellarindex-services.yml" > "$retagged/$TASKS/14-stellarindex-services.yml.new" \
  && mv "$retagged/$TASKS/14-stellarindex-services.yml.new" "$retagged/$TASKS/14-stellarindex-services.yml"
grep -q 'some-other-tag' "$retagged/$TASKS/14-stellarindex-services.yml" || echo "  (fixture retag did not apply)"
check "an install block under a tag other than ops-jobs is caught" 1 "$retagged"

# The real file's shape: install and removal side by side. The assertion must
# still pass here, or it would just be banning the removal convention.
both="$(mk_tree both)"
check "install + paired removal blocks together still pass" 0 "$both"

no_enable="$(mk_tree no_enable)"
install_block | sed '/Enable + start/,$d' \
  > "$no_enable/$TASKS/14-stellarindex-services.yml"
check "rendering the units without enabling the timer is caught" 1 "$no_enable"

disabled="$(mk_tree disabled)"
printf '%s\n' 'verify_served_values_enabled: false' > "$disabled/$DEFAULTS/main.yml"
check "a network gate that is false on pubnet is caught" 1 "$disabled"

testnet_only="$(mk_tree testnet_only)"
printf '%s\n' \
  "verify_served_values_enabled: \"{{ (stellar_network | default('pubnet')) == 'testnet' }}\"" \
  > "$testnet_only/$DEFAULTS/main.yml"
check "a gate that only enables the harness off pubnet is caught" 1 "$testnet_only"

no_var="$(mk_tree no_var)"
printf '%s\n' 'some_other_flag: true' > "$no_var/$DEFAULTS/main.yml"
check "an undefined enable flag is caught" 1 "$no_var"

weekly="$(mk_tree weekly)"
sed 's|OnCalendar=\*-\*-\* 06:20:00 UTC|OnCalendar=Mon *-*-* 06:20:00 UTC|' \
  "$good/$UNITS/verify-served-values.timer.j2" \
  > "$weekly/$UNITS/verify-served-values.timer.j2"
check "an OnCalendar form the gate cannot parse is refused, not guessed" 1 "$weekly"

too_tight="$(mk_tree too_tight)"
rules_file 86400 > "$too_tight/deploy/monitoring/rules/data-freshness.yml"
check "a threshold below one period plus slack is caught" 1 "$too_tight"

too_loose="$(mk_tree too_loose)"
rules_file 604800 > "$too_loose/configs/prometheus/rules.r1/data-freshness.yml"
check "a threshold beyond two periods is caught" 1 "$too_loose"

one_tree="$(mk_tree one_tree)"
rm "$one_tree/configs/prometheus/rules.r1/data-freshness.yml"
check "a missing second rule tree is caught" 1 "$one_tree"

empty_tasks="$(mk_tree empty_tasks)"
rm "$empty_tasks/$TASKS"/*.yml
check "an empty task tree fails rather than passing vacuously" 1 "$empty_tasks"

echo "----"
echo "lint-served-value-cadence-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
