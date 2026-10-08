#!/bin/bash
# stellar-core-auto-upgrade — install apt's newer stellar-core without an operator.
#
# WHY: a captive-core older than the network protocol halts ingest at the
# upgrade-vote ledger. The version-lag probe detected core 29.0.0 (a P29
# prerequisite and SDF security fix) within a day of its publish,
# and the P1 page then sat unactioned for ~2.5 days with the vote 4 days out.
# Detection was never the gap; acting was. This closes it for core, which
# ships through apt.stellar.org. galexie ships from a git tag and needs a new
# build only when the XDR changes, which is an SDK code change anyway, so it
# stays on the lag alert.
#
# Each run: if the apt candidate is newer than the installed core, stop
# galexie, install the candidate, restore the captive-core config group
# (the deb postinst chowns /etc/stellar to stellar:stellar and galexie then
# cannot read its config), start galexie and wait for the live
# ledger tip to advance. No advance → reinstall the previous version and
# publish result="rolled_back", which pages.
#
# Skips (result="skipped", reason label): a stellar_core_version inventory
# pin (the pin is the operator's decision), the stop file, or a
# galexie-archive-fill run in flight (it shares the core binary).
set -uo pipefail

TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
OUT="$TEXTFILE_DIR/stellar_core_autoupgrade.prom"
STELLAR_ETC="${STELLAR_ETC:-/etc/stellar}"
STOP_FILE="${STOP_FILE:-/var/lib/stellarindex/stellar-core-auto-upgrade.stop}"
PIN="${STELLAR_CORE_PIN:-}"
WAIT_SECS="${WAIT_SECS:-2700}"
POLL_SECS="${POLL_SECS:-30}"

result=""
reason=""
installed=""
candidate=""

publish() {
  local tmp now r
  now=$(date +%s)
  tmp=$(mktemp "$TEXTFILE_DIR/.stellar_core_autoupgrade.XXXXXX") || exit 1
  {
    echo '# HELP stellarindex_stellar_core_autoupgrade_result Outcome of the last stellar-core auto-upgrade run (one-hot).'
    echo '# TYPE stellarindex_stellar_core_autoupgrade_result gauge'
    for r in current upgraded skipped rolled_back failed; do
      if [[ "$r" == "$result" ]]; then
        echo "stellarindex_stellar_core_autoupgrade_result{result=\"$r\",reason=\"$reason\",installed=\"$installed\",candidate=\"$candidate\"} 1"
      else
        echo "stellarindex_stellar_core_autoupgrade_result{result=\"$r\",reason=\"\",installed=\"$installed\",candidate=\"$candidate\"} 0"
      fi
    done
    echo '# HELP stellarindex_stellar_core_autoupgrade_last_run_timestamp_seconds When the auto-upgrade last ran to completion.'
    echo '# TYPE stellarindex_stellar_core_autoupgrade_last_run_timestamp_seconds gauge'
    echo "stellarindex_stellar_core_autoupgrade_last_run_timestamp_seconds $now"
  } > "$tmp"
  chmod 0644 "$tmp"
  mv "$tmp" "$OUT"
}

finish() { result="$1"; reason="${2:-}"; publish; echo "stellar-core-auto-upgrade: $result${reason:+ ($reason)} installed=$installed candidate=$candidate"; [[ "$result" == rolled_back || "$result" == failed ]] && exit 1; exit 0; }

# The tip gauge is refreshed by a 5-minute timer; start its oneshot so the
# reading reflects the bucket now, not a pre-stop write.
live_tip() {
  systemctl start galexie-archive-tip-lag.service >/dev/null 2>&1
  local lines
  lines=$(grep -h '^galexie_live_tip_ledger ' "$TEXTFILE_DIR"/*.prom 2>/dev/null)
  awk 'NR == 1 {print $2}' <<<"$lines"
}

fix_perms() {
  local f
  for f in "$STELLAR_ETC"/captive-core-galexie*.cfg; do
    [[ -e "$f" ]] || continue
    chgrp galexie "$f" && chmod 0640 "$f" || return 1
  done
}

install_core() {
  DEBIAN_FRONTEND=noninteractive apt-get install -y -q --allow-downgrades "stellar-core=$1" >/dev/null
}

# Swap the binary under a stopped galexie; succeed once galexie writes a ledger
# past the tip it stopped at. Returns 2 when there is no tip to measure.
swap_and_wait() {
  local version="$1" waited=0 tip0 tip
  systemctl stop galexie || return 1
  tip0=$(live_tip)
  [[ "$tip0" =~ ^[0-9]+$ ]] || { systemctl start galexie; return 2; }
  install_core "$version" || { fix_perms; systemctl start galexie; return 1; }
  fix_perms || { systemctl start galexie; return 1; }
  if systemctl is-active -q stellar-core; then systemctl restart stellar-core || return 1; fi
  systemctl start galexie || return 1
  while (( waited < WAIT_SECS )); do
    sleep "$POLL_SECS"
    waited=$((waited + POLL_SECS))
    systemctl is-active -q galexie || return 1
    tip=$(live_tip)
    [[ "$tip" =~ ^[0-9]+$ ]] && (( tip > tip0 )) && return 0
  done
  return 1
}

[[ -n "$PIN" ]] && finish skipped pinned
[[ -e "$STOP_FILE" ]] && finish skipped stop_file

apt-get update -q >/dev/null 2>&1 || finish failed apt_update
installed=$(dpkg-query -W -f='${Version}' stellar-core 2>/dev/null) || finish failed not_installed
policy=$(apt-cache policy stellar-core 2>/dev/null)
candidate=$(awk '/Candidate:/ && !seen {print $2; seen = 1}' <<<"$policy")
[[ -n "$candidate" && "$candidate" != "(none)" ]] || finish failed no_candidate
dpkg --compare-versions "$candidate" gt "$installed" || finish current

systemctl is-active -q galexie-archive-fill.service && finish skipped archive_fill_running

swap_and_wait "$candidate"
case $? in
  0) finish upgraded ;;
  2) finish skipped no_tip_metric ;;
esac
swap_and_wait "$installed" && finish rolled_back tip_stalled
finish failed rollback_stalled
