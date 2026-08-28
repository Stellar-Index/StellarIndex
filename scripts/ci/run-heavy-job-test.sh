#!/usr/bin/env bash
# run-heavy-job-test.sh — fixture tests for the heavy-job wrapper's
# ClickHouse ops-batch identity import (2026-08-28 r1).
#
# The wrapper lives INSIDE ansible
# (configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml,
# the /usr/local/sbin/run-heavy-job.sh copy task); this extracts that
# exact content and runs it, so the property under test is the shipped
# script, not a hand-copied twin.
#
# What must hold — each was the silent-no-op the profile shipped with
# the first time round:
#
#   1. a job launched from a shell that did NOT source
#      /etc/default/stellarindex-ops (every runbook sources
#      /etc/default/stellarindex instead) still receives
#      STELLARINDEX_CLICKHOUSE_OPS_USER/_PASSWORD, read VERBATIM from
#      the -ops file (passwords contain `=`, `$`, quotes, spaces);
#   2. ONLY that pair is imported — the caller's AWS_*/DSN choice must
#      not be overridden by the -ops file's;
#   3. a value the caller already exported wins;
#   4. with no pair available the wrapper WARNS on stderr that the job
#      runs as CH `default` at serving priority — never quietly.
#
# Runs the wrapper's non-root exec path (no systemd-run / flock needed:
# flock is stubbed on PATH so this runs on macOS too).
#
# Run: bash scripts/ci/run-heavy-job-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="$PWD/configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml"
[[ -r "$TASKS" ]] || { echo "run-heavy-job-test: missing $TASKS" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── extract the shipped wrapper from the ansible task ──────────────
python3 - "$TASKS" "$TMP/run-heavy-job.sh" <<'PY' || { echo "run-heavy-job-test: could not extract wrapper" >&2; exit 2; }
import sys, yaml
tasks = yaml.safe_load(open(sys.argv[1]))
for t in tasks:
    c = t.get("ansible.builtin.copy") or {}
    if c.get("dest") == "/usr/local/sbin/run-heavy-job.sh":
        open(sys.argv[2], "w").write(c["content"])
        sys.exit(0)
sys.exit(1)
PY
chmod +x "$TMP/run-heavy-job.sh"
WRAP="$TMP/run-heavy-job.sh"

mkdir -p "$TMP/bin" "$TMP/lock"
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/bin/flock"   # macOS has no flock
chmod +x "$TMP/bin/flock"
export PATH="$TMP/bin:$PATH"
export HEAVY_JOB_LOCK_DIR="$TMP/lock"

# The payload prints exactly what the wrapped job would see.
PAYLOAD="$TMP/payload.sh"
cat > "$PAYLOAD" <<'SH'
#!/usr/bin/env bash
printf 'USER=%s\n' "${STELLARINDEX_CLICKHOUSE_OPS_USER-<unset>}"
printf 'PASS=%s\n' "${STELLARINDEX_CLICKHOUSE_OPS_PASSWORD-<unset>}"
printf 'AWS=%s\n'  "${AWS_ACCESS_KEY_ID-<unset>}"
SH
chmod +x "$PAYLOAD"

# A realistic /etc/default/stellarindex-ops: comments, other secrets,
# and a password with every character class a shell would mangle.
PASSWORD="p=a\$s\"s w0rd'#x="
OPS_ENV="$TMP/stellarindex-ops"
cat > "$OPS_ENV" <<EOT
# Rendered by Ansible. Do not edit by hand.
AWS_ACCESS_KEY_ID=ops-reader-key
AWS_SECRET_ACCESS_KEY=ops-reader-secret
STELLARINDEX_POSTGRES_DSN=postgres://x:y@127.0.0.1:5432/z
STELLARINDEX_CLICKHOUSE_OPS_USER=ops_batch
STELLARINDEX_CLICKHOUSE_OPS_PASSWORD=$PASSWORD
EOT

run() { # run <extra env assignments...> — runs the wrapper, captures out/err
  env "$@" "$WRAP" test-job "$PAYLOAD" >"$TMP/out" 2>"$TMP/err"
}
out_has() { grep -qxF -- "$1" "$TMP/out"; }
err_has() { grep -qF -- "$1" "$TMP/err"; }

echo "run-heavy-job-test:"

# ── 1. runbook-shaped launch: caller sourced the SERVICE env, not -ops ─
run HEAVY_JOB_OPS_ENV="$OPS_ENV" AWS_ACCESS_KEY_ID=service-key
if out_has "USER=ops_batch"; then ok "pair imported: user"; else bad "pair imported: user ($(cat "$TMP/out"))"; fi
if out_has "PASS=$PASSWORD"; then ok "pair imported: password verbatim (=, \$, quotes, spaces, #)"; else bad "password verbatim ($(grep PASS= "$TMP/out"))"; fi
# ── 2. only the pair — the caller's own AWS identity survives ─────────
if out_has "AWS=service-key"; then ok "only the pair is imported (caller's AWS_* kept)"; else bad "caller's AWS_* overridden ($(grep AWS= "$TMP/out"))"; fi
if err_has "ClickHouse identity: ops_batch"; then ok "stderr names the identity"; else bad "stderr identity line missing ($(cat "$TMP/err"))"; fi
if ! err_has "CH 'default' user"; then ok "no identity WARNING when the pair is present"; else bad "spurious identity WARNING"; fi

# ── 3. caller's explicit value wins ──────────────────────────────────
run HEAVY_JOB_OPS_ENV="$OPS_ENV" STELLARINDEX_CLICKHOUSE_OPS_USER=custom STELLARINDEX_CLICKHOUSE_OPS_PASSWORD=custom-pw
if out_has "USER=custom" && out_has "PASS=custom-pw"; then ok "caller-set pair wins over the file"; else bad "caller-set pair overridden ($(cat "$TMP/out"))"; fi

# ── 4. no pair anywhere → loud, not silent ───────────────────────────
run HEAVY_JOB_OPS_ENV="$TMP/does-not-exist"
if out_has "USER=<unset>"; then ok "missing file: job runs with no identity (pre-fix behaviour)"; else bad "missing file: unexpected identity ($(cat "$TMP/out"))"; fi
if err_has "WARNING" && err_has "CH 'default' user at SERVING priority"; then ok "missing file: WARNING on stderr"; else bad "missing file: no WARNING ($(cat "$TMP/err"))"; fi

printf 'AWS_ACCESS_KEY_ID=ops-reader-key\n' > "$TMP/stellarindex-ops-nopair"
run HEAVY_JOB_OPS_ENV="$TMP/stellarindex-ops-nopair"
if out_has "USER=<unset>" && err_has "CH 'default' user"; then ok "file without the pair (profile not applied): WARNING on stderr"; else bad "file without pair: no WARNING ($(cat "$TMP/err"))"; fi

echo "run-heavy-job-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
