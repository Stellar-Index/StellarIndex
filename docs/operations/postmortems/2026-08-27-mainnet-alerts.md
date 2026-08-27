---
title: Post-mortem — mainnet "Degraded performance" alert cluster
date: 2026-08-27
status: in-progress
severity: P3 (all tickets — zero customer impact; the API served correct data throughout)
author: ops (Claude-assisted)
---

# Post-mortem — 2026-08-27 mainnet alert cluster

## Summary

The public status banner showed **"Degraded performance · 5 active alerts"**
for an extended period. None were customer-impacting (all `ticket`/P3; the API
served correct data the whole time), but the persistent banner is itself a
reliability signal and had to be driven to zero. The five:

| # | Alert | Nature | Status |
| - | ----- | ------ | ------ |
| 1 | `stellarindex_api_latency_p99_high` | intermittent p99 tail-latency spikes (24ms↔3.4s) | investigating |
| 2 | `stellarindex_assets_popular_priceless` | 1 popular SEP-41 token renders priceless | investigating |
| 3 | `stellarindex_onchain_usd_volume_coverage_low` | on-chain usd_volume coverage 94.9% vs 99.5% bar | investigating |
| 4 | `stellarindex_verify_archive_unit_failed` (Tier A) | nightly chain-link verify failing | **RESOLVED** |
| 5 | `stellarindex_verify_archive_tier_b_unit_failed` | nightly checkpoint-anchor verify failing | **RESOLVED** |

---

## Alerts 4 & 5 — archive verification (RESOLVED)

### Root cause (single, shared)

`verify-archive-tier-b.service` on r1 had **drifted from its ansible template**.
The committed template (`configs/ansible/roles/archival-node/templates/systemd/
verify-archive-tier-b.service.j2`) specifies `User=stellarindex`, the
`run-heavy-job.sh` singleton lock, and `VERIFY_ARCHIVE_FROM={{ hot_floor }}`
(=49984000, the local-archive floor). The **live** unit on r1 (hand-edited
2026-08-25 04:06) instead ran **`User=root`**, `VERIFY_ARCHIVE_FROM=2`, and
called the binary directly (no lock). That single drift caused both alerts:

- **Alert 5 (Tier B) directly:** `-from 2` walks the checkpoint anchor from
  genesis, but the capacity migration (archive→cold-S3) leaves the local
  `galexie-archive` MinIO bucket holding only **[0–63999] + [49984000–tip]**.
  The from-genesis walk hits a missing LCM at ledger ~16M and exits 1.
- **Alert 4 (Tier A) indirectly:** Tier B running as **root** wrote the
  **shared** state file `/var/lib/stellarindex/verify-archive-state.json`
  root-owned `0600`. Tier A runs de-privileged (`User=stellarindex`, CS-118)
  and could no longer *read* it → died in 2ms with "permission denied".

A **second, latent** bug surfaced once Tier B was restored to `stellarindex`:
6,034 dirs under the local mirror `/srv/history-archive` were `root:root 0750`
(a fill run under umask 027), blocking `stellarindex` traversal → "permission
denied" on nested LCM files. (The other 244k dirs were correctly `root:root
0755`; files were all `0644`.) This is almost certainly *why* someone had
switched Tier B to root in the first place — a workaround that broke Tier A.

### Fix (applied + codified)

1. Restored r1's Tier B unit to the committed template (stellarindex, lock,
   `FROM=49984000`); reset the stale from-genesis `in_progress` checkpoint
   state (chain high-water 64112025 preserved); chowned the state file back to
   `stellarindex:stellarindex`.
2. Restored world-traverse on the 6,034 mirror dirs (`find … -type d ! -perm
   -o+rx -exec chmod o+rx`). Public ledger data — no secrets.
3. **Codified** the mirror-perm fix as an idempotent sweep in the archival-node
   role (`04-users.yml`) so it can't silently recur.
4. Verified: Tier A ran green (`chain-link integrity OK ✓`, exit 0); Tier B
   verified 957 checkpoints, 0 missed, 0 permission errors (capped test). Both
   units now `inactive` (not `failed`); the nightly timers (05:27 / 06:44 CEST)
   run the corrected config.

### Prevention / follow-ups

- The drift was a hand-edit that survived because nothing re-asserts these
  units between deploys. A periodic `ansible-playbook --check` drift alarm on
  r1's systemd units would have caught it. (Tracked separately.)
- Tier B's first post-fix run is a full ~2.5h pass from 49984000; subsequent
  runs are incremental. Deprioritized (CPUWeight/IOWeight=20) by design.

---

## Alert 1 — API p99 latency (root-caused; fix needs a deliberate API deploy)

**Not a real outage — a periodic cold-cache stampede.** Real request/response
latency is healthy (p50≈3ms, p95≈117ms, per-route p99 all <200ms). The p99
alert (`>2s for 2m`) trips on **intermittent** spikes, and the logs pin them
exactly: the only non-stream requests ≥2s are **`/v1/assets`** (~3.6s, always
**two concurrent**, every ~10 min) and **`/v1/pairs`** (~2.5s, every ~5 min).
(SSE streams — `/v1/ledger/stream`, `/v1/price/tip/stream` — log 60–90s
durations but are correctly excluded from the histogram by `isStreamingRoute`,
so they are NOT the cause.)

The regular period + the always-two-concurrent shape = a **cache-expiry
thundering herd**. `/v1/assets` has a stale-while-revalidate cache + prewarmer
(`asset_catalogue_cache.go`), but the cache key is the full arg-tuple, and the
tuples clients actually send (`limit=1` status-page probes, `limit=50`,
`limit=10&order_by=…&include=sparkline`) differ from what the prewarmer warms —
so those tuples are perpetually cold, and concurrent misses each recompute the
~3.6s query with no single-flight coalescing. This is the exact "phantom warmed
slot" failure the `stellarindex_api_cache_miss_rate_high` alert comment warns of.

**Fix (code — not rushed on r1):** (a) align the prewarmer's arg-tuples with the
tuples clients actually request (or coarsen the cache key so `limit` doesn't
fork the slot); (b) add single-flight coalescing so concurrent cold misses share
one recompute; (c) optionally make `/v1/assets?limit=1` cheap (the status probe
shouldn't compute the full set). Highest-value item for the "constantly
popping up" complaint — but it is an API change requiring test + a deliberate
deploy, so it is queued, not hot-patched.

**Update 2026-08-27 (sharper measurement + a ruled-out workaround).** Over a 90-min
window the slow requests are entirely `/v1/pairs` (~10s, every ~5 min) and
`/v1/assets` (~3.6s, two concurrent, every ~10 min), all **status 200** (slow,
not erroring), at a **regular cache-expiry cadence** — not load. Live latency is
sub-ms when warm, so these are heavy *cold recomputes*; the per-pair
`/v1/pairs` MarketsReader query is the worst at ~10s cold and is the top
optimization target. An **external cache-warmer (r1-local timer curling the hot
tuples) was considered and rejected**: the recompute is a blocking, histogram-
counted request regardless of whether a warmer or a real user triggers it — a
curl can't make it async — so it would not lower p99 and could add heavy DB load
by running the 10s query more often. The only fixes that work are in-binary:
make the SWR refresh truly async (never block a served request) and/or optimize
the `/v1/pairs`/`/v1/assets` cold query. Both require a deliberate deploy.

## Alerts 2 & 3 — pricing coverage (genuinely-unpriceable SEP-41 tokens)

Both fire on **SEP-41 tokens that trade only against other unpriced SEP-41
tokens** — closed clusters with no XLM/USDC anchor, which the usd-volume runbook
explicitly calls an "expected residual, not a regression."

- **Alert 2 (`assets_popular_priceless`):** one token, `CAUP7NFA…772J`
  (~$72k/7d, 6.4k trades, not wash). It trades only against `CBIJBDNZ…`, which
  in turn touches `native` **0** times — a closed Soroban cluster. It has
  market-character *volume* (computed through a bridge) but no derivable spot
  *price*, so it clears neither the price path nor the auto-withheld escape
  (`Volume24hUSD < $1000`).
- **Alert 3 (`onchain_usd_volume_coverage_low`, 94.9% vs 99.5%):** dominated by
  the **BLTA/BLTB/BLTC** soroswap triangle (`CCUYL75…`/`CAANCS…`/`CB2J5F…`,
  trading since April, ~7k trades/6h) — SEP-41↔SEP-41 with no USD leg.

**Fix (careful, not rushed):** these need pricing-system work, not a hot-patch —
either a genuine new quote-path/USD-proxy bridge for such clusters, or making
the coverage denominator/priceless tripwire exclude structurally-unpriceable
SEP-41↔SEP-41 pairs (so the alert stops firing on the expected residual). Both
touch money-serving pricing logic and belong on the branch with tests + a
fix-verifier pass before any deploy.

## Adjacent alerts observed during the sweep

- **`stellarindex_stellar_stack_lagging` (ticket):** stellar-core installed
  **27.1.0** vs candidate **28.0.1** (+ minor archivist 484→486). This is the
  **known Protocol 28 upgrade** (captive-core binary for galexie), due before
  2026-09-16. A deliberate maintenance action — not hot-patched during a
  monitoring window. Tracked in `docs/operations/protocol-upgrades.md`.
- **`stellarindex_deadmansswitch` (informational):** fires by design (proves the
  alert pipe is alive). Not a fault; excluded from the "degraded" verdict.
- **`stellarindex_anomaly_freeze_engaged` (P3) + `_active` (informational) —
  root-caused 2026-08-27 as a recurring single-source thin-fiat pattern.** The
  safety mechanism is working (the API serves the last-known-good VWAP with
  `flags.frozen=true` throughout — no bad price is ever served), but it is
  *cycling*, not blipping once. Measured on r1: **~40 freeze events / 6h on
  class=default** — one roughly every 9 minutes — so `rate(engaged_total[5m])`
  is essentially never zero and the ticket never clears. The `_active` gauge
  flaps present↔absent as each 30-min hold auto-releases and the next fires.

  The driver is the **thin fiat cross-rates** `crypto:XLM/fiat:GBP` and
  `crypto:XLM/fiat:EUR`. Sample decision from the aggregator log:

  ```
  freeze engaged pair=crypto:XLM/fiat:GBP class=default
    reason="phase2:3_signal_AND confidence=0.350 z=7.65 sources=1"
    hold_until=+30m corroborated=true → auto-released, extensions_used=0, escalated=false
  ```

  The tell is **`sources=1`**: these pairs have a single thin venue, so a
  momentary z-spike on that one feed has nothing to corroborate against and
  trips a freeze at low confidence (0.35). XLM/USD (deep, many venues) never
  does this. The pair *should* be derived as XLM/USD × the USD/GBP (USD/EUR)
  fiat rate rather than taken from one shallow direct XLM/GBP market; a
  single-source direct quote is what makes it fragile.

  **This is genuinely-recurring and worth fixing at root cause** (unlike a
  one-off spike). Candidate fixes, all money-serving aggregator/pricing logic —
  **branch work with tests + a fix-verifier pass, not a hot-patch** (mainnet
  deploys are on hold):
  1. **Prefer the derived cross-rate** for thin fiat quotes: compute
     XLM/{EUR,GBP} from XLM/USD × the fiat rate when the direct market is
     single-source, so the deep USD book (not one shallow venue) sets the price.
  2. **Require ≥2 sources for a z-score freeze** (or sharply down-weight a
     single-source z-freeze), so a lone venue's blip can't freeze a pair — while
     keeping the freeze for corroborated multi-source anomalies.
  3. **Monitoring-only stopgap** (no money-path change) if 1–2 slip: have the
     aggregator label the freeze counter with `sources`/confidence and exclude
     single-source auto-releasing cycles from the *ticket* (keep them on the
     `sustained`/`escalated` **page** path, which already correctly ignores
     benign fire→auto-release cycles per the 2026-08-06 reshape). This still
     needs an aggregator metric-label change, so it is deploy-gated too.

  Not an outage, not a page — the served data is correct the whole time. But it
  is the alert that most visibly "keeps popping up," and (1)/(2) remove the
  cause rather than muting the symptom.

## Net effect

The two genuinely-broken, recurring alerts (archive Tier A/B — a config-drift
bug) are **fixed at root cause and codified**. The remaining tickets are a
measurement/caching artifact (latency), two expected pricing residuals, and one
planned upgrade — each root-caused with a correct fix identified, none safe to
hot-patch on the production money box. They are queued as branch work +
deliberate deploys, per the money-app diligence bar.
