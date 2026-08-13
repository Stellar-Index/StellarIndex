#!/usr/bin/env python3
"""Cold FULL-PAGE audit for /contracts/{id} (operator bar 2026-08-13:
"subsecond on cold random loads of contracts, with every part of the
page fully loaded").

The per-endpoint harness (subsecond-audit.py) answers "is this read
fast". It does NOT answer the operator's question, which is about a
PAGE: the contract view fires five reads, and the user waits for the
SLOWEST one — a page is only "loaded" when every panel has its data.

So this measures, per contract:
  * every endpoint the contract page calls, issued CONCURRENTLY the way
    a browser does,
  * the WALL CLOCK from first request to last response = time-to-fully-
    populated, which is the number the operator is judging,
  * plus the per-endpoint split, so a breach names its own culprit.

COLD by construction: contract ids come from a file of lake-drawn
random ids (see --help for how to draw them), and each is requested
once. Prewarmed/popular contracts are exactly what this must avoid.

Usage:
  python3 scripts/ops/contract-page-audit.py contracts.txt
  BUDGET=1.0 BASE=https://api.stellarindex.io/v1 python3 ... contracts.txt

  Draw a fresh cold sample on r1:
    clickhouse-client --port 9300 -q "SELECT contract_id FROM \\
      stellar.contract_events WHERE ledger_seq > 63000000 \\
      GROUP BY contract_id ORDER BY rand() LIMIT 25 FORMAT TabSeparated"

Exit code = number of contracts whose FULL PAGE breached the budget.
"""

import concurrent.futures
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("BASE", "https://api.stellarindex.io/v1")
BUDGET = float(os.environ.get("BUDGET", "1.0"))
UA = {"User-Agent": "stellarindex-contract-page-audit"}
CTX = ssl.create_default_context()

# Every read the contract page needs before it is fully populated.
# Keep in lockstep with web/explorer/src/app/contract/ContractView.tsx.
PANELS = [
    ("detail", "/contracts/{id}"),
    ("interactions", "/contracts/{id}/interactions"),
    ("transfers", "/contracts/{id}/transfers"),
    ("code-history", "/contracts/{id}/code-history"),
    ("wasm", "/contracts/{id}/wasm"),
]


def fetch(path):
    """Return (seconds, status). A 404 is a legitimate answer — an
    honest 'no wasm for this contract' still populates the panel."""
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


def audit(contract):
    """Fire every panel concurrently; the page is loaded when the last
    one lands."""
    started = time.monotonic()
    per = {}
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(PANELS)) as pool:
        futures = {
            pool.submit(fetch, path.format(id=contract)): name
            for name, path in PANELS
        }
        for fut in concurrent.futures.as_completed(futures):
            per[futures[fut]] = fut.result()
    return time.monotonic() - started, per


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    with open(sys.argv[1], encoding="utf-8") as fh:
        contracts = [l.split()[0].strip() for l in fh if l.strip()]
    if not contracts:
        print("no contract ids in", sys.argv[1])
        return 2

    print(f"contracts: {len(contracts)} | panels: {len(PANELS)} | budget {BUDGET}s (full page)")
    breaches, rows = [], []
    for c in contracts:
        total, per = audit(c)
        rows.append((total, c, per))
        slowest = max(per.items(), key=lambda kv: kv[1][0])
        flag = "BREACH" if total > BUDGET else "ok    "
        print(f"{flag} {total:5.2f}s {c[:12]}… slowest={slowest[0]} {slowest[1][0]:.2f}s "
              + " ".join(f"{n}:{v[0]:.2f}({v[1]})" for n, v in sorted(per.items())))
        if total > BUDGET:
            breaches.append((total, c, per))

    rows.sort(reverse=True)
    print(f"\n== full-page breaches: {len(breaches)}/{len(contracts)} (budget {BUDGET}s)")
    # Which panel is responsible, across the whole sample.
    blame = {}
    for total, _c, per in rows:
        if total <= BUDGET:
            continue
        name = max(per.items(), key=lambda kv: kv[1][0])[0]
        blame[name] = blame.get(name, 0) + 1
    for name, n in sorted(blame.items(), key=lambda kv: -kv[1]):
        print(f"   slowest panel {n:3d}x  {name}")
    worst = {n: max(p[n][0] for _t, _c, p in rows) for n, _ in PANELS}
    print("== worst per panel: " + " ".join(f"{n}:{v:.2f}s" for n, v in sorted(worst.items(), key=lambda kv: -kv[1])))
    return len(breaches)


if __name__ == "__main__":
    sys.exit(main())
