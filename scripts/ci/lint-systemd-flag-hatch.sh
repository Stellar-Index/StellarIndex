#!/usr/bin/env bash
# lint-systemd-flag-hatch — a multi-token override variable (EXTRA_FLAGS
# and any other *_FLAGS / *_ARGS / *_OPTS) must reach a DIRECT-EXEC
# ExecStart= in bare `$VAR` form, never `${VAR}`.
#
# THE BUG CLASS (#1231). systemd.service(5): `$VAR` as a whole word is
# split on whitespace into zero or more argv entries; `${VAR}` is
# substituted as exactly ONE entry. The EXTRA_FLAGS hatch was copied from
# supply-snapshot, a `/bin/sh -c` unit where the shell re-splits, onto
# direct-exec units that have no shell. There `EXTRA_FLAGS="-limit 100"`
# arrives as the single token `-limit 100`, Go's flag parser looks up a
# flag named `limit 100`, and every timer fire exits 1.
#
# The check runs each ExecStart through systemd's two expansion rules with
# the hatch set to a two-token value and requires two argv entries back.
# Units whose command is `sh -c` / `bash -c` are exempt: the shell splits.
#
# Usage: lint-systemd-flag-hatch.sh [ROOT]   (ROOT defaults to the repo;
# the self-test points it at a fixture tree).
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROOT="${1:-$PWD}"

python3 - "$ROOT" <<'PY'
import glob
import os
import re
import shlex
import sys

root = sys.argv[1]
patterns = ["configs/**/*.service", "configs/**/*.service.j2",
            "deploy/**/*.service", "deploy/**/*.service.j2"]
files = sorted({f for p in patterns
                for f in glob.glob(os.path.join(root, p), recursive=True)})

HATCH = re.compile(r"\$\{?([A-Z0-9_]*(?:FLAGS|ARGS|OPTS))\}?")
SHELL = re.compile(r"(^|/)(ba|da)?sh$")
TWO_TOKENS = ["-limit", "100"]


def words(cmd):
    try:
        return shlex.split(cmd, posix=True)
    except ValueError:  # an unbalanced quote inside a Jinja expression
        return cmd.split()


def expand(argv, var):
    """systemd.service(5) 'Command lines': $VAR splits, ${VAR} does not."""
    out = []
    for w in argv:
        if w == "$" + var:
            out.extend(TWO_TOKENS)
        else:
            out.append(w.replace("${%s}" % var, " ".join(TWO_TOKENS)))
    return out


def is_shell(argv):
    i = 0
    if argv and argv[0].endswith("/env"):
        i = 1
    return len(argv) > i + 1 and SHELL.search(argv[i]) and argv[i + 1] == "-c"


checked = fail = 0
for path in files:
    text = open(path, encoding="utf-8").read().replace("\\\n", " ")
    for line in text.splitlines():
        m = re.match(r"\s*ExecStart(?:Pre|Post)?=[-@:+!]*(.*)", line)
        if not m or not HATCH.search(m.group(1)):
            continue
        argv = words(m.group(1))
        if is_shell(argv):
            continue
        for var in sorted(set(HATCH.findall(m.group(1)))):
            checked += 1
            got = expand(argv, var)
            n = len(got)
            ok = any(got[i:i + 2] == TWO_TOKENS for i in range(n - 1))
            if not ok:
                fail += 1
                rel = os.path.relpath(path, root)
                print(f"lint-systemd-flag-hatch: {rel}: ${{{var}}} in a direct-exec "
                      f"ExecStart= passes {var}='-limit 100' as ONE argv token; "
                      f"write bare ${var} so systemd word-splits it")

if checked == 0:
    print("lint-systemd-flag-hatch: FAIL — no direct-exec flag hatch found under "
          f"{root}; the gate would be vacuous", file=sys.stderr)
    sys.exit(1)
if fail:
    print(f"lint-systemd-flag-hatch: FAIL — {fail} of {checked} hatch use(s) "
          "would not word-split", file=sys.stderr)
    sys.exit(1)
print(f"lint-systemd-flag-hatch: OK — {checked} direct-exec hatch use(s) "
      f"word-split across {len(files)} unit file(s)")
PY
