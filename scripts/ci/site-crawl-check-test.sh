#!/usr/bin/env bash
# site-crawl-check-test.sh — fixture tests for scripts/ci/site-crawl-check.sh.
#
# The target script talks straight to production over `curl` and runs
# read-only against a live site, so it's never exercised on a PR — the
# only place these defects are provable is a fake `curl` on PATH that
# answers from a fixture manifest instead of the network.
#
# F136: an unfetchable/unparseable issuer listing left KEYS empty, the
# closure loop ran zero times, and the script printed ALL CHECKS PASSED
# instead of failing.
# T328/T300: the sitemap/hub sample can never reach a CF Pages
# shell-fallback Function's long-tail branch (sitemap.ts excludes those
# URLs by design) or the og image Function, so a broken one went unnoticed.
# Q225: nothing compared the deployed BUILD_SHA against main or measured
# its wall-clock age.
#
# Run: bash scripts/ci/site-crawl-check-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/site-crawl-check.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"

pass=0
fail=0

SITE="https://example-site.test"
API="https://example-api.test"

# A 40-hex-char commit SHA fixture — not a credential, just satisfies the
# git-SHA shape the build badge parser looks for.
FRESH_SHA="abc1234567890abc1234567890abc1234567890a" # gitleaks:allow — fake git SHA, not a secret

# Fake curl: reads a JSON fixture manifest ($MANIFEST) and answers by URL
# instead of hitting the network. Mirrors the three call shapes
# site-crawl-check.sh actually uses: a plain body fetch (`-sfL`), an
# http_code probe (`-w %{http_code}`), and a content-type probe
# (`-w %{content_type}`).
cat >"$TMP/bin/curl" <<'PYEOF'
#!/usr/bin/env python3
import json, os, sys

args = sys.argv[1:]
url = args[-1]
with open(os.environ["MANIFEST"]) as f:
    manifest = json.load(f)

entry = next((m for m in manifest["exact"] if m["url"] == url), None)
if entry is None:
    entry = next((m for m in manifest["contains"] if m["match"] in url), None)
if entry is None:
    entry = manifest["default"]

mode = "body"
if "-w" in args:
    w = args[args.index("-w") + 1]
    if "http_code" in w:
        mode = "code"
    elif "content_type" in w:
        mode = "ctype"

status = entry.get("status", 200)
if mode == "code":
    sys.stdout.write(str(status))
elif mode == "ctype":
    sys.stdout.write(entry.get("content_type", "text/html"))
else:
    sys.stdout.write(entry.get("body", ""))
    if status >= 400 and "-f" in args:
        sys.exit(22)
sys.exit(0)
PYEOF
chmod +x "$TMP/bin/curl"

# build_manifest — writes $TMP/manifest.json, defaulting to a fully
# healthy fixture set for anything a test case didn't override. Every
# default fills in below in Python so bash never has to embed JSON
# literals in a parameter expansion.
build_manifest() {
  SITE="$SITE" API="$API" FRESH_SHA="$FRESH_SHA" \
    ISSUERS_BODY="${ISSUERS_BODY-}" \
    OG_STATUS="${OG_STATUS-}" \
    OG_CONTENT_TYPE="${OG_CONTENT_TYPE-}" \
    HOME_BADGE_TIME="${HOME_BADGE_TIME-}" \
    COMPARE_BODY="${COMPARE_BODY-}" \
    PROBE_OVERRIDE_MATCH="${PROBE_OVERRIDE_MATCH-}" \
    PROBE_OVERRIDE_STATUS="${PROBE_OVERRIDE_STATUS-}" \
    MANIFEST="$TMP/manifest.json" \
    python3 - <<'PYEOF'
import datetime, json, os

SITE = os.environ["SITE"]
API = os.environ["API"]
sha = os.environ["FRESH_SHA"]
time = os.environ["HOME_BADGE_TIME"] or datetime.datetime.now(
    datetime.timezone.utc
).strftime("%Y-%m-%dT%H:%M:%SZ")

assets_rows = [{"asset_id": f"a{i}", "slug": f"asset{i}"} for i in range(60)]
issuers_default = json.dumps(
    {"data": [{"g_strkey": "GISSUERONEXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"}]}
)
home_html = (
    "<html><head><title>Stellar Index</title></head><body>"
    f'<footer><a title="Built {time} from commit {sha}" '
    f'href="https://github.com/x/commit/{sha}">build {sha[:8]}</a></footer>'
    "</body></html>"
)
compare_default = json.dumps({"status": "identical", "ahead_by": 0})

exact = [
    {"url": f"{SITE}/sitemap.xml", "body": f"<urlset><url><loc>{SITE}/assets/</loc></url></urlset>"},
    {"url": f"{SITE}/", "body": home_html},
    {
        "url": f"{API}/v1/assets?asset_class=all&limit=100",
        "body": json.dumps({"data": assets_rows}),
    },
    {
        "url": f"{API}/v1/issuers?limit=20",
        "body": os.environ["ISSUERS_BODY"] or issuers_default,
    },
    {
        "url": f"{SITE}/og/assets/native",
        "status": int(os.environ["OG_STATUS"] or "200"),
        "content_type": os.environ["OG_CONTENT_TYPE"] or "image/png",
    },
]

contains = []
if os.environ["PROBE_OVERRIDE_MATCH"]:
    contains.append(
        {
            "match": os.environ["PROBE_OVERRIDE_MATCH"],
            "status": int(os.environ["PROBE_OVERRIDE_STATUS"] or "404"),
            "body": "",
        }
    )
contains += [
    {"match": f"{API}/v1/issuers/", "status": 200, "body": "{}"},
    {
        "match": "api.github.com/repos/Stellar-Index/StellarIndex/compare/",
        "status": 200,
        "body": os.environ["COMPARE_BODY"] or compare_default,
    },
]

manifest = {
    "exact": exact,
    "contains": contains,
    "default": {
        "status": 200,
        "content_type": "text/html",
        "body": "<html><title>ok</title></html>",
    },
}

with open(os.environ["MANIFEST"], "w") as f:
    json.dump(manifest, f)
PYEOF
}

# run — invoke the real script against the fixture manifest.
run() {
  build_manifest
  OUT="$(PATH="$TMP/bin:$PATH" MANIFEST="$TMP/manifest.json" SITE="$SITE" API="$API" bash "$CHECK" 2>&1)"
  RC=$?
}

expect_pass() {
  local name="$1"
  if [ "$RC" -eq 0 ] && grep -q 'ALL CHECKS PASSED' <<<"$OUT"; then
    echo "ok: $name"
    pass=$((pass + 1))
  else
    echo "FAIL: $name — exit $RC" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
  fi
}

expect_fail() {
  local name="$1" want_sub="$2"
  if [ "$RC" -ne 0 ] && grep -q -- "$want_sub" <<<"$OUT"; then
    echo "ok: $name"
    pass=$((pass + 1))
  else
    echo "FAIL: $name — exit $RC, wanted a failure mentioning '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
  fi
}

# --- happy path: every section (including the new 6/7/8) passes clean ---
unset ISSUERS_BODY OG_STATUS OG_CONTENT_TYPE COMPARE_BODY PROBE_OVERRIDE_MATCH PROBE_OVERRIDE_STATUS HOME_BADGE_TIME
run
expect_pass "healthy site: ALL CHECKS PASSED"

# --- F136: an empty/unparseable issuer listing must FAIL, not silently
# skip the closure loop ---
ISSUERS_BODY="not json" run
expect_fail "F136: unparseable issuer listing fails the check" \
  "issuer listing unfetchable or unparseable"
unset ISSUERS_BODY

# --- T328/T300: a broken long-tail shell (CF Pages Function fallback)
# must be caught — the sitemap/hub sample structurally cannot reach it ---
PROBE_OVERRIDE_MATCH="/accounts/G" PROBE_OVERRIDE_STATUS=404 run
expect_fail "T328: a 404 long-tail shell fallback is caught" \
  "CF Pages Function fallback broken"
unset PROBE_OVERRIDE_MATCH PROBE_OVERRIDE_STATUS

# --- T300: the og image Function returning non-image content is caught ---
OG_CONTENT_TYPE="text/html" run
expect_fail "T300: a non-image og response is caught" \
  "is not an image"
unset OG_CONTENT_TYPE

# --- Q225: a deploy far behind main is caught ---
COMPARE_BODY='{"status":"ahead","ahead_by":9001}' run
expect_fail "Q225: a deploy far behind main is caught" \
  "commit(s) behind main"
unset COMPARE_BODY

# --- Q225: a stale (wall-clock-old) deploy is caught even when the
# commit itself still resolves cleanly against main ---
HOME_BADGE_TIME="2020-01-01T00:00:00Z" run
expect_fail "Q225: a wall-clock-stale deploy is caught" \
  "freshness budget"
unset HOME_BADGE_TIME

echo
echo "site-crawl-check-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
