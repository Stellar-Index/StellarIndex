#!/usr/bin/env bash
# Turns an explorer static export into the status.stellarindex.io build: the
# same pages and chrome, with `/` served (200, not a redirect) by /status.
# Used by status-page.yml and the stellarindex-status Pages project build.
set -euo pipefail
out="${1:?usage: status-pages-redirects.sh <explorer out dir>}"
test -f "$out/status/index.html" || { echo "$out/status/index.html missing" >&2; exit 1; }
# CF wants static rules before dynamic ones: `/` first, the splat last.
{
  printf '/  /status/  200\n'
  cat "$out/_redirects"
  printf '/incident/*  /status/incident/:splat  301\n'
} > "$out/_redirects.new"
mv "$out/_redirects.new" "$out/_redirects"
