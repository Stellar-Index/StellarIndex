#!/usr/bin/env bash
# lint-shell-sigpipe — refuse `… | head` inside a pipefail shell script,
# and inside a pipefail `run:` block in a GitHub workflow.
#
# BUG CLASS. Under `set -o pipefail`, a pipeline whose LAST stage stops reading
# early is a coin flip. `head -n N` is the obvious form, but any early-exit
# consumer does it: `awk '...{exit}'`, `sed '...q'`, `grep -m N`, `grep -q`.
# Catching one spelling licenses the others; `if ! … | grep -q "$want"`
# reports a substring MISSING from output that contains it.
#
#     mc ls … | sort | head -n 4 > out      # looks fine, fails ~1 run in 3
#
# `head` exits at N lines and closes the pipe. systemd's default
# IgnoreSIGPIPE=yes turns the signal into EPIPE, so the producer PRINTS
# "write failed: 'standard output': Broken pipe" and exits non-zero; pipefail
# promotes that to the pipeline status and (with `set -e`) kills the script.
# It only bites when the producer writes more than the 64 KiB pipe buffer
# before `head` is done, hence the random, unreproducible failure.
#
# FIX: land the output, then slice it:
#
#     mc ls … | sort > /tmp/x.txt
#     head -n 4 /tmp/x.txt > out
#
# A consumer that reads to EOF is also safe: `sed -n 1p`, `sed -n '1,12p'`,
# `awk 'NR==1'` with no `exit`, "${var%%$'\n'*}" / "${var:0:200}".
# For a match test, a here-string feeds grep with no pipe at all:
#
#     grep -qF -- "$want" <<<"$OUT"
#     grep -q 'GNU tar' <<<"$(tar --version 2>/dev/null)"
#
# ESCAPE HATCH: `# sigpipe-ok: <reason>` on the line, or anywhere in the comment
# block directly above it, when the producer provably writes less than a pipe
# buffer (a `pgrep` result, a single grepped line). "it's fine" is not a reason.
#
# WORKFLOW YAML. A `run:` block is pipefail shell and fails a deploy the same
# way; `ls | sort | head` only moves the SIGPIPE to sort. The workflows
# directory is a root and a `run:` block a subject. Shell is EXTRACTED with a
# real YAML parse (literal `|`, folded `>` or single line), and its RAW source
# lines go to the SAME matcher and `# sigpipe-ok:` rule as .sh files at their
# original line numbers, so reports resolve in the open workflow file. A folded
# `>` block joins its lines, so the joined script is checked at the `run:`
# line. The marker must be inside the run block; YAML comments outside it are
# not part of the script.
#
# Two workflow-only rules, from how GitHub runs a step:
#
#   shell:    the Linux default is `bash -e {0}`; explicit `shell: bash` is
#             `bash --noprofile --norc -eo pipefail {0}`; `python` / `pwsh` /
#             `node` are not bash and never linted. `defaults.run.shell` at
#             workflow or job level supplies a step's shell.
#   pipefail: the precondition, as for .sh. The `-e` half of the .sh predicate
#             does NOT carry over: every run block already has `-e`, and
#             without pipefail an early close cannot fail the step, so `-e`
#             alone would flag every pipeline for a class that cannot fire. A
#             step is in scope when it sets pipefail itself or inherits it from
#             an explicit `shell: bash`.
set -uo pipefail

# scripts/ci is a default root: the gate scripts and their self-tests are
# pipefail shell too.
roots=("${@:-configs/ansible/roles/archival-node/files scripts/ops scripts/dev scripts/ci .github/workflows}")
# shellcheck disable=SC2206
read -r -a roots <<<"${roots[*]}"

# The early-exit consumer set, as one ERE:
#
#   head            stops at N lines
#   awk … exit      stops at the first match
#   sed … q         stops at the first match
#   grep -m N       stops at N matches
#   grep -q/-l/-L   stops at the FIRST match, and the one the tree actually
#                   writes: `seq 1 400000 | grep -q 1` exits 141 under pipefail
#                   though the match SUCCEEDED, so `if ! … | grep -q x`
#                   silently inverts past 64 KiB. -L reads to EOF under BSD grep
#                   but stops at the first match under GNU grep (CI and r1).
#
# Anchors: a stage counts only when a REAL pipe feeds it (`[^|]\|` keeps
# `cmd || grep -q x` out). The flag must be a whole token in THIS stage
# (`[^|&;()]*` stops at the next `&&`, `;` or `)`), so a later `&& ! grep -q x`
# is not read as this pipe's consumer. Cluster forms (`-qE`, `-Fxq`) match too.
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

# join_pipe_continuations <src> <dst> — merge a line ending in a bare trailing
# pipe with the line(s) after it so `producer |` / `    consumer` reads as one
# pipeline. The leading-pipe form (`producer \` then `| consumer`) needs no
# joining: the regex's `^` alternative matches it. The joined text sits on the
# line the pipe OPENS and absorbed lines are blanked, so reports keep original
# line numbers. A trailing `|` inside a comment is never joined.
join_pipe_continuations() {
  local src="$1" dst="$2"
  awk '
    function is_comment(s) {
      sub(/^[ \t]*/, "", s)
      return substr(s, 1, 1) == "#"
    }
    function trailing_pipe(s) {
      return (!is_comment(s)) && s ~ /(^|[^|])\|[ \t]*$/
    }
    {
      raw = $0
      if (open) {
        buf = buf " " raw
        out[NR] = ""
      } else {
        buf = raw
        start = NR
      }
      if (trailing_pipe(raw)) {
        open = 1
      } else {
        out[start] = buf
        open = 0
        buf = ""
      }
    }
    END {
      for (i = 1; i <= NR; i++) print (i in out ? out[i] : "")
    }
  ' "$src" > "$dst"
}

# scan_subject <file-to-scan> <path-to-report> — the one matcher and the one
# notion of "guarded". For .sh both arguments are the file; for a workflow the
# first is the extracted line-aligned shell, the second the YAML.
scan_subject() {
  local scan="$1" label="$2"
  local line body prev back pl joined
  joined="$TMPD/pipe-joined.tmp"
  join_pipe_continuations "$scan" "$joined"
  scan="$joined"
  while IFS=: read -r line _; do
    [ -n "$line" ] || continue
    body=$(sed -n "${line}p" "$scan")
    # Scan the whole contiguous comment block above, so the marker can sit
    # anywhere in a multi-line justification.
    prev=""
    back=$((line - 1))
    while [ "$back" -ge 1 ]; do
      pl=$(sed -n "${back}p" "$scan")
      case "$(printf '%s' "$pl" | sed 's/^[[:space:]]*//')" in
        '#'*) prev="$prev$pl"; back=$((back - 1)) ;;
        *) break ;;
      esac
    done
    # A comment ABOUT the pattern is not an instance of it.
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
# Only YAML in a workflows directory: other YAML under the roots (ansible,
# prometheus rules) is another gate's subject.
: >"$TMPD/wf-files.txt"
while IFS= read -r y; do
  case "$y" in
    .github/workflows/*|*/.github/workflows/*) printf '%s\n' "$y" >>"$TMPD/wf-files.txt" ;;
  esac
done < <(find "${roots[@]}" -type f \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)
wf_files=$(wc -l <"$TMPD/wf-files.txt" | tr -d ' ')

# A root that IS or holds a workflows directory must yield workflow files. An
# empty subject set is the failure this gate was blind to for its whole life, so
# it is a hard failure, which also catches the directory being moved or renamed.
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
  # Interpreter discovery, as in check-alertmanager-parity.sh. PyYAML locates
  # the run blocks (scalar style, indentation and the defaults chain are not
  # regexable); CI is fail-closed.
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
    # Emits per workflow a line-aligned copy where in-scope run-block lines are
    # raw source and all else is blank, so a hit's line number IS the YAML line.
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
