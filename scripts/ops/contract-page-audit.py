#!/usr/bin/env python3
"""Cold FULL-PAGE audit for explorer pages (operator bar 2026-08-13:
"subsecond on cold random loads of contracts, with every part of the
page fully loaded").

The per-endpoint harness (subsecond-audit.py) answers "is this read
fast". It does NOT answer the operator's question, which is about a
PAGE: a page view fires several reads, and the user waits for the
SLOWEST one — a page is only "loaded" when every panel has its data.

So this measures, per page:
  * every endpoint the page calls, issued CONCURRENTLY the way a
    browser does (panels a browser can only issue after another panel's
    response are chained the same way — see `after` below),
  * the WALL CLOCK from first request to last response = time-to-fully-
    populated, which is the number the operator is judging,
  * plus the per-endpoint split, so a breach names its own culprit.

W3.1 built this for /contracts/{id}. W3.2 (launch plan) extends it to a
PER-PAGE-TYPE PANEL MAP rather than a new harness. With no --type the
behavior is the original contract audit, unchanged.

COLD by construction: ids come from a file of lake-drawn random ids
(draw queries below), and each is requested once. Prewarmed/popular
entities are exactly what this must avoid.

Usage:
  python3 scripts/ops/contract-page-audit.py contracts.txt        # contract (unchanged)
  python3 scripts/ops/contract-page-audit.py --type account gs.txt
  python3 scripts/ops/contract-page-audit.py --type home          # singleton
  BUDGET=1.0 PACE=1.0 BASE=https://api.stellarindex.io/v1 python3 ... --type ledger seqs.txt

Page types (ids file = one id per line; panel lists are kept in
LOCKSTEP with the explorer view components named at each map entry):
  contract     C... contract id (default; byte-compatible with W3.1)
  account      G... strkey (56 chars, never lowercased)
  asset        canonical asset_id: CODE-GISSUER | native | C... SAC |
               crypto:X | fiat:X — models the PRE-RENDERED asset page's
               runtime panels (top-~500 assets are baked at build time)
  asset-shell  same ids — the long-tail SHELL path: /assets/{id} then a
               DEPENDENT /price, issued serially like the browser must
  ledger       ledger seq (decimal uint32)
  tx           64-char lowercase hex tx hash (/operation?tx=..&i=.. fires
               the exact same single GET, so this covers both pages)
  pair         market pair slug "base~quote", both sides canonical
               asset_id (the explorer's PAIR_SEPARATOR is '~')
  protocol     protocol registry name (sdex, soroswap, blend, ...)
  operations | home | network | protocols
               singletons: no ids file; RUNS iterations (env RUNS,
               default 3 — run 1 is the coldest, later runs show TTL
               caches; judge singletons primarily on run 1)

Draw a fresh cold sample on r1 (clickhouse-client --port 9300, READ-ONLY):
  contract:  SELECT contract_id FROM stellar.contract_events
             WHERE ledger_seq > 63000000 GROUP BY contract_id
             ORDER BY rand() LIMIT 25 FORMAT TabSeparated
  account+tx (mix OLD and RECENT windows W, e.g. 3e6..63.9e6 step ~7e6;
             stellar.transactions is sorted by ledger_seq so windowed
             draws are cheap, and account_movements is sorted by address
             so do NOT window-scan it):
             SELECT tx_hash, source_account FROM stellar.transactions
             WHERE ledger_seq BETWEEN W AND W+2000
             ORDER BY rand() LIMIT 3 FORMAT TabSeparated
  ledger:    random integers over [2, tip]; tip via
             SELECT max(ledger_seq) FROM stellar.ledgers
  asset+pair: trades live behind the API, so draw from it (read-only):
             GET /v1/markets → keep stellar-native identifiers (skip
             crypto:*/fiat:*); assets = unique base/quote ids;
             pairs = base + "~" + quote

Exit code = number of pages whose FULL PAGE breached the budget, plus
pages with an UNLOADED panel.
"""

import concurrent.futures
import json
import os
import re
import ssl
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("BASE", "https://api.stellarindex.io/v1")
BUDGET = float(os.environ.get("BUDGET", "1.0"))
# Think time between pages. PACE=0 (the default) is a SUSTAINED-CRAWL
# stress: the next page starts while the previous one's detached
# refreshes are still running, so the refresh gate stays saturated.
# That is a real traffic shape, but it is not the same question as "is
# one cold page fast" — set PACE to isolate a single cold page load.
PACE = float(os.environ.get("PACE", "0"))
# Iterations for singleton page types (home, network, ...). Run 1 is
# the coldest; later runs mostly measure the server's TTL caches.
RUNS = int(os.environ.get("RUNS", "3"))
UA = {"User-Agent": "stellarindex-contract-page-audit"}
CTX = ssl.create_default_context()

# The verified USDC asset_id — the asset chart's default quote for
# non-USD assets (ChartPanel.tsx quote selection).
USDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
CLASSIC_RE = re.compile(r"^(native|[A-Za-z0-9]{1,12}-G[A-Z2-7]{55})$")
ISSUER_RE = re.compile(r"^[A-Za-z0-9]{1,12}-(G[A-Z2-7]{55})$")

# Panel tuple: (name, path[, after[, ok404]]).
#   after: name of the panel whose RESPONSE the browser needs before it
#          can issue this one (a true serial hop, not a tab click).
#   ok404: 404 carries an honest "there is none" answer that fully
#          populates the panel (e.g. a SAC has no wasm; an asset has no
#          change history). Where a 404 would render "not found" /
#          an empty panel instead (ledger, tx, account state), it
#          counts as UNLOADED.

# Every read the contract page needs before it is fully populated.
# Keep in lockstep with web/explorer/src/app/contract/ContractView.tsx.
PANELS = [
    ("detail", "/contracts/{id}"),
    ("interactions", "/contracts/{id}/interactions"),
    ("transfers", "/contracts/{id}/transfers"),
    ("code-history", "/contracts/{id}/code-history"),
    ("wasm", "/contracts/{id}/wasm"),
]


# Lockstep: web/explorer/src/app/accounts/AccountView.tsx (+ panels
# AccountActivitySummary/AccountMovements/AccountDefiPositions/
# AccountTrades). All eight fire in parallel on mount; the dependent
# /price/batch second hop (Positions pricing, needs the trustline list
# from /accounts/{id}) is response-derived so it cannot be modeled here
# — noted as unmeasured residue, it is non-fatal to the panel.
def _account_panels(g):
    return [
        ("state", f"/accounts/{g}"),
        ("transactions", f"/accounts/{g}/transactions?limit=50"),
        ("operations", f"/accounts/{g}/operations?limit=50"),
        ("activity", f"/accounts/{g}/activity"),
        ("movements", f"/accounts/{g}/movements?limit=25"),
        ("positions", f"/accounts/{g}/positions"),
        ("trades", f"/accounts/{g}/trades?limit=25"),
        ("issuers", "/issuers?limit=100", None, True),
    ]


# The 18-currency converter batch (AssetConverter.tsx) and the 6-currency
# home ticker batch (HomeCurrencies.tsx).
ASSET_FX = ("/price/batch?asset_ids=" + ",".join(
    f"fiat:{c}" for c in ("EUR", "GBP", "JPY", "CHF", "CAD", "AUD", "CNY",
                          "INR", "BRL", "MXN", "KRW", "HKD", "SGD", "SEK",
                          "NOK", "ZAR", "TRY", "NZD")) + "&quote=fiat:USD")
HOME_FX = ("/price/batch?asset_ids=" + ",".join(
    f"fiat:{c}" for c in ("EUR", "GBP", "JPY", "CHF", "CAD", "AUD"))
    + "&quote=fiat:USD")


# Lockstep: web/explorer/src/app/assets/[slug]/ — the PRE-RENDERED page's
# runtime panels: ChartPanel→MarketChart (ohlc), LiveAssetPrice (price),
# ChangeSummaryStrip (changes), AssetConverter (fx), IssuerPanel (issuer,
# CODE-G assets only). Entity data itself is baked HTML at build time.
def _asset_panels(a):
    quote = "fiat:USD" if (
        a in ("native", "XLM", USDC) or a.startswith(("fiat:", "crypto:"))
    ) else USDC
    p = [
        ("ohlc", f"/ohlc?base={a}&quote={quote}&interval=15m&limit=674"),
        ("price", f"/price?asset={a}&quote=fiat:USD", None, True),
        ("changes", f"/changes/coin/{a}", None, True),
        ("fx-batch", ASSET_FX),
    ]
    m = ISSUER_RE.match(a)
    if m:
        p.append(("issuer", f"/issuers/{m.group(1)}", None, True))
    return p


# Lockstep: web/explorer/src/app/assets/[slug]/AssetPathView.tsx — the
# long-tail shell: /assets/{id} first, then LiveAssetPrice, which needs
# the detail response's asset_id, so the price hop is SERIAL.
def _asset_shell_panels(a):
    return [
        ("detail", f"/assets/{a}"),
        ("price", f"/price?asset={a}&quote=fiat:USD", "detail", True),
    ]


# Lockstep: web/explorer/src/app/ledger/LedgerView.tsx (route
# /ledgers/{seq}). Both fire in parallel; a 404 renders "Ledger not
# found", so it is NOT a loaded panel for lake-drawn seqs.
def _ledger_panels(seq):
    return [
        ("ledger", f"/ledgers/{seq}"),
        ("transactions", f"/ledgers/{seq}/transactions"),
    ]


# Lockstep: web/explorer/src/app/tx/TxView.tsx (route
# /transactions/{hash}) — ONE request; operations + events come back in
# the same payload. /operation?tx=..&i=.. fires the identical GET.
def _tx_panels(h):
    return [("tx", f"/tx/{h}")]


# Lockstep: web/explorer/src/app/markets/[pair]/ runtime panels:
# LivePairPrice (price), PairChart→MarketChart (ohlc), SourceBreakdown
# (markets/sources), OrderBookPanel (classic-asset pairs only — the
# component gates on isClassicAssetId for both sides).
def _pair_panels(slug):
    base, _, quote = slug.partition("~")
    p = [
        ("price", f"/price?asset={base}&quote={quote}", None, True),
        ("ohlc", f"/ohlc?base={base}&quote={quote}&interval=15m&limit=674"),
        ("sources", f"/markets/sources?base={base}&quote={quote}", None, True),
    ]
    if CLASSIC_RE.match(base) and CLASSIC_RE.match(quote):
        p.append(("orderbook",
                  f"/sdex/orderbook?selling={base}&buying={quote}&depth=12"))
    return p


# Lockstep: web/explorer/src/app/protocols/[name]/ProtocolView.tsx —
# a one-request page (roster, events, completeness, bespoke block all in
# the single /protocols/{name} response).
def _protocol_panels(name):
    return [("detail", f"/protocols/{name}")]


# Singletons. Lockstep with, respectively:
#   operations: app/operations/OperationsView.tsx + components/NetworkInsight.tsx
#   home:       app/page.tsx client panels (HomeNetworkStrip, HomeHeroChart,
#               HomeLivePanels, HomeTopAssets, HomeTopMovers, HomeCurrencies,
#               HomeTopMarkets, HomeRecentTrades) — the second-wave
#               /history?...&limit=12 x3 (HomeRecentTrades) is derived from
#               the top-markets RESPONSE, so it is unmodeled residue here.
#   network:    app/network/NetworkView.tsx + NetworkInsight.tsx
#   protocols:  app/protocols/ProtocolsIndex.tsx
SINGLETONS = {
    "operations": [
        ("directory", "/operations?limit=50"),
        ("op-type-mix", "/operations?limit=1"),
        ("throughput", "/network/throughput?window_days=30"),
    ],
    "home": [
        ("network-stats", "/network/stats"),
        ("sources", "/sources?include=stats"),
        ("xlm-price", "/price?asset=native&quote=fiat:USD"),
        ("chart-24h", "/chart?asset=native&quote=fiat:USD&timeframe=24h&granularity=1h",
         None, True),
        ("cursors", "/diagnostics/cursors"),
        ("hero-ohlc", "/ohlc?base=native&quote=fiat:USD&interval=15m&limit=674"),
        ("top-assets", "/assets?limit=10&include=sparkline"),
        ("movers", "/assets?limit=50"),
        ("verified", "/assets/verified", None, True),
        ("fx-batch", HOME_FX),
        ("top-markets", "/markets?limit=25&order_by=volume_24h_usd_desc"),
        ("recent-trades", "/markets?limit=3&order_by=volume_24h_usd_desc"),
    ],
    "network": [
        ("network-stats", "/network/stats"),
        ("ledgers", "/ledgers?limit=12"),
        ("throughput", "/network/throughput?window_days=30"),
        ("op-type-mix", "/operations?limit=1"),
        ("pools", "/pools?limit=8&order_by=volume_24h_usd_desc"),
        ("sources", "/sources?include=stats"),
    ],
    "protocols": [
        ("protocols", "/protocols"),
        ("sources", "/sources?include=stats", None, True),
    ],
}

PAGE_TYPES = {
    "contract": lambda c: [(n, p.format(id=c), None, True) for n, p in PANELS],
    "account": _account_panels,
    "asset": _asset_panels,
    "asset-shell": _asset_shell_panels,
    "ledger": _ledger_panels,
    "tx": _tx_panels,
    "pair": _pair_panels,
    "protocol": _protocol_panels,
}
PAGE_TYPES.update({t: (lambda t: lambda _id: SINGLETONS[t])(t)
                   for t in SINGLETONS})


def _norm(panel):
    """(name, path[, after[, ok404]]) -> (name, path, after, ok404)."""
    name, path = panel[0], panel[1]
    after = panel[2] if len(panel) > 2 else None
    ok404 = panel[3] if len(panel) > 3 else False
    return name, path, after, ok404


# A panel is LOADED only if it carried an answer. 2xx obviously; 404 too
# where 404 IS the answer (an honest "this contract is a SAC, there is
# no wasm" fully populates the panel — panels marked ok404). Everything
# else (notably 503 from a saturated refresh gate) renders as an error
# or a spinner, so counting it as loaded would score a broken page as a
# fast one. That mistake is why the first version of this harness
# reported pages "ok" at 0.10s while three of five panels were failing.
def loaded(status, ok404=True):
    return status < 400 or (status == 404 and ok404)


def fetch(path):
    """Return (seconds, status)."""
    url = BASE + path
    start = time.monotonic()
    try:
        req = urllib.request.Request(url, headers=UA)
        with urllib.request.urlopen(req, timeout=30, context=CTX) as r:
            r.read()
            return time.monotonic() - start, r.status
    except urllib.error.HTTPError as e:
        e.read()
        return time.monotonic() - start, e.code
    except Exception:
        return time.monotonic() - start, 0


def audit(panels):
    """Fire every panel concurrently (dependents as soon as their
    prerequisite lands); the page is loaded when the last one lands."""
    started = time.monotonic()
    per = {}
    remaining = {name: (path, after) for name, path, after, _ in panels}
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(panels)) as pool:
        futures = {}

        def submit_ready():
            for name, (path, after) in list(remaining.items()):
                if after is None or after in per:
                    futures[pool.submit(fetch, path)] = name
                    del remaining[name]

        submit_ready()
        while futures:
            done, _ = concurrent.futures.wait(
                futures, return_when=concurrent.futures.FIRST_COMPLETED)
            for fut in done:
                per[futures.pop(fut)] = fut.result()
            submit_ready()
    return time.monotonic() - started, per


def parse_args(argv):
    page_type, files = "contract", []
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--type":
            page_type = argv[i + 1]
            i += 2
        elif a.startswith("--type="):
            page_type = a.split("=", 1)[1]
            i += 1
        else:
            files.append(a)
            i += 1
    return page_type, files


def main():
    page_type, files = parse_args(sys.argv[1:])
    if page_type not in PAGE_TYPES:
        print(f"unknown --type {page_type!r}; known: "
              + " ".join(sorted(PAGE_TYPES)))
        return 2
    if page_type in SINGLETONS:
        entities = [f"run-{k + 1}" for k in range(RUNS)]
    else:
        if not files:
            print(__doc__)
            return 2
        with open(files[0], encoding="utf-8") as fh:
            entities = [l.split()[0].strip() for l in fh if l.strip()]
        if not entities:
            print(f"no {page_type} ids in", files[0])
            return 2

    panels_for = PAGE_TYPES[page_type]
    label = "contracts" if page_type == "contract" else f"{page_type} pages"
    print(f"{label}: {len(entities)} | panels: {len(panels_for(entities[0]))} "
          f"| budget {BUDGET}s (full page) | pace {PACE}s between pages")
    breaches, rows, unloaded = [], [], []
    for i, c in enumerate(entities):
        if i and PACE:
            time.sleep(PACE)
        panels = [_norm(p) for p in panels_for(c)]
        ok404s = {n: ok for n, _p, _a, ok in panels}
        total, per = audit(panels)
        rows.append((total, c, per))
        slowest = max(per.items(), key=lambda kv: kv[1][0])
        bad = [n for n, v in per.items() if not loaded(v[1], ok404s[n])]
        flag = "FAILED" if bad else ("BREACH" if total > BUDGET else "ok    ")
        print(f"{flag} {total:5.2f}s {c[:12]}… slowest={slowest[0]} {slowest[1][0]:.2f}s "
              + " ".join(f"{n}:{v[0]:.2f}({v[1]})" for n, v in sorted(per.items()))
              + (f"  UNLOADED={','.join(sorted(bad))}" if bad else ""))
        if bad:
            unloaded.append((c, bad))
        if total > BUDGET:
            breaches.append((total, c, per))

    rows.sort(reverse=True)
    print(f"\n== full-page breaches: {len(breaches)}/{len(entities)} (budget {BUDGET}s)")
    # Which panel is responsible, across the whole sample.
    blame = {}
    for total, _c, per in rows:
        if total <= BUDGET:
            continue
        name = max(per.items(), key=lambda kv: kv[1][0])[0]
        blame[name] = blame.get(name, 0) + 1
    for name, n in sorted(blame.items(), key=lambda kv: -kv[1]):
        print(f"   slowest panel {n:3d}x  {name}")
    # Panel-map order (matches the original PANELS iteration for the
    # contract type), plus any conditional panels seen only on some rows.
    names = [n for n, _p, _a, _ok in (_norm(p) for p in panels_for(entities[0]))]
    names += sorted({n for _t, _c, p in rows for n in p} - set(names))
    worst = {n: max(p[n][0] for _t, _c, p in rows if n in p) for n in names}
    print("== worst per panel: " + " ".join(f"{n}:{v:.2f}s" for n, v in sorted(worst.items(), key=lambda kv: -kv[1])))

    # Panels that never carried an answer. Reported SEPARATELY from
    # latency: a page whose panels 503 is not a fast page, it is a
    # broken one, and averaging the two hides it.
    print(f"== pages with an UNLOADED panel: {len(unloaded)}/{len(entities)}")
    if unloaded:
        by_panel = {}
        for _c, bad in unloaded:
            for n in bad:
                by_panel[n] = by_panel.get(n, 0) + 1
        for n, k in sorted(by_panel.items(), key=lambda kv: -kv[1]):
            print(f"   unloaded {k:3d}x  {n}")
    return len(breaches) + len(unloaded)


if __name__ == "__main__":
    sys.exit(main())
