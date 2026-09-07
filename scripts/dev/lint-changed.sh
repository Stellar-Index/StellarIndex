#!/usr/bin/env bash
# lint-changed.sh — run the lints that apply to the files you changed, in
# seconds, before a commit exists.
#
# This is a DISPATCHER over lints that already exist, not a new lint. It
# looks at the diff — staged files, or origin/main...HEAD — picks the
# scripts/ci gates and formatters that apply to each changed file's type,
# scopes each one to the changed files where the gate accepts a file list,
# and runs the cheapest first. Nothing here asserts anything the full gate
# does not; it only moves the moment a failure surfaces.
#
# Why, in numbers from 2026-09-07: `lint-shell-sigpipe.sh` runs in 2.7 s on
# its own and fired at line 632 of a 637-line `make prepush` log, after about
# ten minutes of Go build, doc lints and web typechecks that had nothing to
# do with the one-line shell edit under test. Four prepush attempts on one
# patch that day were the gate correctly catching the operator's own edits —
# each at minute ten instead of second three.
#
# What runs, by changed-file type (in cost order, measured on an M-series
# laptop with warm caches; the whole run on a diff touching one .sh, one .go
# and one .md file is recorded in docs/contributing/local-verification.md):
#
#   *.sh                     bash -n (0.02 s/file); lint-shell-sigpipe over
#                            the pipefail subset (0.05 s); shellcheck -x
#                            (0.5 s/file). A *-test.sh is also RUN, last.
#   *.go                     gofumpt -l, goimports -l (0.1 s/file); then
#                            lint-lexicon, lint-i128, lint-imports (whole
#                            tree, ~1 s each — they take no file list) and
#                            lint-http-timeouts scoped to the package dirs;
#                            then go vet + go build on the touched packages.
#   .github/workflows/*.yml  lint-actions-pinning scoped (0.1 s), actionlint
#                            (0.04 s), zizmor --offline (0.15 s).
#   migrations/*.sql         lint-migrations, lint-migration-immutability,
#                            lint-migration-commands, lint-migration-compat
#                            (whole migrations/ dir; each ~1 s).
#   *.go / *.sql naming a    lint-lake-dedup (whole tree; it pre-filters on
#   duplicate-bearing table  the same two table names this trigger uses).
#   scripts/ci/*.baseline    lint-baseline-growth over the commit range. It
#                            reads commit trailers, so it has nothing to say
#                            about STAGED edits and is deferred there.
#   verify.sh, ci.yml        check-verify-parity (0.7 s).
#   *.md                     nothing runs here, and the plan says so:
#                            lint-doc-links takes no file list and costs 38 s
#                            over the tree, lint-docs 64 s. Both stay in
#                            scripts/dev/verify.sh, where they now run
#                            before the Go build rather than after it.
#
# Two adoption notes, both visible in the plan output rather than silent:
#   - actionlint runs with its embedded shellcheck pass OFF (-shellcheck=).
#     The workflow tree carries 15 pre-existing findings from that pass, one
#     of them SC2260:error in deploy.yml; a Tier-0 gate cannot be the thing
#     that adopts them. actionlint's own checks (syntax, expressions, action
#     inputs, runner labels) are all on and green over the tree.
#   - zizmor runs --offline. CI runs the online audits too, which include
#     ref-version-mismatch (a pin whose version comment lies); see
#     .github/zizmor.yml for the exact CI invocation.
#
# A missing tool (shellcheck, actionlint, zizmor) DEFERS its step, loudly and
# counted in the summary line, and does not fail the run: this is the loop
# before a commit, and scripts/dev/verify.sh remains the gate. gofumpt and
# goimports are different — `make fmt` calls them by absolute path from
# $(go env GOPATH)/bin, so their absence is a broken checkout (`make deps`)
# and the step fails.
#
# Usage:
#   scripts/dev/lint-changed.sh                 staged files; if none are
#                                               staged, origin/main...HEAD
#   scripts/dev/lint-changed.sh --staged        staged files only (what the
#                                               pre-commit hook runs)
#   scripts/dev/lint-changed.sh --base <rev>    files changed in <rev>...HEAD
#   scripts/dev/lint-changed.sh --plan [...]    print the plan, run nothing
#   scripts/dev/lint-changed.sh -- <file>...    an explicit file list
#   make lint-changed                           (LINT_CHANGED_ARGS='--plan')
#   make hooks                                  install it as a pre-commit hook
#
# Exit 0 when every selected lint passed (deferred steps are reported, not
# failed); 1 when any lint failed; 2 on a usage or discovery error. Every
# run ends with a self-accounting line naming how many lints ran over how
# many files, so an empty selection cannot read as a pass over something.
#
# Lints are resolved from the checkout this script lives in; the changed
# files come from the git work tree the caller is standing in. In every real
# invocation those are the same directory — the split exists so the
# self-test (scripts/dev/lint-changed-test.sh) can drive discovery from a
# fixture repository while dispatching to the real gates.
set -uo pipefail

self_dir="$(cd "$(dirname "$0")" && pwd)"
lint_root="$(cd "$self_dir/../.." && pwd)"
ci_dir="$lint_root/scripts/ci"
go_module="github.com/Stellar-Index/StellarIndex"

usage() { sed -n '/^# Usage:/,/^#$/p' "$0" | sed 's/^# \{0,1\}//'; }

mode=""            # staged | base | files
base_rev=""
plan_only=0
explicit=()
while [ "$#" -gt 0 ]; do
    case "$1" in
        --staged) mode=staged ;;
        --base)
            mode=base
            base_rev="${2:-}"
            [ -n "$base_rev" ] || { echo "lint-changed: --base needs a revision" >&2; exit 2; }
            shift ;;
        --plan) plan_only=1 ;;
        --) shift; mode=files; explicit=("$@"); break ;;
        -h|--help) usage; exit 0 ;;
        *) echo "lint-changed: unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

# ── Discovery ───────────────────────────────────────────────────────────────
#
# Added, copied, modified and renamed files only (--diff-filter=ACMR): a
# deleted file has nothing to lint, and its removal is judged by the
# whole-tree gates in verify.sh. Paths are NUL-separated so a name with a
# space cannot split into two.
changed=()
how=""
if [ "$mode" = files ]; then
    for f in ${explicit[@]+"${explicit[@]}"}; do
        [ -e "$f" ] || { echo "lint-changed: no such file: $f" >&2; exit 2; }
        changed+=("$f")
    done
    how="explicit list"
else
    top="$(git rev-parse --show-toplevel 2>/dev/null)" || {
        echo "lint-changed: not inside a git work tree" >&2; exit 2; }
    cd "$top" || exit 2
    if [ "$mode" = "" ]; then
        if git diff --cached --quiet 2>/dev/null; then mode=base; base_rev=origin/main; else mode=staged; fi
    fi
    case "$mode" in
        staged)
            how="staged"
            while IFS= read -r -d '' f; do changed+=("$f"); done \
                < <(git diff --cached --name-only --diff-filter=ACMR -z) ;;
        base)
            git rev-parse -q --verify "${base_rev}^{commit}" >/dev/null 2>&1 || {
                echo "lint-changed: revision '${base_rev}' does not resolve; pass --base <rev>, --staged, or -- <file>..." >&2
                exit 2; }
            mb="$(git merge-base "$base_rev" HEAD 2>/dev/null || true)"
            how="${base_rev}...HEAD (merge-base ${mb:0:12})"
            while IFS= read -r -d '' f; do changed+=("$f"); done \
                < <(git diff --name-only --diff-filter=ACMR -z "${base_rev}...HEAD") ;;
    esac
fi

# In every real invocation the lints and the files share a root; name the
# gates by their tracked path then, and only fall back to absolute paths
# when the self-test drives discovery from a fixture repository.
[ "$PWD" = "$lint_root" ] && ci_dir="scripts/ci"

echo "lint-changed: ${#changed[@]} changed file(s), ${how}"
if [ "${#changed[@]}" -eq 0 ]; then
    echo "lint-changed: OK — 0 lint(s) over 0 changed file(s): nothing to lint"
    exit 0
fi

# ── Classification ──────────────────────────────────────────────────────────
sh_files=(); test_scripts=(); go_files=(); go_dirs=(); wf_files=()
mig_files=(); md_files=(); baseline_files=(); lake_files=(); parity=0; other=()

add_unique() { # add_unique <value> — appends to go_dirs if absent
    local v="$1" d
    for d in ${go_dirs[@]+"${go_dirs[@]}"}; do [ "$d" = "$v" ] && return 0; done
    go_dirs+=("$v")
}

for f in "${changed[@]}"; do
    # A path that is staged but gone from the work tree cannot be linted;
    # the deletion is judged by verify.sh like any other.
    [ -e "$f" ] || { echo "lint-changed: skipping ${f} (not in the work tree)"; continue; }
    hit=0
    case "$f" in
        *.sh)
            sh_files+=("$f"); hit=1
            case "$f" in *-test.sh) test_scripts+=("$f") ;; esac ;;
        *.go)
            go_files+=("$f"); hit=1
            d="$(dirname "$f")"
            case "$d" in .) add_unique "." ;; *) add_unique "./${d#./}" ;; esac ;;
        *.md) md_files+=("$f"); hit=1 ;;
    esac
    case "$f" in
        .github/workflows/*.yml|.github/workflows/*.yaml|*/.github/workflows/*.yml|*/.github/workflows/*.yaml)
            wf_files+=("$f"); hit=1 ;;
        migrations/*.sql|*/migrations/*.sql) mig_files+=("$f"); hit=1 ;;
        scripts/ci/*.baseline|*/scripts/ci/*.baseline) baseline_files+=("$f"); hit=1 ;;
    esac
    case "$f" in
        scripts/dev/verify.sh|*/scripts/dev/verify.sh|.github/workflows/ci.yml|*/.github/workflows/ci.yml)
            parity=1; hit=1 ;;
    esac
    # The lake-dedup gate's own subject filter, applied here so the trigger
    # is exactly the set of files that gate would examine.
    case "$f" in
        *.go|*.sql)
            if grep -qF -e 'stellar.transactions' -e 'stellar.operations' "$f"; then
                lake_files+=("$f"); hit=1
            fi ;;
    esac
    [ "$hit" -eq 1 ] || other+=("$f")
done

# ── Plan ────────────────────────────────────────────────────────────────────
#
# Parallel arrays (bash 3.2 has no associative arrays): an id for the summary,
# the argv joined by newline, and an optional note. An argv whose first word
# is one of the helper functions below runs through it.
step_id=(); step_argv=(); step_note=()
add_step() { # add_step <id> <note> <argv>...
    local id="$1" note="$2" joined="" a
    shift 2
    for a in "$@"; do joined="${joined}${a}"$'\n'; done
    step_id+=("$id"); step_note+=("$note"); step_argv+=("${joined%$'\n'}")
}
deferred=()
skip_line=()
defer() { deferred+=("$1: $2"); skip_line+=("skip  $1: $2"); }

format_clean() { # format_clean <label> <tool> <args>... — a formatter's -l output must be empty
    local label="$1" tool="$2" out rc n=0 a
    shift 2
    [ -x "$tool" ] || { echo "${label}: ${tool} is absent — run 'make deps'" >&2; return 1; }
    out="$("$tool" "$@" 2>&1)"; rc=$?
    if [ "$rc" -ne 0 ] || [ -n "$out" ]; then
        echo "${label}: these files are not formatted — run 'make fmt':" >&2
        printf '  %s\n' "$out" >&2
        return 1
    fi
    for a in "$@"; do case "$a" in *.go) n=$((n + 1)) ;; esac; done
    echo "${label}: OK — ${n} file(s) formatted"
}

# Resolved where the Makefile resolves them: `make fmt` calls both by absolute
# path under $(go env GOPATH)/bin, and PATH need not contain it.
go_bin=""
[ "${#go_files[@]}" -gt 0 ] && go_bin="$(go env GOPATH 2>/dev/null || true)/bin"

# 1. Shell: syntax, then the sigpipe class, then shellcheck.
for f in ${sh_files[@]+"${sh_files[@]}"}; do add_step "bash -n" "" bash -n "$f"; done
if [ "${#sh_files[@]}" -gt 0 ]; then
    pipefail_files=()
    for f in "${sh_files[@]}"; do
        if grep -qE '^set .*(pipefail|-e)' "$f"; then pipefail_files+=("$f"); fi
    done
    if [ "${#pipefail_files[@]}" -gt 0 ]; then
        add_step "lint-shell-sigpipe" "scoped to ${#pipefail_files[@]} pipefail script(s)" \
            "$ci_dir/lint-shell-sigpipe.sh" "${pipefail_files[@]}"
    else
        defer "lint-shell-sigpipe" "none of the changed scripts sets pipefail or -e (nothing for it to catch)"
    fi
    if command -v shellcheck >/dev/null 2>&1; then
        add_step "shellcheck" "" shellcheck -x "${sh_files[@]}"
    else
        defer "shellcheck" "shellcheck is not installed (brew install shellcheck)"
    fi
fi

# 2. Go formatting — the two -l checks `make check` runs, on the files only.
if [ "${#go_files[@]}" -gt 0 ]; then
    add_step "gofumpt" "" format_clean gofumpt "$go_bin/gofumpt" -l "${go_files[@]}"
    add_step "goimports" "" format_clean goimports "$go_bin/goimports" -l -local "$go_module" "${go_files[@]}"
fi

# 3. Workflows: the pinning policy, actionlint, zizmor — all scoped.
if [ "${#wf_files[@]}" -gt 0 ]; then
    add_step "lint-actions-pinning" "scoped" "$ci_dir/lint-actions-pinning.sh" "${wf_files[@]}"
    if command -v actionlint >/dev/null 2>&1; then
        add_step "actionlint" "embedded shellcheck off: 15 pre-existing findings in .github/workflows" \
            actionlint -shellcheck= "${wf_files[@]}"
    else
        defer "actionlint" "actionlint is not installed (brew install actionlint)"
    fi
    if command -v zizmor >/dev/null 2>&1; then
        add_step "zizmor" "offline; CI adds the online ref-version-mismatch audit" \
            zizmor --offline --min-confidence medium "${wf_files[@]}"
    else
        defer "zizmor" "zizmor is not installed (brew install zizmor)"
    fi
fi

# 4. Migrations: the four migration gates over migrations/ (none takes a file
#    list; each is about a second). --staged skips compat's shrink-only
#    baseline bookkeeping, which only means something on a committed tree.
if [ "${#mig_files[@]}" -gt 0 ]; then
    add_step "lint-migrations" "" "$ci_dir/lint-migrations.sh"
    add_step "lint-migration-immutability" "" "$ci_dir/lint-migration-immutability.sh"
    add_step "lint-migration-commands" "" "$ci_dir/lint-migration-commands.sh"
    if [ "$mode" = staged ]; then
        add_step "lint-migration-compat" "--staged" "$ci_dir/lint-migration-compat.sh" --staged
    else
        add_step "lint-migration-compat" "" "$ci_dir/lint-migration-compat.sh"
    fi
fi

# 5. verify.sh ↔ CI parity, when either side of it changed.
if [ "$parity" -eq 1 ]; then
    add_step "check-verify-parity" "" "$ci_dir/check-verify-parity.sh"
fi

# 6. Baseline growth: judged from commit trailers over a range.
if [ "${#baseline_files[@]}" -gt 0 ]; then
    case "$mode" in
        base)
            mb="$(git merge-base "$base_rev" HEAD 2>/dev/null || true)"
            add_step "lint-baseline-growth" "BASE_SHA=${mb:0:12}" env "BASE_SHA=$mb" "$ci_dir/lint-baseline-growth.sh" ;;
        *)
            defer "lint-baseline-growth" "reads the Baseline-Growth commit trailer, so it runs over a commit range (--base), not over staged edits" ;;
    esac
fi

# 7. Go: the whole-tree greps (each about a second), the scoped timeout
#    lint, then vet and build on the touched packages.
if [ "${#go_files[@]}" -gt 0 ]; then
    add_step "lint-lexicon" "whole tree (takes no file list)" "$ci_dir/lint-lexicon.sh"
    add_step "lint-i128" "whole tree (takes no file list)" "$ci_dir/lint-i128.sh"
    add_step "lint-imports" "whole tree (takes no file list)" "$ci_dir/lint-imports.sh"
    # http-timeouts exempts _test.go and refuses a vacuous root, so scope it
    # to the package dirs that hold at least one non-test Go file.
    timeout_dirs=()
    for d in "${go_dirs[@]}"; do
        for g in "$d"/*.go; do
            [ -e "$g" ] || continue
            case "$g" in *_test.go) continue ;; esac
            timeout_dirs+=("$d"); break
        done
    done
    if [ "${#timeout_dirs[@]}" -gt 0 ]; then
        add_step "lint-http-timeouts" "scoped to ${#timeout_dirs[@]} package dir(s)" "$ci_dir/lint-http-timeouts.sh" "${timeout_dirs[@]}"
    fi
    add_step "go vet" "${#go_dirs[@]} package(s)" go vet "${go_dirs[@]}"
    add_step "go build" "${#go_dirs[@]} package(s)" go build "${go_dirs[@]}"
fi

# 8. Lake reads.
if [ "${#lake_files[@]}" -gt 0 ]; then
    add_step "lint-lake-dedup" "whole tree; ${#lake_files[@]} changed file(s) name a duplicate-bearing table" "$ci_dir/lint-lake-dedup.sh"
fi

# 9. A changed self-test is run. Last: these are the only steps whose cost
#    is not known in advance.
for t in ${test_scripts[@]+"${test_scripts[@]}"}; do add_step "test-script" "$t" bash "$t"; done

# Markdown: nothing sub-5 s exists for it today, and the plan must say so
# rather than let a .md-only diff read as linted.
if [ "${#md_files[@]}" -gt 0 ]; then
    defer "markdown" "${#md_files[@]} .md file(s) changed; lint-doc-links (38 s, no file list) and lint-docs (64 s) run in scripts/dev/verify.sh"
fi
for f in ${other[@]+"${other[@]}"}; do
    skip_line+=("skip  ${f}: no changed-file lint applies to this type")
done

# ── Plan output ─────────────────────────────────────────────────────────────
if [ "$plan_only" -eq 1 ]; then
    i=0
    while [ "$i" -lt "${#step_id[@]}" ]; do
        printf 'plan  %s:' "${step_id[$i]}"
        while IFS= read -r a; do printf ' %s' "$a"; done <<<"${step_argv[$i]}"
        [ -n "${step_note[$i]}" ] && printf '   (%s)' "${step_note[$i]}"
        printf '\n'
        i=$((i + 1))
    done
    for l in ${skip_line[@]+"${skip_line[@]}"}; do echo "$l"; done
    echo "lint-changed: plan — ${#step_id[@]} lint(s) over ${#changed[@]} changed file(s), ${#deferred[@]} deferred"
    exit 0
fi

# ── Run ─────────────────────────────────────────────────────────────────────
for l in ${skip_line[@]+"${skip_line[@]}"}; do echo "$l"; done
failed=()
started=$SECONDS
i=0
while [ "$i" -lt "${#step_id[@]}" ]; do
    id="${step_id[$i]}"
    argv=()
    while IFS= read -r a; do argv+=("$a"); done <<<"${step_argv[$i]}"
    if [ -n "${step_note[$i]}" ]; then
        echo "=== ${id} (${step_note[$i]}) ==="
    else
        echo "=== ${id} ==="
    fi
    t0=$SECONDS
    "${argv[@]}"
    rc=$?
    dt=$((SECONDS - t0))
    if [ "$rc" -eq 0 ]; then
        echo "lint-changed: ok   ${id} (${dt} s)"
    else
        echo "lint-changed: FAIL ${id} (exit ${rc}, ${dt} s)"
        failed+=("$id")
    fi
    i=$((i + 1))
done
total=$((SECONDS - started))

echo ""
for d in ${deferred[@]+"${deferred[@]}"}; do echo "lint-changed: deferred — $d"; done
if [ "${#failed[@]}" -gt 0 ]; then
    echo "lint-changed: FAIL — ${#failed[@]} of ${#step_id[@]} lint(s) failed over ${#changed[@]} changed file(s) in ${total} s: ${failed[*]}"
    exit 1
fi
if [ "${#step_id[@]}" -eq 0 ]; then
    echo "lint-changed: OK — 0 lint(s) over ${#changed[@]} changed file(s): no changed-file lint applies (${#deferred[@]} deferred)"
    exit 0
fi
echo "lint-changed: OK — ${#step_id[@]} lint(s) over ${#changed[@]} changed file(s) in ${total} s (${#deferred[@]} deferred)"
