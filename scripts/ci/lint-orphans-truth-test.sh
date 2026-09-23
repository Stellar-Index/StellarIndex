#!/usr/bin/env bash
# lint-orphans-truth-test.sh — regression for T235: deploy/systemd/ORPHANS
# claiming an ExecStart script "does not exist anywhere in this repo" when
# it does (scripts/ops/completeness-incremental.sh). An ORPHANS reason that
# asserts non-existence for a script that is actually live and referenced
# elsewhere (docs/operations/sep41-completeness-diagnosis-2026-07-25.md)
# misleads an operator deciding whether the unit is safe to delete.
#
# check <orphans-file> — for every "/usr/local/(bin|sbin)/<name>" mentioned
# alongside "does not exist anywhere in this repo", fails if a script named
# <name> is actually found under scripts/.
#
# Run: bash scripts/ci/lint-orphans-truth-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

check() {
  local file="$1"
  local bad=0
  local joined
  # Comment lines wrap, so "does not exist anywhere in this repo" and the
  # path it refers to can land on different lines — join into one blob
  # before matching.
  joined="$(sed -E 's/^#\s?//' "$file" | tr '\n' ' ')"
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    local hits
    hits="$(find scripts -iname "$name" 2>/dev/null)"
    if [ -n "$hits" ]; then
      echo "  FALSE CLAIM: $file says $name does not exist, but it does (scripts/**)"
      bad=1
    fi
  done < <(grep -oE '/usr/local/s?bin/[A-Za-z0-9_.-]+[^ ]* which does not exist +anywhere in this repo' <<<"$joined" \
    | grep -oE '/usr/local/s?bin/[A-Za-z0-9_.-]+' \
    | xargs -n1 basename 2>/dev/null)
  return "$bad"
}

pass=0
fail=0

# expect <name> <file> <want-rc>
expect() {
  local name="$1" file="$2" want_rc="$3"
  local out rc
  out="$(check "$file")"
  rc=$?
  if [ "$rc" -ne "$want_rc" ]; then
    echo "FAIL: $name — rc $rc, want $want_rc" >&2
    printf '%s\n' "$out" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── 1. The defect as it shipped: false "does not exist" claim ────────
cat > "$TMP/orphans-stale" <<'EOF'
# Superseded, not deployed. ExecStart is
# /usr/local/bin/completeness-incremental.sh, which does not exist
# anywhere in this repo — the role ships compute-completeness.{service,
# timer}.j2 for the same job instead.
stellarindex-completeness.service
stellarindex-completeness.timer
EOF
expect "stale ORPHANS reason caught as false" "$TMP/orphans-stale" 1

# ── 2. The fixed reason: no false-existence claim ─────────────────────
expect "current ORPHANS carries no false-existence claim" "deploy/systemd/ORPHANS" 0

pass_total=$pass
fail_total=$fail
echo "lint-orphans-truth-test: $pass_total passed, $fail_total failed"
[ "$fail_total" -eq 0 ]
