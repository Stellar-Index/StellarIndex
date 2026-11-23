# shellcheck shell=bash
# gnu-mv-shim.sh — `mv -T` for tests of root textfile publishers on BSD (macOS) mv.
#
# GNU `mv -T SRC DST` refuses a real directory at DST and renames over a
# symlink at DST (the link is replaced, its target untouched). BSD mv has no
# -T; `mv -h` gives the same no-follow rename. On GNU mv nothing is installed.
#
# Source: . "<repo>/scripts/ci/lib/gnu-mv-shim.sh"; gnu_mv_shim "$BIN"  (BIN on PATH)

gnu_mv_shim() {
  [[ "$(mv --version 2>/dev/null)" == *GNU* ]] && return 0
  mkdir -p "$1"
  cat > "$1/mv" <<'MV'
#!/usr/bin/env bash
[ "$1" = -T ] || exec /bin/mv "$@"
shift; [ "$1" = -- ] && shift
if [ -d "$2" ] && [ ! -L "$2" ]; then echo "mv: cannot overwrite directory '$2'" >&2; exit 1; fi
exec /bin/mv -h -- "$1" "$2"
MV
  chmod +x "$1/mv"
}
