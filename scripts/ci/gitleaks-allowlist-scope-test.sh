#!/usr/bin/env bash
# gitleaks-allowlist-scope-test.sh — pins the scope of the secret-scan
# exemptions in .gitleaks.toml and .gitleaksignore against the gitleaks
# binary that actually runs them.
#
#   1. No tracked file is exempted wholesale unless it is a generated or
#      spec artefact listed in ARTEFACTS below. A credential is planted in
#      every tracked file a `paths` entry matches, and gitleaks must report
#      it. A path-only allowlist on a test file blinds every rule to every
#      line of it forever; the fixture shape belongs in a `regexes` entry.
#   2. The shape allowlists still clear what they exist for (a public
#      strkey, a base64 XDR fixture in a Go test) and do not clear a
#      Stellar secret seed or the same XDR shape outside a Go test.
#   3. Every .gitleaksignore fingerprint names a commit reachable from HEAD.
#      A fingerprint for a rewritten or side-branch commit exempts nothing
#      and hides that the finding it was written for has moved.
#
# GITLEAKS_HISTORY_SOURCE points check 3 at a clone with real history when
# the working directory is a synthetic single-commit tree (the pre-push
# container). Without history, check 3 says so and is not counted as passed.
#
# Run: bash scripts/ci/gitleaks-allowlist-scope-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROOT="$PWD"
HISTORY="${GITLEAKS_HISTORY_SOURCE:-$ROOT}"

for tool in gitleaks git python3; do
  command -v "$tool" >/dev/null || { echo "FAIL: $tool not on PATH" >&2; exit 1; }
done

# Generated or spec files whose example values are key-shaped by design, so
# no value regex can separate an example from a real key of the same shape.
ARTEFACTS=(
  openapi/stellar-index.v1.yaml
  docs/reference/api/stellar-index.v1.yaml
  docs/reference/api/index.html
  web/explorer/src/api/types.ts
)

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); }
bad() { echo "FAIL: $*" >&2; fail=$((fail + 1)); }

rand() { python3 -c "import secrets,sys; print(''.join(secrets.choice(sys.argv[1]) for _ in range(int(sys.argv[2]))))" "$1" "$2"; }
ALNUM=ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789
B32=ABCDEFGHIJKLMNOPQRSTUVWXYZ234567

# reportable <prefix> <alphabet> <len>: a random value that gitleaks' default
# rules, with none of our allowlists, report whole. A draw can contain a
# generic-api-key stopword ("md5"), and the too-wide checks below would then
# fail on the value, not the config. The XDR probe uses base64's alphanumeric
# subset because generic-api-key's capture stops at + and /.
reportable() {
  local v dir="$TMP/control"
  mkdir -p "$dir"
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    v="$1$(rand "$2" "$3")"
    printf 'secretKey := "%s"\n' "$v" >"$dir/probe.go"
    (cd "$dir" && gitleaks detect --no-git --source . --no-banner \
      --report-format json --report-path "$TMP/control.json" >/dev/null 2>&1)
    if reported "$TMP/control.json" probe.go "$v"; then
      echo "$v"
      return 0
    fi
  done
  echo "reportable: no reportable $1 value in 10 draws" >&2
  return 1
}

# plant <tree> <relpath> <line>: copy the tracked file (if any) into the
# scan tree and append <line> to it.
plant() {
  local dst="$1/$2"
  mkdir -p "$(dirname "$dst")"
  if [ -f "$ROOT/$2" ]; then cp "$ROOT/$2" "$dst"; else : >"$dst"; fi
  printf '\n%s\n' "$3" >>"$dst"
}

# scan <tree> <report>: working-tree scan with the repo config, from inside
# the tree so reported paths are repo-relative exactly as in a history scan.
scan() {
  (cd "$1" && gitleaks detect --no-git --source . --no-banner \
    --config "$ROOT/.gitleaks.toml" --report-format json --report-path "$2" >"$2.log" 2>&1)
  local rc=$?
  if [ "$rc" -gt 1 ] || [ ! -s "$2" ]; then
    bad "gitleaks did not run cleanly (exit $rc):"
    tail -5 "$2.log" >&2
    return 1
  fi
}

# reported <report> <relpath> <value>: exit 0 iff gitleaks reported <value>
# in <relpath>.
reported() {
  python3 - "$@" <<'EOF'
import json, sys
report, path, value = sys.argv[1:]
hits = [f for f in json.load(open(report)) if f["File"] == path and value in f["Secret"]]
sys.exit(0 if hits else 1)
EOF
}

# Values are generated per run so this file never carries a scannable literal.
canary="$(reportable "" "$ALNUM" 40)" || exit 1
seed="$(reportable S "$B32" 55)" || exit 1
strkey="G$(rand "$B32" 55)"
xdr="$(reportable AAAA "$ALNUM" 60)" || exit 1

# ── 1. every tracked file a path-only exemption matches still reports a key
# An allowlist exempts on its paths alone unless condition = "AND" pairs them
# with a value regex; only those path-alone entries can blank a file.
python3 - "$ROOT/.gitleaks.toml" >"$TMP/patterns" <<'EOF'
import sys, tomllib
cfg = tomllib.load(open(sys.argv[1], "rb"))
lists = [cfg.get("allowlist", {})] + [a for r in cfg.get("rules", []) for a in r.get("allowlists", [])]
for a in lists:
    if a.get("condition", "OR").upper() == "AND" and (a.get("regexes") or a.get("stopwords")):
        continue
    for p in a.get("paths", []):
        print(p)
EOF
git ls-files >"$TMP/tracked"
python3 - "$TMP/patterns" "$TMP/tracked" >"$TMP/planted" <<'EOF'
import re, sys
pats = [re.compile(p) for p in open(sys.argv[1]).read().splitlines()]
for f in open(sys.argv[2]).read().splitlines():
    if any(p.search(f) for p in pats):
        print(f)
EOF
[ -s "$TMP/planted" ] || bad "no tracked file matched any paths entry; the enumeration is broken"
tree1="$TMP/tree1"
while IFS= read -r f; do
  plant "$tree1" "$f" "apiKey := \"$canary\""
done <"$TMP/planted"
if scan "$tree1" "$TMP/r1.json"; then
  while IFS= read -r f; do
    artefact=no
    for a in "${ARTEFACTS[@]}"; do [ "$f" = "$a" ] && artefact=yes; done
    if reported "$TMP/r1.json" "$f" "$canary"; then
      if [ "$artefact" = yes ]; then
        bad "$f is listed as an artefact but is no longer exempted wholesale; drop it from ARTEFACTS"
      else
        ok
      fi
    elif [ "$artefact" = yes ]; then
      ok
    else
      bad "a credential planted in $f is not reported: .gitleaks.toml exempts the whole file"
    fi
  done <"$TMP/planted"
fi

# ── 2. the shape allowlists clear their shape, and only their shape
expect_clear() {
  if reported "$1" "internal/allowlistprobe/$2" "$3"; then bad "$4 is reported; its allowlist no longer matches"; else ok; fi
}
expect_report() {
  if reported "$1" "internal/allowlistprobe/$2" "$3"; then ok; else bad "$4 is NOT reported; an allowlist is too wide"; fi
}
tree2="$TMP/tree2"
plant "$tree2" internal/allowlistprobe/probe_test.go "$(printf 'issuerKey := "%s"\nsecretKey := "%s"\ndataKey := "%s"' "$strkey" "$seed" "$xdr")"
plant "$tree2" internal/allowlistprobe/probe.go "dataKey := \"$xdr\""
if scan "$tree2" "$TMP/r2.json"; then
  expect_clear "$TMP/r2.json" probe_test.go "$strkey" "a public G-strkey"
  expect_report "$TMP/r2.json" probe_test.go "$seed" "a Stellar secret seed (S...)"
  expect_clear "$TMP/r2.json" probe_test.go "$xdr" "a base64 XDR fixture in a Go test"
  expect_report "$TMP/r2.json" probe.go "$xdr" "an AAAA-prefixed base64 value outside *_test.go"
fi

# ── 3. every .gitleaksignore fingerprint is reachable from HEAD
if [ "$(git -C "$HISTORY" rev-parse --is-shallow-repository)" = true ] ||
  [ "$(git -C "$HISTORY" rev-list --count HEAD)" -le 1 ]; then
  echo "NOTE: $HISTORY carries no history; .gitleaksignore reachability NOT checked (set GITLEAKS_HISTORY_SOURCE)" >&2
else
  while IFS= read -r line; do
    case "$line" in '' | '#'*) continue ;; esac
    sha="${line%%:*}"
    if git -C "$HISTORY" merge-base --is-ancestor "$sha" HEAD 2>/dev/null; then
      ok
    else
      bad ".gitleaksignore fingerprint names $sha, which is not a commit reachable from HEAD: $line"
    fi
  done <"$ROOT/.gitleaksignore"
fi

echo "gitleaks-allowlist-scope-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
