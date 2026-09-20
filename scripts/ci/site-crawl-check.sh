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
  grep -qE '>undefined<|>NaN<|\[object Object\]' <<<"$HTML" &&
    fail "$path contains placeholder text"
  grep -q '· Stellar Index · Stellar Index' <<<"$HTML" &&
    fail "$path has a doubled title suffix"
done

echo "== 3. canonicals never double-encode (the %253A incident)"
PAIR_URL=$(echo "$SITEMAP" | grep -oE '<loc>[^<]*/markets/[^<]+</loc>' | awk 'NR==1' | sed 's/<[^>]*>//g')
if [ -n "$PAIR_URL" ]; then
  CANON=$(fetch "$PAIR_URL" | grep -oE '<link rel="canonical" href="[^"]+"' | awk 'NR==1' || true)
  grep -q '%25' <<<"$CANON" && fail "market canonical double-encoded: $CANON"
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
# A fetch/parse failure and a genuinely empty issuer listing both leave
# KEYS empty — the for loop below then iterates zero times and reports
# nothing, so a broken listing endpoint silently skipped this whole
# check instead of failing it (F136).
[ -n "$KEYS" ] || fail "issuer listing unfetchable or unparseable (0 keys) — cannot run the list↔detail closure check"
for g in $KEYS; do
  CODE=$(status_of "$API/v1/issuers/$g")
  [ "$CODE" = "200" ] || fail "listed issuer $g → detail HTTP $CODE"
done

echo "== 6. long-tail shell fallback (structurally unreached by the sitemap sample)"
# T328: sitemap.ts deliberately excludes per-entity long tails ("unbounded
# long tails served as noindex shells") so section 1's sitemap sample can
# never land on a CF Pages Function's fallback branch. Probe one synthetic
# id per shell-fallback Function directly (functions/*/[[path]].js): each
# must answer 200 with its shell document (never a hard 404), and the
# shell itself must clear the same red flags as section 2.
PROBE_ID="sitecrawlprobe$(date +%s)"
for path in \
  "/assets/${PROBE_ID}" \
  "/accounts/G${PROBE_ID}" \
  "/contracts/C${PROBE_ID}" \
  "/ledgers/9${PROBE_ID}" \
  "/markets/native~${PROBE_ID}" \
  "/issuers/G${PROBE_ID}" \
  "/transactions/${PROBE_ID}" \
  "/insights/sponsors/G${PROBE_ID}" \
  "/insights/creators/G${PROBE_ID}" \
  ; do
  CODE=$(status_of "$SITE$path")
  [ "$CODE" = "200" ] || fail "long-tail shell $path → HTTP $CODE (CF Pages Function fallback broken)"
  HTML=$(fetch "$SITE$path" || true)
  if [ -n "$HTML" ]; then
    grep -qE '>undefined<|>NaN<|\[object Object\]' <<<"$HTML" &&
      fail "$path (shell) contains placeholder text"
    grep -q '· Stellar Index · Stellar Index' <<<"$HTML" &&
      fail "$path (shell) has a doubled title suffix"
  fi
done

echo "== 7. og image Function"
# T300: the 8th CF Pages Function (functions/og/[[path]].js) renders a PNG,
# not HTML, so it needs its own shape check rather than section 2's markup
# greps.
OG_PATH="/og/assets/native"
OG_CODE=$(status_of "$SITE$OG_PATH")
[ "$OG_CODE" = "200" ] || fail "$OG_PATH → HTTP $OG_CODE"
OG_CTYPE=$(curl -s -o /dev/null --max-time 30 -w '%{content_type}' "$SITE$OG_PATH")
case "$OG_CTYPE" in
  image/*) ;;
  *) fail "$OG_PATH content-type '$OG_CTYPE' is not an image" ;;
esac

echo
if [ "$FAILURES" -gt 0 ]; then
  echo "site-crawl-check: $FAILURES failure(s)." >&2
  exit 1
fi
echo "site-crawl-check: ALL CHECKS PASSED"
