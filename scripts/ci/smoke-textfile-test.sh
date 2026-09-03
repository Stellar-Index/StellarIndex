#!/usr/bin/env bash
# smoke-textfile-test.sh — fixture tests for the node_exporter textfile
# leg of the 5-minute API smoke wrapper (configs/healthchecks/smoke.sh).
#
# The defect this pins: stellarindex-smoke.timer ran a 34-GET API smoke
# with jq shape assertions every five minutes and its verdict reached NO
# monitoring system. HEALTHCHECKS_URL_SMOKE was empty on r1 (len=0) and
# the wrapper's only other sink was the journal, so the one check that
# can see the API answering 200 with structurally wrong JSON was itself
# unobservable — nothing to query, nothing to alert on, and no way to
# tell a passing run from a timer that had not fired in a week.
#
# What must hold:
#   1. a clean run writes failures=0 AND a last_run_unix stamp — the
#      series stellarindex_api_smoke_stale keys on must advance on a
#      PASS, not only on a failure;
#   2. a failing run writes the smoke's exit code (= failed-check count)
#      and still stamps last_run_unix, so "failing" and "not running"
#      stay distinguishable signals;
#   3. a missing/non-executable smoke script writes failures>0 rather
#      than nothing — leaving the previous run's `failures 0` on disk
#      would have node_exporter vouch for a check that can no longer
#      run (the frozen-textfile trap of #319);
#   4. the write is atomic and world-readable: no .tmp survives, and the
#      file is 0644 so the unprivileged node_exporter can read it (it
#      skips unreadable files silently);
#   5. an unwritable textfile directory does not break the wrapper — it
#      still exits 0 for the timer and still pings Healthchecks.
#
# The SHIPPED wrapper is executed, not a hand-copied twin (same idiom as
# data-freshness-test.sh). curl is stubbed on PATH and TEXTFILE_DIR is
# redirected into a temp dir, so there is no network, no systemd and no
# write outside $TMPDIR.
#
# Run: bash scripts/ci/smoke-textfile-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/configs/healthchecks/smoke.sh"
[[ -r "$SRC" ]] || { echo "smoke-textfile-test: missing $SRC" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# curl stub: records the pinged URL instead of reaching the network, so
# the Healthchecks leg can be asserted without one.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/curl" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do case "$a" in https://*|http://*) echo "$a" >> "$PINGLOG" ;; esac; done
exit 0
STUB
chmod 0755 "$WORK/bin/curl"
export PATH="$WORK/bin:$PATH"

# run_smoke <exit-code> <textfile-dir> — run the shipped wrapper against
# a stub smoke script that exits with the given failed-check count.
# Prints the wrapper's own exit status on stdout.
run_smoke() {
  local rc="$1" dir="$2" stub="$WORK/stub-smoke.sh"
  cat > "$stub" <<STUB
#!/usr/bin/env bash
echo "stub smoke: $rc check(s) FAILED."
exit $rc
STUB
  chmod 0755 "$stub"
  SMOKE_SCRIPT="$stub" TEXTFILE_DIR="$dir" \
    HEALTHCHECKS_URL_SMOKE="https://hc-ping.test/uuid" \
    PINGLOG="$WORK/pings" \
    bash "$SRC" >/dev/null 2>&1
  echo $?
}

# metric <file> <name> — the gauge's value, or empty when the file or
# the series is absent. stderr is dropped: "no textfile at all" is the
# defect state this suite reproduces, so it must read as an empty value
# a check reports on, not as awk noise interleaved with the results.
metric() {
  awk -v n="$2" '$1 == n { print $2 }' "$1" 2>/dev/null
}

now="$(date +%s)"

# ─── 1. a clean run stamps both series ───────────────────────────────
echo "smoke-textfile-test: clean run"
DIR="$WORK/tf-clean"
rc="$(run_smoke 0 "$DIR")"
OUT="$DIR/api_smoke.prom"
if [[ -f "$OUT" ]]; then
  ok "clean run wrote $OUT"
else
  bad "clean run wrote NO textfile — the smoke's verdict is unobservable, which is the whole defect"
fi
if [[ "$(metric "$OUT" stellarindex_api_smoke_failures)" == "0" ]]; then
  ok "clean run recorded failures=0"
else
  bad "clean run recorded failures='$(metric "$OUT" stellarindex_api_smoke_failures)', want 0"
fi
stamp="$(metric "$OUT" stellarindex_api_smoke_last_run_unix)"
if [[ -n "$stamp" ]] && [[ "$stamp" -ge "$now" ]]; then
  ok "clean run stamped last_run_unix ($stamp)"
else
  bad "clean run stamped last_run_unix='$stamp' (want >= $now) — stellarindex_api_smoke_stale would fire on a healthy host"
fi
if [[ "$rc" == "0" ]]; then
  ok "wrapper exits 0 for the timer"
else
  bad "wrapper exited $rc, want 0 (same contract as heartbeat.sh)"
fi

# ─── 2. a failing run reports the count AND keeps stamping ───────────
echo "smoke-textfile-test: failing run"
DIR="$WORK/tf-fail"
rc="$(run_smoke 3 "$DIR")"
OUT="$DIR/api_smoke.prom"
if [[ "$(metric "$OUT" stellarindex_api_smoke_failures)" == "3" ]]; then
  ok "failing run recorded the smoke's failed-check count (3)"
else
  bad "failing run recorded failures='$(metric "$OUT" stellarindex_api_smoke_failures)', want 3"
fi
if [[ -n "$(metric "$OUT" stellarindex_api_smoke_last_run_unix)" ]]; then
  ok "failing run still stamped last_run_unix ('failing' and 'not running' stay distinct)"
else
  bad "failing run dropped last_run_unix — a failing smoke would masquerade as a dead timer"
fi
if [[ "$rc" == "0" ]]; then
  ok "wrapper still exits 0 on a failing smoke"
else
  bad "wrapper exited $rc on a failing smoke, want 0"
fi
if grep -qF '/fail' "$WORK/pings" 2>/dev/null; then
  ok "failing run still pings the Healthchecks /fail endpoint"
else
  bad "failing run did not ping \${URL}/fail — the pre-existing sink regressed"
fi

# ─── 3. a broken install is a FAILED run, not a silent one ───────────
echo "smoke-textfile-test: missing smoke script"
DIR="$WORK/tf-missing"
SMOKE_SCRIPT="$WORK/does-not-exist.sh" TEXTFILE_DIR="$DIR" \
  HEALTHCHECKS_URL_SMOKE="" bash "$SRC" >/dev/null 2>&1
rc=$?
OUT="$DIR/api_smoke.prom"
failures="$(metric "$OUT" stellarindex_api_smoke_failures)"
if [[ -n "$failures" ]] && [[ "$failures" -gt 0 ]]; then
  ok "a non-executable smoke script records failures=$failures"
else
  bad "a non-executable smoke script recorded failures='$failures' — an absent/zero write lets node_exporter keep serving a stale pass for a check that cannot run"
fi
if [[ "$rc" == "0" ]]; then
  ok "wrapper exits 0 when the smoke script is missing"
else
  bad "wrapper exited $rc with a missing smoke script, want 0"
fi

# ─── 4. atomic + node_exporter-readable ──────────────────────────────
echo "smoke-textfile-test: write discipline"
DIR="$WORK/tf-clean"
OUT="$DIR/api_smoke.prom"
# Guarded on the file existing: "there is no textfile" must not satisfy
# "no leftover .tmp" — an emptiness that passes is the vacuous green
# this suite exists to prevent.
if [[ ! -f "$OUT" ]]; then
  bad "no textfile to inspect — the write-discipline checks below cannot pass vacuously"
else
  if [[ -z "$(find "$DIR" -name 'api_smoke.prom.tmp.*' -print -quit 2>/dev/null)" ]]; then
    ok "no .tmp file survives the write (tmp + mv, never a partial read)"
  else
    bad "a .tmp file survived — node_exporter can scrape a half-written textfile"
  fi
  # -perm -044 asserts the property that matters rather than an exact
  # mode string: node_exporter runs as another user and silently skips a
  # textfile it cannot read, which would look identical to a check that
  # never ran.
  if [[ -n "$(find "$DIR" -maxdepth 1 -name 'api_smoke.prom' -perm -044 -print -quit 2>/dev/null)" ]]; then
    ok "textfile is group- and world-readable (the unprivileged node_exporter can read it)"
  else
    bad "textfile is not readable beyond its owner — node_exporter skips unreadable files silently"
  fi
  for m in stellarindex_api_smoke_failures stellarindex_api_smoke_last_run_unix; do
    if grep -q "^# TYPE $m gauge$" "$OUT" 2>/dev/null; then
      ok "$m carries a TYPE header"
    else
      bad "$m has no '# TYPE … gauge' header"
    fi
  done
fi

# ─── 5. an unwritable textfile dir must not break the timer ──────────
echo "smoke-textfile-test: unwritable textfile directory"
DIR="$WORK/tf-readonly"
mkdir -p "$DIR"
chmod 0555 "$DIR"
rc="$(run_smoke 0 "$DIR/nested")"
chmod 0755 "$DIR"
if [[ "$rc" == "0" ]]; then
  ok "wrapper exits 0 when it cannot write the textfile (the ping leg still runs)"
else
  bad "wrapper exited $rc on an unwritable textfile dir, want 0 — metric emission must never break the check"
fi

echo
echo "smoke-textfile-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]] || exit 1
