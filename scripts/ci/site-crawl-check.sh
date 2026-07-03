#!/usr/bin/env bash
# site-crawl-check.sh — the site-audit recurring guard (2026-07-03).
#
# The July 2026 site audit found a CLASS of silent rot: pages 404ing
# from the site's own links, canonicals pointing at dead URLs,
# placeholder text baked into HTML, doubled title suffixes, and a
# curated sliver presented as the asset universe. Each check below
# pins one of those incidents so the class stays closed.
#
# Runs read-only against production. Exit != 0 on any regression.
set -eu
# NOT pipefail: the sitemap greps feed head/awk which close the pipe
# early — SIGPIPE would read as failure (exit 141).

SITE="${SITE:-https://stellarindex.io}"
API="${API:-https://api.stellarindex.io}"
FAILURES=0

fail() {
  echo "FAIL: $1" >&2
  FAILURES=$((FAILURES + 1))
}

fetch() { curl -sfL --max-time 30 "$1"; }
status_of() { curl -s -o /dev/null -w "%{http_code}" --max-time 30 "$1"; }

echo "== 1. sitemap sample resolves (one URL per path family)"
SITEMAP=$(fetch "$SITE/sitemap.xml" || true)
[ -n "$SITEMAP" ] || fail "sitemap.xml unfetchable"
SAMPLE=$(echo "$SITEMAP" | grep -oE '<loc>[^<]+</loc>' | sed 's/<[^>]*>//g' |
  awk -F/ '{print $4}' | sort -u | awk 'NR<=40')
for family in $SAMPLE; do
  URL=$(echo "$SITEMAP" | grep -oE "<loc>$SITE/$family/[^<]*</loc>" | awk 'NR==1' | sed 's/<[^>]*>//g')
  [ -z "$URL" ] && URL="$SITE/$family/"
  CODE=$(status_of "$URL")
  [ "$CODE" = "200" ] || fail "family $family: $URL → HTTP $CODE"
done

echo "== 2. HTML red flags on key pages"
for path in / /assets/ /issuers/ /markets/ /contracts/ /transactions/ /protocols/ /dexes/ /lending/; do
  HTML=$(fetch "$SITE$path" || true)
  [ -n "$HTML" ] || { fail "$path unfetchable"; continue; }
  echo "$HTML" | grep -qE '>undefined<|>NaN<|\[object Object\]' &&
    fail "$path contains placeholder text"
  echo "$HTML" | grep -q '· Stellar Index · Stellar Index' &&
    fail "$path has a doubled title suffix"
done

echo "== 3. canonicals never double-encode (the %253A incident)"
PAIR_URL=$(echo "$SITEMAP" | grep -oE '<loc>[^<]*/markets/[^<]+</loc>' | awk 'NR==1' | sed 's/<[^>]*>//g')
if [ -n "$PAIR_URL" ]; then
  CANON=$(fetch "$PAIR_URL" | grep -oE '<link rel="canonical" href="[^"]+"' | awk 'NR==1' || true)
  echo "$CANON" | grep -q '%25' && fail "market canonical double-encoded: $CANON"
  CANON_URL=$(echo "$CANON" | grep -oE 'https[^"]+' || true)
  if [ -n "$CANON_URL" ]; then
    CODE=$(status_of "$CANON_URL")
    [ "$CODE" = "200" ] || fail "market canonical target dead: $CANON_URL → $CODE"
  fi
fi

echo "== 4. assets census (page 1 fills beyond the catalogue)"
COUNT=$(fetch "$API/v1/assets?asset_class=all&limit=100" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["data"]))' || echo 0)
[ "$COUNT" -ge 50 ] || fail "unified /v1/assets page 1 returned $COUNT rows (fill regression — the 11-asset bug)"

echo "== 5. issuer list↔detail closure (sample 20)"
KEYS=$(fetch "$API/v1/issuers?limit=20" | python3 -c 'import json,sys; [print(r["g_strkey"]) for r in json.load(sys.stdin)["data"]]' || true)
for g in $KEYS; do
  CODE=$(status_of "$API/v1/issuers/$g")
  [ "$CODE" = "200" ] || fail "listed issuer $g → detail HTTP $CODE"
done

echo
if [ "$FAILURES" -gt 0 ]; then
  echo "site-crawl-check: $FAILURES failure(s)." >&2
  exit 1
fi
echo "site-crawl-check: ALL CHECKS PASSED"
