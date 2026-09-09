#!/usr/bin/env bash
# Cross-check the frontend's hand-maintained name sets against the Go
# source-of-truth registries they mirror.
#
# §1 protocol directory. internal/api/v1/protocols_registry.go is
# authoritative for the protocol set. web/explorer/src/app/protocols/registry.ts
# mirrors the NAME set so the Next.js static export knows which
# /protocols/{name} slugs to pre-render. The two are hand-maintained and —
# until this lint — nothing cross-checked them, so a Go-registered protocol
# could silently have no pre-rendered explorer page (a 404), exactly the
# sorocredit gap found 2026-07-07.
#
# §2 DEX pages. internal/sources/external/registry.go is authoritative for
# which sources are Class=Exchange Subclass=DEX. TWO frontend maps mirror
# that set by hand, and BOTH are load-bearing:
#   - DEX_INFO in dexes/[source]/page.tsx feeds generateStaticParams, so a
#     missing key means /dexes/<source> is not pre-rendered and 404s;
#   - ALL_DEXES in dexes/DexesView.tsx is the source-filter chip row, so a
#     missing entry means the venue cannot be filtered for.
# The 404 is not contained to a click, either: sitemap.ts emits
# /dexes/<name> for EVERY subclass=dex source straight off the API, so a
# Go-registered DEX with no DEX_INFO key publishes a known-broken URL in
# the sitemap — the exact failure that map's own comment was written to
# stop. sushiswap_v3 shipped that way (#350): registered, serving trades,
# listed in the live sitemap, 404 on the page.
#
# Fails if any pair of sets disagrees.
#
# blend_backstop is intentionally NOT a top-level Go protocol registry Name
# (it folds into the blend entry via ExtraEventSources), so it never appears
# in either §1 list.
set -euo pipefail
cd "$(dirname "$0")/../.."

rc=0

# diff_sets NAME_A LIST_A NAME_B LIST_B MISSING_MSG STALE_MSG
# Reports both drift directions between two newline-separated sorted sets.
diff_sets() {
	local a_name="$1" a="$2" b_name="$3" b="$4" missing_msg="$5" stale_msg="$6"
	local missing stale
	missing=$(comm -23 <(echo "$a") <(echo "$b") || true)
	stale=$(comm -13 <(echo "$a") <(echo "$b") || true)
	if [ -n "$missing" ]; then
		echo "FAIL: name(s) in $a_name but MISSING from $b_name"
		echo "      ($missing_msg):"
		bullets "$missing"
		rc=1
	fi
	if [ -n "$stale" ]; then
		echo "FAIL: name(s) in $b_name but NOT in $a_name ($stale_msg):"
		bullets "$stale"
		rc=1
	fi
}

# bullets prints one indented list item per line of its argument.
bullets() {
	local line
	while IFS= read -r line; do
		echo "        - $line"
	done <<<"$1"
}

# ─── §1 protocol directory ──────────────────────────────────────────
GO_REG=internal/api/v1/protocols_registry.go
TS_REG=web/explorer/src/app/protocols/registry.ts

# Character class covers every shape a source name takes in this repo:
# plain (comet), hyphenated (reflector-cex), underscored + numbered
# (sushiswap_v3). A narrower class silently drops a name from BOTH captures,
# which passes the comm-diff while cross-checking nothing.
go_names=$(grep -oE 'Name:[[:space:]]+"[a-z0-9_-]+"' "$GO_REG" | grep -oE '"[a-z0-9_-]+"' | tr -d '"' | sort -u)
ts_names=$(grep -oE "name: '[a-z0-9_-]+'" "$TS_REG" | grep -oE "'[a-z0-9_-]+'" | tr -d "'" | sort -u)

diff_sets "$GO_REG" "$go_names" "$TS_REG" "$ts_names" \
	"the static export won't pre-render their /protocols/{name} page → 404" \
	"stale slug — remove it"

# ─── §2 DEX pages ───────────────────────────────────────────────────
SRC_REG=internal/sources/external/registry.go
DEX_PAGE='web/explorer/src/app/dexes/[source]/page.tsx'
DEX_VIEW=web/explorer/src/app/dexes/DexesView.tsx

for f in "$SRC_REG" "$DEX_PAGE" "$DEX_VIEW"; do
	[ -f "$f" ] || {
		echo "FAIL: required input missing: $f (a rename would silently skip this check)"
		exit 1
	}
done

# gofmt aligns the map values, so the run of spaces after the key's colon
# is variable-width — match it rather than a single space.
dex_go=$(grep -oE '"[a-z0-9_-]+":[[:space:]]*\{Class: ClassExchange, Subclass: SubclassDEX' "$SRC_REG" |
	grep -oE '^"[a-z0-9_-]+"' | tr -d '"' | sort -u)

# Range-scoped so an unrelated object literal elsewhere in the file cannot
# contribute a key: DEX_INFO's entries are the only 2-space-indented
# `<name>: {` lines between its declaration and the closing `};`.
dex_info=$(awk '/^const DEX_INFO/{inblock=1} inblock && /^};/{exit} inblock' "$DEX_PAGE" |
	grep -oE '^  [a-z0-9_-]+: \{' | grep -oE '[a-z0-9_-]+' | sort -u)

# ALL_DEXES is a single array literal; prettier may wrap it over several
# lines, so read to the closing `];` rather than assuming one line.
all_dexes=$(awk '/^const ALL_DEXES/{inblock=1} inblock{print} inblock && /\];/{exit}' "$DEX_VIEW" |
	grep -oE "'[a-z0-9_-]+'" | tr -d "'" | sort -u)

for names in "$dex_go" "$dex_info" "$all_dexes"; do
	[ -n "$names" ] || {
		echo "FAIL: a DEX name set extracted EMPTY — the source formatting moved and this lint is"
		echo "      cross-checking nothing. Re-aim the extraction in $0 before trusting a pass."
		exit 1
	}
done

diff_sets "$SRC_REG (Subclass=DEX)" "$dex_go" "$DEX_PAGE (DEX_INFO)" "$dex_info" \
	"generateStaticParams won't pre-render /dexes/<source>, so the page 404s while sitemap.ts still publishes the URL" \
	"stale slug — no such DEX source"

diff_sets "$SRC_REG (Subclass=DEX)" "$dex_go" "$DEX_VIEW (ALL_DEXES)" "$all_dexes" \
	"the venue has no source-filter chip on /dexes" \
	"stale chip — no such DEX source"

if [ "$rc" -eq 0 ]; then
	echo "protocol registry sync OK ($(echo "$go_names" | grep -c .) protocols in both;" \
		"$(echo "$dex_go" | grep -c .) DEX sources in all three)"
fi
exit "$rc"
