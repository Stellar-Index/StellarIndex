#!/usr/bin/env bash
#
# api-latency-sweep.sh — latency profile of the whole anonymous public GET
# surface: hit every non-streaming, non-auth endpoint N times, compute
# p50/p95/p99/max, rank slowest-first, flag anything over the SLO (p95 < 200 ms)
# and a hard "cause for concern" ceiling (>1 s).
#
# A DIAGNOSTIC, not a contract test (r1-smoke.sh pins behaviour/shape). Non-2xx
# is reported (ok%), never a failure by itself; the exit code counts only
# endpoints over the CRIT ceiling, for cron / Healthchecks.io.
#
# Where you run it decides what it measures:
#   - On the host against localhost → pure server compute
#       ssh root@r1 'bash -s' < scripts/dev/api-latency-sweep.sh
#   - From a VPS against the public URL → + network + edge/Caddy
#       API_BASE_URL=https://api.stellarindex.io bash …api-latency-sweep.sh
#   - Point at r2/r3 the same way → cross-region comparison
#
# Usage / knobs (env):
#   API_BASE_URL   default http://localhost:3000
#   ITERS          samples per endpoint (default 20)
#   SWEEP_TIMEOUT  per-request cap, seconds (default 15)
#   CACHE_BUST=1   unique query param per hit → exposes UNCACHED server cost
#                  (the "should be cached but isn't" signal); off = warm
#   WARMUP=1       discard the first hit per endpoint (prime caches)
#   JSON=1         emit a JSON array instead of the table (cross-region diffing)
#
# Flags:  --spec-check   list OpenAPI GET paths NOT covered here (anti-rot; run
#                        from a repo checkout) then exit
#         --pages        time each explorer PAGE instead: fire its panels' reads
#                        together, as the browser does, and report the page as
#                        its slowest panel. With CACHE_BUST=1 this is the cold
#                        load past the HTTP cache; in-process caches stay warm,
#                        so a fully cold number needs a fresh API process.
#                        ACCOUNTS / CONTRACTS (space-separated strkeys) set the
#                        cohort the account / contract page is loaded for; a
#                        panel's p95 pools every load across the cohort.
#                        TEXTFILE=path writes each panel's and page's p95 as a
#                        node_exporter textfile (written atomically). A panel
#                        answered other than 2xx or 404 counts as a timeout,
#                        so a refused page never reads as fast; a 404 is an
#                        answer the page renders (a SAC has no /wasm).
#
# STELLARINDEX_PROBE_API_KEY, when set, is sent as X-API-Key so a sweep of
# hundreds of reads is not answered by the anonymous per-IP rate limit.
#
set -uo pipefail

API_BASE_URL="${API_BASE_URL:-http://localhost:3000}"
ITERS="${ITERS:-20}"
TIMEOUT="${SWEEP_TIMEOUT:-15}"
CACHE_BUST="${CACHE_BUST:-0}"
WARMUP="${WARMUP:-0}"
JSON="${JSON:-0}"

# SLO p95 target = 200 ms. >1 s = "cause for concern" (CRIT).
WARN_MS=200
CRIT_MS=1000

if [ -t 1 ] && [ "$JSON" != "1" ]; then
  G="$(printf '\033[32m')"; Y="$(printf '\033[33m')"; R="$(printf '\033[31m')"
  B="$(printf '\033[1m')"; D="$(printf '\033[2m')"; O="$(printf '\033[0m')"
else G=""; Y=""; R=""; B=""; D=""; O=""; fi

# A real pubnet asset with history (the USDC issuer r1-smoke.sh uses) and its
# issuer strkey for the issuer route.
U="USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
ISS="GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
ACCOUNTS="${ACCOUNTS:-$ISS}"
CONTRACTS="${CONTRACTS:-CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH}"
TEXTFILE="${TEXTFILE:-}"

# The key reaches curl through a header file, never argv, where ps would show it.
HDR=()
hdrfile=""
if [ -n "${STELLARINDEX_PROBE_API_KEY:-}" ]; then
  hdrfile="$(mktemp)"; trap 'rm -f "$hdrfile"' EXIT
  printf 'X-API-Key: %s\n' "$STELLARINDEX_PROBE_API_KEY" > "$hdrfile"
  HDR=(-H "@$hdrfile")
fi

# name|path — the anonymous, non-streaming GET surface. SSE (/v1/*/stream,
# /v1/oracle/streams) and auth-gated routes (/v1/account/*, /v1/dashboard/*,
# /v1/auth/*, /v1/signup/*) are excluded: they hang or 401. --spec-check diffs
# this list against the OpenAPI spec so it cannot rot.
ENDPOINTS=(
  "healthz|/v1/healthz"
  "readyz|/v1/readyz"
  "version|/v1/version"
  "status|/v1/status"
  "methodology|/v1/methodology"
  "sources|/v1/sources"
  "assets|/v1/assets?limit=25"
  "assets-verified|/v1/assets/verified"
  "asset-canonical|/v1/assets/native"
  "asset-slug|/v1/assets/usdc"
  "asset-network|/v1/assets/usdc/stellar"
  "asset-metadata|/v1/assets/native/metadata"
  "price|/v1/price?asset=native&quote=fiat:USD"
  "price-tip|/v1/price/tip?asset=native&quote=fiat:USD"
  "price-batch|/v1/price/batch?pairs=native/fiat:USD,${U}/native"
  "vwap|/v1/vwap?base=${U}&quote=native"
  "twap|/v1/twap?base=${U}&quote=native"
  "ohlc|/v1/ohlc?base=${U}&quote=native"
  "chart|/v1/chart?base=${U}&quote=native"
  "history|/v1/history?base=${U}&quote=native&limit=10"
  "history-since-inception|/v1/history/since-inception?base=${U}&quote=native"
  "observations|/v1/observations?base=${U}&quote=native&limit=10"
  "oracle-latest|/v1/oracle/latest?asset=native"
  "oracle-lastprice|/v1/oracle/lastprice?asset=native"
  "oracle-prices|/v1/oracle/prices"
  "oracle-x-last-price|/v1/oracle/x_last_price?base=native&quote=fiat:USD"
  "markets|/v1/markets?limit=25"
  "pools|/v1/pools"
  "lending-pools|/v1/lending/pools"
  "pairs|/v1/pairs"
  "sac-wrappers|/v1/sac-wrappers"
  "issuers|/v1/issuers?limit=25"
  "issuer-one|/v1/issuers/${ISS}"
  "changes|/v1/changes/asset/native"
  "diagnostics-cursors|/v1/diagnostics/cursors"
  "diagnostics-ingestion|/v1/diagnostics/ingestion"
  "incidents|/v1/incidents"
  "incidents-atom|/v1/incidents.atom"
  "network-stats|/v1/network/stats"
)

# page|space-separated panel reads, with the query params the page's hooks send
# by default (web/explorer/src/app/<page>), mainnet. Home's per-pair /v1/history
# reads, and detail pages' reads that wait on another response, are left out. Detail pages
# use fixed heavy entities (USDC, its issuer, the Soroswap router, an old ledger) so a run
# reads cold rows. Measure the page, not the endpoint.
PAGES=(
  "home|/v1/network/stats /v1/sources?include=stats /v1/assets?limit=10&order_by=volume_24h_usd_desc&include_thin=true /v1/assets/native /v1/assets/verified /v1/assets?limit=50 /v1/price?asset=native&quote=fiat:USD /v1/price/batch?asset_ids=native&quote=fiat:USD /v1/ohlc?base=native&quote=fiat%3AUSD&interval=15m&limit=674 /v1/diagnostics/cursors /v1/coverage /v1/price/batch?asset_ids=fiat%3AEUR%2Cfiat%3AGBP%2Cfiat%3AJPY%2Cfiat%3ACHF%2Cfiat%3ACAD%2Cfiat%3AAUD&quote=fiat:USD /v1/markets?limit=25&order_by=volume_24h_usd_desc /v1/markets?limit=3&order_by=volume_24h_usd_desc"
  "assets|/v1/assets?asset_class=all&limit=100&include=sparkline7d&include_thin=true"
  "markets|/v1/markets?limit=100&order_by=volume_24h_usd_desc&include=sparkline"
  "protocols|/v1/protocols /v1/sources?include=stats"
  "dexes|/v1/pools?order_by=volume_24h_usd_desc&limit=100 /v1/sources?include=stats /v1/protocols"
  "issuers|/v1/issuers?limit=100"
  "network|/v1/network/stats /v1/ledgers?limit=12 /v1/network/throughput?window_days=30 /v1/assets/native /v1/sources?include=stats /v1/pools?limit=8&order_by=volume_24h_usd_desc"
  "rwa|/v1/rwa/assets /v1/rwa/history?timeframe=1y&group_by=none /v1/rwa/premium?timeframe=1y /v1/assets?asset_class=stablecoin&limit=100"
  "asset|/v1/price?asset=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&quote=fiat:USD&include_thin=true /v1/changes/coin/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN /v1/ohlc?base=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN&quote=fiat%3AUSD&interval=15m&limit=674 /v1/assets/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN/holders?limit=100 /v1/markets/sources?asset=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN /v1/issuers/GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
  "account|/v1/accounts/{A} /v1/accounts/{A}/transactions?limit=50 /v1/accounts/{A}/operations?limit=50 /v1/issuers?limit=100 /v1/accounts/{A}/signer-on /v1/accounts/{A}/activity /v1/accounts/{A}/movements?limit=25 /v1/accounts/{A}/positions /v1/accounts/{A}/trades?limit=25 /v1/accounts/{A}/graph"
  "contract|/v1/contracts/{C}/wasm /v1/contracts/{C}?limit=50 /v1/contracts/{C}/transfers?limit=25 /v1/contracts/{C}/code-history /v1/contracts/{C}/interactions?days=90&limit=50 /v1/sac-wrappers"
  "ledger|/v1/ledgers/60000000 /v1/ledgers/60000000/transactions"
  "tx|/v1/tx/8de565f6c81ca5d51361774be8b37a396532db5ce9c36a5723aeba5792c0522b"
)

# bust_url URL ITER — append a unique query param when CACHE_BUST=1.
bust_url() {
  if [ "$CACHE_BUST" != "1" ]; then printf '%s' "$1"; return; fi
  case "$1" in *\?*) printf '%s&_cb=%s%s' "$1" "$RANDOM" "$2";; *) printf '%s?_cb=%s%s' "$1" "$RANDOM" "$2";; esac
}

if [ "${1:-}" = "--spec-check" ]; then
  command -v python3 >/dev/null || { echo "python3 required for --spec-check"; exit 2; }
  covered="$(printf '%s\n' "${ENDPOINTS[@]}" | sed 's/^[^|]*|//; s/?.*//')"
  python3 - "$covered" <<'PY'
import re, sys
covered = set(sys.argv[1].split())
inpaths=False; cur=None; gets=[]
for ln in open('openapi/stellar-index.v1.yaml'):
    if re.match(r'^paths:', ln): inpaths=True; continue
    if inpaths:
        if re.match(r'^[A-Za-z]', ln): break
        m=re.match(r'^  (/\S+):', ln)
        if m: cur=m.group(1); continue
        if re.match(r'^    get:', ln) and cur: gets.append(cur)
SKIP=('stream','/account/','/dashboard/','/auth/','/signup/')
missing=[g for g in gets
         if not any(s in g for s in SKIP)
         and ('/v1'+g) not in covered and g not in covered]
if missing:
    print("Spec GET paths NOT covered by the sweep (templated paths"
          " may be covered with a fixture — verify):")
    for m in missing: print("  ", m)
else:
    print("All non-streaming/non-auth spec GET paths are covered.")
PY
  exit 0
fi

# pctl PCT  < newline-separated sorted-ascending integer ms list
pctl() { sort -n | awk -v p="$1" '
  {a[NR]=$1} END{ if(NR==0){print 0; exit}
  i=int((p/100)*NR + 0.999999); if(i<1)i=1; if(i>NR)i=NR; print a[i] }'; }

if [ "${1:-}" = "--pages" ]; then
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp" ${hdrfile:+"$hdrfile"}' EXIT
  CRIT=0
  printf '%s%s pages vs %s — %s loads/page%s%s\n' "$B" "api-latency-sweep" "$API_BASE_URL" "$ITERS" \
    "$([ "$CACHE_BUST" = 1 ] && echo ' (cache-bust)')" "$O"
  printf '%s%-12s %7s %7s  %s%s\n' "$D" "page" "p50ms" "p95ms" "slowest panel (p95ms, ok%)" "$O"
  prom="$tmp/prom"; : > "$prom"
  for pg in "${PAGES[@]}"; do
    name="${pg%%|*}"; read -r -a panels <<< "${pg#*|}"
    case "$pg" in *"{A}"*) ents=$ACCOUNTS;; *"{C}"*) ents=$CONTRACTS;; *) ents="-";; esac
    : > "$tmp/page"
    for ((k=0;k<${#panels[@]};k++)); do : > "$tmp/p$k"; done
    for ent in $ents; do
    for ((i=0;i<ITERS;i++)); do
      for ((k=0;k<${#panels[@]};k++)); do
        u="${panels[$k]//\{A\}/$ent}"; u="${u//\{C\}/$ent}"
        ( curl -s ${HDR[@]+"${HDR[@]}"} -o /dev/null --max-time "$TIMEOUT" -w '%{http_code} %{time_total}\n' \
            "$(bust_url "${API_BASE_URL}${u}" "$i$k")" 2>/dev/null || echo "000 $TIMEOUT" ) > "$tmp/r$k" &
      done
      wait
      worst=0
      for ((k=0;k<${#panels[@]};k++)); do
        read -r code tt < "$tmp/r$k"
        case "$code" in 2*|404) ;; *) tt=$TIMEOUT;; esac
        ms=$(awk -v t="${tt:-$TIMEOUT}" 'BEGIN{printf "%d", t*1000}')
        echo "$ms $code" >> "$tmp/p$k"
        [ "$ms" -gt "$worst" ] && worst=$ms
      done
      echo "$worst" >> "$tmp/page"
    done
    done
    p50=$(pctl 50 < "$tmp/page"); p95=$(pctl 95 < "$tmp/page")
    printf 'stellarindex_page_sweep_p95_seconds{page="%s"} %s\n' "$name" "$(awk -v m="$p95" 'BEGIN{printf "%.3f", m/1000}')" >> "$prom"
    slow=""; slow95=-1; slowok=0
    for ((k=0;k<${#panels[@]};k++)); do
      k95=$(cut -d' ' -f1 "$tmp/p$k" | pctl 95)
      kok=$(awk '$2 ~ /^(2|404)/ {o++} END{printf "%.3f", (NR? o/NR:0)}' "$tmp/p$k")
      printf 'stellarindex_page_sweep_panel_p95_seconds{page="%s",panel="%s"} %s\n' "$name" "${panels[$k]}" "$(awk -v m="$k95" 'BEGIN{printf "%.3f", m/1000}')" >> "$prom"
      printf 'stellarindex_page_sweep_panel_ok_ratio{page="%s",panel="%s"} %s\n' "$name" "${panels[$k]}" "$kok" >> "$prom"
      if [ "$k95" -gt "$slow95" ]; then
        slow95=$k95; slow="${panels[$k]}"
        slowok=$(awk '$2 ~ /^(2|404)/ {o++} END{printf "%d", (NR? o*100/NR:0)}' "$tmp/p$k")
      fi
    done
    if   [ "$p95" -gt "$CRIT_MS" ]; then c="$R"; CRIT=$((CRIT+1))
    elif [ "$p95" -gt "$WARN_MS" ]; then c="$Y"
    else c="$G"; fi
    printf '%s%-12s%s %7s %s%7s%s  %s (%s, %s%%)\n' "$c" "/$name" "$O" "$p50" "$c" "$p95" "$O" "$slow" "$slow95" "$slowok"
  done
  if [ -n "$TEXTFILE" ]; then
    {
      printf '# HELP stellarindex_page_sweep_p95_seconds p95 of a page load (its slowest panel) in the last sweep.\n# TYPE stellarindex_page_sweep_p95_seconds gauge\n'
      grep '^stellarindex_page_sweep_p95_seconds' "$prom"
      printf '# HELP stellarindex_page_sweep_panel_p95_seconds p95 of one page panel read in the last sweep, pooled over the entity cohort.\n# TYPE stellarindex_page_sweep_panel_p95_seconds gauge\n'
      grep '^stellarindex_page_sweep_panel_p95_seconds' "$prom"
      printf '# HELP stellarindex_page_sweep_panel_ok_ratio Share of 2xx answers for one page panel in the last sweep.\n# TYPE stellarindex_page_sweep_panel_ok_ratio gauge\n'
      grep '^stellarindex_page_sweep_panel_ok_ratio' "$prom"
      printf '# HELP stellarindex_page_sweep_last_run_timestamp_seconds When the last sweep finished.\n# TYPE stellarindex_page_sweep_last_run_timestamp_seconds gauge\nstellarindex_page_sweep_last_run_timestamp_seconds %s\n' "$(date +%s)"
    } > "$TEXTFILE.tmp.$$" && mv -f "$TEXTFILE.tmp.$$" "$TEXTFILE"
  fi
  exit "$CRIT"
fi

CRIT=0
RESULTS=()
for e in "${ENDPOINTS[@]}"; do
  name="${e%%|*}"; path="${e#*|}"
  times=""; okc=0; n=0
  [ "$WARMUP" = "1" ] && curl -s ${HDR[@]+"${HDR[@]}"} -o /dev/null --max-time "$TIMEOUT" "${API_BASE_URL}${path}" >/dev/null 2>&1
  for ((i=0;i<ITERS;i++)); do
    u="$(bust_url "${API_BASE_URL}${path}" "$i")"
    read -r code tt < <(curl -s ${HDR[@]+"${HDR[@]}"} -o /dev/null --max-time "$TIMEOUT" \
      -w '%{http_code} %{time_total}\n' "$u" 2>/dev/null || echo "000 $TIMEOUT")
    ms=$(awk -v t="${tt:-$TIMEOUT}" 'BEGIN{printf "%d", t*1000}')
    times+="${ms}"$'\n'; n=$((n+1))
    case "$code" in 2*) okc=$((okc+1));; esac
  done
  ts="$(printf '%s' "$times" | grep -v '^$')"
  p50=$(printf '%s\n' "$ts" | pctl 50)
  p95=$(printf '%s\n' "$ts" | pctl 95)
  p99=$(printf '%s\n' "$ts" | pctl 99)
  mx=$(printf '%s\n'  "$ts" | sort -n | tail -1)
  okp=$(awk -v o="$okc" -v n="$n" 'BEGIN{printf "%d", (n? o*100/n:0)}')
  [ "$p95" -gt "$CRIT_MS" ] && CRIT=$((CRIT+1))
  RESULTS+=("$p95|$name|$path|$n|$okp|$p50|$p99|$mx")
done

if [ "$JSON" = "1" ]; then
  printf '['
  first=1
  while IFS='|' read -r p95 name path n okp p50 p99 mx; do
    [ $first -eq 1 ] || printf ','
    first=0
    printf '{"name":"%s","path":"%s","n":%s,"ok_pct":%s,"p50_ms":%s,"p95_ms":%s,"p99_ms":%s,"max_ms":%s}' \
      "$name" "$path" "$n" "$okp" "$p50" "$p95" "$p99" "$mx"
  done < <(printf '%s\n' "${RESULTS[@]}" | sort -t'|' -k1,1nr)
  printf ']\n'
  exit "$CRIT"
fi

printf '%s%s vs %s — %s iters/endpoint%s%s\n' "$B" \
  "api-latency-sweep" "$API_BASE_URL" "$ITERS" \
  "$([ "$CACHE_BUST" = 1 ] && echo ' (cache-bust)')" "$O"
printf '%s%-26s %5s %4s %7s %7s %7s %7s%s\n' "$D" \
  "endpoint" "ok%" "n" "p50ms" "p95ms" "p99ms" "maxms" "$O"
while IFS='|' read -r p95 name path n okp p50 p99 mx; do
  if   [ "$p95" -gt "$CRIT_MS" ]; then c="$R"; tag="CRIT"
  elif [ "$p95" -gt "$WARN_MS" ]; then c="$Y"; tag="warn"
  else c="$G"; tag="ok"; fi
  oc=""; [ "$okp" -lt 100 ] && oc="$R"
  printf '%s%-26s%s %s%4s%s %4s %7s %s%7s%s %7s %7s  %s%s%s\n' \
    "$c" "$name" "$O" "$oc" "$okp" "$O" "$n" "$p50" "$c" "$p95" "$O" "$p99" "$mx" "$c" "$tag" "$O"
done < <(printf '%s\n' "${RESULTS[@]}" | sort -t'|' -k1,1nr)

echo
# sigpipe-ok: RESULTS holds one short line per swept route (<100 of them,
# ~80 bytes each) — far under the 64 KiB pipe buffer, so head cannot
# close the pipe before sort finishes.
slow=$(printf '%s\n' "${RESULTS[@]}" | sort -t'|' -k1,1nr | head -1)
printf '%sslowest: %s (p95 %s ms)%s\n' "$B" \
  "$(echo "$slow"|cut -d'|' -f2)" "$(echo "$slow"|cut -d'|' -f1)" "$O"
if [ "$CRIT" -eq 0 ]; then
  printf '%sNo endpoint over the %sms concern ceiling.%s\n' "$G" "$CRIT_MS" "$O"
else
  printf '%s%s endpoint(s) over the %sms ceiling — investigate.%s\n' "$R" "$CRIT" "$CRIT_MS" "$O"
fi
exit "$CRIT"
