#!/bin/sh
# textfile-publish <name> — publishes stdin as <name>.prom in the node_exporter
# textfile dir. The temp is created exclusively and renamed into place, so a
# link planted at the target name is replaced, never written through.
set -eu
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
case "${1:-}" in
  '' | *[!A-Za-z0-9_-]*) echo "usage: textfile-publish <name>" >&2; exit 2 ;;
esac
tmp=$(mktemp "$DIR/$1.prom.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
# mktemp creates 0600 and node_exporter silently skips a file it cannot read.
chmod 644 "$tmp"
mv -T "$tmp" "$DIR/$1.prom"
