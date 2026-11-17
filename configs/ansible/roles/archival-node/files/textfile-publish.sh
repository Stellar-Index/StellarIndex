#!/bin/sh
# textfile-publish <name> -- <cmd> [args...] — runs <cmd> and publishes its
# stdout as <name>.prom in the node_exporter textfile dir. The publisher runs
# the producer itself so a crash cannot hide behind a pipe's exit status: a
# non-zero exit or empty output fails the unit and leaves the previous file
# to age into the staleness alert. The temp is created exclusively and
# renamed into place, so a link planted at the target name is replaced,
# never written through.
set -eu
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
usage() { echo "usage: textfile-publish <name> -- <cmd> [args...]" >&2; exit 2; }
case "${1:-}" in
  '' | *[!A-Za-z0-9_-]*) usage ;;
esac
name=$1
shift
[ "${1:-}" = "--" ] || usage
shift
[ $# -gt 0 ] || usage
tmp=$(mktemp "$DIR/$name.prom.XXXXXX")
trap 'rm -f "$tmp"' EXIT
if ! "$@" > "$tmp"; then
  echo "textfile-publish: $name: producer failed; keeping the previous $name.prom" >&2
  exit 1
fi
if [ ! -s "$tmp" ]; then
  echo "textfile-publish: $name: producer wrote nothing; keeping the previous $name.prom" >&2
  exit 1
fi
# mktemp creates 0600 and node_exporter silently skips a file it cannot read.
chmod 644 "$tmp"
mv -T "$tmp" "$DIR/$name.prom"
