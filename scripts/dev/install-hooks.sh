#!/usr/bin/env bash
# install-hooks.sh — install (or remove) the opt-in pre-commit hook that runs
# scripts/dev/lint-changed.sh over the staged files.
#
# Opt-in by design: nothing in the repository forces a hook on a clone. A
# contributor who wants the changed-file lints at `git commit` runs
# `make hooks` once; `git commit --no-verify` skips it for one commit, and
# `make hooks-remove` takes it out.
#
# Where the hook goes is git's answer, not this script's: `git rev-parse
# --git-path hooks` honours `core.hooksPath` and resolves to the shared git
# directory from a linked worktree, so one install covers every worktree of
# the checkout. Two refusals keep that honest:
#
#   - `core.hooksPath` set to a directory that does not exist. git runs NO
#     hooks for the repository in that state, and creating a directory inside
#     whatever that path points at (it may be another repository) is not
#     this script's decision. It reports the path and the setting and stops.
#   - an existing pre-commit hook that this script did not write. It is not
#     overwritten and not chained into; the one-line invocation to add to it
#     is printed instead.
#
# The hook itself is a no-op in any repository without
# scripts/dev/lint-changed.sh, so a shared `core.hooksPath` cannot make it
# fail commits elsewhere.
#
# Usage:
#   scripts/dev/install-hooks.sh              install or refresh the hook
#   scripts/dev/install-hooks.sh --uninstall  remove it (only if it is ours)
#   make hooks / make hooks-remove
#
# Exit 0 when the requested state holds; 1 when it refused; 2 on usage.
set -uo pipefail

marker="stellarindex-lint-changed-hook"

action=install
case "${1:-}" in
    "") ;;
    --uninstall) action=uninstall ;;
    -h|--help) sed -n '/^# Usage:/,/^#$/p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "install-hooks: unknown argument: $1" >&2; exit 2 ;;
esac

top="$(git rev-parse --show-toplevel 2>/dev/null)" || {
    echo "install-hooks: not inside a git work tree" >&2; exit 2; }
cd "$top" || exit 2

hooks_dir="$(git rev-parse --git-path hooks)"
case "$hooks_dir" in /*) ;; *) hooks_dir="$top/$hooks_dir" ;; esac
hook="$hooks_dir/pre-commit"
hooks_path_setting="$(git config --get core.hooksPath 2>/dev/null || true)"

is_ours() { [ -f "$hook" ] && grep -qF "$marker" "$hook"; }

if [ "$action" = uninstall ]; then
    if [ ! -e "$hook" ]; then
        echo "install-hooks: no pre-commit hook at ${hook}; nothing to remove"
        exit 0
    fi
    if ! is_ours; then
        echo "install-hooks: ${hook} was not installed by this script; leaving it in place" >&2
        exit 1
    fi
    rm -f "$hook"
    echo "install-hooks: removed ${hook}"
    exit 0
fi

if [ ! -d "$hooks_dir" ]; then
    if [ -n "$hooks_path_setting" ]; then
        cat >&2 <<EOF
install-hooks: core.hooksPath is set to '${hooks_path_setting}', which resolves to
  ${hooks_dir}
and that directory does not exist. git runs no hooks at all for this
repository while it stays that way. Either point core.hooksPath at an
existing directory or unset it (git config --unset core.hooksPath), then
re-run make hooks.
EOF
        exit 1
    fi
    mkdir -p "$hooks_dir" || exit 1
fi

if [ -e "$hook" ] && ! is_ours; then
    cat >&2 <<EOF
install-hooks: ${hook} already exists and was not written by this script.
It is left untouched. To run the changed-file lints from it, add this line:

  "\$(git rev-parse --show-toplevel)/scripts/dev/lint-changed.sh" --staged || exit 1
EOF
    exit 1
fi

state=installed
is_ours && state=refreshed

cat > "$hook" <<EOF
#!/usr/bin/env bash
# ${marker} — written by scripts/dev/install-hooks.sh (make hooks).
# Runs the changed-file lints over the staged files before each commit.
#   skip once:  git commit --no-verify
#   remove:     make hooks-remove
top="\$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
# A repository without the dispatcher (a shared core.hooksPath) has nothing
# for this hook to run.
[ -x "\$top/scripts/dev/lint-changed.sh" ] || exit 0
exec "\$top/scripts/dev/lint-changed.sh" --staged
EOF
chmod +x "$hook" || exit 1

echo "install-hooks: ${state} ${hook}"
echo "  runs:   scripts/dev/lint-changed.sh --staged before each commit"
echo "  skip:   git commit --no-verify"
echo "  remove: make hooks-remove"
