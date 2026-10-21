#!/usr/bin/env bash
# verify-cross-region.sh — assert closed-bucket VWAP values are
# byte-identical across R1 / R2 / R3 (per ADR-0015).
#
# api.stellarindex.io resolves to the nearest region, but the same closed
# bucket + pair must return the same price everywhere; otherwise ADR-0015 is
# broken and the multi-region story is fiction.
#
# Pre-launch only R1 is deployed (R2/R3: docs/architecture/ha-plan.md §10).
# Skip a region by setting its env var empty (`R2= R3= ./verify-cross-region.sh`).
# Single-region runs report "no regions to compare against" and exit 0:
# failing on absent R2/R3 trains operators to ignore the script.
#
# Usage:
#   ./scripts/dev/verify-cross-region.sh
#       # default pair set, default per-region URLs (only R1)
#   PAIRS='native,fiat:USD;USDC-G...,fiat:USD' \
#     ./scripts/dev/verify-cross-region.sh
#   R1=https://api-r1.stellarindex.io \
#     R2=https://api-r2.stellarindex.io \
#     R3=https://api-r3.stellarindex.io \
#     ./scripts/dev/verify-cross-region.sh
#   R2= R3= ./scripts/dev/verify-cross-region.sh
#     # explicit "R1 only — R2/R3 not deployed yet"
#
# Exit:
#   0 — every available-region pair returned a price AND, with ≥ 2 regions
#       configured, every pair's regional prices are byte-identical.
#   1 — any pair has divergent prices across configured regions.
#   2 — at least one configured region failed to respond for a pair.

set -euo pipefail

# ─── Per-region URLs (override via env) ──────────────────────
# R1 defaults to the public hostname; R2/R3 to empty so pre-multi-region
# deployments skip them rather than fail.
R1="${R1:-https://api.stellarindex.io}"
R2="${R2:-}"
R3="${R3:-}"

# ─── Pair set (override via PAIRS env, semicolon-separated) ──
DEFAULT_PAIRS="native,fiat:USD"
PAIRS="${PAIRS:-${DEFAULT_PAIRS}}"

# ─── Pretty-print helpers ────────────────────────────────────
green() { printf '\033[32m%s\033[0m\n' "$*"; }
red()   { printf '\033[31m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

# ─── Workspace ───────────────────────────────────────────────
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

# ─── Test loop ───────────────────────────────────────────────
PASS=0
FAIL_DIVERGE=0
FAIL_UNREACH=0

bold "▶ verify-cross-region against:"
echo "    R1=${R1:-(unset)}"
echo "    R2=${R2:-(unset — skipped)}"
echo "    R3=${R3:-(unset — skipped)}"
echo

# Single-region runs short-circuit (see header).
CONFIGURED=0
[ -n "$R1" ] && CONFIGURED=$((CONFIGURED + 1))
[ -n "$R2" ] && CONFIGURED=$((CONFIGURED + 1))
[ -n "$R3" ] && CONFIGURED=$((CONFIGURED + 1))
if [ "$CONFIGURED" -lt 2 ]; then
    bold "Only $CONFIGURED region configured."
    echo "Cross-region consistency check needs ≥ 2 regions to compare."
    echo "R2/R3 bringup is tracked in docs/architecture/ha-plan.md §10;"
    echo "until those land, this script is a no-op — passing exit 0."
    exit 0
fi

IFS=';' read -ra PAIR_LIST <<<"$PAIRS"
for pair in "${PAIR_LIST[@]}"; do
    pair="${pair## }"; pair="${pair%% }"   # trim
    [ -z "$pair" ] && continue
    base="${pair%%,*}"
    quote="${pair##*,}"

    bold "Pair: ${base}/${quote}"

# Fetch /v1/price per region. Plain vars: macOS bash 3.2 has no associative
# arrays.
    p1=""; p2=""; p3=""
    a1=""; a2=""; a3=""
    r1ok=no; r2ok=no; r3ok=no

    if ! command -v jq >/dev/null 2>&1; then
        red "  ✗ jq not installed; can't parse responses"
        exit 2
    fi

    for region_label in R1 R2 R3; do
        case "$region_label" in
            R1) host="$R1" ;;
            R2) host="$R2" ;;
            R3) host="$R3" ;;
        esac
# Skip regions not configured.
        if [ -z "$host" ]; then
            continue
        fi
        out_file="${TMPDIR}/${region_label}-${base//\//_}-${quote//\//_}.json"
        http_code=$(curl -sk -o "$out_file" -w "%{http_code}" \
            "${host}/v1/price?base=${base}&quote=${quote}" \
            --max-time 10 || echo 000)

        if [ "$http_code" != "200" ]; then
            red "  ✗ ${region_label} (${host}) returned http=${http_code}"
            FAIL_UNREACH=$((FAIL_UNREACH + 1))
            continue
        fi

        price=$(jq -r '.data.price' "$out_file" 2>/dev/null || echo "")
        as_of_v=$(jq -r '.as_of' "$out_file" 2>/dev/null || echo "")
        if [ -z "$price" ] || [ "$price" = "null" ]; then
            red "  ✗ ${region_label} returned 200 but body has no .data.price"
            FAIL_UNREACH=$((FAIL_UNREACH + 1))
            continue
        fi

        case "$region_label" in
            R1) p1="$price"; a1="$as_of_v"; r1ok=yes ;;
            R2) p2="$price"; a2="$as_of_v"; r2ok=yes ;;
            R3) p3="$price"; a3="$as_of_v"; r3ok=yes ;;
        esac
    done

# Prices from configured regions that answered; unset regions are absent by
# design, not failure.
    collected=()
    [ -n "$R1" ] && [ "$r1ok" = "yes" ] && collected+=("R1=$p1")
    [ -n "$R2" ] && [ "$r2ok" = "yes" ] && collected+=("R2=$p2")
    [ -n "$R3" ] && [ "$r3ok" = "yes" ] && collected+=("R3=$p3")

    expected=0
    [ -n "$R1" ] && expected=$((expected + 1))
    [ -n "$R2" ] && expected=$((expected + 1))
    [ -n "$R3" ] && expected=$((expected + 1))

    if [ "${#collected[@]}" -lt "$expected" ]; then
        red "  → SKIP consistency check — ${#collected[@]}/${expected} configured regions reachable"
        echo
        continue
    fi

# Compare every collected price against the first.
    first_price="${collected[0]#*=}"
    diverged=no
    for entry in "${collected[@]}"; do
        if [ "${entry#*=}" != "$first_price" ]; then
            diverged=yes
            break
        fi
    done

    if [ "$diverged" = "no" ]; then
        green "  ✓ all ${#collected[@]} regions: ${first_price}"
        [ -n "$R1" ] && [ "$r1ok" = "yes" ] && echo "    R1 as_of=${a1}"
        [ -n "$R2" ] && [ "$r2ok" = "yes" ] && echo "    R2 as_of=${a2}"
        [ -n "$R3" ] && [ "$r3ok" = "yes" ] && echo "    R3 as_of=${a3}"
        PASS=$((PASS + 1))
    else
        red "  ✗ DIVERGENCE — closed-bucket consistency violated (ADR-0015):"
        [ -n "$R1" ] && [ "$r1ok" = "yes" ] && echo "    R1: price=${p1}  as_of=${a1}"
        [ -n "$R2" ] && [ "$r2ok" = "yes" ] && echo "    R2: price=${p2}  as_of=${a2}"
        [ -n "$R3" ] && [ "$r3ok" = "yes" ] && echo "    R3: price=${p3}  as_of=${a3}"
        echo "    If as_of timestamps differ across regions by more"
        echo "    than the bucket window (typically 1m), one region"
        echo "    is replication-lagged. Investigate before launch."
        FAIL_DIVERGE=$((FAIL_DIVERGE + 1))
    fi
    echo
done

# ─── Summary ─────────────────────────────────────────────────
echo "─────────────────────────────────────────"
TOTAL=$((PASS + FAIL_DIVERGE + FAIL_UNREACH))
if [ "$FAIL_DIVERGE" -eq 0 ] && [ "$FAIL_UNREACH" -eq 0 ]; then
    green "$PASS / $TOTAL pairs consistent across $CONFIGURED region(s)"
    bold "ADR-0015 closed-bucket consistency holds. ✓"
    exit 0
fi
if [ "$FAIL_DIVERGE" -gt 0 ]; then
    red "$FAIL_DIVERGE pairs DIVERGED — ADR-0015 contract broken."
fi
if [ "$FAIL_UNREACH" -gt 0 ]; then
    red "$FAIL_UNREACH region/pair attempts UNREACHABLE — incomplete check."
    bold "Run again once the affected region is healthy."
fi
echo
bold "Cross-reference docs/architecture/ha-plan.md (edge failover)."
[ "$FAIL_DIVERGE" -gt 0 ] && exit 1
exit 2
