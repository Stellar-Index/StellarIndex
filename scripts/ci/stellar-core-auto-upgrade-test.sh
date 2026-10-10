#!/usr/bin/env bash
# shellcheck disable=SC2016  # check() evals its single-quoted assertions late
# stellar-core-auto-upgrade-test.sh — fixture tests for the archival-node
# role's stellar-core-auto-upgrade script.
#
# What must hold:
#   1. candidate == installed touches nothing and reports current;
#   2. a newer candidate is installed under a stopped galexie, the
#      captive-core configs get group galexie 0640 back, and the run
#      reports upgraded only once galexie writes past its stop tip;
#   3. a candidate that never advances the tip is rolled back to the
#      previous version and reports rolled_back with a failing exit;
#   4. a rollback that also stalls reports failed;
#   5. a pin, the stop file, or an archive fill in flight skip without
#      stopping galexie or installing anything;
#   6. no readable tip skips without installing and leaves galexie running;
#   7. the textfile is valid one-hot exposition, 0644, with no temp file left.
#
# apt-get, apt-cache, dpkg, dpkg-query, systemctl, chgrp and sleep are stubbed
# on PATH; the shipped file runs as-is.
#
# Run: bash scripts/ci/stellar-core-auto-upgrade-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/configs/ansible/roles/archival-node/files/stellar-core-auto-upgrade.sh"
[[ -r "$SCRIPT" ]] || { echo "stellar-core-auto-upgrade-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
BIN="$TMP/bin"
mkdir -p "$BIN"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

cat > "$BIN/dpkg-query" <<'EOF'
#!/bin/bash
cat "$ST/installed"
EOF
cat > "$BIN/apt-cache" <<'EOF'
#!/bin/bash
echo "stellar-core:"; echo "  Installed: $(cat "$ST/installed")"; echo "  Candidate: $(cat "$ST/candidate")"
EOF
cat > "$BIN/apt-get" <<'EOF'
#!/bin/bash
echo "apt-get $*" >> "$ST/calls"
[[ "$1" == update ]] && exit "${APT_UPDATE_RC:-0}"
for a in "$@"; do [[ "$a" == stellar-core=* ]] && echo "${a#stellar-core=}" > "$ST/installed"; done
exit 0
EOF
cat > "$BIN/dpkg" <<'EOF'
#!/bin/bash
# dpkg --compare-versions A gt B
[[ "$1" == --compare-versions && "$3" == gt ]] || exit 2
[[ "$2" != "$4" ]] && [[ "$(printf '%s\n%s\n' "$2" "$4" | sort -V | tail -1)" == "$2" ]]
EOF
cat > "$BIN/systemctl" <<'EOF'
#!/bin/bash
echo "systemctl $*" >> "$ST/calls"
case "$1" in
  is-active)
    case "$3" in
      galexie) [[ -e "$ST/galexie_running" ]] ;;
      galexie-archive-fill.service) [[ -e "$ST/fill_running" ]] ;;
      *) exit 3 ;;
    esac ;;
  stop) [[ "$2" == galexie ]] && rm -f "$ST/galexie_running"; exit 0 ;;
  start)
    case "$2" in
      galexie) touch "$ST/galexie_running" ;;
      galexie-archive-tip-lag.service)
        [[ -e "$ST/no_tip" ]] && exit 1
        if [[ -e "$ST/galexie_running" ]] && grep -qxF "$(cat "$ST/installed")" "$ST/good_versions"; then
          echo $(( $(cat "$ST/tip") + 5 )) > "$ST/tip"
        fi
        echo "galexie_live_tip_ledger $(cat "$ST/tip")" > "$TEXTFILE_DIR/galexie_archive_tip_lag.prom" ;;
    esac
    exit 0 ;;
  *) exit 0 ;;
esac
EOF
cat > "$BIN/chgrp" <<'EOF'
#!/bin/bash
echo "chgrp $*" >> "$ST/calls"
EOF
printf '#!/bin/bash\nexit 0\n' > "$BIN/sleep"
chmod +x "$BIN"/*

# setup INSTALLED CANDIDATE GOOD_VERSIONS...
setup() {
  export ST="$TMP/st.$RANDOM$RANDOM"
  export TEXTFILE_DIR="$ST/textfile" STELLAR_ETC="$ST/etc" STOP_FILE="$ST/stop"
  mkdir -p "$TEXTFILE_DIR" "$STELLAR_ETC"
  echo "$1" > "$ST/installed"; echo "$2" > "$ST/candidate"; shift 2
  printf '%s\n' "$@" > "$ST/good_versions"
  echo 1000 > "$ST/tip"
  echo "galexie_live_tip_ledger 1000" > "$TEXTFILE_DIR/galexie_archive_tip_lag.prom"
  touch "$ST/galexie_running" "$ST/calls"
  echo x > "$STELLAR_ETC/captive-core-galexie.cfg"; chmod 0644 "$STELLAR_ETC/captive-core-galexie.cfg"
  echo x > "$STELLAR_ETC/captive-core-galexie-backfill.cfg"; chmod 0644 "$STELLAR_ETC/captive-core-galexie-backfill.cfg"
}
# shellcheck disable=SC2034  # rc is read inside check's eval strings
run() { PATH="$BIN:$PATH" WAIT_SECS=3 POLL_SECS=1 bash "$SCRIPT" >/dev/null 2>&1; rc=$?; }
result_is() { grep -q "stellarindex_stellar_core_autoupgrade_result{result=\"$1\",reason=\"$2\"[^}]*} 1\$" "$TEXTFILE_DIR/stellar_core_autoupgrade.prom"; }
V28=28.0.1-3508.947aad841.noble
V29=29.0.0-3589.4eb833373.noble

echo "stellar-core-auto-upgrade-test: current"
setup "$V29" "$V29" "$V29"; run
check "same version reports current, exit 0" '[[ $rc == 0 ]] && result_is current ""'
check "same version never stops galexie" '! grep -q "systemctl stop galexie" "$ST/calls"'

echo "stellar-core-auto-upgrade-test: upgrade that advances the tip"
setup "$V28" "$V29" "$V28" "$V29"; run
check "reports upgraded, exit 0" '[[ $rc == 0 ]] && result_is upgraded ""'
check "installed the candidate" '[[ $(cat "$ST/installed") == "$V29" ]]'
check "galexie stopped before the install and started after" \
  '[[ $(grep -oE "^(systemctl stop galexie|apt-get install|systemctl start galexie)$|^apt-get install" "$ST/calls" | tr "\n" ,) == "systemctl stop galexie,apt-get install,systemctl start galexie," ]]'
check "captive-core configs chgrp galexie and mode 0640" \
  '[[ $(grep -c "^chgrp galexie" "$ST/calls") == 2 ]] && [[ $(stat -c %a "$STELLAR_ETC/captive-core-galexie.cfg" 2>/dev/null || stat -f %Lp "$STELLAR_ETC/captive-core-galexie.cfg") == 640 ]]'
check "galexie left running" '[[ -e "$ST/galexie_running" ]]'
check "exposition is one-hot" '[[ $(grep -c "^stellarindex_stellar_core_autoupgrade_result{.*} 1$" "$TEXTFILE_DIR/stellar_core_autoupgrade.prom") == 1 ]]'
check "run stamp written" 'grep -qE "^stellarindex_stellar_core_autoupgrade_last_run_timestamp_seconds [0-9]+$" "$TEXTFILE_DIR/stellar_core_autoupgrade.prom"'
check "textfile is 0644" '[[ $(stat -c %a "$TEXTFILE_DIR/stellar_core_autoupgrade.prom" 2>/dev/null || stat -f %Lp "$TEXTFILE_DIR/stellar_core_autoupgrade.prom") == 644 ]]'
check "no temp file left" '[[ -z $(find "$TEXTFILE_DIR" -name ".stellar_core_autoupgrade.*") ]]'
if command -v promtool >/dev/null 2>&1; then
  check "promtool accepts the textfile" 'promtool check metrics < "$TEXTFILE_DIR/stellar_core_autoupgrade.prom" >/dev/null 2>&1'
fi

echo "stellar-core-auto-upgrade-test: upgrade that stalls the tip"
setup "$V28" "$V29" "$V28"; run
check "reports rolled_back, exit non-zero" '[[ $rc != 0 ]] && result_is rolled_back tip_stalled'
check "previous version reinstalled" '[[ $(cat "$ST/installed") == "$V28" ]]'
check "galexie left running" '[[ -e "$ST/galexie_running" ]]'

echo "stellar-core-auto-upgrade-test: rollback that also stalls"
setup "$V28" "$V29"; run
check "reports failed, exit non-zero" '[[ $rc != 0 ]] && result_is failed rollback_stalled'

echo "stellar-core-auto-upgrade-test: skips"
setup "$V28" "$V29" "$V29"; STELLAR_CORE_PIN="$V28" run
check "pin skips before apt" '[[ $rc == 0 ]] && result_is skipped pinned && ! grep -q apt-get "$ST/calls"'
setup "$V28" "$V29" "$V29"; touch "$STOP_FILE"; run
check "stop file skips before apt" '[[ $rc == 0 ]] && result_is skipped stop_file && ! grep -q apt-get "$ST/calls"'
setup "$V28" "$V29" "$V29"; touch "$ST/fill_running"; run
check "archive fill in flight skips without stopping galexie" \
  '[[ $rc == 0 ]] && result_is skipped archive_fill_running && ! grep -q "systemctl stop" "$ST/calls" && [[ $(cat "$ST/installed") == "$V28" ]]'
setup "$V28" "$V29" "$V29"; touch "$ST/no_tip"; rm -f "$TEXTFILE_DIR/galexie_archive_tip_lag.prom"; run
check "no tip skips without installing, galexie restarted" \
  '[[ $rc == 0 ]] && result_is skipped no_tip_metric && ! grep -q "apt-get install" "$ST/calls" && [[ -e "$ST/galexie_running" ]]'
setup "$V28" "$V29" "$V29"; APT_UPDATE_RC=100 run
check "apt update failure reports failed" '[[ $rc != 0 ]] && result_is failed apt_update'
setup "$V28" "$V29" "$V29"; run
check "apt update reads only the SDF list" 'grep -q "^apt-get update .*sourcelist=sources.list.d/sdf.list -o Dir::Etc::sourceparts=- " "$ST/calls"'

echo
echo "stellar-core-auto-upgrade-test: $pass passed, $fail failed"
[[ $fail == 0 ]]
