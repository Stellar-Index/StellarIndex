#!/usr/bin/env python3
"""Sub-second acceptance audit (operator goal 2026-08-08): every module
on every explorer page must serve in < 1s.

Random-samples entities from files (one per line: KIND|VALUE, kinds:
CONTRACT, ACCOUNT, ASSET, SAC, VERIFIED, TX:ledger:hash) or uses the
built-in fixed set, hits every page-backing API endpoint sequentially
(concurrency 1 — the point is per-read latency, not load), twice per
URL (cold-ish + warm), and reports every (endpoint, entity) whose WARM
read breaches the budget plus every cold read over the cold budget.

Usage:
  python3 scripts/ops/subsecond-audit.py [samples.txt ...]
  BUDGET=1.0 COLD_BUDGET=1.0 BASE=https://api.stellarindex.io/v1 ...

Exit code = number of warm-budget breaches (cron-friendly).
"""
import json, os, sys, time, urllib.request, urllib.error, ssl

BASE = os.environ.get("BASE", "https://api.stellarindex.io/v1")
BUDGET = float(os.environ.get("BUDGET", "1.0"))
COLD_BUDGET = float(os.environ.get("COLD_BUDGET", "1.0"))
UA = {"User-Agent": "stellarindex-subsecond-audit"}
ctx = ssl.create_default_context()

contracts, accounts, assets, sacs, verified, txs = [], [], [], [], [], []
for path in sys.argv[1:]:
    for line in open(path):
        line = line.strip()
        if not line or "|" not in line:
            continue
        kind, _, val = line.partition("|")
        {"CONTRACT": contracts, "ACCOUNT": accounts, "ASSET": assets,
         "SAC": sacs, "VERIFIED": verified, "TX": txs}.get(kind, []).append(val)

if not contracts:
    contracts = ["CCW5IBJ7CSEOVJZYIBIUCJBI52Z5BJGM52EBF3ZUZJJYYX3EE3V4ECAV",
                 "CDMAFG3HBSH5ANZ22FSGR6TS3OS4HQBNE7WVNDIENOTEFGCEDVXDFKYN"]
if not accounts:
    accounts = ["GATL3ETTZ3XDGFXX2ELPIKCZL7S5D2HY3VK4T7LRPD6DW5JOLAEZSZBA",
                "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"]
if not assets:
    assets = ["USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "native"]
if not verified:
    verified = ["usdc", "xlm"]

jobs = []
for c in contracts:
    for ep in ("", "/transfers", "/interactions", "/code-history"):
        jobs.append((f"contract{ep or '/detail'}", f"/contracts/{c}{ep}"))
for g in accounts:
    for ep in ("", "/transactions", "/operations", "/movements", "/positions", "/trades", "/activity"):
        jobs.append((f"account{ep or '/state'}", f"/accounts/{g}{ep}"))
for a in assets:
    jobs.append(("asset/detail", f"/assets/{a}"))
    jobs.append(("asset/holders", f"/assets/{a}/holders"))
    jobs.append(("asset/markets", f"/markets?asset={a}"))
for c in sacs:
    jobs.append(("sac/contract", f"/contracts/{c}"))
    jobs.append(("sac/asset", f"/assets/{c}"))
for s_ in verified:
    jobs.append(("verified/detail", f"/assets/{s_}"))
for t in txs:
    _, _, h = t.partition(":")
    jobs.append(("tx/detail", f"/tx/{h}"))
for p in ("/accounts", "/contracts", "/ledgers", "/operations", "/markets", "/pools",
          "/liquidity-pools", "/lending/pools", "/protocols", "/issuers",
          "/network/stats", "/coverage", "/status", "/assets", "/sac-wrappers"):
    jobs.append(("hub" + p, p))

def probe(path):
    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(urllib.request.Request(BASE + path, headers=UA),
                                    timeout=20, context=ctx) as r:
            r.read()
            return r.status, time.monotonic() - t0
    except urllib.error.HTTPError as e:
        return e.code, time.monotonic() - t0
    except Exception:
        return 0, time.monotonic() - t0

cold_breaches, warm_breaches, failures = [], [], []
for group, path in jobs:
    s1, t1 = probe(path)          # cold-ish
    time.sleep(0.15)
    s2, t2 = probe(path)          # warm
    time.sleep(0.15)
    if s2 >= 500 or s2 == 0:
        failures.append((group, path, s2, t2))
    elif t2 > BUDGET:
        warm_breaches.append((group, path, s2, t2))
    if s1 < 500 and s1 != 0 and t1 > COLD_BUDGET:
        cold_breaches.append((group, path, s1, t1))

print(f"probes: {len(jobs)} × 2 | budget warm {BUDGET}s cold {COLD_BUDGET}s")
print(f"\n== FAILURES (5xx/unreachable on warm): {len(failures)}")
for g, p, s, t in failures:
    print(f"  {s} {t:6.2f}s {p}")
print(f"\n== WARM breaches: {len(warm_breaches)}")
for g, p, s, t in sorted(warm_breaches, key=lambda r: -r[3]):
    print(f"  {t:6.2f}s {p}")
print(f"\n== COLD breaches: {len(cold_breaches)}")
for g, p, s, t in sorted(cold_breaches, key=lambda r: -r[3]):
    print(f"  {t:6.2f}s {p}")
sys.exit(len(warm_breaches) + len(failures))
