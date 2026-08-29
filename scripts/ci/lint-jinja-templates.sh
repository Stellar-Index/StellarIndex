#!/usr/bin/env bash
# Parse every ansible Jinja template (*.j2) the way ansible will.
#
# Why this exists (r1, 2026-08-29): pgbackrest-backup.sh.j2 contained the
# bash array-length idiom (dollar, brace, hash). In Jinja, brace-hash opens
# a COMMENT, so the template failed to render with "Missing end of comment
# tag" — and NOTHING in CI caught it: `ansible-playbook --syntax-check`
# parses playbooks/roles, never template BODIES, and the wrapper's own
# self-test runs the raw file. The nightly pgBackRest job therefore kept
# running the pre-repo2 command on r1 for a full day after #305 "shipped".
#
# Parse-only: no variables are resolved, so undefined vars/filters are not
# errors here — this catches syntax the renderer can never get past.
# Fail-closed in CI (jinja2 missing is a failure); skipped locally.
set -euo pipefail

# JINJA_LINT_ROOT lets the self-test point the gate at a fixture tree;
# unset (every real run) it is the repo root.
cd "${JINJA_LINT_ROOT:-$(dirname "$0")/../..}" || exit 2

PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import jinja2' >/dev/null 2>&1; then
    PY="$cand"; break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  # ansible ships its own interpreter (shebang of the console script)
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2' >/dev/null 2>&1; then PY="$cand"; fi
fi

if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "lint-jinja-templates: FAIL — no python3 with jinja2 (required in CI)" >&2
    exit 1
  fi
  echo "lint-jinja-templates: SKIP (no python3 with jinja2 locally; CI enforces)"
  exit 0
fi

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT
git ls-files '*.j2' > "$TMPD/list"

cat > "$TMPD/parse.py" <<'PY_PARSE_EOF'
import sys
import jinja2

with open(sys.argv[1], encoding='utf-8') as fh:
    paths = sorted(l.strip() for l in fh if l.strip())

if not paths:
    print("lint-jinja-templates: FAIL - no *.j2 files found (bad glob?)", file=sys.stderr)
    sys.exit(1)

# Ansible's defaults; parse-only, so undefined names/filters never matter.
env = jinja2.Environment(trim_blocks=True, lstrip_blocks=False,
                         keep_trailing_newline=True)

bad = 0
for path in paths:
    with open(path, encoding='utf-8') as fh:
        src = fh.read()
    if src.lstrip().startswith('#jinja2:'):
        print("  skip (custom delimiters): " + path)
        continue
    try:
        env.parse(src, filename=path)
    except jinja2.TemplateSyntaxError as e:
        bad += 1
        lines = src.splitlines()
        print("%s:%s: %s" % (path, e.lineno, e.message), file=sys.stderr)
        if 0 < e.lineno <= len(lines):
            print("    " + lines[e.lineno - 1].strip()[:160], file=sys.stderr)
        if 'comment' in (e.message or '').lower():
            print("    hint: bash's array-length idiom (dollar brace hash) opens a Jinja "
                  "comment - count by hand, or wrap the literal in a raw block",
                  file=sys.stderr)

print("lint-jinja-templates: parsed %d template(s), %d unrenderable" % (len(paths), bad))
sys.exit(1 if bad else 0)
PY_PARSE_EOF

"$PY" "$TMPD/parse.py" "$TMPD/list"
