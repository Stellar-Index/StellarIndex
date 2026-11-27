# shellcheck shell=bash
# selftest-tree.sh — run a gate self-test inside a throwaway git copy of the
# tree, so no fixture ever lands in the real working tree, index or history.
# A parallel lane walking the real tree saw fixtures appear and vanish, an
# interrupted run left debris, and restore-from-backup discarded a concurrent edit.
#
# Source: . "$(dirname "$0")/lib/selftest-tree.sh"
# then:   selftest_tree <name> [<path the copy must contain>...]
# which cds into the copy and sets SELFTEST_SRC (the real tree), SELFTEST_TMP
# (scratch dir, removed on EXIT; put temp files here) and SELFTEST_TREE.

# A git hook exports GIT_DIR/GIT_INDEX_FILE; left set, the copy's `git init`,
# `git add` and any fixture commit would act on the REAL repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

# Copy the files `git ls-files -z <args>` names from $SELFTEST_SRC into the copy.
# A file deleted but not yet staged is skipped (tar warns, so its stderr is dropped).
_selftest_copy() {
  git -C "$SELFTEST_SRC" ls-files -z "$@" |
    (cd "$SELFTEST_SRC" && tar --null -T - -cf - 2>/dev/null) |
    tar -xf - -C "$SELFTEST_TREE"
}

selftest_tree() {
  local name="$1" f
  shift
  SELFTEST_SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)" || exit 1
  SELFTEST_TMP="$(mktemp -d)" || { echo "$name: mktemp -d failed" >&2; exit 1; }
  # shellcheck disable=SC2064  # expand now: the path is fixed for this run
  trap "rm -rf '$SELFTEST_TMP'" EXIT
  SELFTEST_TREE="$SELFTEST_TMP/tree"
  mkdir -p "$SELFTEST_TREE"
  # Tracked files are staged in the copy and untracked non-ignored ones are
  # not, so a gate that reads only the index sees what it sees in the real tree.
  # tar keeps modes; the copy must be writable even where the source is not.
  _selftest_copy -c
  if ! { chmod -R u+w "$SELFTEST_TREE" && git -C "$SELFTEST_TREE" init -q &&
    git -C "$SELFTEST_TREE" add -A; }; then
    echo "$name: could not build the fixture repo in $SELFTEST_TREE" >&2
    exit 1
  fi
  _selftest_copy -o --exclude-standard
  chmod -R u+w "$SELFTEST_TREE"
  for f in .gitignore "$@"; do
    if [ ! -e "$SELFTEST_TREE/$f" ]; then
      echo "$name: could not copy the tree into $SELFTEST_TREE (no $f)" >&2
      exit 1
    fi
  done
  cd "$SELFTEST_TREE" || exit 1
}
