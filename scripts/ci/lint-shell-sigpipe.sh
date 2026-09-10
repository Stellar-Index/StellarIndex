#!/usr/bin/env bash
# lint-shell-sigpipe — refuse `… | head` inside a pipefail shell script,
# and inside a pipefail `run:` block in a GitHub workflow.
#
# THE BUG CLASS (#475). Under `set -o pipefail`, a pipeline whose LAST
# stage stops reading early is a coin flip. `head -n N` is the obvious
# form, but any early-exit consumer does it: `awk '...{exit}'`,
# `sed '...q'`, `grep -m N`, `grep -q`. The first version of this lint
# matched only `head`, and an independent review found `awk ... exit` five
# lines from the fix it shipped with — a lint that catches one spelling of a
# class licenses the others. The next version named `grep -m N` and not
# `grep -q`, which is the spelling the tree actually writes: 44 sites over
# the four roots, among them the substring assertion at the heart of ten
# gate self-tests, where `if ! … | grep -q "$want"` reports a substring
# MISSING from output that contains it. Example of the obvious form:
#
#     mc ls … | sort | head -n 4 > out      # looks fine, fails ~1 run in 3
#
# `head` exits the moment it has N lines and closes the pipe. systemd's
# default IgnoreSIGPIPE=yes turns the resulting signal into a plain EPIPE,
# so the upstream process does not die silently — it PRINTS
# "write failed: 'standard output': Broken pipe" and exits non-zero.
# pipefail promotes that to the pipeline's status, and (with `set -e`) it
# kills the script. It only bites when the producer writes more than the
# 64 KiB pipe buffer before `head` is done, which is why it presents as a
# random, unreproducible failure: three of r1's galexie-archive-fill runs,
# the last on 2026-09-02 18:19:35 UTC, exited 2 exactly this way.
#
# THE FIX is always the same shape — land the output, then slice it:
#
#     mc ls … | sort > /tmp/x.txt
#     head -n 4 /tmp/x.txt > out
#
# A consumer that reads to EOF is as safe and usually shorter: `sed -n 1p`,
# `sed -n '1,12p'`, `awk 'NR==1'` with no `exit`, or slicing a captured
# value with "${var%%$'\n'*}" / "${var:0:200}".
#
# For a match test the landing place is a value, and a here-string feeds it
# with no pipe at all — bash writes the whole string before grep starts, via
# a temp file once it outgrows a pipe buffer:
#
#     grep -qF -- "$want" <<<"$OUT"
#     grep -q 'GNU tar' <<<"$(tar --version 2>/dev/null)"
#
# ESCAPE HATCH: put `# sigpipe-ok: <reason>` on the line, or anywhere in
# the comment block directly above it, when the producer provably writes
# less than a pipe buffer
# (a `pgrep` result, a single grepped line). State the reason; "it's
# fine" is not one.
#
# WORKFLOW YAML (2026-09-10). The subject set was `find … -name '*.sh'`,
# and a `run:` block inside GitHub workflow YAML is not a .sh file — so
# every pipeline in the workflows directory was outside this gate for its
# whole life. The v0.69.0 deploy failed on exactly that blind spot:
#
#     sort: write failed: 'standard output': Broken pipe
#     ##[error]Process completed with exit code 2
#
# in the step that merely LISTS the staged migrations. All 305 files had
# staged correctly. The line carried a comment naming this trap (F-1300,
# filed after an EARLIER deploy died the same way) and did it anyway: the
# previous fix had turned `ls | head` into `ls | sort | head -n 10`, which
# only moved the SIGPIPE from ls to sort. It is a race, so it deployed
# v0.67.0 and v0.68.0 from identical code and killed v0.69.0.
#
# So the workflows directory is a root and a `run:` block is a subject. The
# shell is EXTRACTED with a real YAML parse — a run block is a scalar with
# its own indentation, literal `|` or folded `>` or a single line — rather
# than regexed out of the YAML, and the block's RAW source lines are handed
# to the SAME matcher and the same `# sigpipe-ok:` rule the .sh files get,
# at their original line numbers, so a report resolves in the workflow file
# a reader has open. (A folded `>` block is the exception: folding joins its
# source lines, so no single source line holds the whole pipeline and the
# joined script is checked at the `run:` line instead.) The marker lives in
# the shell, i.e. inside the run block; a YAML comment outside it is not
# part of the extracted script.
#
# Two workflow-only rules, both from how GitHub runs a step:
#
#   shell:    the default for `run:` on a Linux runner is `bash -e {0}`; an
#             explicit `shell: bash` is `bash --noprofile --norc -eo
#             pipefail {0}`; `shell: python` / `pwsh` / `node` are not bash
#             at all and are never linted as such. `defaults.run.shell` at
#             workflow or job level supplies a step's shell.
#   pipefail: the precondition, exactly as for a .sh file. The `-e` half of
#             the .sh predicate deliberately does NOT carry over: every run
#             block already has `-e` from GitHub, and without pipefail an
#             early close cannot fail the step, so honouring `-e` alone
#             would flag every pipeline in every workflow for a class that
#             cannot fire there. A step is in scope when it sets pipefail
#             itself (`set -euo pipefail`) or inherits it from an explicit
#             `shell: bash`.
set -uo pipefail

# scripts/ci is a default root because the gate scripts and their self-tests
# are themselves pipefail shell, and the lint never looked at its own
# directory: a `printf … | head -1` in check-public-dataset-test.sh took down
# a full local verify with "printf: write error: Broken pipe" on 2026-09-03,
# while this gate reported OK over the three roots it did scan.
roots=("${@:-configs/ansible/roles/archival-node/files scripts/ops scripts/dev scripts/ci .github/workflows}")
# shellcheck disable=SC2206
read -r -a roots <<<"${roots[*]}"

# The early-exit consumer set, as one ERE:
#
#   head            stops at N lines
#   awk … exit      stops at the first match
#   sed … q         stops at the first match
#   grep -m N       stops at N matches
#   grep -q/-l/-L   stops at the FIRST match — the same shape, and the one
#                   the tree actually writes. `seq 1 400000 | grep -q 1`
#                   exits 141 under pipefail: the match SUCCEEDED and the
#                   pipeline still reports failure, so `if ! … | grep -q x`
#                   silently inverts on any producer past 64 KiB. -L reads
#                   to EOF under BSD grep but stops at the first match under
#                   GNU grep, which is what CI and r1 run.
#
# Two anchors keep the match honest. A stage only counts when a REAL pipe
# feeds it — `[^|]\|` keeps `cmd || grep -q x` out, where grep reads a file
# and there is no upstream to kill. And the flag is a whole token in THIS
# stage: `[^|&;()]*` stops the scan at the next `&&`, `;` or `)` so that a
# later `&& ! grep -q x` on the same line is not read as this pipe's
# consumer. The cluster form (`-qE`, `-Fxq`, `-qxF`) is matched too.
early_exit_re='(^|[^|])\|[[:space:]]*(head([[:space:]]|$)|awk[^|]*\<exit\>|sed[^|]*\<q\>|grep[^|&;()]*[[:space:]]-([A-Za-z]*[qlL][A-Za-z]*([[:space:]]|$)|m[[:space:]]*[0-9]|-(quiet|silent|files-with-matches|files-without-match|max-count)([=[:space:]]|$)))'

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

fail=0
checked=0
wf_files=0
wf_blocks=0
wf_skipped_shell=0
wf_skipped_nopipefail=0
wf_note=""

# scan_subject <file-to-scan> <path-to-report> — the one matcher, and the one
# notion of "guarded". For a .sh file the two arguments are the same file; for
# a workflow the first is the extracted, line-aligned shell and the second is
# the YAML the reader has open.
scan_subject() {
  local scan="$1" label="$2"
  local line body prev back pl
  while IFS=: read -r line _; do
    [ -n "$line" ] || continue
    body=$(sed -n "${line}p" "$scan")
    # Look back over the whole contiguous comment block above the line, so
    # the marker can sit anywhere in a multi-line justification.
    prev=""
    back=$((line - 1))
    while [ "$back" -ge 1 ]; do
      pl=$(sed -n "${back}p" "$scan")
      case "$(printf '%s' "$pl" | sed 's/^[[:space:]]*//')" in
        '#'*) prev="$prev$pl"; back=$((back - 1)) ;;
        *) break ;;
      esac
    done
    # A comment ABOUT the pattern is not an instance of it — this lint's
    # own explanatory comments, and the fixed sites', quote the bad shape.
    case "$(printf '%s' "$body" | sed 's/^[[:space:]]*//')" in
      '#'*) continue ;;
    esac
    case "$body$prev" in
      *sigpipe-ok:*) continue ;;
    esac
    printf 'lint-shell-sigpipe: %s:%s pipes into an EARLY-EXIT consumer (head / awk exit / sed q / grep -q, -l, -L, -m) under pipefail — land the output in a file or variable first, then slice it\n' "$label" "$line"
    printf '    %s\n' "$body"
    fail=$((fail + 1))
  done < <(grep -nE "$early_exit_re" "$scan" | cut -d: -f1 | sed 's/$/:/')
}

# ── 1. shell scripts ─────────────────────────────────────────────────
while IFS= read -r f; do
  [ -f "$f" ] || continue
  grep -qE '^set .*(pipefail|-e)' "$f" || continue   # only pipefail/-e scripts can be killed by it
  checked=$((checked + 1))
  scan_subject "$f" "$f"
done < <(find "${roots[@]}" -type f -name '*.sh' 2>/dev/null | sort)

# ── 2. GitHub workflow `run:` blocks ─────────────────────────────────
# Only YAML that lives in a workflows directory: the other YAML under the
# roots (ansible tasks, prometheus rules) is not a workflow, and parsing it
# here would report another gate's failure with less context.
: >"$TMPD/wf-files.txt"
while IFS= read -r y; do
  case "$y" in
    .github/workflows/*|*/.github/workflows/*) printf '%s\n' "$y" >>"$TMPD/wf-files.txt" ;;
  esac
done < <(find "${roots[@]}" -type f \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)
wf_files=$(wc -l <"$TMPD/wf-files.txt" | tr -d ' ')

# A root that IS a workflows directory, or holds one, must yield workflow
# files. An empty subject set is the exact failure this gate was blind to for
# its whole life, so it is a hard failure and never a quiet OK — and because a
# default run always names the workflows directory as a root, this is also what
# stops the workflow half going silently absent if that directory is ever moved
# or renamed underneath it.
wf_dirs=$(find "${roots[@]}" -type d -path '*.github/workflows' 2>/dev/null | wc -l | tr -d ' ')
wf_expected=0
for r in "${roots[@]}"; do
  case "${r%/}" in
    */.github/workflows|.github/workflows) wf_expected=1 ;;
  esac
done
if { [ "$wf_dirs" -gt 0 ] || [ "$wf_expected" -eq 1 ]; } && [ "$wf_files" -eq 0 ]; then
  echo "lint-shell-sigpipe: FAIL — a workflows directory is in scope but holds no workflow YAML; the workflow half of the gate would be vacuous" >&2
  exit 1
fi

if [ "$wf_files" -gt 0 ]; then
  # Interpreter discovery, as in check-alertmanager-parity.sh. PyYAML is how
  # the run blocks are located — scalar style, block indentation and the
  # defaults chain are not regexable — and CI is fail-closed.
  PY=""
  for cand in python3 /opt/homebrew/bin/python3; do
    if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import yaml' >/dev/null 2>&1; then
      PY="$cand"
      break
    fi
  done
  if [ -z "$PY" ]; then
    if [ "${CI:-}" = "true" ]; then
      echo "lint-shell-sigpipe: FAIL — no python3 with PyYAML, so the $wf_files workflow file(s) cannot be scanned (required in CI)" >&2
      exit 1
    fi
    wf_note=" — WORKFLOWS NOT SCANNED (no python3 with PyYAML locally; CI enforces)"
  fi

  if [ -n "$PY" ]; then
    mkdir -p "$TMPD/shadow"
    # Emits, per workflow, a line-aligned copy of the file in which every line
    # of an in-scope run block is its raw source line and every other line is
    # blank — so a grep hit's line number IS the YAML line number.
    if ! wf_stats=$("$PY" - "$TMPD/wf-files.txt" "$TMPD/shadow" "$TMPD/index.tsv" <<'PY'
import os
import re
import sys

try:
    import yaml
except ImportError:  # pragma: no cover - the caller checks first
    print("lint-shell-sigpipe: PyYAML not importable", file=sys.stderr)
    sys.exit(1)

list_path, shadow_dir, index_path = sys.argv[1], sys.argv[2], sys.argv[3]

# The .sh predicate is `^set .*(pipefail|-e)`; leading whitespace is allowed
# here because a run block's script keeps whatever indentation the author
# wrote inside the block scalar.
PIPEFAIL_RE = re.compile(r"^[ \t]*set[ \t][^\n]*pipefail", re.M)


def mget(node, key):
    """The value node for `key`, without constructing the document."""
    if not isinstance(node, yaml.MappingNode):
        return None
    for k, v in node.value:
        if isinstance(k, yaml.ScalarNode) and k.value == key:
            return v
    return None


def defaults_shell(node):
    shell = mget(mget(mget(node, "defaults"), "run"), "shell")
    return shell.value if isinstance(shell, yaml.ScalarNode) else None


def in_scope(shell, script):
    """(is a bash-ish shell, pipefail is in effect) for one step."""
    if shell is None:
        # GitHub's implicit default on a Linux/macOS runner is `bash -e {0}`
        # (with a `sh -e {0}` fallback) — `-e`, and no pipefail.
        return True, bool(PIPEFAIL_RE.search(script))
    tokens = str(shell).split()
    if not tokens:
        return False, False
    prog = os.path.basename(tokens[0])
    if prog not in ("bash", "sh"):
        return False, False          # python, pwsh, powershell, cmd, node…
    if len(tokens) == 1:
        # `shell: bash` is `bash --noprofile --norc -eo pipefail {0}`;
        # `shell: sh` is `sh -e {0}`, which has to say pipefail itself.
        return True, prog == "bash" or bool(PIPEFAIL_RE.search(script))
    # A custom command template (`shell: bash -x {0}`) gets only what it says.
    return True, ("pipefail" in str(shell)) or bool(PIPEFAIL_RE.search(script))


with open(list_path, encoding="utf-8") as fh:
    paths = [p for p in fh.read().split("\n") if p]

problems = []
blocks = skipped_shell = skipped_nopipefail = 0
index = []

for path in paths:
    try:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
        docs = list(yaml.compose_all(text))
    except (OSError, UnicodeDecodeError) as exc:
        problems.append("%s: unreadable (%s)" % (path, exc))
        continue
    except yaml.YAMLError as exc:
        problems.append(
            "%s: does not parse as YAML, so its run blocks cannot be checked (%s)"
            % (path, str(exc).replace("\n", " "))
        )
        continue

    lines = text.split("\n")
    shadow = [""] * len(lines)
    found_jobs = False
    for doc in docs:
        jobs = mget(doc, "jobs")
        if not isinstance(jobs, yaml.MappingNode):
            continue
        found_jobs = True
        wf_shell = defaults_shell(doc)
        for _, job in jobs.value:
            job_shell = defaults_shell(job) or wf_shell
            steps = mget(job, "steps")
            if not isinstance(steps, yaml.SequenceNode):
                continue
            for step in steps.value:
                run = mget(step, "run")
                if not isinstance(run, yaml.ScalarNode):
                    continue
                shell = mget(step, "shell")
                shell = shell.value if isinstance(shell, yaml.ScalarNode) else job_shell
                is_shell, pipefail = in_scope(shell, run.value)
                if not is_shell:
                    skipped_shell += 1
                    continue
                if not pipefail:
                    skipped_nopipefail += 1
                    continue
                blocks += 1
                start, end = run.start_mark.line, run.end_mark.line
                if run.style == "|":
                    # A literal block's content starts on the line AFTER
                    # `run: |` and ends before the next token's line, and
                    # every source line is a shell line: copy them verbatim.
                    for i in range(start + 1, min(end, len(lines))):
                        shadow[i] = lines[i]
                elif run.style == ">":
                    # A folded block JOINS its source lines, so no single
                    # source line carries the whole pipeline (`mc ls … |` on
                    # one line, `head -n 4` on the next, one command after
                    # folding). Put the folded script on the `run:` line: the
                    # report then points at the block, which is the smallest
                    # thing a reader can act on for this style.
                    shadow[start] = run.value.replace("\n", " ")
                else:
                    # Plain or quoted — the script sits on the `run:` line.
                    for i in range(start, min(end + 1, len(lines))):
                        shadow[i] = lines[i]

    if not found_jobs:
        problems.append(
            "%s: no `jobs:` mapping — the extractor did not recognise it as a "
            "workflow, so nothing in it was checked" % path
        )
        continue
    if any(shadow):
        out = os.path.join(shadow_dir, path.replace("/", "%") + ".shadow")
        with open(out, "w", encoding="utf-8") as fh:
            fh.write("\n".join(shadow) + "\n")
        index.append("%s\t%s" % (out, path))

if problems:
    for msg in problems:
        print("lint-shell-sigpipe: %s" % msg, file=sys.stderr)
    sys.exit(1)

with open(index_path, "w", encoding="utf-8") as fh:
    fh.write("\n".join(index) + ("\n" if index else ""))
print("%d\t%d\t%d\t%d" % (len(paths), blocks, skipped_shell, skipped_nopipefail))
PY
    ); then
      echo "lint-shell-sigpipe: FAIL — the workflow shell could not be extracted (see above); the workflow half of the gate did NOT run" >&2
      exit 1
    fi
    wf_blocks=$(printf '%s' "$wf_stats" | cut -f2)
    wf_skipped_shell=$(printf '%s' "$wf_stats" | cut -f3)
    wf_skipped_nopipefail=$(printf '%s' "$wf_stats" | cut -f4)
    if [ "$wf_blocks" -eq 0 ]; then
      echo "lint-shell-sigpipe: FAIL — $wf_files workflow file(s) in scope but not one run: block runs under pipefail ($wf_skipped_shell non-bash, $wf_skipped_nopipefail without pipefail); the workflow half of the gate would be vacuous" >&2
      exit 1
    fi
    while IFS=$'\t' read -r shadow label; do
      [ -n "$shadow" ] || continue
      scan_subject "$shadow" "$label"
    done <"$TMPD/index.tsv"
  fi
fi

if [ "$checked" -eq 0 ] && [ "$wf_blocks" -eq 0 ]; then
  echo "lint-shell-sigpipe: FAIL — no pipefail scripts found under ${roots[*]}; the gate would be vacuous" >&2
  exit 1
fi
if [ "$fail" -gt 0 ]; then
  echo "lint-shell-sigpipe: FAIL — $fail early-exit-consumer site(s) in $checked pipefail script(s) and $wf_blocks pipefail run: block(s)" >&2
  exit 1
fi
echo "lint-shell-sigpipe: OK — $checked pipefail script(s), $wf_blocks pipefail run: block(s) in $wf_files workflow file(s), no unguarded pipe into an early-exit consumer${wf_note}"
