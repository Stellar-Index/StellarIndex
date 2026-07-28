---
title: v1 launch plan — THE single source of truth
last_verified: 2026-07-27
status: active
severity: P1
---

# Stellar Index — v1 launch plan (single source of truth)

> **⭐ THIS IS THE ONE PLAN.** Consolidated 2026-07-27 from every prior
> launch/production document, with every carried item **re-verified against
> live r1 + the repo on 2026-07-27** (not copied on trust). If you are
> resuming: read §0 (verified state), then execute §2 in order.
>
> Superseded by this doc (banners added; keep for history/recipes only):
> `production-readiness-master-plan-2026-07-18.md` (the campaign log),
> `production-readiness-remaining.md`, `docs/audit/audit-2026-07-16/go-live-master-plan.md`,
> `launch-todo.md`, `launch-day-checklist.md`, `public-flip.md`,
> `public-flip-runbook.md`, `notes/ROADMAP.md`, `notes/BACKLOG.md`.
> Still ACTIVE as companions: `production-confidence-campaign-2026-07-23.md`
> (the adversarial proof harness — its E-gate is §2.6 here) and the
> gitignored `production-remediation-ledger-2026-07-23.md` (finding-status
> authority). Runbooks under `runbooks/` remain the execution recipes.

## OPERATOR INBOX (questions parked by the launch loop — answer inline, loop picks up next iteration)

> Loop contract: no mid-loop questions to Ash. Everything needing an operator
> lands here with context + recommendation + what was done meanwhile.

### ⭐ Ash: do these in this order (everything else below is FYI)

| # | Item | Why it is first | Effort |
|---|---|---|---|
| 1 | **Flip `stellarindex_clickhouse_serving_enabled: true`** (§2.4) | The explorer is DOWN — 21/94 routes 503, user-visible on stellarindex.io. One-line change, dry-run verified, but it restarts all 3 services so it wants a human watching | ~10 min, ATTENDED |
| 2 | **Deploy v0.21.2 when cut** — carries the CS-102 supply-freshness fix | **37 of 48 watched assets currently serve FROZEN supply.** No decision needed any more; the fix is written, verified and committed (`e21fa3d0`) | deploy only |
| 3 | **Wire paging** — [runbooks/wire-paging.md](runbooks/wire-paging.md) | Alerts currently route to NOBODY; the first-24h watch would be blind | ~20 min |
| 4 | **Book the external security review** | Longest lead time of anything remaining — start it now even if other work continues | one email |
| 5 | **Approve removing archived rows written by the SAC seed** | ROOT-CAUSED 2026-07-28 and much narrower than first scoped. Our live SAC observer is CORRECT (0.009% vs Horizon); the whole PHO +157% is seeded rows for entries that are no longer live. Fix = filter the seed against the 1.15B `ttl` entries ALREADY in the lake, then re-seed. I have not touched the existing rows because that is a DELETE | decision only |

Lower priority / no rush: CoinGecko Pro key, off-site backup decision,
accepted-risk sign-off, IP-rotation + SSH CIDR, announcement copy,
the SolvBTC quote mislabel, the anomaly-freeze paging calibration.

- **[OP-standing]** The §3 register items all stand (paging wiring 2b is the
  most launch-critical). Nothing new requires a decision yet.
- **[DECIDE-new] Anomaly-freeze pages on CORRECT prices (thin FX crosses).**
  Found 2026-07-27 while the SAC seed ran (freeze predates the seed —
  engaged 14:39Z vs seed 15:36Z, NOT caused by it). 3 active freezes,
  all thin FX crosses (XLM/GBP ×2 windows, XLM/EUR), reason
  `phase2:3_signal_AND … z=8.71 sources=1`. **I verified both served
  prices against an independent FX cross: XLM/GBP +0.06%, XLM/EUR
  +0.21% — both correct.** So a `severity=page` alert
  (`anomaly_freeze_sustained`) is firing on good data, and
  `stellarindex_anomaly_freeze_engaged_total{class="default"}=382`
  says it is chronic, not a blip. At launch this burns the first-24h
  on-call for nothing. Also noted: the log carries
  `writer_wired=false` — the freeze is advisory, it does not gate
  writes, so the page has no corresponding automatic protection.
  My recommendation (NOT applied — it changes a money-path protective
  mechanism and I am not ≥70% on WHICH variant is right): the minimal
  fix is to stop paging on freezes where `sources=1`, since a z-score
  from a single source has no cross-source corroboration and is
  sparse-sampling noise. The larger question — should the freeze
  require corroboration to engage at all, and should the writer be
  wired? — wants your call. Ties into §4's C4-012/13 thin-pool row.
  Meanwhile: no change made; prices verified correct; documented here.
- **[RESOLVED 2026-07-28 — NO DECISION NEEDED. Superseded by CS-102.]**
  ~~supply-guard dormancy calibration~~. **Do not spend time on this; the
  question was wrong.** I had escalated it as "the sole thing preventing
  any supply value from updating" and asked you to sign off on loosening
  a data-trust guard. Root-causing it (see the CS-102 loop entry) showed
  the horizon was never the problem: the freshness anchor was measuring
  the wrong quantity — per-asset last activity instead of the observer's
  watermark — so quiet assets read as stalled. Fixed in code
  (`e21fa3d0`), verified against live r1, regression-tested.

  Two corrections to what I told you earlier, both mine:
  - "**0 snapshots in 6 hours**" was wrong — it read cumulative
    since-boot counters as a rate. The live delta was 3 of 4 ticks
    persisting.
  - "**966 dormancy rejections**" was wrong — `dormant` is an ACCEPTED
    outcome that inserts a snapshot. The real rejections were
    `stale_component` (7,025).

  Nothing to loosen: with the anchor fixed, a quiet asset reads fresh and
  publishes normally, and the guard still catches a genuinely dead
  observer. **The only action left is deploying it.**
- **[DECIDE-new] SolvBTC unsuffixed-feed quote mislabel (latent, pre-existing):**
  `SolvBTC_FUNDAMENTAL` / `SolvBTC.BBN_FUNDAMENTAL` are registered quote
  `fiat:USD` but demonstrably publish a NAV **ratio vs BTC** (~1.003 live +
  on-chain; BBN stores exactly 1.0). Correct quote is arguably `crypto:BTC`,
  but changing it rewrites an existing stored series. Recommendation: fix
  quote + one-time series annotation post-v1; NOT launch-blocking (redstone
  is IncludeInVWAP=false). Parked — no action taken beyond documenting the
  distinction in the registry comments.
- **DECIDED (auto, revertible) 2026-07-27:** moved the untracked old-vault
  backup `configs/ansible/inventory/r1.secrets.yml.lost-password-2026-07-27`
  → `~/.config/stellarindex/` (guard-rail script requires a clean tree;
  committing dead ciphertext to the public repo has no value; file preserved).
- **DECIDED (auto) 2026-07-27:** cut **v0.21.1** (sep41 ops-verify
  statement_timeout fix + CI/ansible guard entries; CHANGELOG entries for
  06ff3b5e and the six ops/CI commits added at promote time). This consumes
  the session's one-release budget — the redstone registry fix (below) lands
  on main and ships as v0.21.2 in a LATER session.

## Loop log (newest first)

- 2026-07-28 ~07:55Z — ✅ **EVICTION HYPOTHESIS PROVEN AGAINST THE LAKE —
  the TTL join works and the fix mechanism is validated.** No longer an
  inference from "Horizon disagrees"; it is now read directly from data
  we already hold.

  **Linkage established.** A Soroban TTL `LedgerKey` is 36 bytes —
  `type=00000009` + `sha256(LedgerKey)` — and the `TTLEntry` is 48 bytes:
  `lastModified(4) | type(4) | keyHash(32) | liveUntilLedgerSeq(4) |
  ext(4)`. So:

  ```sql
  SHA256(base64Decode(cd.key_xdr)) = substring(base64Decode(ttl.key_xdr),5,32)
  live_until = reinterpretAsUInt32(reverse(substring(base64Decode(ttl.entry_xdr),41,4)))
  ```

  Verified on a 50-ledger window: 500 contract_data keys → 294 joined
  (the remainder simply had no TTL *change* in that window).

  **Proof on the actual PHO offenders.** Resolving the contract_data keys
  at ledger 54,414,471 — where the largest seeded PHO balance was
  written — against tip 63,684,077:

  | keyhash | live_until | verdict |
  |---|---|---|
  | EB2333… | 54,431,750 | **EXPIRED** (9.25M ledgers ago) |
  | F5851B… | 54,431,750 | **EXPIRED** |
  | 0A4970… | 56,252,151 | **EXPIRED** |
  | 8742CB… | 56,488,043 | **EXPIRED** |
  | 3B1C75… | 66,771,588 | LIVE — extended at 63,661,189 |

  Four of five lapsed in 2024/2025 and were never extended. The fifth WAS
  extended recently and is correctly live — so the filter discriminates
  rather than blanket-dropping old rows, which is exactly the property
  needed to avoid destroying the seed's legitimate dormant-balance
  recovery (AQUA's).

  **Consequences.** The fix is a `WHERE` on data already in the lake, not
  new ingest. Note this also means `ledger_entries_current` serves
  archived contract_data to EVERY current-state reader, not just supply —
  the blast radius is wider than the supply number, and worth stating
  plainly in §2.4 rather than leaving implied.

- 2026-07-28 ~07:45Z — 🔬 **PHO +157% ROOT-CAUSED — it is the SAC SEED
  writing archived entries as live, not a general "eviction isn't
  ingested" gap. Narrower, provable, and fixable.**

  **Component isolation.** Three of our four PHO components match Horizon
  EXACTLY; only `sac` is wrong:

  | component | ours | Horizon | |
  |---|---|---|---|
  | trustline | 76,473,207.08 | 76,473,207.08 | ✓ exact |
  | claimable | 3.92 | 3.92 | ✓ exact |
  | lp | 6,603.53 | 6,603.53 | ✓ exact |
  | **sac** | **123,520,184.77** | **1,372,101.36** | ✗ 90× |

  **Split by row origin, and it resolves completely.** Seeded rows carry
  `intra_ledger_seq = 4294967295` (`SeedIntraLedgerSeq`), so they are
  trivially separable from live-observer rows:

  | origin | holders | units |
  |---|---|---|
  | live observer | 7 | **1,371,980.36** |
  | seeded (sentinel) | 39 | **122,148,204.41** |

  **Our LIVE observer is correct to 0.009%** against Horizon's
  1,372,101.36. The entire error is seeded rows — top 5 alone are 122.06M,
  at ledgers 54.1M–56.4M (Oct 2024 – Mar 2025), never updated since.

  **Cause.** `internal/storage/clickhouse/sac_balance_seed.go` selects
  `WHERE entry_type = 'contract_data'` and takes the latest state per key
  with **no liveness check whatsoever** — zero references to ttl /
  evict / archiv in the file. A Soroban entry that was archived still has
  its last-known state sitting in the lake, and the seed reads that as
  current. Horizon, which reflects live state, does not.

  **This is NOT PHO-only.** Seeded vs live SAC, worst first:

  | asset | seeded | live |
  |---|---|---|
  | KALIEN | **4,032,232,808,125** | 0.00 |
  | AQUA | 172,630,315 | 4,808,549,808 |
  | PHO | 122,148,204 | 1,371,980 |
  | XAU | 108,659,466 | 9,066,614,566 |
  | XRF | 95,315,974 | 593,082 |
  | KALE | 45,382,271 | 7,433,937 |

  KALIEN is 4 TRILLION units seeded against ZERO live. **But do NOT
  conclude every seeded row is phantom** — the seed's legitimate purpose
  is recovering dormant balances the live observer never saw, and AQUA
  reconciles at +0.18% WITH its seeded 172.6M included. Seeded ≠ wrong;
  seeded-and-archived is wrong. Only a TTL check separates them.

  **The fix is tractable because we already have the data:** the lake
  carries **1,153,487,878 `ttl` entries**. A Soroban TTL entry is keyed
  on `sha256(LedgerKey)`, and `ledger_entry_changes.key_xdr` is present
  for both sides, so the seed can join contract_data → ttl and drop keys
  whose `live_until_ledger_seq` has passed. That is a filter on an
  existing seed, not the "build eviction ingest" project this was
  originally scoped as.

  **Not acted on: removing already-seeded phantom rows is a DELETE, so
  it is parked per the guardrails.** See OPERATOR INBOX #5.

- 2026-07-28 ~07:23Z — ✅ **AQUA CLAIMABLE FIX CONFIRMED LANDED**, and the
  full supply-vs-Horizon sweep now separates three distinct causes.
  `scripts/ops/reconcile-supply-vs-horizon.sh`, all 8 classic assets
  against Horizon's FULL component sum:

  | asset | Horizon | ours | delta | verdict |
  |---|---|---|---|---|
  | AQUA | 99,923,674,166 | 100,104,871,790 | **+0.18%** | PASS ✅ |
  | VELO | 23,999,754,042 | 24,000,220,889 | +0.00% | PASS |
  | USDC | 351,793,900 | 351,945,989 | +0.04% | PASS |
  | yXLM | 154,910,541 | 155,056,806 | +0.09% | PASS |
  | BLND | 112,304,366 | 111,766,751 | −0.48% | PASS |
  | EURC | 2,632,900 | 2,601,251 | −1.20% | FAIL |
  | KALE | 303,494,623 | 307,406,857 | +1.29% | FAIL |
  | PHO | 77,851,915 | 199,999,995 | **+156.90%** | FAIL |

  **AQUA went −13.2% → +0.18%** — its claimable component now reads
  13.74B, matching the size of the former understatement. This is the
  fix Ash asked to have landed, and it is landed and evidenced.

  The three FAILs are NOT three problems:
  - **EURC + KALE are CS-102 casualties, not independent defects.** Both
    are frozen assets, so their delta is just drift accrued since they
    stopped publishing (KALE froze 03:23Z). Both should return inside
    tolerance once the CS-102 fix deploys — that is the acceptance test
    for it, and it must be re-run post-deploy rather than assumed.
  - **PHO (+157%) is the known Soroban-eviction blocker**, unrelated to
    either. Sharper evidence now: ours is 199,999,995 — essentially
    exactly PHO's 200M cap — while Horizon's components sum to 77.85M.
    So we are not mis-summing components; we are serving a
    max-supply-shaped figure, consistent with archived `contract_data`
    reading as live. Still parked for the [DECIDE] in OPERATOR INBOX.

  Caveat on this table: it covers the 8 CLASSIC assets the script knows.
  It says nothing about the ~30,745 other assets the claimable seed
  touched, and nothing about SEP-41/Soroban-native supply.

- 2026-07-28 ~07:20Z — 🔴 **CS-102: SUPPLY FROZEN FOR 37 OF 48 WATCHED
  ASSETS — I CAUSED IT, root-caused and fixed.**

  **What broke.** `MinClassicComponentLedger` computed the supply
  freshness anchor as `MIN over components of (per-ASSET MAX(ledger))`.
  For an event-driven observer that writes only on CHANGE, a per-asset
  MAX answers "when did this asset last see activity here" — which is
  not a freshness signal at all. **A quiet asset is not a stale asset.**

  **Why it was invisible for months.** `claimable_observations` was ~4%
  populated, so the `NULLIF` in that query excluded the claimable
  component for almost every asset and the wrong quantity was never
  consulted. **My claimable seed (03:00Z, 3.69M rows / 30,753 assets)
  populated it and un-latented the bug.** Every watched asset whose last
  claimable event predated the 17,280-ledger dormancy horizon began
  failing the gate → `stale_component` → snapshot refused.

  **The evidence is unambiguous.** Tip 63,684,077. Only the claimable
  component lags — trustline/lp/sac track tip within a few hundred
  ledgers for EVERY asset:

  | asset | trustline | lp | sac | claimable | supply |
  |---|---|---|---|---|---|
  | AQUA | 63,684,077 | 63,684,077 | 63,684,035 | 63,683,912 (−165) | fresh ✅ |
  | BLND | 63,684,077 | 63,684,075 | 63,683,730 | 63,653,006 (−31k) | frozen |
  | EURC | 63,684,077 | 63,684,035 | 63,683,843 | 63,518,086 (−166k) | frozen |
  | VELO | 63,684,077 | 63,684,054 | 63,683,580 | 63,363,440 (−321k) | frozen |

  The three assets still publishing (AQUA, yXLM, USDC) are EXACTLY the
  three with live claimable writes in 24 h — perfect correlation, no
  exceptions. KALE stopped 03:23 and PHO 03:28, minutes after the seed;
  XLM followed at 04:46. The effective production rule had become *"an
  asset publishes supply only if it recently had claimable activity."*

  **The observer was never dead** — its global watermark is 63,683,912,
  165 ledgers off tip. Nothing was actually stale; the gate was asking
  the wrong question.

  **Fix (`internal/storage/timescale/classic_supply_observations.go`).**
  The anchor is now the per-component OBSERVER WATERMARK — `MAX(ledger)`
  across ALL assets — so it answers "has this observer processed recent
  ledgers?". A dead observer still stops advancing across every asset,
  so stall detection survives; an asset with no observations anywhere
  still returns 0 (uninstrumented → gate skipped), which stops the
  change from handing a zero-valued supply a healthy-looking anchor.
  Note the function's doc comment ALREADY said "the slowest observer" —
  the query had always implemented something else.

  **Perf.** Naively the watermark is identical for every asset, so
  recomputing it per asset cost 5.6 s/asset (33.5 s for 6) — a 48-asset
  tick would take 4.5 min. Memoized on the Store for 30 s (two orders of
  magnitude tighter than the ~85-min threshold it feeds, so it cannot
  mask a stall): ~102 ms/asset, a ~55× improvement.

  Verified read-only against live r1: all five frozen assets now anchor
  at 63,683,912 (lag 165, inside the 1000-ledger threshold); the
  uninstrumented control still returns 0. Regression test added
  reproducing the exact production shape (quiet asset + live asset +
  stalled-observer case + uninstrumented case).

  **Consequence:** F-1320's dormancy carve-out and R-002's 24 h bound
  were both compensating for the wrong measurement. With the right one,
  a quiet asset reads fresh and publishes as `ok`; the carve-out stays
  only for genuine observer stalls. **Ships in v0.21.2 — until deployed,
  37 assets keep serving frozen supply.**

- 2026-07-28 ~07:05Z — D3 reproject running at ~100k ledgers / 3.6 min →
  25.7M ledgers ≈ **15 h**, ETA ~22:30Z. It holds the single heavy-job
  slot all day, so the sep41 tail rebuilds + redstone replay queued
  behind v0.21.2 cannot start until it finishes or is stopped.

- 2026-07-28 ~07:05Z — ✅ **ORDINAL RE-DERIVE COMPLETE** (5/5 chunks, **0
  failures**, ~35 min/chunk at ~52 ledgers/s) and **D3 STARTED**.
  `setup` created `stellar.ledger_entries_current_v2` as
  **`ReplacingMergeTree(version)`** — the composite
  `(ledger_seq << 32) | intra_ledger_seq` — while v1 keeps serving
  unchanged on `ReplacingMergeTree(ledger_seq)`. MV capturing live from
  tip 63,683,991. `reproject [38M → 63,683,991)` now running under the
  heavy wrapper, monitored.
  Nothing reads v2 until cutover, so this whole stretch is reversible
  via `rollback-precutover` (drop v2 + its MV). **Cutover remains
  ATTENDED** — acceptance is `reconcile-balances -sample 50` going
  19 mismatches → 0.

- 2026-07-28 ~07:45Z — **ordinal re-derive verified across ALL chunks,
  not just the first.** Sampling `ledger_entry_changes FINAL` at five
  points spanning the band: 63.05M **99.9%**, 63.15M **99.9%**, 63.25M
  **99.9%**, 63.40M **100%**, 63.52M **100%** non-zero
  `intra_ledger_seq` (~0.5M rows per probe). Coverage is uniform, so the
  chunking did not leave a seam and no chunk silently no-opped —
  the earlier single-chunk check could not have told us that.
  Final chunk ~15 min from done; D3's safe phases (setup → reproject →
  verify) queue next, cutover stays ATTENDED.

- 2026-07-28 ~06:30Z — **ordinal re-derive VERIFIED WORKING on the first
  completed chunk**, checked early rather than after the full 2.6 h.
  Reading `ledger_entry_changes FINAL` over [63,050,000, 63,050,200]:
  **969,427 of 969,992 rows (99.9%) now carry a non-zero
  `intra_ledger_seq`**; an untouched control band [63.30M, 63.31M) is
  still **0%**. The ~565 remaining zeros are legitimate — the FIRST
  change in each ledger genuinely has ordinal 0.
  *Read it with FINAL.* A non-FINAL count over the same chunk showed
  only 53.5%, because the re-derived rows and the originals coexist as
  unmerged ReplacingMergeTree parts until a background merge collapses
  them by `ingested_at`. That is the tool working as designed, not a
  half-finished job — but it would read as one.
  This unblocks D3: its composite version
  `(ledger_seq << 32) | intra_ledger_seq` can now actually discriminate
  a `state` before-image from its `updated` after-image in this band.

- 2026-07-28 ~06:05Z — **ordinal re-derive: first attempt OOM-KILLED, retuned from measurement, now running.** `-parallel 4` with the DEFAULT `-flush-every 500` was killed at the 20 G cap **22 seconds in** (4 workers x 500 buffered Soroban-era ledgers). Measured 1 worker @ flush-every=100 = **2.8 GB**, so retried at `-parallel 3 -flush-every 100` → **6.7 GB steady**, matching the 8.4 G prediction. Also CHUNKED into ~110k-ledger pieces (`scripts/ops/ordinal-rederive-chunks.sh`) because ch-backfill has NO resume — a multi-hour single run that dies loses everything, whereas each chunk is durable and idempotent. Original note:
- 2026-07-28 ~05:40Z — ordinal re-derive STARTED for [63.0M, 63.55M)
  (`ch-backfill`), first step of
  the C2-4c fix chain. Heavy slot was free after the claimable seed.
  ⚠️ **Hazard caught before launching: do NOT use
  `d2-ordinal-reproject.sh` on partition 63.** That script ends in
  `ALTER TABLE … REPLACE PARTITION` (line 143), which is safe on the
  STATIC partitions 39–53 it was written for, but partition 63 is the
  LIVE one — ingest appends to it continuously, so any row written
  between the staging snapshot and the replace would be silently
  DROPPED. `ch-backfill` is the correct tool here: it re-derives
  through `ExtractLedger` (which calls `extractLedgerEntryChanges`,
  extract.go:88) and writes idempotent ReplacingMergeTree inserts that
  supersede by `ingested_at` — no partition swap, safe against live
  ingest. Partition 38 IS static, so the D2 script remains correct
  there. Range is inside the retention-trimmed `galexie-live` bucket
  (tip 63.68M), so no `-bucket galexie-archive` needed.

- 2026-07-28 ~05:40Z — 🔴🔴 **THE SUPPLY REFRESH WORKER HAS A 0% SUCCESS
  RATE. Served supply values are FROZEN and cannot reflect any data
  fix.** Found by re-running the reconciliation after the claimable seed
  and asking why AQUA had not moved.
  - **The seed itself SUCCEEDED**: `claimable_observations` went
    1,030 → **3,694,623 rows** across **30,753 assets**, clean exit, 276
    chunks, no lock errors. For AQUA specifically the DB now holds
    **41,783 claimable balances = 13.90B AQUA** (was 927 = 574.6M)
    against Horizon's 13.74B — the data gap is CLOSED.
  - **But the served number is unchanged at −13.21%**, because the
    aggregator's supply-refresh worker persists nothing. Six hours of
    journal: **966× "rejecting snapshot — component ledger frozen past
    the dormancy horizon", 271× "no ledger … (lake lag?) — refresher
    will retry next tick", 2× "rejecting stale-component snapshot",
    and 0 successful persists.**
  - **This reframes the 35 `supply_refresh_error_dominant` alerts**:
    they are not "a few dormant assets are noisy", they are a TOTAL
    write-path outage. Every served supply figure is stale by at least
    6 hours and probably far longer.
  - **Consequence for the gate**: fixing claimable was necessary but
    NOT sufficient. §1 "Supply trustworthy" now needs the refresh
    unblocked as well, and the dormancy-horizon [DECIDE] in the
    OPERATOR INBOX is promoted from calibration-nicety to BLOCKER.
  - ✏️ **CORRECTION (verified 2026-07-28): the `no ledger` arm is NOT a
    bug and needs no code change.** I first flagged it as a probably-cheap
    target-ledger race worth fixing. Checked directly: ledger 63,681,736
    IS present in `stellar.ledgers`, and [63,681,700, 63,681,800] is
    perfectly contiguous (101 present / span 101). So the write simply
    had not landed at the instant the refresher looked, and it
    self-heals on the next tick exactly as its message claims. **The
    dominant blocker is the dormancy guard alone** (966 rejections vs
    271 transient races in 6h) — which is the OPERATOR INBOX [DECIDE],
    now the single thing standing between a corrected supply value and
    the API.

- 2026-07-28 ~04:35Z — **claimable seed WRITE PHASE underway**: rows
  1,031 → 1,786,622 → 2,344,508 against the dry-run's expected
  3,605,321. **276 chunks** created so far, matching the ~290 estimate,
  and no lock errors — the `max_locks_per_transaction=4096` precondition
  check held. Scan took ~5h10m (vs the dry-run's 3h50m; the extra is
  contention from the `archive-completeness` timer that overlapped).
  Next on completion: re-run `reconcile-supply-vs-horizon.sh` (expect
  AQUA −13.2% → ~0) and the AQUA spot-check.

- 2026-07-28 ~04:10Z — **verified the seeded rows will actually be READ**
  (checked before claiming the fix works, not after).
  `SumClaimableBalancesAtOrBefore`
  (`internal/storage/timescale/classic_supply_observations.go:263`) does
  `DISTINCT ON (claimable_id) … WHERE asset_key=$1 AND ledger <= $2
  ORDER BY claimable_id, ledger DESC … WHERE NOT is_removal`. All three
  properties the seed needs hold: historical rows are included (they are
  ≤ asOf), the highest-ledger row per balance wins (so a later live
  `is_removal` from a claim correctly supersedes a seeded row), and
  removals are excluded. The seed is therefore effective as written.
  ⚠️ **Latent, same class as C2-4c**: that `ORDER BY` tie-breaks on
  `ledger` ALONE — no `intra_ledger_seq` — so two rows at the SAME
  ledger for one `claimable_id` resolve arbitrarily. Not reachable by
  today's seed (it emits only live balances, and a same-ledger
  same-`observed_at` row collides on the natural key and is resolved by
  the `intra_ledger_seq`-guarded upsert), but the READ path lacks the
  guard the WRITE path has. Cheap hardening: add `, intra_ledger_seq
  DESC` to the ORDER BY. Not done tonight — it touches a money-path
  read while a seed is mid-flight.

- 2026-07-28 ~03:50Z — claimable LIVE seed progress + a false alarm worth
  recording. Go-side CPU fell from 73% to ~15%, which LOOKS like a stall;
  it is not. The work is server-side: ClickHouse shows **20 queries in 10
  min, avg 25.4 s, up to 633M rows read each**. Position extracted from
  `system.query_log`: ledger **~60.86M–60.92M of 63.68M ≈ 96% by ledger
  count**. Nothing is writing yet by design (`pg_stat_activity` has no
  insert — the reducer emits only after the final fold).
  **The AIMD window fix is demonstrably working in production**: observed
  windows of 62,500 then 125,000 ledgers, i.e. it narrowed under memory
  pressure and is doubling back up rather than staying pinned at the
  floor — exactly the failure the `9226f324` re-widen was added to
  prevent. *Diagnostic note for next time: low process CPU on this job
  means "waiting on ClickHouse", not "stuck"; the decisive checks are
  `system.query_log` for rate/position and `pg_stat_activity` for whether
  the write phase has begun.*

- 2026-07-28 ~03:20Z — **ops finding: the "one heavy job at a time" rule
  has no enforcement against SCHEDULED timers.** A long manual job that
  overruns into a timer window simply gets a second heavy scope beside
  it: the claimable seed (4h+) was joined by `archive-completeness`
  when its daily 04:19 timer fired. `run-heavy-job.sh`'s flock is
  PER-JOB-NAME, so different names never exclude each other, and each
  scope gets its own MemoryMax=20G — two concurrent jobs can therefore
  reserve 40G on a 188G box. Not an incident here (the seed stayed
  healthy at 73% CPU / 11.7 GB, just slower from contention), but the
  doctrine is aspirational rather than enforced. Options if it matters:
  a shared lock file for all heavy jobs, or a systemd slice with a
  global memory cap. Logged, not fixed — it needs a design decision and
  the current behaviour is degraded-but-safe.

- 2026-07-27 ~17:25Z (iter 3): the config-assertions unit (activated by
  the apply) fired 3 alerts — **2 were harness bugs, now fixed**
  (`52be4f98`): `galexie_writer_creds_valid` ran `[[ ]]` under dash
  (exit 127 → always FAIL; creds verified VALID by hand — the
  archivewriter fix DID land, so **rehydrate rollback is unblocked**),
  and `zfs_module_on_disk` was blocked by the unit's own
  `ProtectKernelModules=true` (dropped — it disabled a real
  pool-integrity guard and bought nothing here). r1 now 3 fails → 1.
  The survivor `compression_policies_applied` is REAL = §2.3.7,
  already queued. Also filed H1/H4 evidence (injection + error-leak
  spot-audit: PASS, RFC-7807 clean, no banners) and L1/L4 (freshness
  stamps live; `/v1/anomalies` honestly reports
  `divergence_checked:false` — post-v1 integration item).

- 2026-07-27 ~15:35Z (iter 2, cont. 4): drift rounds 3–5 — fixed the
  remaining idempotency classes: galexie/minio dataset dir_mode
  ping-pong (04820ce0) and the migrations sync stamping the
  controller's uid (6b2346e5 — owner/group sync off entirely; one-time
  chown to root on r1). Targeted re-apply now `changed=0` from the
  laptop. Drift-5 pending; expected residual = ONLY the stopped
  compute-completeness.timer (deliberate, §2.1; baseline file's own
  contract forbids parking it — drift goes green when the chain ends).
  sep41_supply rebuild confirmed healthy: 5 parallel CH streams ~9M
  rows each + active PG inserts; counters only tick per completed
  window; overnight ETA stands.
- 2026-07-27 ~14:50Z (iter 2, cont. 3): drift round 2 exposed three
  role idempotency bugs (disable-thp oneshot without RemainAfterExit;
  /var/lib/stellarindex 0750↔0755 mode ping-pong between two tasks;
  migrations rsync stamping the CONTROLLER's uid) — all fixed
  (`660e3b69`), applied to r1 (targeted tags, changed=3), drift round 3
  triggered. Expected residual red: ONLY the stopped
  compute-completeness.timer (self-heals when §2.1 finishes).
- 2026-07-27 ~14:30Z (iter 2, cont. 2): **CI SSH lockout caught+fixed** —
  the exclusive `authorized_keys` apply deleted the post-org-migration
  deploy key (`gh-actions-deploy@stellarindex`), hand-added 2026-07-15
  but never codified → `ansible-drift` went `unreachable=1`. Key
  recovered from the apply's `--diff`, restored on r1, codified in the
  inventory, `R1_INVENTORY_B64` secret updated, drift re-triggered.
  [OP] new sub-item: confirm the old `github-actions-deploy@r1-20260506`
  key is unused and prune it from admin_ssh_keys + r1.
- 2026-07-27 ~13:50Z (iter 2, cont.): **NEAR-MISS during the ansible
  apply** — the role's galexie install task copied a STALE pre-P27
  go-install leftover (`/root/go/bin/stellar-galexie`, June-10
  pseudo-version) over the live v27.0.0 binary; the post-install sha
  assert failed the play (good guard, wrong ordering) with production
  one galexie restart away from re-running the P27 crash-loop.
  RESTORED from the running process inode (sha `045caa5f…` == pin,
  verified); stale artifact renamed `.stale-pre-p27-20260610`; role
  hardened to assert the SOURCE artifact against the pin BEFORE
  install (`9670ef29`). Postgres restart from apply #1 landed clean
  (all services active). Apply #2 running to complete the remaining
  ~49 changes. ALSO: D4 reframed — account_observations is dormant
  reserve accounts, not a stall (see §2.3.2 + OPERATOR INBOX).
  sep41_supply rebuild paused for the apply (resumable, windows
  checkpointed); resumes right after apply #2.

- 2026-07-27 ~13:15Z (iter 2): **MAJOR — sep41 completeness was NOT a
  timeout artifact.** The v0.21.1 full verify finished and exposed a
  REAL zero-writer hole: since the sole-writer deploy (~ledger
  63,419,139 / 2026-07-13) the dispatcher skips the sep41 domain
  (F-1316) while the projector NEVER registered the sep41 sources —
  `BuildRegistry` only builds from `enabled_sources`, and the sep41
  names aren't in `KnownSources`, so no config could carry them. Both
  sep41 tables frozen at 63,419,138; 249k mismatched ledgers, Σ|Δ|=22M.
  FIXED on main (`ae7a082d`: registry always attempts sep41 from the
  watched set + regression test pinning the production shape + the
  reconcile topic0Syms perf fix — full verify was ~35/37 min discarded
  firehose, measured). Catch-up: `projected-rebuild -source sep41_supply
  -from 63419138 -to 63671020 -write -allow-live-overlap` RUNNING
  (justified: live projector provably has no sep41 source);
  sep41_transfers rebuild queued next; then full re-verifies; residual
  tail after v0.21.2 deploys. Ansible 69-task apply reviewed clean in
  check mode (`changed=70` incl. my stopped timer; minio chown is
  non-recursive) — applies AFTER the sep41 rebuilds (postgres restart
  would kill them). Perf report: sep41_supply_events is 276M rows not
  9.3M; needs VACUUM/ANALYZE (42/130 chunks never vacuumed) +
  compression policy (§2.3.7) — full report in loop context, key
  numbers preserved in the commit messages. NOTE: the 39 supply alerts
  "clearing" at 12:30Z was cosmetic (aggregator restart reset counters);
  observers still stalled, D4 still required.

- 2026-07-27 ~12:10Z (iter 1 close): **v0.21.1 DEPLOYED to r1** (all 6
  binaries, edge smoke 13/13, `-ch` copy done). sep41_supply FULL
  compute-completeness RUNNING under heavy wrapper (timer stopped;
  sep41_transfers queued next). Redstone registry fix (19→30 feeds)
  committed to main (`9bfcf5da`, verify.sh green) — ships v0.21.2 next
  session, then replay. **D3 CONFIRMED still required** (r1 table still
  `ReplacingMergeTree(ledger_seq)`, no v2); runner script committed
  (`scripts/ops/d3-lecur-v2-rebuild.sh`, staged on r1). Ordinal probe:
  only partition 38 + [63.0M,63.55M) lack ordinals among trafficked
  bands → pre-D3 step added to §2.3. B1 price sweep broadened + closed
  (campaign doc; gitignored so local-only). DECIDED (auto): savUSD/USDe/
  sUSDe classed crypto per ADR-0028's tradfi-only rwa definition.
  Launch trailhead: `set -a` before sourcing /etc/default/stellarindex
  when hand-running ops binaries (EnvironmentFile vars aren't exported).
- 2026-07-27 ~09:50Z (iter 1): guard cron armed; r1 spot-verified (matches
  §0; soak 5 PASS/0 FAIL). v0.21.1 tagged, release.yml building, deploy
  next. **Redstone §2.4 ROOT-CAUSED** (see §2.4). Redstone registry fix
  being implemented on main.

## What "v1 launch" means

Public, announced availability of the Stellar Index API + explorer as a
production service fit to present to Stellar: substrate certified complete,
served money-values proven correct, a signed repeatable deploy path, honest
capacity/HA posture, paging wired to a human, and the v1.0 wire shape frozen
(ADR-0042 — **Accepted and implemented**; the `kind` discriminator is live in
the spec, so the wire-freeze prerequisite is met).

## 0. Verified current state — 2026-07-28 (all checked live)

> **⚠️ READ THIS FIRST — four blockers were FOUND on 2026-07-27/28 that
> no prior plan knew about. If you are resuming, these dominate:**
>
> 1. **The explorer is DOWN in production.** 21 of 94 GET routes return
>    503 (all of `/accounts`, `/contracts`, `/ledgers`, `/tx`,
>    `/operations`, `/liquidity-pools`, plus `/assets/{id}/supply` and
>    `/holders`). Cause is one unflipped flag; the fix is dry-run
>    verified and waits only for an ATTENDED apply (§2.4). User-visible
>    on stellarindex.io today.
> 2. **~38% of sampled accounts serve a STALE pre-transaction balance**
>    (C2-4c reproduced live). Needs the ordinal re-derive BEFORE D3 —
>    D3 alone cannot fix it (§2.3.1, §2.4).
> 3. **Claimable balances were never seeded** → 30,748 assets
>    understated (AQUA −13.2%). Seed built, dry-run clean, **live seed
>    running now** (§2.3.3).
> 4. **We never ingest Soroban state eviction**, so archived entries
>    read as live — PHO supply +156.9% (§2.4). [DECIDE] interim TTL
>    filter vs real eviction ingest.
>
> Two tools were added to stop this class recurring:
> `scripts/ops/route-sweep.sh` (every OpenAPI GET) and
> `scripts/ops/reconcile-supply-vs-horizon.sh` (all classic assets vs
> Horizon's FULL component sum). Both exit non-zero on failure and
> belong in the post-deploy battery — the existing 13-GET smoke passed
> throughout every one of the failures above.

| Area | State |
|---|---|
| **Explorer** | 🔴 **21/94 GET routes 503** — whole tier dark. One-line fix dry-run verified, ATTENDED (§2.4) |
| **Balances** | 🔴 **C2-4c live**: ~38% of sampled accounts serve a before-image. Ordinal re-derive → D3 → re-verify (§2.3.1) |
| **Claimable** | ✅ SEED DONE — 3,694,623 rows / 30,753 assets; AQUA DB now 41,783 balances = 13.90B (Horizon 13.74B). Data gap CLOSED |
| **Supply refresh** | 🔴 **CS-102 — 37 of 48 watched assets serve FROZEN supply. ROOT-CAUSED + FIXED in code (`e21fa3d0`), pending the v0.21.2 deploy.** The freshness anchor measured per-ASSET last activity instead of the OBSERVER watermark, so quiet assets read as stalled and every snapshot was refused. Un-latented by the claimable seed. NO operator decision needed (the earlier dormancy-horizon ask is retracted). My earlier "0% success / 966 dormancy rejections" line was two measurement errors: cumulative counters read as a rate, and `dormant` (an ACCEPTED outcome) counted as a rejection |
| **Eviction** | 🔴 Not ingested at all; archived contract_data reads as live (PHO +157%) [DECIDE] |
| Deployed | **v0.21.1** (cut + deployed 2026-07-27, all 6 binaries, edge smoke 13/13, `-ch` copy done). Main is ahead with **v0.21.2 material NOT yet deployed**: sep41 projector wiring `ae7a082d`, redstone registry `9bfcf5da`, SAC seed windowing `7bede7e7`, claimable seed `120bf7c3`, **CS-102 supply-freshness `e21fa3d0`** |
| Lake | Dedup complete; post-dedup completeness re-audit PASSED; CH ingest at tip (lag seconds) |
| Galexie trim | Done + verified; cold reads OK. **Soak 8× PASS / 0 FAIL — evidence half MET**; now waiting only on the clock (treat as 17:00 **UTC**, see §2.5); snapshot `data/minio@pre-trim-2026-07-26` held (3.2 T) |
| D-series | D1 ✅, D2 ✅ (all partitions, 2026-07-23), CAGG re-mat ✅ ("ALL CAGG REMAT DONE" 2026-07-26). **D3: no run evidence on r1** — confirm need. **D4 NOT run** |
| Supply | REFRAMED — the 39 alerts decompose into the sep41 wiring bug (fixed, pending deploy) + **CS-102** (fixed, pending deploy), NOT a stall and NOT a calibration question. Historical note: `account_observations` **frozen at 63,632,946** (lake tip 63,669,421); guard correctly refusing stale snapshots → **39 `supply_refresh_error_dominant` alerts**. Fix = D4 (§2.3). `seed-sep41-genesis` WAS run 2026-07-26 (overriding the 2026-07-07 "do not run" verdict — verify AQUA in §2.6) |
| Completeness | All 3 ROOT-CAUSED + fixed in code 2026-07-27, pending the v0.21.2 deploy. sep41 ×2 = a 14-day ZERO-WRITER wiring hole (rebuilds cut mismatches 249,436→891 and 652, residual = post-rebuild tail only). redstone = upstream relayer added 11 feed_ids on 2026-07-24, NOT a regression; needs replay from 63,624,934 |
| Alerts | Above, plus `dex_nonstandard_decimals_detected` ×5 (informational — genuine non-7dp aquarius C-tokens, working as designed) + deadmansswitch (by design) |
| GH secrets | Deploy + Cloudflare + `R1_INVENTORY_B64` ✅. `ANSIBLE_VAULT_PASSWORD` / `ANSIBLE_VAULT_FILE_B64` ✅ set 2026-07-27 (drift now runs) |
| **Vault password** | ✅ **REBUILT + ROTATED 2026-07-27.** The old password (clobbered 2026-07-25 by a locally-run CI syntax step) was unrecoverable, so the vault was rebuilt from live r1 rendered values (26 keys; secret-template re-render proven byte-identical), encrypted under a NEW operator-held passphrase, pass file locked (`chflags uchg`), CI clobber-path guarded (2a23698e). Fresh creds generated for not-yet-deployed components (patroni ×2, CH serving profile, pgbackrest repo2 cipher, core placeholder); repo1 cipher + webhook keys empty matching live. Old vault kept as `.lost-password-2026-07-27`. GH secrets `ANSIBLE_VAULT_PASSWORD`/`ANSIBLE_VAULT_FILE_B64` set |
| Config drift | ✅ **GREEN 2026-07-27** (run 7) — apply landed, baseline 3→1, §1 gate CHECKED. History below: ✅ **`ansible-drift` FUNCTIONAL again** (first complete verdict since the rotation, 2026-07-27): `ok=243 changed=69 failed=0`. Three check-mode bugs fixed en route (timer-enables on unitless hosts e5edb17a/10802588; version-probe skip 2309f4d0 — which also proved the galexie drift-guard constants ALREADY agree, closing that "open operator action"). The red verdict is now REAL drift: **69 changed tasks = the pending config apply** (grown from the "33-task" estimate; incl. archivewriter cred fix, captive-core 18-validator quorum — 24 still live, triangulation chains, z=5.0, cold-tier render, postgres conf, ownership flips, timescale-jobs-probe + CH schema-snapshot units). Apply is §2.2 step 3, [ATTENDED] — service restarts incl. galexie (~1–3 min tip pause) + postgres |
| Deploy gate | `DEPLOY_APPROVAL_RELAXED=true` still set — **re-arm at launch** (§2.7) |
| Feeds | `COINGECKO_API_KEY` **not set** (feed dead since 2026-06-19, [OP]). `min_usd_volume=10000` since 2026-07-01 (older docs claiming 0 are stale) |
| Paging | 🔴 **NOT wired** — now TURNKEY via [runbooks/wire-paging.md](runbooks/wire-paging.md) (~20 min, [OP]); a silent-failure trap in the secrets file was fixed 2026-07-27. Baseline `pre-launch-check.sh` = 4 FAILs, 0 is the acceptance test. Original detail: (corrected 2026-07-27 — the env files exist but every value is EMPTY: 5× `HEALTHCHECKS_URL_*`, `HEALTHCHECKS_DEADMANSSWITCH_URL`, `SLACK_WEBHOOK_URL` all blank; only the node-level `HEALTHCHECK_PING_URL` is populated). Alert pages currently route to nobody — the original [OP] item stands: create Healthchecks.io checks + chat webhooks, paste URLs into `/etc/default/stellarindex-healthchecks` + `/etc/default/alertmanager-secrets` (then codify in the vault), rerun `pre-launch-check.sh` |
| ADRs | 0040–0048 ALL Accepted (incl. **ADR-0042 v1 wire shape** — the old "biggest unsigned gate" is resolved). hashdb wired but `enabled=false` on r1 |

## 1. Go-live gate (all must be true)

- [ ] **Supply trustworthy**: `supply_refresh_error_dominant` +
      `supply_cross_check_divergence` clear (or per-asset justified);
      AQUA/SEP-41 genesis-seed values spot-checked vs issuance truth.
      **2026-07-27: the AQUA spot-check RAN and FAILED** — not the
      expected +15.7% overstatement but a **−13.2% understatement** from
      the unseeded claimable component (§2.4). Three distinct blockers
      now: (a) claimable seed, (b) SAC full-history seed OOM, (c)
      dormancy calibration [OP].
- [ ] **Completeness green**: all sources `complete=true` (incl. sep41 ×2 + the new redstone gap); `/v1/coverage` two-axis verdict honest.
- [ ] **Prove-it battery passed** (§2.6): reconcile-balances, verify-lake/contiguity/hash-chain, re-derive determinism, price+supply vs external truth, `verify-usd-volume` calibrated.
- [x] ✅ **Config codified = live** — DONE 2026-07-27. The 69-task apply
      landed (`ok=259 changed=60 failed=0`) and **`ansible-drift.yml` is
      GREEN** (run 7; the first fully-green verdict this repo has ever
      produced). Getting there fixed 6 real defects: a CI-lockout from
      the exclusive-keys apply, a stale-galexie-binary near-miss, and 4
      idempotency bugs; the drift baseline SHRANK 3 → 1 entry.
- [ ] **Security posture**: creds rotated (ratesengine-admin, MinIO, anything session-exposed); approval gate re-armed; accepted-risk list explicitly signed; external security review booked/closed [OP].
- [ ] **DR honest**: off-site backup decision executed or explicitly
      risk-accepted [OP]; ~~restore-drill timer re-enabled~~ ✅ **DONE
      2026-07-27** (its capacity gate cleared: pool 94%→85%, 2,657 GB
      free vs the 200 GB floor; enabled + codified `205b041a`, next
      fire 2026-08-01); ZFS trim snapshot resolved (auto — §2.5 gate).
- [ ] **Launch mechanics**: ✅ `auth_mode=apikey_optional` VERIFIED live
      2026-07-27 (healthz + price both 200 unauthenticated); ✅ status
      page (301→/status/), explorer, docs, /methodology, /operations,
      /diagnostics all 200; ✅ **SLA definition PUBLISHED** — `/sla` was
      **404**; the four targets existed only in internal ops docs and
      are now a public page with the error budget and explicit
      exclusions (`535c7bcc`). Remaining: announcement ready;
      first-24h watch staffed [OP].
      ⚠️ **New fragility found**: the explorer static export fetches the
      live API per asset page and one run FAILED on **HTTP 429** from
      our own rate limiter (passed on retry). A launch-day rebuild
      could fail on this. Fix candidates: exempt the build's egress IP,
      lower build concurrency, or make the fetch back off on 429 rather
      than exhausting 5 attempts. Not launch-blocking; log it.

## 2. Critical path (dependency-ordered)

### 2.1 The sep41 chain (REVISED 2026-07-27 — deeper than the timeout)
✅ v0.21.1 cut + deployed (all 6 binaries; smoke 13/13; `-ch` copy done).
The full verify then exposed the REAL cause (see loop log iter 2): a
sep41 zero-writer wiring hole since ~2026-07-13. Remaining chain:
1. ✅ sep41_supply projected-rebuild DONE (37m3s, 21,939,833 events
   emitted, 0 decode errors, 250k ledgers — the 14h ETA was counter
   noise; matches the expected Σ|Δ|≈22M).
2. ✅ sep41_transfers projected-rebuild DONE (22m4s, 14,183,347 events,
   0 decode errors, 252,510 ledgers).
3. ✅ sep41_supply re-verify DONE — **fix PROVEN**: mismatched ledgers
   **249,436 → 891**, Σ|Δ| 22,051,087 → 74,269, and the first residual
   is ledger 63,671,021 = exactly the rebuild's `-to` bound. The
   remainder is purely the tail accumulating since the rebuild, which
   only stops growing when v0.21.2 (`ae7a082d`) deploys and live
   projection resumes. sep41_transfers re-verify RUNNING.
   `lake_complete=true` throughout — the archive was never at risk.
4. ✅ sep41_transfers re-verify DONE — same clean shape: **652
   mismatched, first = ledger 63,671,648 = exactly the transfers
   rebuild's `-to` bound.** Both sep41 sources are now correct up to
   their rebuild boundary; the only residual is the tail accruing until
   v0.21.2 deploys. `compute-completeness.timer` RESTARTED (§2.1.4
   done) → drift's last residual item cleared.
4b. **Post-deploy catch-up boundaries (measured 2026-07-27 18:36Z)** —
   `sep41_supply_events` is frozen at exactly **63,671,020** and
   `sep41_transfers` at **63,671,647**, i.e. each rebuild's own `-to`.
   Confirms no sep41 writer is running (expected until v0.21.2) and
   nothing else regressed. After the deploy, run `projected-rebuild`
   for each source `-from` its boundary above `-to` the then-tip, then
   re-verify. The gap grows ~1.5k ledgers/hour until then.
5. v0.21.2 (next session: carries `ae7a082d` + redstone `9bfcf5da`) →
   deploy → live sep41 projection resumes → final small rebuild for the
   deploy-gap tail → redstone replay from 63624934 (§2.4).

### 2.2 Restore the vault password → drift → config apply
1. ✅ ~~Vault password~~ — rebuilt + rotated 2026-07-27 (see §0).
2. ✅ ~~GH secrets + drift run~~ — drift functional, verdict `changed=69`.
3. ✅ **Config batch APPLIED 2026-07-27** (~14:00Z, two passes: pass 1
   died on the galexie stale-artifact guard — near-miss documented in
   the loop log, binary restored, role hardened `9670ef29`; pass 2
   clean `ok=259 changed=60 failed=0`). Post-apply battery green:
   all services active, edge smoke 13/13, galexie sha == pin,
   timescale-jobs-probe firing, ch-schema-snapshot/drift armed.
   Drift rounds 2–5 then burned down every idempotency bug (key
   lockout, thp oneshot, mode ping-pong ×3 dirs, migrations
   mtime+ownership) — **drift-5 residual = ONLY the deliberately
   stopped `compute-completeness.timer`** (self-heals at §2.1.4; the
   baseline file's contract rightly refuses to park it). Gate
   effectively met; confirm the first fully-green run after §2.1.
4. ✅ ~~Pass-file protection~~ — `chflags uchg` + ci.yml guard (2a23698e).

### 2.3 Served-tier population batch (heavy; ONE at a time under `run-heavy-job.sh`)
Order matters; each gates the next check. The DO-NOTHING trap applies:
`trades`/`oracle_updates` upserts never overwrite — corrections DELETE first.
1. ✅ D3 CONFIRMED required (2026-07-27: engine still
   `ReplacingMergeTree(ledger_seq)`, no v2 table). Runner:
   `scripts/ops/d3-lecur-v2-rebuild.sh` (staged on r1 at
   /usr/local/sbin).
   **PRE-STEP (mandatory — §2.4's C2-4c reproduction proves D3 alone
   cannot fix the affected accounts). Use the ALREADY-PROVEN D2
   script, not a bespoke ch-backfill:**
   ```
   run-heavy-job.sh d2-p63 /usr/local/sbin/d2-ordinal-reproject.sh 63 63
   run-heavy-job.sh d2-p38 /usr/local/sbin/d2-ordinal-reproject.sh 38 38
   ```
   Recomputing an already-ordinaled range is idempotent (the D2 doc
   proves the formula reproduces live-written ordinals EXACTLY above
   ledger 63,555,000), so covering all of partition 63 is safe even
   though only [63.0M, 63.55M) needs it.
   **Three preconditions VERIFIED 2026-07-27 — this is safe to run:**
   - The append-log is COMPLETE. The `state` and `updated` rows for one
     change carry DIFFERENT `change_index` (362 vs 363 on the sampled
     account), so they have different ORDER BY keys and both coexist.
     Nothing was lost — only the current-state dedup is ambiguous.
   - Because `change_index` differs, the D2 formula
     (`row_number() OVER (PARTITION BY ledger_seq ORDER BY tx_index,
     change_index)`) gives the two rows DISTINCT ordinals — exactly
     what D3's composite version needs to stop tying.
   - `ledger_entry_changes` is `ReplacingMergeTree(ingested_at)`
     ORDER BY `(ledger_seq, tx_hash, op_index, change_index)`; the
     re-derive preserves that key, so new rows SUPERSEDE old ones by
     `ingested_at`. No truncate, no duplication — DELETE-first does not
     apply here.
   **Then D3, split by risk — the first three phases are SAFE to run
   unattended, the fourth is NOT:**
   ```
   # SAFE: builds v2 ALONGSIDE v1, which keeps serving throughout.
   run-heavy-job.sh d3-setup     /usr/local/sbin/d3-lecur-v2-rebuild.sh setup
   run-heavy-job.sh d3-reproject /usr/local/sbin/d3-lecur-v2-rebuild.sh reproject 38000000 <tip>
   /usr/local/sbin/d3-lecur-v2-rebuild.sh verify     # read-only
   ```
   `reproject` is resumable (progress file) and every phase is
   idempotent; nothing reads v2 until cutover, so a failure at any
   point costs only time.
   ```
   # ATTENDED ONLY — swaps the SERVED current-state table.
   /usr/local/sbin/d3-lecur-v2-rebuild.sh cutover
   ```
   Cutover drops both MVs, double-RENAMEs, recreates the MV and runs a
   catch-up from the recorded pre-cutover tip. It is a few ms of DDL,
   but it is the moment account-state / asset-holder / SAC reads change
   table underneath them, and `rollback-precutover` stops being the
   easy escape. Reference:
   `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql`.
   **Acceptance after cutover**: `reconcile-balances -sample 50` must
   report 0 mismatches (it was 19/50 before).
   ⚠️ **Read `verify` output with care**: its divergence rows are only
   meaningful where `v2_ils > 0`. A row with `v2_ils = 0` is an
   UNRESOLVED legacy tie, not a correction — the 2026-07-18 rehearsal
   note in that SQL file explains why it looks identical otherwise.
2. **D4 REFRAMED (2026-07-27 investigation)** — `account_observations` is
   NOT stalled: it holds exactly the 16 SDF reserve accounts, whose last
   changes are legitimately sparse (dormant by design; trustline/LP/SAC
   observation siblings are all AT TIP). The 39 supply alerts decompose:
   (a) SEP-41 assets → the sep41 zero-writer bug (fix in flight, §2.1);
   (b) XLM + slow classic assets (e.g. BLND, gap 17,922 vs horizon
   17,280) → the supply-refresh dormancy horizon (~1 day) is too tight
   for structurally-dormant components. No replay needed for (b) — a
   replay would re-derive identical rows. Remaining D4 action: after the
   sep41 chain clears, re-count the surviving alerts; for those, decide
   the `WithMaxDormantComponentLedgers` calibration (see OPERATOR INBOX
   [DECIDE-new]).
3. `supply seed-sac-balances -full-history` — **FIX LANDED `7bede7e7`**
   (was: OOM at its own 8 GB ceiling, the THIRD budget breach of this
   query). Now ledger-WINDOWED with a Go-side latest-wins reducer over
   the same C2-4 ordering tuple; measured 1.48–1.75 GiB per 250k window.
   The fix also caught a latent **correctness** bug: per-column argMax
   could resolve a same-key tie differently for each column and stitch a
   row from two different changes — now one argMax over a column tuple.
   **VALIDATED on real data 2026-07-27**: the dry-run that previously
   OOM'd now completes — 54,849 Balance rows across **38/38** SAC
   wrappers. LIVE seed RUNNING (side-loaded `stellarindex-ops-sacfix`;
   r1's deployed ops binary stays v0.21.1 until the v0.21.2 deploy).
   Additive fill of absent rows, not a correction, so the DELETE-first
   rule does not apply. ✅ **LIVE SEED DONE 2026-07-27: 54,863 Balance
   rows across 38/38 wrappers.** Next: confirm the 2
   `supply_cross_check_divergence` alerts clear on the next
   supply-refresh cycle, then re-run the AQUA reconciliation to split
   the −13.2% into its SAC vs claimable parts.
4. `projector-replay -source redstone -from 63624934` (after the v0.21.2
   registry fix deploys — §2.4; then re-run redstone compute-completeness
   including the false-clean [63,624,934, 63,661,714] range).
5. `ch-participant-backfill -from 2 -window 500000` (~2–4 d, resumable —
   queued since 2026-07-07; incoming-ops surface is ~1-day-only until run).
6. `MATERIALIZE idx_lecur_account_id` (off-peak) + bloom index only if the
   bound-UNION fix proves insufficient (measure first).
7. TimescaleDB compression policies (`scripts/ops/add-missing-compression-policies.sql`, post-D4); CH system-log TTL at next CH restart.

### 2.4 Investigations (parallel, code-side)
- **🔴 NEW 2026-07-27 — THE EXPLORER'S CORE ENDPOINTS 503 IN
  PRODUCTION.** `/v1/accounts/{addr}`, `/v1/ledgers`, `/v1/contracts`
  all return **503 "Explorer unavailable — this deployment hasn't
  wired the ClickHouse explorer reader (ADR-0038)"**. `/v1/assets/*`
  and pricing are unaffected (200).
  - Cause: `storage.clickhouse_serving_user = ""` in
    `/etc/stellarindex.toml`, because
    `stellarindex_clickhouse_serving_enabled` defaults to **false**
    (`archival-node/defaults/main.yml:326`). This is a deliberate
    TWO-STEP: provision the CH-side profile first, flip the API flag
    second. Today's apply did step 1; step 2 was never done.
  - **Step 1's precondition is now satisfied and I verified the whole
    path works**: CH user `api_serving` EXISTS
    (`system.users`), `STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD` is
    populated on r1, and authenticating as that user against the lake
    returns real data (counted 8,314 ledgers above 63.67M). So only
    the flag stands between us and working explorer endpoints.
  - **NOT flipped tonight — deliberate.** It restarts all three
    services and enables previously-unused serving read paths while a
    4h heavy seed is mid-flight. Doing that unattended at midnight is
    how a config gap becomes an incident.
  - ✅ **DRY-RUN VERIFIED 2026-07-28** (`--check --diff`, extra-var, no
    file edits). The change is exactly one line —
    `clickhouse_serving_user = "" → "api_serving"` — plus an unrelated
    pending "Tier D verify-archive weekly cron". It fires handlers
    **Restart stellarindex-{indexer,aggregator,api}**, so expect a
    brief ingest + serving blip; run it attended.
    **Exact command (note the tag is `stellarindex`, NOT
    `stellarindex-services` — the latter matches nothing and returns a
    misleading `changed=0`):**
    ```
    cd configs/ansible
    ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
      --diff --tags stellarindex \
      -e stellarindex_clickhouse_serving_enabled=true
    ```
    Then persist it by setting `stellarindex_clickhouse_serving_enabled:
    true` in `inventory/r1.yml` (+ re-upload `R1_INVENTORY_B64`) so it
    survives the next apply, and verify with:
    ```
    bash scripts/ops/route-sweep.sh          # expect server_5xx=0 (was 21)
    ```
    plus an account-balance spot-check against Horizon, which also
    re-tests the §2.4 C2-4c finding through the public API.
  - ⚠️ **FAR WIDER THAN FIRST THOUGHT — 21 of 94 GET routes (22%) are
    503**, found by the new `scripts/ops/route-sweep.sh`. Not 3 routes,
    a whole product tier:
    `/accounts`, `/accounts/{id}`, `/accounts/{id}/transactions`,
    `/accounts/{id}/operations`, `/accounts/{id}/movements`,
    `/contracts`, `/contracts/{id}`, `/contracts/{id}/wasm`,
    `/contracts/{id}/interactions`, `/contracts/{id}/code-history`,
    `/ledgers`, `/ledgers/{id}`, `/ledgers/{id}/transactions`,
    `/tx/{id}`, `/operations`, `/liquidity-pools`, `/pools/reserves`,
    `/lending/pools/{id}/reserves`, `/network/throughput`, and —
    notably — **`/assets/{id}/supply` and `/assets/{id}/holders`**.
    These cannot be fixture artifacts: the 503 is returned BEFORE
    parameter validation.
  - **Sweep now fully trustworthy** (2026-07-28, after fixing a bash 3.2
    associative-array bug in my own tool that made every route request
    the same nonsense id). Final tally: **21×5xx** (the real defect),
    **9×401** (auth-scoped — correct), **26×400** (missing required
    QUERY params; the sweep fills path params only — legitimate),
    **2×404** (`/external/assets/usdc`, `/issuers/{G…}` — plausibly
    absent records, not drift). The 5xx count was identical across
    every fixture bug, which is why it was safe to act on first.
  - **Why every prior check passed**: `r1-smoke.sh` is 13 hand-picked
    GETs and the SLA probe exercises pricing. Neither touches the
    explorer tier. The campaign's C1 track claimed a "98-route smoke ✅"
    — that claim does not survive this sweep and should be treated as
    refuted until re-run.
  - **User-visible on stellarindex.io TODAY** — `/accounts/`,
    `/ledgers/`, `/contracts/`, `/liquidity-pools/`, `/operations/`
    all serve a 200 static shell whose data comes from the dead
    routes. Each of those segments has an `error.tsx` boundary
    rendering the shared `RouteError` surface — **"The <section> page
    hit an error"** plus a Try-again button — so a visitor gets a
    visible failure, not an empty page.
    *Evidence honesty*: the 503s and the error-boundary copy are both
    VERIFIED (route sweep; `web/explorer/src/components/RouteError.tsx`
    lines 37-45). That the boundary actually trips on these fetches is
    INFERRED, not observed — a `curl` only returns the pre-JS shell,
    and my first attempt to prove it by grepping the HTML for "error"
    was a FALSE POSITIVE (it matched framework strings in the bundle).
    Browser verification was attempted and blocked (extension not
    connected). Confirm visually when convenient.
  - **This is a hard launch blocker** — "explorer" is the product
    name; an explorer that 503s on accounts, ledgers, contracts and
    transactions is not launched. Add to §1 Launch mechanics, and add
    `route-sweep.sh` to the post-deploy battery so a dark subsystem
    can never again pass a green smoke.
- **🔴🔴 NEW 2026-07-27 — 38% OF SAMPLED ACCOUNTS SERVE A STALE
  PRE-TRANSACTION BALANCE. C2-4c reproduced on live data.**
  `reconcile-balances -sample 50` (tolerance 0): **18 matched, 19
  mismatched**, every mismatch **exactly 1 stroop, always ours LOW**.
  Not noise — systematic.
  - Ruled out first: the verifier's Horizon parse is exact
    (string-based `DecimalStringToScaledInt`, truncating, no float),
    and Horizon's `last_modified_ledger` for a mismatched account
    equals OUR snapshot ledger — so this is **not** a missed change.
    We disagree about the SAME ledger's state.
  - **Direct evidence** — `ledger_entry_changes` for account
    `GA3GJ…QZA3L` at ledger 63,378,766 holds TWO rows:
    `state` balance **10,099,944** and `updated` balance
    **10,099,945** — and **both carry `intra_ledger_seq = 0`**. The
    `ReplacingMergeTree(ledger_seq)` therefore ties and keeps an
    ARBITRARY row; here it kept the before-image. That is exactly
    audit C2-4c / CS-021.
  - **Why D3 ALONE WOULD NOT FIX THESE ACCOUNTS**: D3's composite
    version is `(ledger_seq << 32) | intra_ledger_seq`, which still
    ties when the ordinal is 0 on both rows. Ledger 63,378,766 sits in
    the **un-ordinaled [63.0M, 63.55M) band** the ordinal probe found
    earlier today. So the §2.3.1 pre-step (ch-backfill re-derive over
    partition 38 + [63.0M, 63.55M)) is **mandatory before D3**, not
    optional — this is the empirical proof of that, previously only a
    theoretical concern.
  - **Impact**: account balance is the single most-read money value in
    an explorer, and ~38% of active accounts can serve a pre-transaction
    figure. Broader than PHO. The uniform 1-stroop delta is consistent
    with a 1-stroop dust campaign supplying the transactions; the SIZE
    of the error is incidental — the same defect serves an arbitrarily
    wrong balance whenever the last change is larger.
  - **BLAST RADIUS QUANTIFIED 2026-07-27 — it is not just accounts.**
    Sampling ledgers [63,378,000, 63,379,000] for rows with
    `intra_ledger_seq = 0`, the share of (key, ledger) pairs carrying
    MORE THAN ONE row — i.e. a `state` before-image tied with its
    `updated` after-image, tie-broken arbitrarily:

    | entry_type | tied | total | % tied |
    |---|---|---|---|
    | liquidity_pool | 60,415 | 60,416 | **100.00** |
    | data | 145 | 145 | **100.00** |
    | account | 797,267 | 797,838 | **99.93** |
    | trustline | 238,463 | 239,168 | **99.71** |
    | offer | 128,156 | 134,866 | **95.02** |
    | contract_data | 146,035 | 275,082 | 53.09 |
    | ttl | 68,375 | 197,436 | 34.63 |
    | claimable_balance | 6,740 | 56,979 | 11.83 |

    So virtually EVERY entry that changed in the un-ordinaled band has
    an ambiguous current-state row. The 38% observed wrong-balance rate
    is simply how often the arbitrary pick lands on the before-image —
    the AMBIGUITY is ~universal there.
  - **Which entries are actually at risk**: only those whose LATEST
    change falls in the un-ordinaled range — partition 38 and
    [63.0M, 63.55M) (live ingest has written ordinals since
    ~63,550,000). That is still ~550k ledgers ≈ a month of history, and
    it covers accounts, trustlines (→ classic supply), LP reserves,
    and offers.
  - Why the supply reconciliation still passed 5/8: a supply total sums
    thousands of trustlines, most of which last changed OUTSIDE the
    band, and errors in both directions partly cancel. Aggregates mask
    a defect that per-entity reads expose — which is exactly why the
    account-level check found it and the asset-level one did not.
  - **Blocks §1 "Prove-it battery" and arguably "Supply trustworthy".**
    Sequence: ordinal re-derive → D3 → re-run `reconcile-balances
    -sample 50` and require 0 mismatches as the acceptance test.
- **🔴 NEW 2026-07-27 — PHO served supply is +156.9% vs Horizon.**
  Found by the new `scripts/ops/reconcile-supply-vs-horizon.sh`, which
  reconciles ALL 8 tracked classic assets against Horizon's full
  component sum (the check B3 never did). Full run: **5 PASS, 3 FAIL**
  — AQUA −13.22% (known claimable gap), **PHO +156.90%** (NEW, severe),
  KALE +1.31% (NEW, marginal — actively minted, may be timing).
  - PHO isolated to the **SAC component**: ours 123.5M PHO across 46
    contract holders vs Horizon's `contracts_amount` **1.37M** (~90×).
    That difference (122.1M) almost exactly equals the total gap.
  - **Control proves the pipeline is sound generally**: for USDC our
    SAC component is 40.13M vs Horizon's 40.26M — 0.3%. So this is not
    a systematic SAC bug; PHO is specifically anomalous.
  - The PHO holders' latest lake change is ledger **54.4–56.4M**, i.e.
    the dormant pre-floor pool balances CLAUDE.md describes, and the
    rows carry `intra_ledger_seq=4294967295` (the seed sentinel), so
    they came from today's full-history seed reading the lake's latest
    state for those keys.
  - **TWO LIVE HYPOTHESES, opposite conclusions — do not assume:**
    (a) the balances are STALE and we overcount (the lake's last change
    for those keys is old because later changes are missing), or
    (b) the balances are REAL and **Horizon undercounts** — Horizon
    began tracking contract balances relatively recently, so a balance
    written before that and never touched since could be absent from
    its aggregate. This is the exact mirror of the claimable case, and
    the repo's own 2026-07-06 verdict says these ARE ordinary
    `Vec(Symbol("Balance"), Address(pool))` entries.
  - ⭐ **HYPOTHESIS (c), added 2026-07-27 and now the most likely —
    SOROBAN STATE ARCHIVAL.** Contract data entries have a TTL and are
    ARCHIVED when it lapses; an archived entry is no longer live state,
    so Horizon correctly excludes it while our seed — which takes each
    key's LATEST WRITE as current — still counts it. This explains
    every observation at once: the PHO holders' last write is ledger
    54.4–56.4M (old enough for any TTL to have lapsed), USDC passes
    because its contract balances are actively used and therefore TTL-
    renewed, and it is exactly the "dormant" population the seed was
    built to recover. **The lake DOES track this: `entry_type='ttl'`,
    150,636,726 rows above ledger 63M.** If true, the "recover dormant
    pre-floor balances" premise is partly recovering DEAD state, and
    both `StreamSACBalanceSeedsFullHistory` and the cross-check's
    documented BLND/EURC/KALE/PHO case need revisiting.
  - **Note the claimable seed is NOT exposed to this**: claimable
    balances are CLASSIC ledger entries with no TTL and no archival.
    Only `contract_data` (SAC balances) can be archived. So this does
    not undermine the AQUA fix.
  - ✅ **(c) CONFIRMED BY CODE READ 2026-07-27 — we do not ingest
    Soroban state eviction AT ALL.** `rg` for
    `EvictedTemporaryLedgerKeys` / `EvictedPersistentLedgerEntries` /
    `evicted` across `internal/` + `cmd/` returns **nothing**;
    `extract_entry_changes.go` knows `ttl` only as an entry TYPE
    (line 268) and has no archival logic. Explicit deletions ARE
    captured (4,351,427 `removed` contract_data changes in ledgers
    [63.0M, 63.1M]), so the gap is specific to EVICTION, not removals
    generally. Consequence: an archived entry's last write stands as
    "current" in our lake forever.
  - **Scope is wider than supply.** `ledger_entries_current` — the
    served current-state projection behind account-state, asset-holder
    and SAC-seed reads — never sees the eviction either, so it serves
    archived entries as live. Any surface reading current contract
    state inherits this.
  - **Fix direction**: capture the LedgerCloseMeta eviction fields in
    `ledgerstream`/`extract_entry_changes` and emit them as `removed`
    changes, then re-derive. Until then a cheaper mitigation is to
    filter the SAC seed on TTL liveness (the lake HAS `ttl` entries —
    150.6M above ledger 63M — so `live_until_ledger` is derivable
    without new ingest).
  - **Not launch-blocking by itself for the 5 passing assets**, but PHO
    is served wrong TODAY and the class is systemic. [DECIDE] whether
    v1 ships with a TTL-filtered seed (fast) or waits for real
    eviction ingest (correct).
  - Until settled, PHO's served supply is NOT trustworthy in either
    direction. Blocks §1 "Supply trustworthy" alongside the claimable
    seed.
- **🔴 NEW 2026-07-27 — claimable-balance supply component is UNSEEDED
  (material classic-supply understatement).** Found running the §2.6
  AQUA honesty check. `claimable_observations` holds **997 rows total**,
  ledger range [63,301,831 → tip] — i.e. only live-observed changes; it
  was never seeded from history like `trustline_observations` (2.48M
  AQUA rows alone). For AQUA we hold 927 of Horizon's 41,685 claimable
  balances = **574.6M of 13,737.6M AQUA (4.2%)**, so served AQUA total
  supply is **86.70B vs Horizon's component sum 99.92B = −13.2%**.
  Arithmetic confirms the component IS summed but under-populated:
  trustlines 80.74B + claimable 0.57B + LP 0.52B + SAC 4.93B = 86.76B
  ≈ served 86.70B (0.07%). **Every classic asset with pre-63.3M
  claimable balances is understated by them.**
  - Why prior checks missed it: campaign track B3 verified Algorithm 2
    against the **trustline sum**, which is exact — the claimable
    component was never in the comparison. §2.6's AQUA item was also
    looking for the 2026-07-07 **+15.7% OVERSTATEMENT**; the seed
    fixed that direction and the real defect is the opposite sign.
  - **LP shares the root cause but NOT the impact — MEASURED, no seed
    needed (2026-07-27).** `lp_reserve_observations` also starts at
    ledger 63,300,828 (never seeded), yet our latest-per-pool AQUA total
    is **516,524,268 across 1,072 pools vs Horizon's 517,261,343 across
    1,303 — only −0.14%**. The 231 missing pools are dust. **Why the two
    components diverge so sharply is the point**: LP reserves change on
    EVERY swap, so any pool with activity re-observes itself within days
    and self-heals; a claimable balance is written ONCE and then sits
    untouched until claimed, so it can never self-heal and the live-only
    window captures almost none of them. That asymmetry is what makes
    claimable 4% populated and LP 99.86%. **Decision: no LP seed for
    v1** — the fix exists if a dormant-pool audit ever justifies it.
    Seeding state by component: trustlines seeded deep (34.96M rows from
    ledger 31.8M) ✅; sac partial (2.30M from 61.3M); **claimable +
    lp NOT seeded (both from ~63.30M = observer deploy)**.
  - **`state-snapshot` is NOT the fix** (checked 2026-07-27): it writes
    via `clickhouse.InsertEntryChanges` (`internal/ops/ingest/state_snapshot.go:137`)
    — ClickHouse only. The observation tables are written by a separate
    Postgres path (`internal/storage/timescale/classic_supply_observations.go:139,208`)
    fed by the live observers. Correct fix = a NEW seed subcommand
    mirroring `supply seed-sac-balances`: read current state from the
    lake, write into the Postgres observation table.
  - ⚠️ **Shares a blocker with §2.3.3**: a *current-state* seed inherits
    the CH projection's ~62M coverage floor, so it needs the
    `-full-history` read — which is exactly the query that just OOM'd.
    The per-contract/windowing fix in flight for the SAC seed is the
    precedent the claimable seed should reuse. Sequence: land the SAC
    memory fix first, then build the claimable seed on the same shape.
  - Gate impact: blocks §1 "Supply trustworthy" independently of the
    SAC/dormancy items.
  - ✅ **DRY-RUN COMPLETED CLEAN 2026-07-27 (no OOM, 3h50m)** and it
    answers the blast-radius question: **3,605,321 live claimable
    balances across 30,748 classic assets**. This was never an
    AQUA-only defect — every one of those assets has been understated
    by its pre-63.3M claimable balances. Peak memory ~12.4 GB, settling
    ~11.6 GB, inside the 20 GB cap; the O(window) redesign is NOT
    needed. **LIVE SEED RUNNING.**
  - ✅ **FIX BUILT `120bf7c3`** — `stellarindex-ops supply
    seed-claimable-balances`, built on the proven windowed reader.
    Defaults to EVERY classic credit asset (`-assets` narrows only by
    explicit opt-in). Writes through the SAME upsert SQL as the live
    observer (extracted to a shared constant) so seeded rows are
    indistinguishable from observed ones, stamped
    `SeedIntraLedgerSeq` so a live change can never be overwritten.
    **First r1 dry-run FAILED and produced a real fix (`9226f324`)**: it
    bisected to the then-floor 15,625 ledgers and still exceeded the CH
    ceiling at [40,484,378, 40,500,002] — the airdrop era, where a few
    thousand ledgers mint millions of claimable balances, so the floor's
    "a few thousand keys" premise was false. Floor now 256, and the
    width RECOVERS after sustained success (it was monotonically
    narrowing, which would have pinned the walk at the floor for the
    remaining ~23M ledgers). Re-run IN FLIGHT.
    verify.sh green + 4 testcontainers integration tests (`0e73d789`,
    incl. a parity test proving the seed's output matches the LIVE
    observer's for the same fixture). **Dry-run against r1 IN FLIGHT**
    (side-loaded `stellarindex-ops-claimable`); expect it to account
    for AQUA's missing ~13.16B. Then live seed → re-run the AQUA
    reconciliation.
    ⚠️ **MEASURED 2026-07-27, and it is the pessimistic case**: 55 min
    into the dry-run the Go process sits at **12.4 GB against the
    heavy-job wrapper's 20 GB cap (58%) and is still walking**. The
    author's own estimate put 50M live balances at ~12 GB "tight under
    the cap" — we are in that regime. The reducer holds every live
    balance until the final fold and **emits nothing until the end**,
    so an OOM-kill at 95% loses the entire run. If this dry-run dies,
    the design needs bounding, not tuning; the promising redesign is to
    emit EVERY change as the walk proceeds instead of only the final
    state — memory becomes O(window), and correctness still holds
    because `claimable_observations` is an append-style observations
    table whose reader already does
    `DISTINCT ON (claimable_id) … ORDER BY ledger DESC`, and the
    natural key is `(claimable_id, ledger, observed_at)` so historical
    rows are additional, not conflicting. Costs more write volume.
    **Watch on the first live run** (author-flagged residuals): (1)
    resident memory — now measured, see above; (2) the seed lands rows at TRUE historical ledgers, creating
    ~290 new 7-day chunks on `claimable_observations` — harmless but
    it moves the `max_locks` math — ✅ CHECKED 2026-07-28:
    `max_locks_per_transaction` is already **4096** and the table has
    only 4 chunks on a 7-day interval, so the ~570 chunks the historical
    span will create are affordable. Tightest case is a 2,000-row batch
    whose rows are emitted in KEY order (so their `observed_at` are
    unrelated and can touch many chunks at once) — still inside 4096,
    and the upsert is idempotent so a failed batch is re-runnable;
    (3) ✅ CLEARED — checked r1:
    **zero** compression jobs on `claimable_observations`, so the
    seed's inserts cannot hit compressed chunks. Out of scope + still open: the identical
    never-seeded gap on `lp_reserve_observations`.
  - ✅ **ISOLATED 2026-07-27 (post-SAC-seed measurement).** The live SAC
    seed moved AQUA by only +9.9M (86,701,915,082.74 →
    86,711,792,598.11; −13.232% → −13.222%), so SAC was NOT the cause.
    Against Horizon's total-MINUS-claimable (86,186,028,534.15) we are
    **+0.61%** — i.e. every other component reconciles and the
    claimable component is the WHOLE remaining gap. The claimable seed
    is therefore the single fix for this gate item, and it now has a
    PROVEN template: the windowed reader + Go latest-wins reducer that
    `7bede7e7` validated on 38/38 wrappers.
- **redstone projection blind — ROOT-CAUSED 2026-07-27 (not a code
  regression)**: RedStone's relayer expanded past our 19-feed registry on
  2026-07-24 10:56Z (ledger 63,624,934), publishing 11 unknown feed_ids
  (`EUROC` bare, `SolvBTC*_FUNDAMENTAL/USD` variants, `USDe`, `sUSDe`,
  `USDY_FUNDAMENTAL/USD`, `USST_FUNDAMENTAL`, `savUSD_FUNDAMENTAL`,
  `XAUm_FUNDAMENTAL/USD`, `deJAAA/deJTRSY_FUNDAMENTAL/USD`). All-unknown
  batches → `ErrEmptyUpdates` → undecodable-but-matched. v0.21.0's C4-059
  (6c51c760) only made pre-existing blindness VISIBLE; the pre-deploy range
  [63,624,934, 63,661,714] (~4,276 ledgers) was **false-clean** and needs
  re-verify after replay. FIX: registry + canonical-asset additions with
  per-feed quote/orientation diligence (in progress on main → ships
  v0.21.2); THEN `projector-replay -source redstone -from 63624934` (added
  to §2.3 queue). Fail-closed behavior worked as designed; optional
  hardening = distinct "registry stale" signal (post-v1).
- sep41 completeness 40-min count perf (non-blocking follow-up).

### 2.5 Soak close-out (timed gate — READ THE TIMEZONE NOTE)

> ⚠️ **Interpret the deadline as 2026-07-28 17:00 UTC (= 19:00 CEST on
> r1), i.e. the LATER reading.** The original wording said "17:00" with
> no timezone while r1 runs Europe/Berlin, and the action it authorizes —
> `zfs destroy data/minio@pre-trim-2026-07-26` — is IRREVERSIBLE and
> discards the 3.23 T pre-trim safety copy. Waiting the extra two hours
> costs nothing; destroying two hours early costs the only rollback we
> have if a cold-read problem surfaces. Ambiguity on an irreversible act
> resolves toward the safer side.
>
> Evidence half is ALREADY MET as of 2026-07-28 05:56Z: **8 PASS / 0
> FAIL** (needed ≥8 and 0). So this gate is now purely waiting on the
> clock — any session that fires after 17:00 UTC should re-confirm the
> counts are still ≥8/0 at that moment and then execute.
_Status 2026-07-27 16:45Z: 5 PASS / 0 FAIL, timer active, snapshot
3.23 T held. Needs ≥8 PASS — on the current cadence that lands before
the deadline; the loop executes this gate automatically (time+evidence,
not operator)._
If `grep -c FAIL /var/log/galexie-soak.log` = 0 and ≥8 PASS:
`zfs destroy data/minio@pre-trim-2026-07-26` (reclaims 1.07 T) +
`systemctl disable --now galexie-soak-check.timer`. Any FAIL → investigate
cold tier; rehydrate needs 2.2's archivewriter fix first.

### 2.6 Prove correctness (Phase E — the go-live evidence pack)
Run the confidence-campaign E-gate end to end and FILE the artifacts:
reconcile-balances (+ N random accounts/trustlines), verify-lake /
contiguity / hash-chain to genesis, compute-completeness all-green,
re-derive determinism proof, prices top-50 vs CoinGecko/Chainlink, supply
vs external truth **including the seeded SEP-41 genesis baselines (AQUA
overstated +15.7% in the 2026-07-07 test — verify the 2026-07-26 seed
doesn't serve that)**, first `verify-usd-volume -days 30` → calibrate the
C4-055/066 alert. Also: SEV-1/2 paging drill + rollback rehearsal —
evidence files have never been produced across three generations of plans.

### 2.7 Security + launch hardening
- Rotate `ratesengine-admin` + MinIO creds (session-exposed); confirm vault
  passphrase rotation; re-enable restore-drill timer.
- `gh variable delete DEPLOY_APPROVAL_RELAXED` + r1 environment
  Required-reviewers (re-arm the deploy approval gate).
- [OP] sign the 15 accepted-risk candidates (tail-triage-2026-07-26.md);
  decide IP rotation + SSH CIDR narrowing (C6-041).
- [OP] CoinGecko Pro key; hashdb `enabled=true` first-deploy opt-in.
- [OP] External security review; off-site backup decision (§4).
- [OP] Book/verify: `security@stellarindex.io` mailbox actually exists.

### 2.8 Launch execution
Refreshed launch-day sequence (the old checklist's CalVer/public-flip steps
are obsolete — repo has been public since 2026-07-03):
1. Tag the launch release (SemVer), deploy via the re-armed gate.
2. Confirm `auth_mode=apikey_optional`; external SLA-probe smoke with a
   `sip_` key; outside-internet `make smoke` 13/13.
3. Status page + API docs + SLA/error-budget page current; F-0100
   counter-presence PromQL sanity; Grafana launch-watch board from
   `post-launch-queries.md` (refresh metric names first).
4. Announcement; open the first-24h watch (every alert = SEV-2 minimum).

### 2.9 Explicitly deferred to post-v1 (decide, don't drift)
- **HA / R2+R3 / ClickHouse HA** — the single-box SPOF ships at v1 as a
  documented accepted risk with tested restore ([DECIDE] — the standing
  recommendation: R1 + one warm standby bootstrapping from the verified
  snapshot, post-launch). R1 is NOT hardware-upgradeable — never propose drives.
- CH Phase 8 `soroban_events` decommission (#39 — destructive, LAST;
  enumerate live readers first), monthly galexie trim timer, `/v1/tx`
  10.2B tx_hash_index backfill, contract_events_daily v2 swap
  (`feat/ced-v2-rebuild` branch — land WITH the rebuild), CEX dust DELETE
  (#68), P4 tail (i128 lint tooling, strkey/SCVal stubs, ADR-0025 CF-range),
  email-verification flip-on, site-promised features (order-book depth /
  DEX TVL / per-token oracles) [DECIDE build-or-drop], residual DeFi
  decoders, team-asks (§5), the "road to top-tier" ambition set (explorer
  depth, point-lookup path, generic Soroban decoding).

## 3. [OP] register (operator-only, consolidated + deduplicated)

1. **Vault password re-entry** (blocks §2.2). In a Claude Code session run:
   `! mkdir -p ~/.ansible && read -s VP && echo -n "$VP" > ~/.ansible/r1_vault_pass && chmod 600 ~/.ansible/r1_vault_pass && unset VP`
   …then have the agent verify decrypt + set the two GH secrets.
2. CoinGecko Pro purchase → `COINGECKO_API_KEY` on r1 + indexer restart.
2b. **Wire paging** (go-live gate) — ⭐ **NOW TURNKEY: follow
   [runbooks/wire-paging.md](runbooks/wire-paging.md)** (~20 min,
   copy-paste). Prepared 2026-07-27, which also fixed a **silent-failure
   trap**: `/etc/default/alertmanager-secrets` offered
   `SLACK_WEBHOOK_URL`, but `apply.sh` reads `DISCORD_WEBHOOK_URL_PAGES`
   / `_ALERTS` — filling in the name the file itself suggested would
   have produced no-op stubs while every command appeared to succeed.
   Names corrected on r1 (values still empty, `.bak` kept).
   Baseline captured: `pre-launch-check.sh` → **4 FAILs** today (the
   four `HEALTHCHECKS_URL_*`), and **0** is the acceptance test.
   Correction: that script is NOT installed on r1 and needs no install —
   pipe it: `ssh root@… 'bash -s' < scripts/ops/pre-launch-check.sh`.
3. External security review engagement.
4. Accepted-risk sign-off (15 items) + IP-rotation/SSH-CIDR decision.
5. pgbackrest retention number + off-site S3 provider (+account/creds).
6. HA v1-or-fast-follow decision (§2.9).
7. Stripe: C3-081 reconcile needs SDK + `[billing]` seam — deferred unless
   paid tiers ship at launch.
8. Team-asks (never sent — forward): Aquarius pool-set authority; DeFindex
   vault registry + 9 unproven emitters; Phoenix pool→stake map; Blend V1
   backstop address/emitter schema.

## 4. Open decisions ([DECIDE])

| Decision | Recommendation |
|---|---|
| CH backup posture | ADR-0043 §2.1 schema+state snapshot + re-derive (ledger direction); do NOT resurrect `clickhouse-backup` full-lake copies. Apply the drafted §2.3 amendment to the ADR. Warm standby is the real RTO answer |
| HA at v1 | Accepted-risk + tested restore at v1; warm standby fast-follow |
| Genesis edge [2→287,404] | Accept as documented-unfillable (recover via op-replay if ever needed) |
| Served-tier retention/serve-window policy | Document current reality (projection-scoped windows per source) as the v1 contract |
| Site-promised features (#34 residue) | Build or retract before announcement copy is finalized |
| C4-012/13 third-alias thin-pool VWAP surface | Deliberate review before public traffic |

## 5. Corrections to prior plans (so nobody re-trusts stale rows)

- `min_usd_volume=10000`, ADR-0042 signing, comet gating, deploy/CF secrets,
  k6 cron, branch protection: **DONE** — older docs listing them open are
  wrong. (Healthchecks/Discord wiring is NOT done — see §0 Paging; the env
  files exist but all values are empty.)
- `seed-sep41-genesis`: the 2026-07-07 "❌ do not run" verdict was
  overridden in practice (run 2026-07-26). The honesty check moves to §2.6.
- "Deploy pipeline can't authenticate" / "capacity 94%" / "Phase 0 running":
  resolved-by-events; ignore in superseded docs.
- restore drill "never ran": refuted — PASSED 2026-07-03; the real residuals
  are the disabled timer + cadence drift (§2.7).
- `dex_nonstandard_decimals_detected` firing is informational detection
  working (aquarius C-tokens), not the master plan's "cleared" claim nor a
  regression of the AdjustPrice normalization work.

_Update this file in the same commit as any change that lands or
invalidates an item. One plan; no forks._
