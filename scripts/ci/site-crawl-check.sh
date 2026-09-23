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
  "/lending/C${PROBE_ID}" \
  "/sources/${PROBE_ID}" \
  "/external/assets/${PROBE_ID}" \
  "/embed/asset/${PROBE_ID}" \
  "/embed/currency/${PROBE_ID}" \
  "/embed/pair/native~${PROBE_ID}" \
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

echo "== 8. deployed build freshness (BUILD_SHA vs main, wall-clock age)"
# Q225: nothing compared the deployed build against main or measured its
# age. The explorer's own footer (BuildBadge, components/nav/Footer.tsx)
# stamps `title="Built <ISO time> from commit <sha>"` — read it back and
# check it two ways: the commit is actually an ancestor of main (via
# GitHub's compare API, read-only/unauthenticated), and the build isn't
# older than the freshness budget below. Catches the exact failure mode
# explorer-deploy.yml's own header names as a reason it exists: "CF git
# integration paused" silently freezing the live site.
HOME_HTML=$(fetch "$SITE/" || true)
BADGE=$(grep -oE 'title="Built [^"]*"' <<<"$HOME_HTML" | awk 'NR==1' || true)
if [ -z "$BADGE" ]; then
  fail "no build badge on $SITE/ — can't verify deploy freshness (BuildBadge renders nothing when NEXT_PUBLIC_BUILD_SHA is unset)"
else
  DEPLOY_SHA=$(grep -oE '[0-9a-fA-F]{40}' <<<"$BADGE" | awk 'NR==1' || true)
  DEPLOY_TIME=$(sed -E 's/.*Built ([^ ]+) from commit.*/\1/' <<<"$BADGE")
  if [ -z "$DEPLOY_SHA" ]; then
    fail "build badge present but no parseable 40-char commit SHA: $BADGE"
  else
    COMPARE=$(fetch "https://api.github.com/repos/Stellar-Index/StellarIndex/compare/$DEPLOY_SHA...main" || true)
    read -r CMP_STATUS CMP_AHEAD <<<"$(python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
    print(d.get("status", "unknown"), d.get("ahead_by", -1))
except Exception:
    print("unknown", -1)
' <<<"$COMPARE")"
    case "$CMP_STATUS" in
      identical | behind) ;; # deployed sha IS main, or main hasn't caught up to it yet
      ahead)
        # CF auto-deploys on every push (explorer-deploy.yml); a small lag
        # is normal mid-build. Budget is generous so one CF build window
        # never false-positives, but still catches a stuck deploy inside
        # the same weekly crawl run. Tune via STALE_COMMIT_BUDGET.
        [ "$CMP_AHEAD" -le "${STALE_COMMIT_BUDGET:-200}" ] ||
          fail "deployed build $DEPLOY_SHA is $CMP_AHEAD commit(s) behind main — auto-deploy may be stuck (docs/operations/explorer-deployment.md)"
        ;;
      *)
        fail "could not resolve deployed commit $DEPLOY_SHA against main via GitHub compare (status: $CMP_STATUS)"
        ;;
    esac
  fi
  if [ -n "$DEPLOY_TIME" ]; then
    DEPLOY_EPOCH=$(python3 -c '
import sys, datetime
try:
    print(int(datetime.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00")).timestamp()))
except Exception:
    print(0)
' "$DEPLOY_TIME")
    if [ "$DEPLOY_EPOCH" -gt 0 ]; then
      NOW_EPOCH=$(date +%s)
      AGE_HOURS=$(((NOW_EPOCH - DEPLOY_EPOCH) / 3600))
      # 72h ≈ 10x CF Pages' normal build latency (minutes) — generous
      # enough not to flag a slow build, tight enough to still be caught
      # inside the weekly crawl window rather than a full week later.
      # Tune via STALE_HOURS_BUDGET.
      [ "$AGE_HOURS" -le "${STALE_HOURS_BUDGET:-72}" ] ||
        fail "deployed build is ${AGE_HOURS}h old (built $DEPLOY_TIME) — exceeds the ${STALE_HOURS_BUDGET:-72}h freshness budget"
    fi
  fi
fi

echo
if [ "$FAILURES" -gt 0 ]; then
  echo "site-crawl-check: $FAILURES failure(s)." >&2
  exit 1
fi
echo "site-crawl-check: ALL CHECKS PASSED"
