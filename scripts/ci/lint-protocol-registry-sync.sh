#!/usr/bin/env bash
# Cross-check the frontend protocol registry against the Go source-of-truth.
#
# internal/api/v1/protocols_registry.go is authoritative for the protocol set.
# web/explorer/src/app/protocols/registry.ts mirrors the NAME set so the Next.js
# static export knows which /protocols/{name} slugs to pre-render. The two are
# hand-maintained and — until this lint — nothing cross-checked them, so a
# Go-registered protocol could silently have no pre-rendered explorer page
# (a 404), exactly the sorocredit gap found 2026-07-07.
#
# Fails if the two name sets disagree. blend_backstop is intentionally NOT a
# top-level Go registry Name (it folds into the blend entry via
# ExtraEventSources), so it never appears in either list.
set -euo pipefail
cd "$(dirname "$0")/../.."

GO_REG=internal/api/v1/protocols_registry.go
TS_REG=web/explorer/src/app/protocols/registry.ts

go_names=$(grep -oE 'Name:[[:space:]]+"[a-z-]+"' "$GO_REG" | grep -oE '"[a-z-]+"' | tr -d '"' | sort -u)
ts_names=$(grep -oE "name: '[a-z-]+'" "$TS_REG" | grep -oE "'[a-z-]+'" | tr -d "'" | sort -u)

missing_in_ts=$(comm -23 <(echo "$go_names") <(echo "$ts_names") || true)
stale_in_ts=$(comm -13 <(echo "$go_names") <(echo "$ts_names") || true)

rc=0
if [ -n "$missing_in_ts" ]; then
	echo "FAIL: protocol(s) in $GO_REG but MISSING from $TS_REG"
	echo "      (the static export won't pre-render their /protocols/{name} page → 404):"
	echo "$missing_in_ts" | sed 's/^/        - /'
	rc=1
fi
if [ -n "$stale_in_ts" ]; then
	echo "FAIL: protocol(s) in $TS_REG but NOT in $GO_REG (stale slug — remove it):"
	echo "$stale_in_ts" | sed 's/^/        - /'
	rc=1
fi

if [ "$rc" -eq 0 ]; then
	echo "protocol registry sync OK ($(echo "$go_names" | grep -c . ) protocols in both)"
fi
exit "$rc"
