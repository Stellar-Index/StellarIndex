#!/usr/bin/env bash
# Self-test for bootstrap-worktree.sh's web-app survey. Runs a copy of the
# script inside a throwaway tree so the verdict depends on the fixture's
# node_modules, never on this checkout's.
#
# The case that matters: node_modules present with every binary, but older
# than pnpm-lock.yaml. verify.sh fails on that and sends the operator to
# `make bootstrap-worktree`, so the survey must report it and reinstall.
#
# Run: bash scripts/dev/bootstrap-worktree-test.sh
set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bootstrap-worktree.sh"
pass=0
fail=0

ok() { echo "  ok   $1"; pass=$((pass + 1)); }
no() { echo "  FAIL $1"; echo "       $2"; fail=$((fail + 1)); }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/tree/scripts/dev" "$tmp/bin"
cp "$SCRIPT" "$tmp/tree/scripts/dev/bootstrap-worktree.sh"
: >"$tmp/tree/Makefile"

make_app() { # make_app <dir> <bin>
    d="$tmp/tree/$1"
    mkdir -p "$d/node_modules/.bin"
    printf '#!/bin/sh\n' >"$d/node_modules/.bin/$2"
    chmod +x "$d/node_modules/.bin/$2"
    : >"$d/pnpm-lock.yaml"
    : >"$d/node_modules/.modules.yaml"
    touch -t 202601010000 "$d/pnpm-lock.yaml"
    touch -t 202601020000 "$d/node_modules/.modules.yaml"
}
make_app web/explorer openapi-typescript
make_app web/status tsc

# Stands in for pnpm: records the call and rewrites .modules.yaml as a real
# install does.
cat >"$tmp/bin/pnpm" <<EOF
#!/bin/sh
echo "\$*" >>"$tmp/pnpm.log"
touch "\$2/node_modules/.modules.yaml"
EOF
chmod +x "$tmp/bin/pnpm"

run() { PATH="$tmp/bin:$PATH" bash "$tmp/tree/scripts/dev/bootstrap-worktree.sh" "$@" 2>&1; }
blocking() { awk '/blocking gaps:/ { print $3; exit }' <<<"$1"; }

# ── 1. current install: no web-app gap ──────────────────────────────────
fresh="$(run --check)"
if grep -q 'STALE' <<<"$fresh"; then
    no "fresh install reports no staleness" "$(grep 'STALE' <<<"$fresh")"
else
    ok "fresh install reports no staleness"
fi

# ── 2. lockfile newer than the last install: blocking gap ────────────────
touch -t 202601030000 "$tmp/tree/web/explorer/pnpm-lock.yaml"
stale="$(run --check)"
if grep -q 'MISSING   web/explorer .*STALE' <<<"$stale"; then
    ok "stale web/explorer is reported"
else
    no "stale web/explorer is reported" "$(grep 'web/explorer' <<<"$stale")"
fi
if grep -q 'MISSING   web/status' <<<"$stale"; then
    no "current web/status is not reported" "$(grep 'web/status' <<<"$stale")"
else
    ok "current web/status is not reported"
fi
if [ "$(blocking "$stale")" = "$(( $(blocking "$fresh") + 1 ))" ]; then
    ok "stale install counts as one blocking gap"
else
    no "stale install counts as one blocking gap" "fresh=$(blocking "$fresh") stale=$(blocking "$stale")"
fi

# ── 3. install mode reinstalls only the stale app ────────────────────────
installed="$(run)"
if [ "$(cat "$tmp/pnpm.log" 2>/dev/null)" = "--dir web/explorer install --frozen-lockfile" ]; then
    ok "install runs pnpm for web/explorer only"
else
    no "install runs pnpm for web/explorer only" "pnpm calls: $(cat "$tmp/pnpm.log" 2>/dev/null)"
fi
after="$(sed -n '/── after install/,$p' <<<"$installed")"
if grep -q 'ok        web/explorer' <<<"$after"; then
    ok "re-survey sees the refreshed install"
else
    no "re-survey sees the refreshed install" "$(grep 'web/explorer' <<<"$after")"
fi

echo ""
echo "bootstrap-worktree-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
