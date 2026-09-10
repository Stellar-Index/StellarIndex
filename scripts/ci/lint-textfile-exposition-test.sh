#!/usr/bin/env bash
# lint-textfile-exposition-test.sh — fixtures for the textfile-exposition
# gate. A gate nobody has seen red is not a gate.
#
# THE LOAD-BEARING CASE is the first one below: the r1 2026-09-10 probe,
# reproduced as it was written before b96982c22. It ran
#
#     q "SET statement_timeout = '10s';
#        SELECT count(*), …"
#
# through `psql -At -F'|'`. psql prints the SET's command tag — the bare
# word `SET` — as its own stdout line, `-At` does not suppress it, and the
# probe's `while IFS='|' read -r convoyed worst blocked` read it as data:
#
#     stellarindex_pg_lock_convoy_backends SET
#
# node_exporter rejected the whole file, taking 127 unrelated
# stellarindex_timescale_* series off the host with it. The pre-fix form
# must be RED here and the shipped single-statement form GREEN, or this
# gate is decoration.
#
# The other fixtures pin the rest of the contract, INCLUDING the cases that
# must stay green: a lint that cries wolf on correct work is a lint someone
# disables, so the mutually-exclusive-branch shape that two real producers
# use (ch-schema-drift.sh, internal/supply/textfile.go) is asserted clean.
#
# Hermetic: fixture trees in $TMPDIR, no network, no Postgres, no host. The
# final check runs the gate against the REAL tree and asserts it is clean,
# so a fixture-only pass cannot hide a broken sweep.
#
# Run: bash scripts/ci/lint-textfile-exposition-test.sh
#
# shellcheck disable=SC2016
# Single quotes are load-bearing in nearly every fixture below: this file
# WRITES shell scripts, so `${count:-0}` and `$kind` must reach them
# unexpanded. Expanding here would test the gate against a rendered line
# instead of the template it actually reads — which is the same mistake,
# in miniature, that let 2026-09-10 ship green.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-textfile-exposition.sh"
[ -x "$GATE" ] || { echo "lint-textfile-exposition-test: missing $GATE" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || {
  echo "lint-textfile-exposition-test: python3 is required — refusing to pass vacuously" >&2
  exit 2
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── fixture harness ────────────────────────────────────────────────
#
# `if/then/else`, not `A && ok "…" || bad "…"`: the third arm of that
# idiom runs whenever the second one fails (SC2015), and CI's
# changed-script shellcheck gate is clean-or-nothing. Same shape as
# timescale-jobs-probe-test.sh.

# new_tree <name> — a fixture root with the scanned dir layout; echoes it.
new_tree() {
  local root="$TMP/$1"
  rm -rf "$root"
  mkdir -p "$root/configs/ansible/roles/fixture/files"
  echo "$root"
}

# run_gate <root> <manifest> — runs the gate against a fixture tree;
# leaves stderr+stdout in $OUT and the exit code in $RC.
run_gate() {
  OUT="$(TEXTFILE_LINT_ROOT="$1" TEXTFILE_LINT_MANIFEST="$2" bash "$GATE" 2>&1)"
  RC=$?
}

# expect <label> <red|green> — assert on the last run_gate.
expect() {
  case "$2" in
    red)   if [ "$RC" -gt 0 ]; then ok "$1"; else bad "$1 (gate passed; expected a finding)"; fi ;;
    green) if [ "$RC" -eq 0 ]; then ok "$1"; else bad "$1 (rc=$RC)
$(printf '%s\n' "$OUT" | sed 's/^/        /')"; fi ;;
  esac
}

# says <label> <substring> — assert the diagnostic names the real problem,
# not merely that something failed.
# Here-strings rather than `printf … | grep -q`: grep -q exits on the first
# match, and under pipefail that SIGPIPEs the writer (lint-shell-sigpipe).
says() {
  if grep -qF -- "$2" <<<"$OUT"; then ok "$1"; else bad "$1 (diagnostic did not mention '$2')"; fi
}

# ─── 1. the r1 2026-09-10 probe, pre-fix and post-fix ────────────────
#
# Both variants below are byte-faithful excerpts of the shipped probe: the
# same q() helper, the same psql invocation, the same read loop. Only the
# SQL differs, which is the whole point — the emit line is IDENTICAL in
# both, so nothing about the line itself could have told them apart.
convoy_probe() { # convoy_probe <root> <sql-prefix>
  cat > "$1/configs/ansible/roles/fixture/files/convoy-probe.sh" <<SH
#!/usr/bin/env bash
set -euo pipefail
DIR="\${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
TMP=\$(mktemp)
Q_OUT=""
q() {
  Q_OUT=\$(runuser -u postgres -- psql -d stellarindex -At -F'|' -c "\$1" 2>/dev/null) || return 0
}
q "${2}SELECT count(*),
          COALESCE(max(EXTRACT(epoch FROM clock_timestamp() - a.query_start))::bigint, 0),
          (SELECT count(*) FROM pg_stat_activity w WHERE w.wait_event_type = 'Lock')
     FROM pg_stat_activity a
    WHERE a.wait_event_type = 'Lock'"
{
  echo "# HELP stellarindex_pg_lock_convoy_backends Backends waiting on a heavyweight lock whose blocker is ITSELF waiting."
  echo "# TYPE stellarindex_pg_lock_convoy_backends gauge"
} > "\$TMP"
while IFS='|' read -r convoyed worst blocked; do
  [ -n "\${convoyed:-}" ] || continue
  echo "stellarindex_pg_lock_convoy_backends \${convoyed:-0}"
done <<< "\$Q_OUT" >> "\$TMP"
mv "\$TMP" "\$DIR/timescale_jobs.prom"
SH
  cat > "$1/manifest" <<'MAN'
configs/ansible/roles/fixture/files/convoy-probe.sh  timescale_jobs.prom  -
MAN
}

root="$(new_tree prefix)"
convoy_probe "$root" "SET statement_timeout = '10s';
     "
run_gate "$root" "$root/manifest"
expect "the pre-fix probe (SET; SELECT through psql -At) is RED" red
says   "…and the diagnostic names the statement count" "2 SQL statements in one command string"
says   "…and points at the remedy that was actually used" "PGOPTIONS"

root="$(new_tree postfix)"
convoy_probe "$root" ""
run_gate "$root" "$root/manifest"
expect "the shipped single-statement form is GREEN" green

# A second data-returning statement is the same defect wearing a SELECT:
# `-q` would hide the command tag but not these rows.
root="$(new_tree twoselect)"
convoy_probe "$root" "SELECT pg_sleep(0);
     "
run_gate "$root" "$root/manifest"
expect "two SELECTs in one command string are RED (a -q remedy would not help)" red

# ─── 2. non-vacuity ─────────────────────────────────────────────────
root="$(new_tree empty)"
: > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "a tree with no producers FAILS rather than passing on nothing" red
says   "…and says so" "discovered NO textfile-collector producers"

# ─── 3. census parity, in both directions and column by column ──────
# A well-formed producer, used as the base for the manifest fixtures.
good_producer() { # good_producer <root>
  cat > "$1/configs/ansible/roles/fixture/files/good.sh" <<'SH'
#!/usr/bin/env bash
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
TMP=$(mktemp)
{
  echo "# HELP stellarindex_fixture_widgets Widgets observed."
  echo "# TYPE stellarindex_fixture_widgets gauge"
  echo "stellarindex_fixture_widgets{kind=\"$kind\"} ${count:-0}"
} > "$TMP"
mv "$TMP" "$DIR/fixture.prom"
SH
}

root="$(new_tree manifest)"; good_producer "$root"
: > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "an unregistered producer is RED" red
says   "…and names it as new" "NEW textfile-collector producer"

printf 'configs/ansible/roles/fixture/files/good.sh fixture.prom -\n' > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "a registered, well-formed producer is GREEN" green

printf 'configs/ansible/roles/fixture/files/good.sh fixture.prom -\nconfigs/ansible/roles/fixture/files/gone.sh gone.prom -\n' > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "a manifest row whose producer vanished is RED" red

printf 'configs/ansible/roles/fixture/files/good.sh renamed.prom -\n' > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "an output column that does not match the producer is RED" red
says   "…and says which name it could not find" "renamed.prom"

printf 'configs/ansible/roles/fixture/files/good.sh fixture.prom scripts/ci/no-such-test.sh\n' > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "a self-test column pointing at a missing file is RED" red

printf 'configs/ansible/roles/fixture/files/good.sh\n' > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "a manifest row missing its columns is RED" red

# ─── 4. exposition grammar ──────────────────────────────────────────
# emit <root> <line> — a producer whose only sample line is <line>.
#
emit() {
  { printf '#!/usr/bin/env bash\nDIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"\n'
    printf 'echo "# HELP stellarindex_fixture_widgets Widgets observed."\n'
    printf 'echo "# TYPE stellarindex_fixture_widgets gauge"\n'
    printf 'echo "%s"\n' "$2"
    printf 'mv "$TMP" "$DIR/fixture.prom"\n'
  } > "$1/configs/ansible/roles/fixture/files/good.sh"
  printf 'configs/ansible/roles/fixture/files/good.sh fixture.prom -\n' > "$1/manifest"
}

root="$(new_tree grammar)"

emit "$root" 'stellarindex_fixture_widgets ${count:-0}'
run_gate "$root" "$root/manifest"
expect "a plain numeric-guarded value is GREEN" green

emit "$root" 'stellarindex_fixture_widgets'
run_gate "$root" "$root/manifest"
expect "a bare metric name with no value is RED" red
says   "…and says the value is missing" "no value field"

emit "$root" 'stellarindex_fixture_widgets SET'
run_gate "$root" "$root/manifest"
expect "the literal line the incident produced is RED" red
says   "…and names the offending value" "'SET' is not a float"

emit "$root" 'stellarindex_fixture_widgets 3 4 5'
run_gate "$root" "$root/manifest"
expect "extra fields after the value are RED" red

emit "$root" 'stellarindex_fixture_widgets{kind=$kind} 1'
run_gate "$root" "$root/manifest"
expect "an unquoted label value is RED" red
says   "…and says the label value is unquoted" "is not a quoted string"

emit "$root" 'stellarindex_fixture_widgets{kind="$kind" 1'
run_gate "$root" "$root/manifest"
expect "an unclosed label brace is RED" red

emit "$root" 'stellarindex_fixture_other 1'
run_gate "$root" "$root/manifest"
expect "a family emitted with no HELP/TYPE of its own is RED" red

# Go's %q renders its own quotes; %s does not. Getting this wrong in the
# canary would call every correct Go emitter a violation.
mkdir -p "$root/internal/fixture"
cat > "$root/internal/fixture/textfile.go" <<'GO'
package fixture

// writes into node_exporter's textfile collector dir
func write(w io.Writer, ep string, v float64) {
	fmt.Fprintf(w, "# HELP stellarindex_fixture_latency Latency.\n")
	fmt.Fprintf(w, "# TYPE stellarindex_fixture_latency gauge\n")
	fmt.Fprintf(w, "stellarindex_fixture_latency{endpoint=%q} %.3f\n", ep, v)
}
GO
rm -f "$root/configs/ansible/roles/fixture/files/good.sh"
printf 'internal/fixture/textfile.go operator-supplied -\n' > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "a Go emitter quoting its label with %q is GREEN" green

sed -i.bak 's/endpoint=%q/endpoint=%s/' "$root/internal/fixture/textfile.go"
rm -f "$root/internal/fixture/textfile.go.bak"
run_gate "$root" "$root/manifest"
expect "a Go emitter quoting its label with %s is RED" red

# ─── 5. headers ─────────────────────────────────────────────────────
root="$(new_tree headers)"
cat > "$root/configs/ansible/roles/fixture/files/good.sh" <<'SH'
#!/usr/bin/env bash
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
echo "# HELP stellarindex_fixture_widgets Widgets observed."
echo "# TYPE stellarindex_fixture_widgets counterr"
echo "stellarindex_fixture_widgets 1"
mv "$TMP" "$DIR/fixture.prom"
SH
printf 'configs/ansible/roles/fixture/files/good.sh fixture.prom -\n' > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "a misspelled metric type is RED" red

cat > "$root/configs/ansible/roles/fixture/files/good.sh" <<'SH'
#!/usr/bin/env bash
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
echo "# HELP stellarindex_fixture_widgets Widgets observed."
echo "# TYPE stellarindex_fixture_widgets gauge"
echo "stellarindex_fixture_widgets 1"
echo "# HELP stellarindex_fixture_widgets Widgets observed."
echo "# TYPE stellarindex_fixture_widgets gauge"
echo "stellarindex_fixture_widgets 2"
mv "$TMP" "$DIR/fixture.prom"
SH
run_gate "$root" "$root/manifest"
expect "one family typed twice in the same unconditional run is RED" red
says   "…and says the file is lost, not the line" "rejects the whole file"

# The anti-cry-wolf case: two MUTUALLY EXCLUSIVE paths rendering the same
# .prom, each declaring the family once. ch-schema-drift.sh and
# internal/supply/textfile.go both do this, correctly.
cat > "$root/configs/ansible/roles/fixture/files/good.sh" <<'SH'
#!/usr/bin/env bash
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
if [ "${refused:-0}" = 1 ]; then
  echo "# HELP stellarindex_fixture_widgets Widgets observed."
  echo "# TYPE stellarindex_fixture_widgets gauge"
  echo "stellarindex_fixture_widgets 0"
else
  echo "# HELP stellarindex_fixture_widgets Widgets observed."
  echo "# TYPE stellarindex_fixture_widgets gauge"
  echo "stellarindex_fixture_widgets ${count:-0}"
fi
mv "$TMP" "$DIR/fixture.prom"
SH
run_gate "$root" "$root/manifest"
expect "the same family declared once per mutually-exclusive branch is GREEN" green

# ─── 6. what is exposition text and what is prose ───────────────────
#
# A heredoc body IS the file (galexie-archive-tip-lag.sh writes its whole
# textfile that way), so its lines must be checked. A shell comment that
# happens to begin "# TYPE …" is prose and must NOT be — reading one as a
# header is the cry-wolf failure that gets a lint disabled, and this gate
# committed it once during its own construction.
root="$(new_tree heredoc)"
cat > "$root/configs/ansible/roles/fixture/files/good.sh" <<'SH'
#!/usr/bin/env bash
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
# TYPE is what node_exporter calls the header line; this sentence is not one.
# HELP neither is this.
cat > "$TMP" <<EOF
# HELP stellarindex_fixture_widgets Widgets observed.
# TYPE stellarindex_fixture_widgets gauge
stellarindex_fixture_widgets ${count:-0}
EOF
mv "$TMP" "$DIR/fixture.prom"
SH
printf 'configs/ansible/roles/fixture/files/good.sh fixture.prom -\n' > "$root/manifest"
run_gate "$root" "$root/manifest"
expect "prose beginning '# TYPE' is not read as a header" green

sed -i.bak 's/^stellarindex_fixture_widgets .*$/stellarindex_fixture_widgets SET/' \
  "$root/configs/ansible/roles/fixture/files/good.sh"
rm -f "$root/configs/ansible/roles/fixture/files/good.sh.bak"
run_gate "$root" "$root/manifest"
expect "a bad line inside a heredoc body IS still caught" red

# ─── 7. exposition composed inside SQL ──────────────────────────────
#
# data-freshness.sh redirects psql's stdout into the textfile, so the
# exposition text is concatenated in SQL and nothing in the shell looks
# like a metric line. That producer has the largest exposure to this whole
# class and would otherwise be swept by nothing.
root="$(new_tree sqlcompose)"
sql_producer() { # sql_producer <root> <value-expression>
  cat > "$1/configs/ansible/roles/fixture/files/freshness.sh" <<SH
#!/usr/bin/env bash
OUT="\${TEXTFILE_OUTPUT:-/var/lib/node_exporter/textfile_collector/fixture.prom}"
TMP="\$(mktemp)"
{
  echo '# HELP stellarindex_fixture_age_seconds Seconds since the newest row.'
  echo '# TYPE stellarindex_fixture_age_seconds gauge'
} > "\$TMP"
psql "\$DSN" -tA -F\$'\t' >> "\$TMP" <<'SQL'
SELECT 'stellarindex_fixture_age_seconds{source="'||src||'"} '${2}
  FROM f;
SQL
mv "\$TMP" "\$OUT"
SH
  printf 'configs/ansible/roles/fixture/files/freshness.sh fixture.prom -\n' > "$1/manifest"
}

sql_producer "$root" "||round(age)::text"
run_gate "$root" "$root/manifest"
expect "a well-formed SQL-composed line is GREEN" green

# The space before the value is load-bearing and lives inside a string
# literal, where no shell-level check would ever look at it.
sql_producer "$root" "||round(age)::text"
sed -i.bak 's/"} .||round/"}||round/' "$root/configs/ansible/roles/fixture/files/freshness.sh"
rm -f "$root/configs/ansible/roles/fixture/files/freshness.sh.bak"
run_gate "$root" "$root/manifest"
expect "an SQL-composed line with no space before its value is RED" red

# ─── 8. the real tree ───────────────────────────────────────────────
# Fixtures prove the gate can fail; this proves the sweep it actually runs
# is clean, so neither result can be inferred from the other.
OUT="$(bash "$GATE" 2>&1)"; RC=$?
expect "the real tree is clean" green
if grep -qE 'producers checked \([0-9]+ in manifest\), [1-9][0-9]* emitted metric lines' <<<"$OUT"; then
  ok "the real run reports a non-zero line count (a silent zero-work run is not a pass)"
else
  bad "the real run did not report work done: $OUT"
fi

printf 'lint-textfile-exposition-test: %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
