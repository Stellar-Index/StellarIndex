#!/usr/bin/env bash
# ledger-meta-decode-probe-test.sh — fixture tests for
# configs/ansible/roles/archival-node/files/ledger-meta-decode-probe.sh,
# installed by the archival-node role as /usr/local/bin/ledger-meta-decode-probe.
#
# THE DEFECT THIS PINS. The probe counted decode-failure signatures with
#   n=$(journalctl -u "$u" ... 2>/dev/null | grep -cE "$PATTERN" 2>/dev/null || true)
#   n=${n:-0}
# `grep -c` legitimately prints 0 and exits 1 when nothing matches (the
# healthy case), so `|| true` was load-bearing there -- and it also
# swallowed a genuine journalctl failure (journald unreachable, permission
# denied, missing binary) with it, defaulting n to 0 either way. The
# script's own header promises "fail-open by design: any probe error emits
# nothing rather than a false 0", but the redirected stderr plus `|| true`
# published exactly that false 0 as a real gauge value.
#
# What must hold:
#   1. a healthy read with real decode-failure lines counts them per unit
#      and in the total, and refreshes _updated_seconds;
#   2. a healthy read that found nothing publishes a genuine 0 (must stay
#      distinguishable from a suppressed failure only by the next case);
#   3. a journalctl READ FAILURE (non-zero exit) must NOT publish 0 -- the
#      probe must leave the previous textfile (and its _updated_seconds)
#      untouched so the staleness alert can page, instead of writing a
#      fabricated 0 that masks a real decode failure;
#   4. a renamed/absent unit (journalctl exits 0, no output) is a genuine
#      zero and must still be published, unlike case 3.
#
# Run: bash scripts/ci/ledger-meta-decode-probe-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
PROBE="$PWD/configs/ansible/roles/archival-node/files/ledger-meta-decode-probe.sh"
[[ -r "$PROBE" ]] || { echo "ledger-meta-decode-probe-test: missing $PROBE" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

mkdir -p "$TMP/bin" "$TMP/textfile"
# journalctl stub. All three probed units (galexie, galexie-backfill,
# stellarindex-indexer) get the same scenario per run.
cat > "$TMP/bin/journalctl" <<'SH'
#!/usr/bin/env bash
case "${SCENARIO:-healthy}" in
  healthy)
    echo "Sep 05 09:30:00 r1 galexie[1]: exported ledger 42"
    exit 0 ;;
  decoding_failure)
    echo "Sep 05 09:30:00 r1 galexie[1]: error decoding LedgerCloseMetaV2: decoding GeneralizedTransactionSet"
    echo "Sep 05 09:30:01 r1 galexie[1]: error decoding TransactionPhase: ParallelTxExecutionStage"
    exit 0 ;;
  read_failed)
    echo "Failed to open files: No such file or directory" >&2
    exit 1 ;;
  renamed_unit)
    exit 0 ;;
  no_binary)
    exit 127 ;;
esac
echo "journalctl stub: unknown SCENARIO ${SCENARIO:-}" >&2
exit 9
SH
chmod +x "$TMP/bin/journalctl"
export PATH="$TMP/bin:$PATH"
export TEXTFILE_DIR="$TMP/textfile"
PROM="$TEXTFILE_DIR/ledger_meta_decode.prom"

run() { # run <scenario>
  SCENARIO="$1" bash "$PROBE"
  RC=$?
}

metric() { # metric <exact-name>
  awk -v want="$1" '$1 == want { print $2 }' "$PROM"
}

# expect_eq <label> <got> <want> -- ok/bad without the A && B || C idiom
# (shellcheck SC2015: C can run when A is true too).
expect_eq() {
  local label="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    ok "$label"
  else
    bad "$label (want '$want', got '${got:-<absent>}')"
  fi
}

# expect_rc <label> <got-rc>
expect_rc() {
  local label="$1" got="$2"
  if [[ "$got" -eq 0 ]]; then
    ok "$label"
  else
    bad "$label (got $got)"
  fi
}

# expect_unix_stamp <label> <value>
expect_unix_stamp() {
  local label="$1" got="$2"
  if [[ "$got" =~ ^1[0-9]{9}$ ]]; then
    ok "$label"
  else
    bad "$label (got '${got:-<absent>}')"
  fi
}

# ─── 1. a healthy read with real decode failures ────────────────────
run decoding_failure
expect_rc "a decoding-failure run exits 0" "$RC"
got=$(metric 'stellarindex_ledger_meta_decode_failures{unit="galexie"}')
expect_eq "a decoding-failure run counts the signature lines" "$got" "2"
got=$(metric stellarindex_ledger_meta_decode_failures_total)
expect_eq "the total sums all three units" "$got" "6"
stamp1=$(metric stellarindex_ledger_meta_decode_probe_updated_seconds)
expect_unix_stamp "a decoding-failure run stamps updated_seconds" "$stamp1"

# ─── 2. a healthy read that found nothing: a genuine zero ───────────
run healthy
got=$(metric 'stellarindex_ledger_meta_decode_failures{unit="galexie"}')
expect_eq "a clean run publishes the zero it measured" "$got" "0"
stamp2=$(metric stellarindex_ledger_meta_decode_probe_updated_seconds)
expect_unix_stamp "a clean run stamps updated_seconds" "$stamp2"

# ─── 3. journalctl READ FAILURE must not fabricate a 0 ──────────────
#
# This is the defect: pre-fix, a failed journal read still wrote 0 for
# every unit and refreshed updated_seconds, hiding the failure behind a
# fresh, false "no decode failures" gauge. The prior run (decoding_failure)
# left a genuinely NON-ZERO total on disk: if a failed read fabricated a 0,
# it would show up here as the total dropping to 0. Re-running "healthy"
# afterwards would also read back as 0 and mask the same fabrication, so
# the discriminating check has to happen right after a non-zero run.
run decoding_failure
prev_total=$(metric stellarindex_ledger_meta_decode_failures_total)
prev_stamp=$(metric stellarindex_ledger_meta_decode_probe_updated_seconds)
expect_eq "setup: a decoding-failure run leaves a non-zero total on disk" "$prev_total" "6"

sleep 1
run read_failed
expect_rc "a failed read exits 0 (no crash-loop)" "$RC"
got=$(metric stellarindex_ledger_meta_decode_failures_total)
expect_eq "a failed read does NOT fabricate a total (leaves the prior non-zero value)" "$got" "$prev_total"
stamp3=$(metric stellarindex_ledger_meta_decode_probe_updated_seconds)
expect_eq "a failed read leaves updated_seconds unrefreshed (staleness can page)" "$stamp3" "$prev_stamp"

# ─── 4. a renamed/absent unit is a genuine, publishable zero ────────
run renamed_unit
got=$(metric 'stellarindex_ledger_meta_decode_failures{unit="galexie"}')
expect_eq "a renamed unit still publishes a real zero" "$got" "0"

# ─── 5. journalctl missing from PATH is a read failure too ──────────
run decoding_failure
prev_total=$(metric stellarindex_ledger_meta_decode_failures_total)
run no_binary
got=$(metric stellarindex_ledger_meta_decode_failures_total)
expect_eq "a missing journalctl does NOT fabricate a total (leaves the prior value)" "$got" "$prev_total"

printf 'ledger-meta-decode-probe-test: %d passed, %d failed\n' "$pass" "$fail"
[[ "$fail" -eq 0 ]]
