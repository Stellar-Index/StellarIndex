---
title: v1 launch plan — THE single source of truth
last_verified: 2026-08-15
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

## THE PLAN — refreshed 2026-08-15 (supersedes the old inbox + §2 ordering)

> **Read this section first.** It replaces the 2026-07-27 operator inbox and
> re-orders §2. Everything below §"Loop log" is kept for HISTORY and for the
> execution RECIPES (§2.3's heavy-job commands, §2.8's launch sequence,
> runbooks) — but where the old §1–§4 disagree with this section on *what is
> still outstanding*, this section is right and they are stale.
>
> Every item carries a verification marker:
>
> - **[V]** — verified against live r1 or the current code on 2026-08-15.
> - **[C]** — CARRIED from an earlier audit and NOT re-verified in this pass.
>   Treat the claim as unproven: step 1 of the item is to reproduce it.
>
> The distinction is the point. A plan that presents carried claims as
> verified is the same failure mode as an API that renders absent as zero.

### What this pass changed

Verifying the backlog rather than reciting it moved a lot of it:

- **The replay/backfill queue is essentially DONE, not pending.** [V]
  `account_movements` is at ledger 63,962,775 against a lake tip of
  63,962,824 (10.1B rows); comet's projected floor is 51,499,922 — exactly
  the `-from 51499000` target; defindex's is 55,483,698; phoenix_stake's is
  51,572,639. All four tables are duplicate-free under their own row
  identity. Nothing is running on r1.
- **Duplicate-hunting needs the PER-TABLE identity.** [V] A generic
  `(ledger, tx_hash, op_index)` group-by reports 20 "duplicate" comet rows
  and 50 aquarius ones. Both are false: a `join_pool` adds two tokens under
  one op (distinct `token`/`event_index`), and an aquarius deposit emits
  both legs under one `event_index` (distinct `token_index`). Under the real
  identity every table is clean. Encode the identity per table before ever
  concluding corruption.
- **Two "code + data" items are already half-done.** [V] Phoenix's
  pre-upgrade 7-event shape has a decoder + test
  (`phoenix/adapter_test.go:264`), and defindex `harvest` now models a
  StrategyFlow with "Recovery: projector-replay" in its own doc comment. The
  code landed; only the re-derive is outstanding — so these belong in the
  replay queue, not the correctness backlog.
- **The frontend honesty sweep is mostly landed.** [V] `191f58cc` fixed 31
  files across exchanges/issuers/lending/oracles/sources/status, each with a
  test asserting BOTH directions. What remains is blocked on the server half.
- **`#38` was incomplete.** [V] There are 8 failed units on r1, not 3.
- **The SLA probe diagnosis was wrong** and is corrected in W4.

---

### audit-2026-08-14 remediation merged — plan deltas [V] (2026-08-15)

PR #71 (79 verified fixes) squash-merged to main `dd7b995b`. It **advanced or
reframed** these plan items (each verified against the merged code; details inline):

- **W4.1** (restore-drill) — **code DONE** (BDR-03): explicit `LOG_DIR` + fail-loud
  + textfile metric + `restore_drill_stale` alert. Only the r1-ops drill run
  remains → **unblocks W6.6**.
- **W4.6** (template the untemplated sla-probe units) — **DONE** (`50d84d27` +
  AAI-1/2). **W4.5** — the 0640 vault-sourced secret EnvironmentFile is **in
  code**; key mint/rotate stays r1-ops.
- **W8** partials, now verified: item 2 (SDEX, $100M ceiling), item 6
  (recognition_ok can fail), item 9 (derive_generation projector-replay), item 15
  (revocation-drift cache-hit + TWAP detector). See the W8 reconciliation box.
- **W3.1** got further contract-surface hardening (interactions cache-key +
  census freshness).
- **Gates reconciled to decisions:** **W6.4** (deploy-approval re-arm) is a
  launch-flip [OP] toggle, not a defect — the relaxed state is ACCEPTED
  (NS-4/CID-4). **D10** GDPR erasure DROPPED (PRV-1); privacy hygiene landed
  (PRV-2/3). **D9** Stripe reconcile ready to DROP citing ADR-0049.
- **NEW launch-day requirement (§2.8):** apply migrations **through 0143 before
  the binary** (REC-06 fail-closes `/readyz` otherwise); **0143 forces a one-time
  dashboard re-login** — a watch note, not an incident.

It did **NOT** touch the headline blockers — **W1.1** (status incidents
absent-as-zero), **W2** (alias registry), **W3.2–3.4**, **D1** (freeze paging),
and the W5 backfills — those remain the real path to v1. **The
launch-completion campaign below then closed most of them** (W1, W2, W2-tail,
the W8 live-code items, W7.1); W3.2/D1 remain, now bucketed with reasons.
**W5.2 (dfees) CLOSED 2026-08-21**: body shape captured from the r1 lake
(`Map{"distributed_fees" → Vec[(token, i128)]}`, per-asset), modelled into
`defindex_fees` (migration 0146, PR #118), deployed in v0.39.0, 3,813
historical rows backfilled + verified complete=true the same evening.

---

### launch-completion campaign — landed status [V] (2026-08-16)

The unattended launch-completion campaign drove the headline blockers to a
terminal state — every item is either **merged-on-green** or **moved to an
operator/decision bucket with a reason**. Landed on `origin/main`:

- **W1 — ✅ DONE.** All four merged (see the W1 box): #73/#74/#75/#76.
- **W2 (alias registry) — ✅ DONE `bd475a49` (#78).** `AliasRegistry` built at
  start-up from `[supply].sac_wrappers`, SAC form ordered **last**, threaded to
  the read paths — the keystone that folds SAC-wrapped identities. Supersedes
  "W2 remains the real path" above.
- **W2-tail — ✅ DONE (#85/#86/#87).** The ~11 alias-blind money readers were
  alias-completed in three batches (8 asset-detail storage readers → `ANY(alias)`;
  4 api-v1 pair reads → first-hit alias loop; aggregate tiers 2/3 +
  `change_summary`). **Two readers deferred (NEEDS-COORDINATION):**
  `GetAssetBySlug` (SQL slug resolution, no DB alias table) and `ListAssetsExt`
  (aggregator-written rollup) — neither feeds the `/v1/assets/{id}` headline, so
  no headline undercount remains. **r1 note:** non-XLM folding is gated on
  `sac_wrappers` being populated for the high-volume SAC classics (USDC/AQUA/…)
  in r1 config — the code is correct regardless, but the "53.5% of USDC volume
  invisible" headline only actually closes once that config lands (operator).
- **W8 (correctness backlog) live-code — ✅ merged / dispositioned.** item 3
  (manage_data + Soroban participant injection) → #79 **plus** the W7.1 tail
  hardening below; item 7 (MEV impossible-evidence + `mev_events` retention) →
  #81; item 13 (`accounts/{g}/trades` gate) → #80; item 10 (build-frozen prices
  served live) → #83/#84; item 4 Band-oracle auth-forgery corroboration → #83;
  item 15 partials (lint-metric-refs, goleak, txindex-backfill) → #82. **NO-OP
  (already fixed, re-derived):** W8.4b, W8.5, `/readyz` schema-head. **DEFERRED
  with reason (bucket 3):** item 8 confidence bootstrap-cap (money-safety panel
  REJECTED the narrow fix — the conservative cap errs safe; a real fix needs
  cross-layer first-observation age), the ops-CLI write-gate unification
  (flipping ~15 money-path defaults risks the INV-3 DO-NOTHING trap), and the
  soroswap `routed_via` attribution residual (VWAP itself is unforgeable —
  Option A accept, Option B surfaced). item 14 (ADR-0017 verify-archive) →
  operator bucket.
- **W3.2 (page-type cold-perf pass) — → BUCKET 2 (operator).** The harness
  extension is code, but its value is the measurement, which needs the live r1
  lake (cold→populated with lake-drawn ids). Prepared; not runnable here.
- **W5.2 (dfees) — → BUCKET 2 (operator, r1-blocked).** The money-adjacent event
  body shape is NOT derivable in-repo (do-not-invent discipline); the ~8,018
  historical dfees are cleanly dropped, not lost. Unblock = **one**
  contract-scoped r1-lake read of a dfees `value` blob, then the type + table
  0144 + projector arm is mechanical.
- **W7.1 (final cold audit) — ✅ RAN on the fixed surface, DRY.** It found
  exactly two residual tail defects, both fixed + merged in **#88** (`e97441bb`):
  the Soroban InvokeContract participant-injection (arg/auth-derived participants
  removed entirely — both are attacker-controllable at decode time) and the
  incidents Atom feed `<updated>` wall-clock lie (now the most-recent-entry
  timestamp). A **fresh** W7.1 re-audit on the post-#88 surface came back **DRY**
  across the security/injection, honesty/freshness, money/alias-regression, and
  merged-diff-regression dimensions (default-reject verified).

**Still the operator's to run / decide (not code):** the ordered r1-ops
sequence (migrations→143 before the binary, replays, SLA-probe key mint+rotate,
restore-drill run, the r1 `sac_wrappers` populate, the dfees sample read, the
W3.2 perf run) and the launch decisions (D1 freeze-paging, D2 HA, W6.2 security
review, W6.5/6.6/6.7, and a **new failed-tx participant** product call — a
failed *classic* op naming a victim still lands a public participant row, since
the account-history reader UNION has no tx-success filter; recommended default
is to success-filter the reader). These are surfaced separately in the
operator's audit workspace.

---

### W1 — Absent-vs-zero: stop publishing false empirical claims

> **✅ W1 COMPLETE (2026-08-16) — all four merged.** W1.1 `87e5b1aa` (#73),
> W1.2 `03050689` (#74), W1.3 `96a38485` (#76), W1.4 `351bcea2` (#75). The
> `/v1/status`, `/v1/tx`, and `/v1/protocols` surfaces now emit an explicit
> tri-state / `coverage_note` instead of serializing a failure as an all-clear;
> the protocols roster serves from an SWR cache (no per-request unauth scan);
> and `DegradedBanner` renders "alert status unknown". Detail kept below for history.

**Why one workstream:** three separately-tracked items are one bug class, and
they have a strict order. `DegradedBanner.tsx:76` does
`incs?.active_count ?? 0` — it *cannot* be fixed in the UI while the server
sends a zero struct, because absent and zero are indistinguishable on the
wire. Server first, then UI, then the endpoint that exhibits the same shape.

**W1.1 — `/v1/status` incidents. [V] ✅ DONE `87e5b1aa` (#73).** `status.go:53` declares
`Incidents StatusIncidents` with **no `omitempty`**, and the assembly does
`if incErr == nil { out.Incidents = incidents }` — so a FAILED Prometheus
query serialises as zero counts. `DegradedBanner` and the public status page
then publish "0 active alerts" / "No active incidents" **while alerting is
blind**. This is the single most launch-relevant item in the plan: it is an
all-clear derived from a failure, on the banner a visitor sees first.
*Approach:* add an explicit tri-state (`ok` / `degraded` / `unknown`) rather
than just omitting — the UI needs to say "unknown", and an omitted field
would still coalesce to 0 in a `??` chain. Ship with the OpenAPI change and
all three generators.

**W1.2 — tx events + op results. [V] ✅ DONE `03050689` (#74).** `explorer/tx.go:103` sets
`events = nil // non-fatal` and `:96` sets `results = nil // non-fatal`;
both fields are `omitempty`, so a failed read is byte-identical to "this tx
genuinely had none". Two instances, not one — the second (`results`, the
per-op result codes) was not in the original finding.
*Approach:* one wire convention for the whole explorer surface — a
`partial` / coverage note like `movements.go` already uses — applied to both
sites at once, rather than a bespoke `events_unavailable` boolean per field.
That convention is then the thing new handlers copy.

**W1.3 — `/v1/protocols` index. [V] ✅ DONE `96a38485` (#76).** `handleProtocolsList` calls
`protocolRoster(ctx, meta)` inside the registry loop with no server-side
cache — only `Cache-Control: max-age=60`. For registry-empty sources that is
a `SELECT DISTINCT … LIMIT 5000` full served-tier scan per protocol, on an
unauthenticated route, and its own doc comment says it degrades "to
zeros/absent". Measured live 0.54–0.77s, i.e. currently masked by edge cache.
*Approach:* this is not a new cache — reuse the prewarm/SWR shape `a8284d64`
already established for the bespoke and throughput blocks, and make the
failure path OMIT rather than zero (which is W1.1's convention). Consolidating
here means one cache pattern and one honesty convention, not three.

**W1.4 — UI residue. [V] ✅ DONE `351bcea2` (#75).** After `191f58cc`, what is left is `DegradedBanner`
(blocked on W1.1) and `NetworkInsight.tsx`'s several `?? 0` sites.
*Exit criteria for W1:* a grep for `?? 0` / `?? []` in explorer components
returns only sites where the API genuinely serves a zero, and each fixed site
has a both-directions test. *(Note: audit-2026-08-14's web fixes F1–F4
(`2b58260b`) — sub-cent decimal rendering, the fiat dual-page canonicalization,
the v1-stable-vs-pre-launch copy, search a11y — are a **different bug class** and
do NOT count toward this `?? 0`/`?? []` grep; `DegradedBanner.tsx:76` and the
`NetworkInsight.tsx` sites are still untouched.)*

---

### W2 — Asset identity: one registry, not ten handler patches

> **✅ DONE `bd475a49` (#78) + W2-tail #85/#86/#87.** See "launch-completion —
> landed status" above. Two readers deferred (NEEDS-COORDINATION); non-XLM
> folding is gated on the r1 `sac_wrappers` config. Analysis below is the *why*.

**Why one workstream:** the audit filed "SAC-wrapped = a second un-aliased
identity (53.5% of USDC volume invisible)" and "ten money handlers are
alias-blind" as separate findings. They are one defect with one fix, and the
codebase has already designed it.

**[V] Verified:** `canonical/alias.go` builds `aliasFamilies` from
`xlmAliasFamily` **only** — XLM is the sole family, so every other asset's
SAC form is a distinct un-aliased identity. `alias.go:125-128` states the
intended fix in its own words: an explicit `AliasRegistry` constructed at
binary start-up from `[supply].sac_wrappers` and passed to the read paths,
"a change that touches wiring in three fences [that] should land as its own
unit of work". The config seam already exists — `SupplyConfig.SACWrappers`
maps SAC C-strkey → `CODE:ISSUER`.

**Approach:** build the registry from existing config; keep
`AssetAliases`'s signature so the ~11 call sites that already loop aliases
need no change; the "ten alias-blind handlers" then get fixed by adopting
that same loop rather than by ten bespoke patches. Priority order must
preserve the deliberate rule already documented: **SAC form LAST**, because
Soroban XLM pools are thin and putting the SAC form first would let a few
thousand dollars of pool liquidity become the served price.

**Exit criteria:** a USDC read keyed by classic form returns the SAC-form
volume too; the XLM three-form behaviour is unchanged (its existing tests
must still pass unmodified).

---

### W3 — Cold-read performance: finish the page-level pass

**Why one workstream:** §2.6b pass 3, the explorer cold-read audit, and the
contract↔tx index are the same programme at three depths.

**W3.1 — [V] Contract pages: DONE.** 23/25 cold pages breaching → 6/25,
0 pages with a failed panel, worst page 8.2s → 1.6s. Root causes were an
unbounded SAC probe on the `/wasm` 404 path (95.35M rows, cancelled at the
8s deadline) and four panels sharing one refresh-gate class so a page starved
itself. *(audit-2026-08-14 hardened the same contract surface further:
W1-explorer-perf-1 quantized the `/v1/contracts/{id}/interactions` cache key
(`9e42e8fb`) and REC-05/W1-explorer-perf-2 added a `max(day)` census-freshness
gate to `/v1/contracts` (`bb64ff3c`). W3.2/W3.3/W3.4 remain untouched.)*

**W3.2 — [V] Every other page type: MEASURED 2026-08-22.** The harness was
extended as planned — `scripts/ops/contract-page-audit.py` now takes
`--type {account,asset,asset-shell,ledger,tx,pair,protocol,operations,home,
network,protocols}` with a per-page-type panel map kept in lockstep with the
explorer views (contract behavior unchanged/byte-compatible), and the first
cold→fully-populated run against the live API landed. Full per-type tables +
the id-draw method:
`docs/operations/w32-page-type-cold-perf-2026-08-22.md`. **Residue stays
named:** the systemic breachers found there are W3.3's existing classes
(per-entity O(scan) cold reads — account family, asset detail on the shell
path) plus whatever the results doc flags as worth a W3.3-style fix; the
dependent second hops the harness cannot model statically (account
`/price/batch` after state, home `/history` ×3 after top-markets) are noted
unmeasured in the doc.

**W3.3 — [C] The cold-read audit residue.** Per-entity ClickHouse reads are
O(scan) for non-prewarmed keys (census 40s×160, quiet-contract inversion,
account family 8s, holders fail, refresh-gate crawler-saturable). Fix designs
are already written up; DDL-heavy.

**W3.4 — The narrow contract↔tx index.** Would close W3.1's residual
1.0–1.6s tail. New table + genesis backfill over 578 GiB of
`contract_events`. *Do this last* — it is the most expensive item here and
W3.2/W3.3 may reshape what it needs to hold.

---

### W4 — r1 unit health: template what you enable

**Why one workstream:** six failed units and one near-miss share a root
cause — **ansible enables units it does not install.** That is exactly what
made the drift check red for four days (it failed on `census-rollup.timer`,
a unit the playbook enables but never templates). Fixing the units without
fixing the pattern re-earns it.

**W4.1 — [V] `restore-drill.service`. CORRECTED 2026-08-15 — the earlier
framing ("two weeks with no proven-restorable backup") overstated it.**

What is actually true:

- **The backups are healthy.** pgBackRest stanza `status: ok`; full backup
  2026-08-09, daily diffs through 02:05 on 2026-08-15, continuous WAL
  archive. Database 616 GB, repo 214 GB.
- **They have been PROVEN restorable.** Drill run 5 (2026-07-03) passed with
  0 failures: restore 871s, recovery to consistency, core tables 4/4, tip lag
  240 ledgers, 0 hash-chain breaks, and an EXACT trades-window match
  (5,770,426 restored = 5,770,426 live).
- **The drill is non-destructive by construction** — restores into a fresh
  timestamped dir on the dedicated `data/restore-drill` ZFS dataset (5.18 TB
  free), starts a disposable instance on **port 5499, never 5432**, only
  READS the backup repo, and refuses below `MIN_FREE_GB`. Live PGDATA is
  never touched. It is safe to run on demand.
- **The timer is MONTHLY** (last 2026-08-01, next 2026-09-05), so this is one
  failed attempt, not a repeating failure.

**RESOLVED + PROVEN 2026-08-19 — the drill now PASSES on its own unit,
0 failures:**

    restore: 539s; wal_drain: replay reached 1A5D/65BAE000
    tip lag 206 ledgers; hash-chain breaks: 0
    trades window match: 5,905,148 = 5,905,148

Same backup, same box: measuring AFTER draining the archive stream gives
**206 ledgers instead of 13,392** — a 65x drop that is entirely
measurement, which settles that the old failure was never a backup or
WAL-archiving problem. `r1` now has a restore verified end-to-end through
the path that actually runs on schedule, so W6.6 (off-site decision) is
unblocked.

**How it got there — see BDR-04/BDR-05.** Running the UNIT (not the
script) exposed three stacked blockers, each hidden behind the one before:
`PrivateTmp=true` made the `/var/tmp` drill dataset invisible inside the
service namespace (`226/NAMESPACE`, before `ExecStart`); then
`NoNewPrivileges=true` blocked `sudo`'s privilege DROP to postgres; then
pgbackrest-as-postgres could not traverse `/var/lib/stellarindex`. The
dataset moved to `/srv/restore-drill` (postgres-owned) and the drill now
runs on its own unit. **The drill had therefore never once run on its
schedule** — every passing record on file came from manual runs, which have
no namespace. Separately, `tip_lag` was measuring the backup's AGE rather
than recoverability (`hot_standby` + `pg_ctl -w` return at consistency while
replay continues), so it now drains the archive stream to a live-primary LSN
before measuring.

**The original defect (BDR-03, already fixed before this pass):** `LOG_DIR` is derived from the script's own path —
`$(dirname $0)/../../docs/operations/drills`. Installed at
`/usr/local/bin/restore-drill.sh` that resolves to `/usr/docs/operations/drills`,
which does not exist, and the append is guarded by `if [[ -d "$LOG_DIR" ]]`.
Run 5 was logged only because it was run from a repo checkout. The 2026-08-01
timer run exited **status 1** — meaning it passed every precondition (those
exit 2), actually ran, and a verification check FAILED — but which check is
now unrecoverable, because the evidence write was skipped and journald has
rotated.

So CS-110's whole deliverable — the evidence that the backup restores — is
not produced by the scheduled path.

**[V] CODE FIX LANDED — BDR-03, merged to main `dd7b995b` (audit-2026-08-14).**
`restore-drill.sh` now uses an explicit, env-overridable `LOG_DIR`
(`/var/lib/stellarindex/restore-drills`, `restore-drill.sh:56`) and **fails
loudly** — an unwritable evidence dir increments `fail_count` → non-zero exit
(`:299-310`), no more silent `[[ -d ]]` skip. It also now emits a
`stellarindex_restore_drill_last_success_unix` node_exporter textfile metric
(only on a clean run) and ships a `restore_drill_stale` alert (both rule trees
+ rule-test + runbook + catalog). **What remains is r1-ops only:** re-run
ansible (reinstalls the script + unit) + `systemctl daemon-reload`, then
`systemctl start restore-drill.service` (or wait for the monthly timer) to seed
a CURRENT verdict + the metric. Only after that manual run is the "is our backup
good?" question answerable — which is why W6.6 (off-site decision) still
sequences behind this **manual run** (no longer behind a code fix).

**W4.2 — [V] `galexie-archive-fill.service`.** Not previously tracked.
Succeeded 12:20 today, failed 13:18 with `status=2/INVALIDARGUMENT` in one
second — it errors when there is nothing left to fill. A no-work case
reported as failure, which also means a REAL failure here is invisible.

**W4.3 — [V] `compute-completeness.service`** — `result=timeout`. Feeds the
public coverage verdict.

**W4.4 — [V] `ch-schema-drift.service`.** Re-run FIRST: its intent file
(`/usr/local/share/stellarindex/tier1_schema.sql`) was stale on r1 until the
2026-08-15 apply shipped the TTL-liveness DDL, so it may now pass — or may
surface real ClickHouse schema drift.

**W4.5 — [V] SLA probe, corrected diagnosis.** The earlier claim ("has never
had an API key") was WRONG. A valid operator-tier key exists —
`kid_abcae429583012b8`, label "SLA probe (r1 — F-1305)", 10,000/min, created
2026-05-13 — but it is set in `/etc/default/stellarindex-healthchecks`,
which is the EnvironmentFile for `stellarindex-sla-probe.service` (the
Healthchecks heartbeat wrapper). The failing Go probe is a *different* unit,
`sla-probe.service`, reading `/etc/default/sla-probe`, which has no key. So
it runs anonymous, hits the 60/min anon tier, and reports `verdict: fail` at
2–10% availability while healthz/readyz sit at 100%. The runaway sample
count (~45,000 per 30s burst vs the ~430 the config assumes) is a SYMPTOM:
429s return instantly so the probe spins. The 10k/min limit matches the
intended ~8,600/min design.
*Approach:* put the key in a mode-restricted EnvironmentFile, not the
world-readable 0644 `/etc/default/sla-probe`. **[V] CONFIG LANDED — `50d84d27`
(audit-2026-08-14):** `10-observability.yml:115-129` now provisions a
`/etc/default/sla-probe.secret` EnvironmentFile at mode **0640** (group
`stellarindex`, `no_log`, sourced from vault var `stellarindex_probe_api_key`)
— 0640 not 0600 because the probe runs *as* the `stellarindex` user, not root.
**Still r1-ops:** mint the Partner/Operator-tier key, set
`stellarindex_probe_api_key` in the r1 vault, and **rotate** the key whose
plaintext was exposed in a session transcript on 2026-08-15 (= r1-ops #2,
overlaps W6.3).

**W4.6 — [V] Template the untemplated. DONE — `50d84d27` + AAI-1/AAI-2
(`8221092c`), merged `dd7b995b` (audit-2026-08-14).** `sla-probe.service`/`.timer`
had NO install in the repo (the live file is a Jun 12 pre-rename artifact
carrying `RATESENGINE_PROBE_API_KEY` in comments and a `User=ratesengine`
drop-in) yet `10-observability.yml` *enabled* `sla-probe.timer`.
`10-observability.yml:97-107` now **installs** `deploy/systemd/sla-probe.{service,timer}`
into `/etc/systemd/system` *before* the `:131` enable; AAI-1/AAI-2 fixed the
sibling archival-node drop-in copy + the notify-a-real-handler bug. **Still
r1-ops:** remove the stale hand-installed `/etc/systemd/system/sla-probe.service`
(pre-rename `User=ratesengine` artifact) on live r1. **Residual (keep open):**
confirm the "audit every `systemd:` enable task for the same shape" sweep is
exhaustive role-wide — the fix templated the known offenders, not proven every one.

**W4.7 — [V] Remove dead units:** `lec-repair.service` /
`lec-repair-v2.service` both exec `/tmp/lec-repair.sh`.

**Exit criteria for W4:** `systemctl list-units --state=failed` is empty, and
every unit ansible enables is also templated by ansible.

---

### W5 — Data + backfills: what actually remains

**[V] Complete, verified 2026-08-15 — do NOT re-run:** cap67/movements
archive; comet replay; defindex replay; aquarius; phoenix stake history. All
duplicate-free under per-table identity.

> **⚠️ audit-2026-08-14 caveat — reconcile before trusting "do NOT re-run":**
> the remediation added *enforcement/detectors* for migrations that empty data
> and rely on an un-run follow-up (REC-01/W1-migrations-1 = 0126 TWAP CAGG
> refresh; REC-02/W1-migrations-2 = 0137 comet `-from 51499000`; W1-migrations-4
> = 0139 aquarius fee-token) — a `data-freshness` detector (`513c4f6d`) that will
> now *fire* if the follow-up hasn't run. If 0137/0139 were applied on r1 without
> their replays, the comet/aquarius "complete, do NOT re-run" rows above and the
> new detector disagree — check the detector's verdict on r1 before concluding
> either. These replays are listed as r1-ops follow-ups (r1-ops #4).
> **Also new:** migrations **0142** (int4→bigint `derive_generation`) + **0143**
> (`sessions.token_hash`) must be applied **before** the launch binary — see §2.8
> step 1.

**W5.1 — [V] `ch-instance-backfill -from 2`: probably already satisfied.**
The index floor is 50,457,429, which is Soroban's mainnet activation — there
may be nothing earlier to fill. *One confirmation query, not a run.*

**W5.2 — dfees. [V] still absent → BUCKET 2 (operator, r1-blocked).** No type,
table, or migration exists (8,018 events cleanly dropped, not lost). This is a
CODE item (new event type + table + projector arm + `IsProjectedEvent`) that
then needs a replay; it is the only member of the old "Tier 2" list that is
genuinely unstarted. **The money-adjacent event body shape is NOT derivable
in-repo (do-not-invent discipline); unblock = one contract-scoped r1-lake read
of a dfees `value` blob, then the code is mechanical.**

**W5.3 — [C]** Pre-2026-07-23 USD-volume re-stamp.

**W5.4 — [C]** Reset the 13 supply rollups (EURC done 2026-08-05).

**W5.5 — [C]** `/v1/tx` 10.2B `tx_hash_index` backfill.

**W5.6 — [C]** `contract_events_daily` v2 swap (`feat/ced-v2-rebuild`) —
land WITH the rebuild.

**W5.7 — [C]** CEX dust DELETE (#68); monthly galexie trim timer enable.

**W5.8 — [C]** ClickHouse Phase 8 `soroban_events` decommission (#39) —
destructive, LAST, enumerate live readers first.

**Sequencing rule (unchanged, still binding):** one heavy job at a time under
`/usr/local/sbin/run-heavy-job.sh`; decompress before replaying through
compressed chunks.

---

### W6 — Launch gate (the only true v1 blockers)

**W6.1 — [V] Wire paging. [OP]** All five `HEALTHCHECKS_URL_*` on r1 are
blank and no alertmanager receiver holds a real URL. Turnkey runbook exists
(`runbooks/wire-paging.md`, ~20 min). Acceptance: `pre-launch-check.sh` → 0
FAILs (currently 4). **Note the overlap with W4.5** — `HEALTHCHECKS_URL_SLA_PROBE`
lives in the same file as the probe's API key, so wire them in one edit.

**W6.2 — [OP]** Book the external security review — longest lead time of
anything remaining.

**W6.3** Rotate session-exposed credentials: `ratesengine-admin`, MinIO, and
the SLA-probe key from W4.5. *(The SLA-probe key's secure home is now in code —
`50d84d27`, the 0640 vault-sourced EnvironmentFile; the three rotations remain
r1-ops. The SLA-probe rotation == r1-ops #2 "mint `STELLARINDEX_PROBE_API_KEY`".)*

**W6.4 — [OP] launch-flip toggle, not a defect.** Re-arm the deploy approval
gate at the production flip: `gh variable delete DEPLOY_APPROVAL_RELAXED` + r1
Required-reviewers. *(The relaxed state is an ACCEPTED risk-until-launch, not an
open bug — Ash accepted it 2026-08-15 per NS-4/CID-4: the gate fails **closed**
and the relaxation is honest/visible, so there is nothing to "fix," only the
one-line toggle to delete the variable at the flip.)*

**W6.5 — [OP]** Sign the 15 accepted-risk candidates.

**W6.6 — [OP]** Off-site backup decision executed or explicitly
risk-accepted. **Sequence after W4.1's manual drill run** — W4.1's *code* is now
DONE (BDR-03, `dd7b995b`); the only thing left before this decision is no longer
blind is the one **r1-ops** run of the drill (r1-ops #3) to get a current
verdict. The provisioning itself (BDR-01/02/05: offsite pgBackRest repo2, off-box
CH copy, owned deep-history copy) stays Ash-deferred ("sort after").

**W6.7 — [OP]** Announcement copy; first-24h watch staffed. Gate on W6.1 and
on the anomaly-freeze decision (D1) — otherwise the watch opens with a pager
that is either silent or crying wolf.

**W6.8** SEV drill — blocked on W6.1.

**W6.9** Convert the three `supply_cross_check_divergence` dispositions
(PHO/BLND/EURC) to annotated silences.

---

### W7 — Pre-launch passes (§2.6b)

**W7.1 — ✅ DONE (2026-08-16), came back DRY.** Ran on the fixed surface after
W1+W2 landed; it surfaced exactly two residual tail defects, both fixed + merged
in #88 (`e97441bb`) — Soroban participant-injection + incidents Atom `<updated>`
honesty — and a fresh re-audit on the post-#88 surface was DRY (see
"launch-completion — landed status" above). *Originally: full cold adversarial
audit — the last was 2026-07-01 and ~40 tags had shipped; run AFTER W1+W2 so it
audits the fixed surface rather than re-finding known items.*

**W7.2** Visuals-opportunity pass — every endpoint/page: what chart is
possible from data we already serve but don't visualise.

**W7.3** = W3.2 (page load-time pass). Tracked there, not duplicated here.

---

### D — Decisions only Ash can make

**D1 — [V] Anomaly-freeze pages on CORRECT prices.** Verified worsening:
`stellarindex_anomaly_freeze_engaged_total{class="default"}` was 382 on
2026-07-27 and is **1,700** now. Fires on thin FX crosses with `sources=1`;
the served prices were independently verified correct (0.06% / 0.21% off).
`writer_wired=false`, so the page has no automatic protection behind it.
Recommendation: stop paging when `sources=1`. **Blocks W6.7.**

**D2** HA at v1 vs fast-follow (single-box SPOF as accepted risk + tested
restore; warm standby fast-follow).

**D3** ClickHouse backup posture — ADR-0043 §2.1 snapshot + re-derive; apply
the drafted §2.3 amendment; do not resurrect full-lake copies.

**D4** Site-promised features (order-book depth, DEX TVL, per-token oracles)
— build or retract **before** announcement copy is finalised (W6.7).

**D5** Served-tier retention/serve-window policy — document projection-scoped
windows as the v1 contract.

**D6** Genesis edge [2 → 287,404] — accept as documented-unfillable.

**D7** C4-012/13 third-alias thin-pool VWAP surface — review **before**
public traffic, and note it interacts with W2's priority ordering.

**D8** SolvBTC quote mislabel — registered `fiat:USD`, publishes a NAV ratio
vs BTC. Recommendation: fix post-v1 (redstone is `IncludeInVWAP=false`).

**D9** Stripe C3-081 reconcile — **ready to DROP.** The anon/free/partner access
model is now recorded in **ADR-0049** (`7ae91445`, audit-2026-08-14), which is
the documented basis; close C3-081 as a formal DROP citing it.

**D10** Privacy review — **reduced to a sign-off, not an engineering review.**
GDPR Art.17 erasure was DROPPED as overdesigned (PRV-1, Ash 2026-08-15: not
storing user data meaningfully). Privacy hygiene is addressed in code:
magic-link reaper (PRV-2, `828de74c`) bounds the unauth-writable table; IPs
are documented with their retention rationale (PRV-3, `0b3a783c`). What remains
is a lightweight documentation sign-off, not open work.

---

### W8 — Correctness backlog [C — all carried, none re-verified this pass]

Each item's first step is to REPRODUCE it; several 2026-08-04 findings have
already been silently fixed (W1.4, W5's replays and phoenix/defindex decoders
were all found to be done or half-done once checked).

> **audit-2026-08-14 reconciliation [V] (merged `dd7b995b`) — verify these
> against the merged code before reproducing them:**
> - **Item 2 (SDEX downside protection): PARTIAL.** W1-flow-price-serve-1
>   (`af1f8985`) bounds the uncross-checkable single-leg DEX print at a $100M
>   plausibility ceiling (base-unresolvable → refuse/NULL). It does NOT restore
>   the cross-check — sub-$100M fake prints on a base-unresolvable pair are still
>   unchallenged (accepted residual). Remaining work: both-legs-corroborate /
>   bridge-quote gating.
> - **Item 6 (recognition_ok always-true): the recognition-axis half is DONE.**
>   W1-flowcompleteness-1 (`ac6458e5`) folds all pool families into `ownerOf` and
>   fails closed on a registry-read error, so `recognition_ok` can now fail for
>   topic-matched sources (incl. defindex). Still open: "defindex emitter
>   ungated" and "defindex gate poisoning" — separate, untouched.
> - **Item 9 (derive_generation blocks projector-replay): that sub-item is
>   DONE.** CWR-1 (`a893f8f7`, resume-stalled resolver + positive generation) +
>   W1-flowtradeingest-1 (`af1f8985`, gen-aware usd_volume) + W1-migrations-3
>   (`640c0a09`, 0142 int4→bigint) + the follow-up detectors (`513c4f6d`). Still
>   open: "MinBatchLimit wedge" and "zero mail instrumentation".
> - **Item 15 (CI/test gaps): PARTIAL.** The revocation-drift cache-hit path is
>   closed at runtime by F-A (`e880093e`, admin PATCH evicts the key cache); the
>   TWAP-coverage gap now has a runtime detector (`513c4f6d`) though not the CI
>   coverage test. Still open: lint-metric-refs-accepts-comments, goroutine-leak
>   detection, the two ops-CLI write-gate conventions, txindex-backfill defaults.
> - **NOT closed, do not over-credit:** Item 1 (alias registry — untouched; the
>   *analogous* stale-`as_of` bug was fixed only on `/v1/contracts` via
>   REC-05/W1-explorer-perf-2 `bb64ff3c`, a pattern to copy onto `/v1/markets`);
>   Item 4 (`/v1/readyz` got a schema-head check via REC-06 but is still
>   uncapped + auth-exempt — the pool-exhaustion surface stands; the oracle-price
>   forgery + middleware-deadlock sub-items are untouched); Item 5 (the 6-digit
>   code entropy is a *different* credential from the session hashing);
>   Items 3, 7, 8, 10, 11, 12, 13, 14 — untouched by audit-2026-08-14.

1. Ten money handlers alias-blind → **folded into W2**; `markets` reports
   `stale:false` when it isn't; XLM supply 2.11× split.
2. SDEX downside protection OFF for 27.5% of pairs, attacker-inducible.
3. `manage_data` G-address injection into other accounts' histories (proven
   live at filing time).
4. Auth-tree oracle-price forgery; auth middleware deadlocks; `/v1/readyz`
   unlimited (it is in the auth-exempt list — pool-exhaustion surface).
5. Dashboard 6-digit code derivable from the stored hash.
6. `recognition_ok` structurally always-true for match-by-topic sources;
   defindex emitter ungated; defindex gate poisoning.
7. MEV sandwich detector names accounts on impossible evidence;
   `mev_events` unbounded growth.
8. Served confidence capped at 0.5; co-equal routes publish the LOWER value;
   MinMAD floors 24/27 baselines; `native`/`fiat:USD` unscoreable.
9. `derive_generation` blocks projector-replay; MinBatchLimit wedge; zero
   mail instrumentation.
10. Build-frozen prices served as live; `/assets/{CODE}` returns the worst
    impersonator.
11. Observations `as_of` lie; three VWAP windows on one SSE topic;
    `?asset=native` matches nothing; SSE payload schema mismatch; tip stream
    6 qps/conn.
12. LP reserves live-only from ledger 63.3M — no trustline/LP backfill.
13. `accounts/{g}/trades` windowing; movements 11-month gap; wasm full-scan.
14. ADR-0017 contract 4 never runs; archive `chmod o+rx` one-off.
15. CI/test gaps: `lint-metric-refs` accepts comments; TWAP CAGG 5-month
    coverage; revocation drift guard misses the cache-hit path; no
    goroutine-leak detection; two opposite ops-CLI write-gate conventions;
    `txindex-backfill` defaults.

---

### W9 — Post-v1 (decide, don't drift)

R2 + R3 regions and ClickHouse HA · email-verification flip-on · P4 tail
(i128 lint tooling, strkey/SCVal stubs, ADR-0025 CF-range) · residual DeFi
decoders and generic Soroban decoding · explorer depth + point-lookup path ·
team-asks (Aquarius pool-set authority; DeFindex vault registry + 9 unproven
emitters; Phoenix pool→stake map; Blend V1 backstop schema).

---

### Recommended order

1. **W4.1** (restore-drill) — code DONE (BDR-03, `dd7b995b`); now just the one
   **r1-ops** run of the drill for a current verdict (unblocks W6.6).
2. **D1** — one decision, unblocks W6.7 and stops the pager crying wolf.
3. **W6.1 + W4.5** — one file, closes the paging gate and the SLA verdict.
   (W4.5's secure key-home + W4.6's install are now in code (`50d84d27`); this
   step is now the r1-ops key-mint + `HEALTHCHECKS_URL_*` wiring in one edit.)
4. ~~**W1** — server → UI → protocols~~ ✅ DONE 2026-08-16 (#73/#74/#76/#75).
5. **W4.2–W4.7** — unit sweep + template the pattern.
6. **W2** — the alias registry.
7. **W5.2** (dfees), then the [C] backfills.
8. **W3.2 → W3.3 → W3.4**.
9. **W7.1** full audit, on the fixed surface.
10. **W6** remainder → launch.

W6.2 (security review) and W6.6 (backup decision) run in parallel from day
one — they are lead-time items, not sequenced work.

---

## Loop log (newest first)

- 2026-08-01 ~11:30Z — 🎬 **LOOP WRAP: the engineering queue is EMPTY.**
  Closing state: **17/17 completeness** (evidence filed, publicly
  served), supply 8/8, prove-it battery filed, routes 0×5xx, taker
  100%, §2.6b all three passes complete (audit + visuals 10/10 +
  cold-load architecture), v0.21.12 live (12 releases this week:
  v0.21.1→12), alert board clean (heartbeat + rolling-off burn +
  4 annotated 14-day silences with per-asset/source matchers).
  Final polish landed: sink-shrink cycle test, hourly
  tx_hash_index_parity assertion (live+green on r1, ansible-codified),
  intra_ledger_seq backfill plan (post-launch), decompress-first
  replay runbook. Background: prices_* cagg refresh finishing.
  **Everything remaining is YOURS, Ash** (in priority order): (1) wire
  paging (~20 min turnkey) → I run the SEV drill on your word; (2)
  book the external security review; (3) the decisions list (§3/§4:
  accepted-risk sign-off, off-site backup, HA posture, freeze-paging
  sources=1, CoinGecko key, cred rotation + gate re-arm); (4)
  announcement copy + first-24h watch. The loop stops here — restart
  it anytime with /loop.

- 2026-08-01 ~08:45Z — 🏆 **17/17. EVERY SOURCE COMPLETE — FIRST TIME
  EVER.** The aquarius full-range verify (75 min, quiet box) returned
  `complete=true` and the verdict table reads t across all seventeen
  sources; public `/v1/coverage` serves it (17 sources, 0 incomplete,
  two-axis details per source). The dirty-window hardening cleared its
  first production window in the same verdict line. Evidence:
  `evidence/2026-08-01-completeness-17of17.md`. The completeness
  go-live gate row is CLOSED. Remaining gate rows are operator-only
  (paging→SEV drill, security posture, DR decision, announcement).


- 2026-08-01 ~07:45Z — 🔧 **v0.21.12 HOTFIX: the aquarius replay wedge
  was a REAL projector bug — sink-side budget exhaustion never shrank
  the adaptive window** (the missing half of the 2026-07-10 fix, which
  only covered stream-level timeouts). A reserves-dense window's scan
  completed but its writes exhausted PerSourceTimeout mid-batch; every
  remaining write fast-failed on the dead cycle context, the cursor
  held, and the IDENTICAL 1,000-ledger window retried for 3.5h.
  Diagnosis walked four layers (recompressed chunks — real but
  insufficient, decompressed 8 chunks incl. the aquarius_* tables MY
  19-policy application newly armed; pool/locks/disk — all clean;
  restart — no effect; then the deterministic fast-fail signature).
  Fix: budget-exhausted cycles with held transient rows now halve the
  window to the MinBatchLimit floor. Cut+deployed v0.21.12; cursor
  confirmed moving (63,487,686 → 63,491,691+). Ops lesson banked to
  memory: replays through compressed history must decompress ALL
  target tables' window chunks FIRST (runbook fix queued). Verify
  chain re-armed with an honest long bound (the old one nearly fired
  prematurely mid-wedge).

- 2026-08-01 ~02:50Z — 🟢🟢 **REDSTONE + SOROSWAP COMPLETE=TRUE; the
  blind ladder ends 1,626 → 0.** The v0.21.8 chain landed both
  targets: redstone projection verified full-range (state-write
  attribution) and soroswap likewise (non-directional recognition +
  skim dedup; taker 100.00%). Verdict table 16/17: **aquarius's
  FIRST full-range reconcile** (prior verdicts carried) surfaced its
  own long-standing backlog — 41 old-WASM blind events + 482
  over-projected trade rows from an older decoder era — queued as
  the next per-source unit on the proven playbook; NOT a regression.
  Evidence: `evidence/2026-08-01-completeness-16of17-and-ac2.md`.
  **AC2 final framing**: SLA scenario PASSES huge (0 fails, p95
  37.1ms); the 400-VU mixed stress scenario consistently lands p95
  ~5s / 4.4% across two differently-conditioned runs = the honest
  single-box ceiling beyond rated load — filed as capacity datum for
  the HA decision. ALSO tonight: the FULL AUDIT PASS closed (§2.6b
  #1 — 4 cold audits, 3 fix waves + redstone security hardening +
  completeness dirty-window hardening + 19,366 lake-classified cctp
  twins deleted; specs → 1.18.0, migrations 0124+0125). §2.6b is now
  COMPLETE (all three passes). v0.21.9 (audit payload) cutting now
  under Ash's standing word.

- 2026-07-31 ~23:00Z — 🚀 **v0.21.8 CUT + DEPLOYED on Ash's word**
  ("you can cut and deploy whenever you want"). Release build green
  (11 assets), deploy green, smoke 13/13, live version v0.21.8.
  Payload: redstone state-write attribution + soroswap
  non-directional recognition (the 17/17 pair) + native XLM holders
  + prewarm/stale-serve/analytics-status + all three visuals waves
  (specs → 1.17.0) + crossed-book quarantine fix + the
  fraction-based unit-ratio alert. Post-deploy chain running
  detached (/root/postdeploy-0218.log): redstone rewind from
  62,056,000 → tip → scoped verifies (redstone/soroswap/aquarius)
  → verdict table (**17/17 EXPECTED — the first ever**) → mixed4 k6
  on the quiet box (the definitive AC2 mixed number). Inbox #4 is
  now CLOSED — no operator word outstanding except the standing
  three (paging, security review, and the decisions list).

- 2026-07-31 ~22:30Z — 🏁 **TAIL CHAIN DONE: soroswap taker coverage
  100.00% (554,221/554,221).** Full evidence:
  `evidence/2026-07-31-taker-coverage-and-verify.md`. Verify came
  back 15/17 with every gap root-caused same-day: redstone (known —
  v0.21.8), soroswap 20 skim legacy twins (event_index=0 pre-
  discriminator rows vs the replay's true-index rows, the CCTP
  0112 class — DELETED, 0 remain) + 1 blind ledger (LP-share
  transfers from gated pairs, classify fix agent running), aquarius
  (verify timed out under post-replay churn — overnight re-verify
  armed). mixed3 was a third contaminated mixed run (post-replay
  cold caches; also self-tripped both SLO burn pages, rolling off)
  — **mixed4 armed overnight post-recompression**; AC2 primary
  anchor stays the clean acceptance run (0 fails, p95 37.1ms).
  Ops closures: 19 compression policies applied + the assertion's
  stale postgres password file repaired (cred drift — it could
  never pass; codify with the vault OP item), root disk 81→78%,
  five alerts cleared. **v0.21.8 payload now: 17/17 closure
  (redstone state-writes + soroswap LP-transfer classify) + native
  holders + prewarm/analytics-status + visuals waves + crossed-book
  fix + skim-dedup verified.**

- 2026-07-31 ~17:30Z — 📊 **VISUALS PASS COMPLETE: survey top-10 =
  10/10 BUILT + merged (3 agent waves, specs 1.16.1→1.17.0).**
  Survey found 10 ranked opportunities + 7 defects; all built same
  day: order-book depth chart (/markets + /sdex goes live), asset
  visual pack (source donut/sparklines/change strip/concentration/
  mint-burn), dexes bespoke suite render (+ the series-name bug that
  NULLED the 90d volume panel on all 5 DEX pages), protocol TVL bars
  (lower-bound hatched), pool depth curves, account chart pack,
  lending/yield real bars, /network chain economics (fee-burn,
  total-XLM, protocol-upgrade markers — stroop strings, ADR-0003),
  divergence Δ% series endpoint + threshold band, anomalies
  donut/histogram/day×reason heatmap. §2.6b pass 2 DONE. New finding
  → task: /v1/sdex/orderbook serves CROSSED books (stale rows in the
  in-process live_offers feed) — UI defends, Go investigation queued.
  ⚠️ LIVE OPS in parallel: the soroswap taker replay LIVELOCKED at
  63.058M — retro-fill upserts into a compressed trades chunk blew
  write deadlines (373k retried inserts/h, all recoverable); fix =
  decompress the 4 compressed chunks between cursor and tip (69GB
  uncompressed monster chunk grinding, ETA evening; the 22:45Z
  policy recompresses automatically). Tail chain resumes after.

- 2026-07-31 ~14:15Z — 🔥 **PREWARM/STALE-SERVE LANDED (#13, spec
  1.16.0): no request ever pays for an analytics build.** Background
  sweep warms all 15 protocols × 4 windows (~13–16 min cycle, 20-min
  TTL, stale-serve + single-flight detached rebuild past TTL — a
  previously-built page never blanks); op-mix prewarmed at boot + in
  the 5-min loop; new `analytics{status, as_of}` wire field so
  degraded ≠ zero (frontend renders from status, not absence);
  paired refresh metrics. Measurement settled intrinsic-vs-starved:
  worst bespoke battery ~1.9s / op-mix ~70ms even UNDER replay load —
  the live failures were saturation × request budgets, now removed
  from the user path. Holders cache verified already stale-serving.
  All four of Ash's live-site reports now have merged fixes riding
  v0.21.8 (Go) + CF (web). §2.6b pass 3 substantially advanced.

- 2026-07-31 ~12:30Z — 🧹 **Morning batch: both live-site bug classes
  FIXED + two alerts closed + key hygiene.** (1) Frontend honesty
  sweep merged (7 panels rendered absent-as-zero — roster "0 events",
  /network "no operations", protocols-index fallback, exchanges/dex
  stats, composition, cross-anchor badge — now "—"/"unavailable";
  CF auto-deploys). (2) Native XLM holders fixed (trustline-shaped
  query was empty by construction; native arm ranks account entries
  by balance — PK-range read via the existing SWR cache, exact 9.9M
  count, alias-folded; rides v0.21.8). (3) `dex_trade_unit_ratio`
  root-caused BENIGN (yUSDC/USDC + yETH/ETH wrapper parity, ~2% of
  flow) → alert rewritten fraction-based (>25 AND >50% of inserts),
  both trees + runbook, applied to r1, now inactive.
  (4) `config_assertion_failed` = the DAT-03 nag working as designed:
  run `add-missing-compression-policies.sql` post-mixed3 (backfill
  precondition met; sequenced after k6 to keep latency evidence
  clean). (5) Key hygiene: weekly-k6 GH secret rotated to the
  high-rate key (its default scenario needs >20k/min), both 5000/min
  keys revoked (verified 401), on-disk plaintext scrubbed.
  Remaining alerts: freeze family (inbox [DECIDE]), completeness
  (v0.21.8), projector-lag + compression-lag + usd-coverage
  (replay-induced, clears-check post-replay), supply cross-check ×3
  + nonstandard-decimals ×5 (dispositioned; silence-with-annotation
  sweep pre-launch).

- 2026-07-31 ~08:45Z — 🔑 **STATE-WRITE ATTRIBUTION LANDED ON MAIN**
  (squash of the build agent's 9b919be8; verify green; 20 files,
  +1,220). Ground truth CORRECTED the design premise: write_prices
  rewrites EVERY requested feed's entry — the accepted set is the
  VALUE-CHANGING writes (pre-image Val != post-image Val), verified
  against all three tx classes at ledger 62056824. Plumbed as
  `events.Event.StateWriteKeys` from both paths (dispatcher: per-op
  LedgerEntryChanges; CH re-derive: batched PK lookups on
  ledger_entry_changes with replacing-parts dedup), per-source opt-in
  like OpArgs. Redstone attribution now: exact-subset first
  (arity-checked), payload-median fallback, honest refusal last.
  Golden fixture pins 62056824 → rwa:iBENJI (the price matched both
  BENJI twins' medians — unresolvable before). **The 17/17 flip is
  now one deploy away: inbox #4 asks for the word on v0.21.8.**
  Meanwhile the r1 tail chain (soroswap wait → all-source verify →
  taker coverage → mixed3 k6) runs detached; overnight note: the
  original chain waiter timed out before the verify — replaced.

- 2026-07-31 ~05:15Z — 🔵 **COMPLETENESS 16/17; redstone blind ladder
  1,626 → 170 → 15.** The v0.21.7 redstone replay reached tip and the
  scoped verify ran: the order-preserving rule cleared 91% of the
  residue. The surviving 15 ledgers (first 62,056,824, probed against
  the live decoder) are a PROVABLY ambiguous class — a single
  surviving price matching TWO feeds' signer medians, where order
  adds nothing. The verifier refuses by design (honest-blind beats
  misattributed). Evidence: `evidence/2026-07-31-completeness-verdicts.md`.
  **The exact closure is designed, not hacked**: the adapter's
  write_prices STORES each accepted feed — the tx's ledger-entry
  write keys name the accepted feed set exactly. Plumbing state-write
  keys through the dispatcher into `events.Event` (the OpArgs
  pattern, PR 166) gives attribution with zero heuristics; unit
  delegated to a build agent this morning. Meanwhile: soroswap taker
  replay continues (chain waiter re-runs the all-source verify);
  mixed3 k6 queued for the quiet box after it.

- 2026-07-30 ~20:30Z — 🎨 **ALL-PROTOCOLS VISUAL SUITE COMPLETE**
  (8199fe36; three parallel agents, all merged, full verify green).
  Every protocol page is now a window-reactive showcase: bridges
  (donuts/per-chain lines/cumulative net — earlier today), DEX/AMM
  (volume-by-pair donuts, top-5 pair lines, traders, largest trades),
  lending (pool/kind donuts, supply-vs-borrow + backstop + auction
  series, flash loans), yield (vault flow lines + donut), oracles
  (cadence, per-feed lines, feeds table, freshness). Window pills on
  EVERY category. More honest-data catches by agent verification:
  soroswap trades carry NO taker (0% coverage — omitted, not zeroed);
  DEX "all-time" honestly floored at the 2026-03-18 rollup start;
  sdex heavy surfaces bounded to 24h/7d with a note. Everything on
  main for v0.21.7.

- 2026-07-30 ~18:50Z — 🎨 **CCTP SDF showcase LANDED (merge) — and it
  CORRECTED the flow numbers.** The suite: verified chain attribution
  (the $66M mystery source = SOLANA — the token slice is the low 20
  bytes of the 32-byte burnToken; Solana's USDC mint tail matches
  byte-exactly; Aptos + Starknet also identified; every EVM tail maps
  to Circle-published addresses; one 1-cent residual honestly
  "Unverified"), donuts in/out, per-chain lines, cumulative net
  headline chart, largest transfers, unique-wallet KPIs. **Material
  correction (agent ground-truthed all 52,205 rows): phase-1 formulas
  over-counted** — legacy duplicate rows + mint_and_forward RESTATING
  mint_and_withdraw at 10× (7-dec vs 6-dec). Honest all-time flows:
  **inbound $11.87M / outbound $3.87M / net +$8.00M USDC; 851
  depositors, 8,803 recipients** (my earlier "$148.6M" quote was the
  inflated formula — corrected to Ash). Spec 1.15.0. Operator
  cleanup candidate: drop the stale legacy duplicate rows (queries
  are safe regardless).

- 2026-07-30 ~17:15Z — 🎯 **Verify: blind 1,626→170; ORDER-PRESERVING
  attribution resolves the residue (84ee3e05).** The v0.21.5 chain
  completed (replay hit tip; verify rc=0): the subset fix cleared ~90%
  of the blind class; the 170 residual = prices matching TWO feeds'
  medians (real fixture: iBENJI vs SolvBTC.BBN). Insight: the adapter
  builds updated_feeds in ONE PASS over feed_ids → survivors are an
  ordered SUBSEQUENCE → attribution is a DP-counted alignment (unique
  → attribute; else refuse). Order can only disambiguate, never
  misattribute. The residual event now decodes (golden-pinned).
  **17/17 needs one more post-v0.21.7 replay pass** (the projector
  must write those rows with the new rule). ALSO: bridge-flows chart
  landed (merge e1e4cbde — ?days windows, hourly 24h grain,
  window-keyed cache, dual-line chart + pills, spec 1.14.0); phase-2
  SDF visual suite agent launched (pies by chain, per-chain lines,
  cumulative net-inflow, largest transfers, verified chain maps).
  **Heavy queue running: k6 (AC2) → ops_by_source backfills**
  (/root/postreplay-queue.log, waiter armed).

- 2026-07-30 ~16:10Z — 🕵️ **"Missing data" investigation closed + the
  address-intelligence build LANDED.** (1) The GDVJM "Partial Coverage"
  banner was the frontend's honest 503 copy, not missing data — all 4
  routes serve; the 6s sourced-arm fix (ops_by_source) ships next tag.
  (2) **REAL gap found via the lake: CCTP's projected history started
  121k ledgers (~8 days) AFTER the contract's first on-chain event** —
  86 head events unprojected. Cursor rewound to true genesis 62,146,641;
  head is landing (min(ledger) already correct). GDVJM still unmatched —
  either in the remaining head window or its "CCTP usage" is via a
  forwarder (the activity endpoint's bridge note documents that
  structural limit). (3) Agent build merged (4f6e87d7): /accounts/{g}/
  trades + /activity + explorer UI, spec 1.13.0, migration 0123
  (taker/maker partial indexes — build CONCURRENTLY on r1). **Agent
  falsified my premise: trades has NO source_account column (my
  "99.9998%" was a stderr-swallowed artifact — 2>/dev/null ate a psql
  error and I read the wrong number). Attribution is taker/maker;
  soroswap captures neither (decoder drops SwapEvent.to) — follow-up
  candidate.** Aggregate op-counts arm aligned to ops_by_source
  (7afebd7d). v0.21.7 payload now: ops_by_source readers + both new
  endpoints + UI. r1 queue: replay → k6 → ops_by_source backfills +
  0123 CONCURRENT indexes → deploy.

- 2026-07-30 ~15:30Z — 🔬 **Ash's GDVJM report root-caused + fixed in
  code: the account-history sourced arm was a 6-second bloom probe.**
  The account has only 328 ops — the cost was the READ SHAPE, not the
  account: `source_account = ?` bloom-probes the whole 23B/34B-row
  table (6.17s) while the participant arm is PK (0.056s). Fix =
  `stellar.ops_by_source` slim projection (ttl_live_until class; MVs
  from BOTH operations + transactions with a sentinel op_index so
  op-source overrides stay exact) — both arms now PK reads, fail-loud
  without the table (16e9a03c). **Step-1 DDL + MVs applied on r1 at
  tip 63,718,785** (live capture running); Step-2 windowed backfills
  queue behind the replay. ALSO: address-intelligence build delegated
  (agent, worktree): /v1/accounts/{g}/trades (565.5M trades already
  carry source_account, 99.9998%) + /v1/accounts/{g}/activity
  (segmented op/DeFi/bridge breakdown) + explorer UI — Ash's "see
  everything every address has been doing" directive.

- 2026-07-30 ~14:40Z — 🔧 **k6/AC2 path unblocked; run queued behind the
  replay.** The GH k6-weekly workflow had failed SILENTLY since the org
  migration ate its secrets; restored the load key (freshly minted
  kid_b5425b41bc16f133, 5000/min) but discovered the suite's deliberate
  prod-host guard refuses api.stellarindex.io — the historical AC2
  evidence (p95 54.4ms) was captured ON r1 against localhost. Staged
  exactly that method (/root/k6-run.sh, k6 v0.50.0 binary + shallow
  checkout on r1; deleted the K6_TARGET_STAGING secret I briefly set so
  the weekly cron keeps its designed skip-until-staging behavior).
  Runs AFTER the replay frees the box. Hygiene flag: first mint attempt
  left an orphan key (plaintext never displayed to anyone — unusable;
  revoke in the ops sweep); /root/StellarIndex-k6 checkout + /root/k6
  binary are ephemeral, remove post-run.

- 2026-07-30 ~12:30Z — 🏁 **ROUTE GATE CLOSED: 0×5xx across all 94
  routes** (evidence: `evidence/2026-07-30-route-sweep-zero.md`),
  taken deliberately under replay saturation minutes after a restart.
  The closing move came from Ash's architectural instinct ("don't we
  have near-instant access to balances?"): account trustline/offer
  reads rode the account_id bloom index — rewritten as key_xdr
  PK-prefix ranges (**5.18s → 0.069s measured, 75×**), so account
  detail is INTERACTIVE for every account (top-20 wealth accounts
  probed 200 @ ~0.25s each). v0.21.6 cut+deployed carrying that +
  stale-serve past TTL + flags.stale honesty (85f706e1, 714a72a6).
  Compression backlog from the re-stamp is self-draining (2 chunks
  left, policy green). k6 launch-load re-run triggered (AC2).
  Remaining: 17/17 verdict when the replay lands (~16:30Z, waiter
  armed) + k6 result + the operator-only list.

- 2026-07-30 ~10:45Z — 🚀 **v0.21.5 CUT + DEPLOYED (Ash's word); final
  acceptance nearly closed.** Deploy green (all 5 binaries; smoke
  13/13; one transient gh-watch network blip — the run itself
  succeeded). **Route-sweep: 94 routes → 93 clean.** The spill fix
  proved out live (contracts_dir refreshed successfully in production
  for the first time — 27 straight OOMs before); contract detail ×3 +
  holders all land warm on retry exactly per the SWR contract. The
  final holdout (/accounts whale) exposed one more layer: the fill
  now SUCCEEDS (probed 200) but the 30s TTL hard-missed expired
  entries, so the warm window was ~30s per fill → **fixed on main
  (714a72a6, stale-serve past TTL — the wealth cache's contract
  applied), rides the next tag**. Redstone replay running with the
  subset-attribution decode: cursor 59.50M → tip 63.72M at ~12k
  ledgers/min, ETA ~16:30Z, then the chain auto-runs the full verify
  → 17/17 expected. Waiter armed.

- 2026-07-30 ~02:20Z — ✅ **usd_volume history FULLY CLEAN: 13.3M rows
  re-stamped, all 66 dirty days re-verify at 0 violations** (evidence
  updated in `2026-07-30-verify-usd-volume-30d.md`). The path found its
  final form after three course-corrections: the general
  `ch-rebuild -sdex` route measured ~47h (620 rows/s into compressed
  chunks) and was killed for a scoped SQL identity UPDATE — valid
  because the day-sweep proved every violation is ONE homogeneous
  class ([base_pegged] sdex, USDC base → `base_amount/1e7`, the
  verifier's own `expected`). Ops lessons banked in the artifact:
  sdex ch-rebuild windows OOM the 10 GiB client pin above ~50k
  ledgers; failed run-heavy-job runs leave stale locks that silently
  SKIP relaunched windows (unique job names per attempt); compressed-
  chunk DML needs the per-session decompression-cap raise. **Every
  data-quality item in the plan is now closed**; the only remaining
  loop-executable work is the v0.21.5 chain, on Ash's word.

- 2026-07-30 ~02:30Z — ✅ **verify-usd-volume CALIBRATED — the prove-it
  battery's last unfiled non-paging row** (evidence:
  `evidence/2026-07-30-verify-usd-volume-30d.md`). First 30-day
  production run: **current pipeline EXACT — 7 consecutive clean days
  (07-23→07-29)**; the 7,973 violations are bounded entirely to
  [06-30, 07-22] — the pre-peg-identity stamping era, ~+0.7% drift on
  base_pegged SDEX dust groups (worst group-day 1,858 USD).
  Calibration verdict for C4-055/066: estimated tier spans a 700×
  daily range (167k–117M USD) — no stable band; keep the coverage
  alerts primary, add NO sum threshold. **Queued buildable: a
  `usd-volume-restamp` ops tool** for the pre-07-23 pegged rows —
  must reuse `USDVolumeQuoteSpec` (the verifier's own
  reimplementation-trap warning), windowed under run-heavy-job,
  acceptance = a full-span verify-usd-volume at 0 violations.

- 2026-07-30 ~01:40Z — ✅ **SUPPLY GATE CLOSED: 8/8 vs Horizon** (evidence
  filed: `evidence/2026-07-30-supply-reconcile-8of8.md`). The chain
  completed 23:27Z: re-seed landed 47,093 rows / 38/38 wrappers (USDC's
  46,035 dormant holders restored — the class six scan-era attempts
  couldn't seed). First reconcile read +1.07% FAIL — refresher snapshot
  lag, not data (direct component SQL already summed to the passing
  figure); 5 min later: **USDC +0.14% PASS**. Residual decomposed:
  claimable +35k + SAC +499k = live rows for since-archived holders
  (the un-ingested eviction class — bounded, documented in the
  artifact). **Route-sweep after the heavy drain: 8→5×5xx, and all 5
  root-caused + FIXED on main (65617654)**: contracts-dir 30d GROUP BY
  genuinely needs >8 GiB (23/23 refresh failures — caught by the new
  swr metric) → external spill added to the shared explorer scan
  settings (r1-measured: OOM → 55.7 s clean); contract detail ×3 +
  account-state ran INLINE on request deadlines (cache could never
  fill for busy contracts/whale accounts) → all four moved to the
  established SWR shape (shared contract_detail cache + detached
  account-state fill). These ride v0.21.5 with the redstone
  subset-attribution fix — **the sweep's 0×5xx acceptance lands at
  that deploy**.

- 2026-07-30 ~00:20Z — 🔬 **Redstone's REAL root cause found + FIXED on
  main (f51e414d): subset-filtered batches, not empty batches.** The
  v0.21.4 full verify still showed 1,626 blind events (growing at tip —
  ongoing, not historical). Lake forensics on the first blind event
  (ledger 59,258,375): body decodes FINE; the failure is
  `2 feed_ids, 1 updated_feeds` — the adapter's freshness verifier
  dropped ETH and the positional zip refused the event. Fix: recover
  attribution from the SIGNED PAYLOAD in OpArgs[2] (feed → signer
  values → the adapter's stored MEDIAN), demanded unique + bijective;
  **verified byte-exact — the surviving price equals BTC's median of
  three signer values**. Ambiguity refuses the event (honest-blind).
  Fixture-driven tests (real lake event + adversarial synthetic
  payloads), CLAUDE.md + protocol page updated, verify green. **Needs
  v0.21.5 cut+deploy + projector-replay from 59,258,375 + full verify
  to flip 17/17 — fourth tag, needs Ash's word (inbox #4 updated).**
  En route, also proven: the "legacy shape" hypothesis was WRONG — the
  pre-59.4M event schema is identical plus a bonus payload field; the
  blind class spans both WASM eras.

- 2026-07-29 ~22:50Z — ✅ **TTL backfill COMPLETE + VERIFIED; completion
  chain phase 2 launched.** All 7 windows landed 21:42→22:38Z (~8
  min/window, ≤152 MiB observed — the slim-projection design costing
  what it promised vs the six OOM'd scan attempts). **Step-3 verify:
  3,182 sampled keys, 0 mismatched** against the old
  ledger_entries_current extraction; table = 21.35 GiB (predicted
  20-30). The artifact's final `uniqExact` sanity count OOM'd at the
  14 GiB users.d cap — diagnostic only, backfill unaffected (use
  uniqCombined if ever needed). `/root/v0214-chain.sh` now running
  detached: redstone compute-completeness (→17/17 expected) → SAC
  re-seed `-full-history` (TTL-gated via the new table) → verdict
  readback. Then: reconcile 8/8 acceptance + route-sweep target
  0×5xx.

- 2026-07-29 ~22:00Z — 🚀 **v0.21.4 CUT + DEPLOYED (Ash: "you can cut
  it"); chain running.** Sequence executed: CHANGELOG promoted (dup
  Added headers merged) → tag via guard-rail script → release.yml (11
  assets) → Step-1 DDL applied on r1 (`ttl_live_until` + MV; tip at
  creation 63,708,653) → windowed backfill launched detached
  (7×2M-ledger windows from 50M, heavy-wrapped, ~7 min/window) →
  deploy.yml green (indexer+aggregator+api+sla-probe+ops all
  v0.21.4). **Post-deploy verified live**: smoke 13/13; order-book
  initial load **83 s** (vs 30-min cap — acceptance item closed);
  dex_tvl refresh ok=1 (all 4 protocols); the new ttl_liveness PK
  lookup served its first verdict snapshot (ok=1, fail-open on
  not-yet-backfilled keys); advance ticking. **Gotcha for next time:
  every binary exposes every registered metric series — :9464 showed
  the API's series at zero because it is NOT the API's port; the API
  serves /metrics on :3000.** The instrumentation caught its first
  real event within minutes: contracts_dir refresh OOM'd at its own
  8 GiB pin while the backfill saturates CH (guard rail working —
  fail the query, not the host; prewarm retries). Route-sweep
  baseline DURING backfill: 8×5xx (the lake-scan class competing
  with the heavy job) — the acceptance re-run happens after the
  backfill + completeness + re-seed. Remaining chain: backfill done →
  Step-3 verify → completeness full run (redstone → 17/17) → SAC
  re-seed (→ 8/8) → route-sweep target 0×5xx.

- 2026-07-29 ~22:10Z — ✅ **Dependabot CLEAN (PR #51 vitest 3→4 merged
  green — 6/6 done) + r1 pre-deploy TOML check PASSED**:
  `/etc/stellarindex.toml` carries `usd_pegged_classic_assets` (line
  175) + `[supply.sac_wrappers]`, so the DEX TVL peg shortcut is
  configured the moment v0.21.4 lands — that checklist row closes.
  Inbox refreshed: the ONE new operator word needed is the v0.21.4
  tag permission (item 4).

- 2026-07-29 ~21:55Z — ✅ **Worker metrics LANDED on main (3f599177) +
  redstone args-order fix (7ad97523); v0.21.4 payload is now
  code-complete.** The metrics agent died THREE times on API-529
  (and its resume lost worktree isolation — it was branching in the
  main checkout), so I built the instrumentation inline via the full
  /add-metric chain: paired counter+histogram for the DEX TVL
  refresher, the SDEX order-book maintainer (op-qualified
  load/advance; pre-Load ticks deliberately unobserved), and ONE
  shared per-cache metric for the five explorer SWR refreshers.
  Rationale: all three workers fail INVISIBLY by design
  (carried-forward/stale-but-real serving) — these series are the
  only pre-hours-stale failure signal. 2 ticket alerts in BOTH
  trees + runbooks + catalog rows + promtool unit tests (fire AND
  healthy-silent proven) + seeded zero series + obstest regression
  tests. Verify green (one gocognit split in the seed func). NOT yet
  exercised on r1 — the workers only run post-v0.21.4 deploy;
  post-deploy check = `curl :9464/metrics | grep -E
  "dex_tvl|sdex_orderbook|explorer_swr"`.

- 2026-07-29 ~21:15Z — ✅ **Redstone check-order fix committed
  (7ad97523): empty batches classify as no-ops even WITHOUT op args.**
  Night-chain completeness stayed `projection_ok=f` (1,624
  undecodable across 1,612 ledgers) because the v0.21.3 empty-batch
  fix sat BELOW the `len(OpArgs)==0 → ErrMissingOpArgs` gate — and
  empty pushes often lack usable args. Order corrected (body decode →
  empty short-circuit → args only for the non-empty path); the
  real-lake golden test now passes NO args to pin args-independence.
  **Redstone → 17/17 completeness flips at the first full run after
  the v0.21.4 deploy + replay.** Night-chain also confirmed: redstone
  replay hit tip, completeness exit=0 (16/17 held), and the
  participant backfill correctly NO-OP'd — `operation_participants`
  is ALREADY fully populated ([3, 63.7M], 4.38B rows); the
  "1-day-only" claim in the accounts brief was stale, which also
  removes the residual-risk caveat on the account tx/ops UNION arms.

- 2026-07-29 ~19:10Z — ✅ **Phoenix + Comet TVL LANDED on main
  (b6169612) — WITH on-chain validation the building agent could not
  do.** The agent derived both storage layouts from protocol Rust
  sources and flagged them VALIDATE-ON-R1; before landing I ran its
  actual decode functions against the REAL lake through an SSH tunnel:
  **phoenix 6/6 keys decoded** (real reserves + CONFIG token pairs on
  two curated pools), **comet's Vec[Symbol] record decoded** (~748k
  USDC + ~71.7M BLND — Blend-backstop magnitudes; the dual-probe design
  hit on the first encoding). Comments upgraded to VALIDATED with the
  evidence. Every DEX now serves TVL post-v0.21.4 (soroswap + aquarius
  live math, phoenix + comet validated decode, SDEX = order book).
  **Tonight's build tally: 3 agents, 13 commits, all landed, all
  gates green.** /dexes honesty prose update (phoenix/comet flow-only
  note) rides the same deploy per the API-before-frontend rule.

- 2026-07-29 ~19:00Z — ✅ **Accounts-routes fix LANDED on main** (6
  commits through 360234be; verify green post-pick, shapes unchanged —
  degradation via the existing flags.stale/as_of envelope). Per-route:
  every scan pinned (threads/memory in-SQL); /accounts root cause was
  the wealth-refresher dying from the SAME 40× fan-out (its snapshot
  never filled → permanent 503) — now pinned + stale-served;
  /pools/reserves was running ClassifyTTLLiveness PER REQUEST — now a
  30-min SWR verdict snapshot (durable fix = the landed ttl_live_until
  projection at v0.21.4); /contracts + holders + op-stats get
  stale-while-revalidate snapshots. **Acceptance = route-sweep after
  the v0.21.4 deploy — target 10×5xx → 0** (residual risk noted on the
  account tx/ops UNION arms until the participant backfill densifies).
  Phoenix/Comet TVL agent still building; night chain ~1.5h from tip.

- 2026-07-29 ~17:55Z — ✅ **TTL lookup-table redesign LANDED on main**
  (4 commits through 9e97f71e; agent-built, reviewed, verify green
  post-pick). `stellar.ttl_live_until` (slim RMT projection, ~20-30 GB)
  + ingest MV (with the correctly-reasoned `tryBase64Decode` — a throw
  inside an MV would block ingest) + the classifier now a bounded PK
  lookup; scan path DELETED with a fail-loud missing-table guard; real
  ClickHouse integration tests incl. adversarial insert order. **The
  v0.21.4 operator sequence is in the artifact**: apply DDL → windowed
  backfill (from 50M; ~7 windows/heavy-wrapped) → verify → deploy →
  re-run the SAC seed → USDC restored → supply 8/8. Also this hour:
  **AC5 fresh-clone reproducibility captured** (public repo → install →
  verify green) + the RFP AC1-7 mapping added to the evidence index +
  blog postscript closing the May gaps list.

- 2026-07-29 ~16:45Z — ✅ **DEX TVL + SDEX order book + charts LANDED on
  main** (b1711519 / 3dbd941b / 4a78058b — agent-built in a worktree,
  reviewed, cherry-picked; verify + web gates green POST-merge against
  the dependabot-moved deps). Shipped: per-protocol `tvl` (snapshot-
  cached, lower-bound-honest, priced through the SAME tier system as
  usd_volume) on /v1/protocols + /dexes; `GET /v1/sdex/orderbook`
  (in-process live book: one bounded FINAL load + 60s incremental
  applies, exact rational prices, spec 1.12.0) + OrderBookPanel on
  /markets/{pair}; 90d volume chart per DEX; /company stale claims
  corrected. **Deploy-watch items for v0.21.4:** (1) the book's initial
  FINAL load wall-time is unmeasured on r1 — endpoint 503s honestly
  until loaded, avoid first-deploying while the participant backfill
  hammers CH; (2) API TOML must carry `[trades].usd_pegged_classic_assets`
  or TVL reports all-unpriced; (3) Phoenix/Comet TVL = follow-up
  (needs pool-storage decode); (4) discovered dead table:
  `sdex_offer_events` (migration 0026, nothing writes/reads it).

- 2026-07-29 ~15:30Z — 🏗️ **Decision B9 RESOLVED by Ash + build started:**
  per-token oracle layer = ALREADY SHIPPED (the July tier-valuation —
  blog/methodology copy to be updated); **BUILD: DEX TVL/liquidity
  aggregation + SDEX order-book depth + per-DEX volume/chart wiring**
  (Ash: "any dex's and sdex I want the order book / depth for, as well
  as volume and charts and liquidity"); CEX order-book depth stays
  retracted post-v1. Build delegated to a fresh-context agent in a
  worktree (this session too deep for clean multi-unit feature work);
  unit-by-unit with the /add-endpoint chain, reviewed + landed on main
  after gates. Inputs: reserve tables (aquarius_reserves/_liquidity,
  soroswap pair state, phoenix_liquidity, comet_liquidity, Blend TVL
  proxy) + classic `offer` entries in `ledger_entries_current`.

- 2026-07-29 ~14:45Z — 🌙 **Night chain launched**
  (`/var/log/night-chain.log`, self-sequencing): redstone full-history
  replay (~650k ledgers/h, tip ETA ~21:00Z) → full completeness run
  (expect **17/17**) → `ch-participant-backfill -from 2 -window 500000`
  takes the heavy slot (2–4 days, resumable — queued since 2026-07-07;
  a prerequisite component of the account-route fixes AND the C-F1c
  incoming-ops coverage gap). Local integration suite confirmed the
  CI-timeout diagnosis: **passes in 699s** under the new 20m cap.
  /accounts snapshot reader build: next session (fresh context; the
  participant backfill changes its measurement baseline anyway).

- 2026-07-29 ~14:30Z — ⚖️ **v0.21.3 chain results — one win, one
  mechanical tail, one honest park.**
  1. **Completeness 16/17 → redstone tail launched**: the fixed
     verifier's FULL run now sees the whole history's small-shape
     population (1,603 events to 59.2M) + a real Δ=941 unprojected rows
     from the pre-fix era. Mechanical close: full-history
     `projector-replay -source redstone -from 58758722` RUNNING (~7h
     background; the fixed decoder re-derives everything), then one
     full completeness run → 17/17. No code needed.
  2. **SAC re-seed: PARKED FOR REDESIGN after failure #6** — the
     classifier OOM'd its own new 8 GB pin. Root cause is DESIGN, not
     tuning: `ttlLiveUntilExpr` computes over the wide `entry_xdr`
     column across all 586M ttl rows per batch (my 89 MiB probe read
     only key_xdr — unrepresentative). **v0.21.4 item: a slim
     `ttl_live_until` projection table (key_hash → live_until,
     MV-maintained, ~20 GB → bounded lookups by design)** replaces
     scan-per-batch. Supply impact remains USDC −1.1% only
     (dispositioned; served value UNDERSTATES, the conservative
     direction).
  3. Also this hour: **4/6 dependabot PRs merged** (51+50 auto-rebasing
     post-conflict), **main-red causes fixed** (grpc v1.82.1 for
     GO-2026-6061 + integration deadline 10m→20m) — issue #53
     auto-closes on the next green main run.

- 2026-07-29 ~13:40Z — 🚀 **v0.21.3 CUT + DEPLOYED (express operator
  permission for the second same-session tag).** Version confirmed
  live, services active, ops-ch refreshed, ingest 5s behind, external
  smoke 13/13. The completion chain is running
  (`/var/log/v0213-chain.log`): redstone FULL completeness (fixed
  verifier re-judges the formerly-blind ledgers) → SAC re-seed
  (self-bounding classifier). Acceptance after: coverage 17/17, supply
  reconcile 8/8 (USDC restored), route-sweep. Also re-measured the 10
  slow routes post-merge-settling: still failing → the /accounts
  snapshot reader build IS needed (starts after the chain lands);
  measured single-account keyed FINAL lookups are 0.08s, so the
  snapshot work targets the LIST-shaped scans specifically.

- 2026-07-29 ~13:00Z — ✅ **Pipeline steady-state confirmed healthy +
  a self-correction.** Live watermarks: trustline/lp/sac observations
  AT TIP; claimable −250 (normal); lake lag 8s; all projectors at tip;
  supply publishing (137 snapshots/15 min); no backfills running.
  **Correction (the "quiet is not stale" trap caught the LOOP twice in
  one day, on the same table):** I read `max(ledger)` on
  `account_observations` as a lagging/converging backlog and told Ash
  it would "reach tip in ~1.5h". It is neither lagging nor converging —
  it is last WATCHED-ACCOUNT activity (SDF reserves move on multi-day
  cadence by design), the XLM anchor's dormant-accept path covers quiet
  spans past the horizon, and `supply_assets_stale` (>30h) guards a
  real stall. The table being ~8.5k behind tip is HEALTHY. Even with
  the fix deployed, the failure mode survives in the reader's habits —
  the runbook's triage shape (compare producer watermark, not entity
  activity) applies to humans and loops alike.

- 2026-07-29 ~12:10Z — 📐 **/accounts snapshot reader: implementation
  brief filed for fresh-context build** (deliberate: a multi-file
  feature at this session's depth risks quality; the brief de-risks
  next session instead).
  - **Problem**: 10 explorer routes 503 on the 8s read budget — all
    account-state class (`/accounts`, `/accounts/{g}`, its
    /transactions //operations //movements, `/contracts` list,
    `/operations` list, one /holders, wasm/interactions/code-history on
    /contracts/{id}). Root: per-request FINAL scans over the (now
    version-keyed) `ledger_entries_current` + huge fact tables; NOT the
    40× memory class (that was thread fan-out, fixed).
  - **Fix pattern (per the plan's own C-F1 line): CoverageCache** —
    `internal/api/v1/coverage_cache.go` is the template: a background
    goroutine refreshes a snapshot on an interval (30 min fine for
    list-shaped surfaces), handlers serve the snapshot instantly, and
    degraded-but-honest (200 + `flags.degraded:true` + stale-as-of
    timestamp) replaces 503s when the snapshot is old.
  - **Scope order**: (1) `/accounts` list (the 214M-row FINAL scan —
    snapshot top-N by balance + count), (2) `/contracts` +
    `/operations` lists same pattern, (3) per-account detail reads use
    bounded keyed lookups (should already be fast post-D2's ordinal
    index — MEASURE first; may just need `max_threads` pinning like the
    TTL classifier), (4) route-sweep target: 10×5xx → 0.
  - Wire per /add-endpoint if response shapes change (they should NOT —
    same shape + a degraded flag that already exists in the envelope).

- 2026-07-29 ~11:30Z — ✅ **redstone empty-batch fix LANDED (`78486ae6`,
  verify green)** — decoded the 156-byte shape end-to-end: it is
  `{updated_feeds: [], updater}` (Bytes-wrapped), the adapter's
  freshness-dropped no-op push. Now decodes to zero updates with no
  error; real-lake-bytes golden test (ledger 63,699,567) pins it.
  **v0.21.3 is now code-complete on both headline items** (TTL
  classifier `5cdec8da` + this). Next session: cut v0.21.3 → deploy →
  redstone replay + full completeness (→ **17/17 complete**) → SAC
  re-seed (→ USDC back → **supply 8/8**). Remaining v0.21.3-optional:
  /accounts snapshot reader (the 10 route timeouts).

- 2026-07-29 ~10:45Z — 🔎 **redstone "866 undecodables" INVESTIGATED and
  re-scoped — NOT a regression.** Findings, each measured against the
  lake:
  1. A **small REDSTONE event shape (data_xdr 140–180 bytes, ~1.5% of
     events)** fails decode; the big shape (>300 bytes) decodes fine.
  2. The small shape exists in EVERY ledger band back to the source's
     genesis — nothing changed on-chain at the deploy window.
  3. What changed is the VERIFIER: v0.21.2's honest-blind accounting
     marks undecodable-but-matched ledgers as unverifiable instead of
     silently passing them → projection_ok=f. The verifier is being
     honest about a long-standing designed skip.
  4. Red herring eliminated: `op_args_xdr` is 3 bytes for ALL 330k
     redstone events ever — the working decode path never used it.
  **v0.21.3 item (fresh-context task): classify the small shape**
  (sample: ledger 63,699,567 contract CA526Y2N…, data_len 156) per the
  EVERY-event binding, then redstone completeness goes green. Also
  landed this hour: the TTL-classifier self-bounding fix (`5cdec8da`,
  verify green) — the re-seed's only remaining gate is the v0.21.3
  tag+deploy.

- 2026-07-29 ~10:00Z — 🎯 **40× READ-AMPLIFICATION ROOT-CAUSED — it is
  thread scheduling, not the table.** Measured: the identical probe at
  `max_threads=4` costs **89 MiB on the NEW table vs 94 MiB on _old**
  (parity; slightly better). The 4.76 GiB figure was default-threads
  parallelism fanning wide over the new part layout — per-stream read
  buffers × many more concurrent ranges. Consequences:
  1. **The SAC re-seed unblocks with a one-line class of fix**:
     `classifyTTLLivenessBatch` gains `SETTINGS max_threads = 4` (+ a
     bounded max_memory), exactly the pattern its sibling
     `scanSACSeedWindow` already uses. → v0.21.3 queue, top item; USDC's
     +1.12% closes when it ships + re-seeds.
  2. Codecs/granularity identical; compression ~31% worse per row on
     the new table (window-ordered insert layout) — cosmetic, will
     improve as background merges continue.
  3. The 10 route timeouts are latency-class, not this memory class —
     related part-layout effects possible but unproven; /accounts
     snapshot reader remains their fix.
  4. **E3 determinism filed earlier this hour** (byte-identical phoenix
     window). Evidence pack: 9/11; the 2 remaining rows are
     operator-gated (paging → SEV drill; CG Pro key → top-50).

- 2026-07-29 ~09:15Z — ✅ **Substrate prove-it battery PASSED + usd-volume
  calibrated.** (1) verify-contiguity + verify-hashchain + verify-lake
  all exit 0: **0 broken hash links genesis→tip [2, 63,699,907]** on the
  post-campaign lake — filed. (2) First 30-day `verify-usd-volume`
  filed: 8,397 violations = ONE coherent class (base_pegged SDEX/USDC:
  verifier assumes the $1 peg with slack=0, stored values track market
  rate, deltas <1%) — methodology [DECIDE] for next release, NOT
  corruption; calibration table filed (estimated tier ≈ $0.4–0.9 M/day
  vs $3.5–3.9 B exact → recommend ~$10 M/day alert threshold).
  §2.6 remaining: prices top-50 (B1), re-derive determinism (E3),
  SEV drill + rollback rehearsal (paging-gated).

- 2026-07-29 ~08:30Z — ✅ **ansible-drift GREEN (exit 0)** after
  post-campaign re-measure: 69 → 2 → 0-beyond-allowance. The last real
  drift was an orphaned-uid owner on the migrations dir (stale deploy
  sync identity) — chowned root:root to match the role. Watch item: if
  the NEXT deploy re-orphans it, the deploy workflow's sync step needs
  an ownership flag (next-release queue). Also running: first
  `verify-usd-volume -days 30` production report (C4-055/066 alert
  calibration input).

- 2026-07-29 ~06:15Z — 🔁 **Completeness: my own `-skip-substrate` flag
  poisoned the INV-5 latch** ("prior verdict's substrate was FAILING —
  refusing to upgrade without evidence"). projection_ok=t for sep41×2
  is solid; but `complete=t` needs one full run per source WITHOUT
  -skip-substrate — launched (log
  `/var/log/completeness-substrate.log`). Lesson for the runbook: a
  skip-flag run does not merely "carry" the prior verdict — if the
  prior verdict was failing it re-writes substrate_ok=f and the latch
  then demands full evidence. redstone's substrate should also green
  (its projection stays failed until the decoder fix).

- 2026-07-29 ~06:00Z — 🔁 **Completeness runs 2+3 failed on a
  NON-incident and were relaunched.** sep41_transfers (CH "query
  cancelled" at 44 min) + redstone ("database system is shutting down")
  both trace to ONE cause: **unattended-upgrades applied the libc6
  security update at 06:24 local, restarting postgres** (routine Ubuntu
  needrestart behavior, not a crash — no OOM, deliberate systemd stop/
  start, PG healthy on the new libc). sep41_supply's run had already
  PASSED (exit=0). Reruns for the two launched (setsid-detached this
  time; log `/var/log/completeness-reruns.log`). Worth knowing:
  unattended-upgrades CAN bounce postgres at any ~06:24 local — heavy
  PG-dependent jobs should expect it (add to runbook lore at next doc
  pass).

- 2026-07-29 ~05:50Z — 🏆 **SUPPLY GATE: 7/8 PASS**
  (`evidence/2026-07-29-supply-reconcile-post-fixes.md`): **PHO +157% →
  −0.0002%** (phantom rows were the whole error), EURC/KALE back inside
  tolerance (CS-102, as predicted), AQUA +0.16% WITHOUT the seed. Sole
  FAIL = USDC +1.12% — expected: the approved delete removed its 48,505
  seeded dormant holders; restores when the re-seed lands.
  **Re-seed PARKED after 5 attempts, fully diagnosed:** (1) IN-list >
  256 KiB parse cap (fixed, codified), (2+3) classifier OOMs the
  CLIENT-PINNED 10 GiB openRead cap — and the root measurement:
  identical probe = **122 MiB on the old table vs 4.76 GiB on the
  post-D3 table (~40× read amplification;** same ORDER BY, both merged,
  cause unknown — NEW §2.4 INVESTIGATION, prime suspect for the 8s
  route timeouts too). Fix rides the next release (classifier batch +
  per-query settings); investigate the table regression first.
  sep41_transfers completeness run still going (supply run exit=0).

- 2026-07-29 ~03:40Z — 🔁 **Re-seed attempt 2 FAILED (10 GiB OOM in the
  TTL classifier's aggregation) → DECIDED (auto, revertible): switch to
  the CURRENT-STATE seed path (attempt 3, running).** Reasoning: the
  full-history path exists to recover balances below the current-state
  coverage floor — but post-D3 that floor is 38M, and **SAC balances
  are Soroban `contract_data`, which cannot predate protocol 20
  (~ledger 50.5M) — every SAC balance's current state is therefore
  inside the rebuilt table**. The current-state path is the
  memory-bounded streaming one and was TTL-filtered in the same wave
  (`7d9614c1`). Equivalence for SAC ≈ provable; acceptance = the
  supply reconcile (AQUA must recover its dormant holders' ~0.17%).
  **Post-launch code note (rides next release): the full-history
  classifier needs both a smaller batch bound AND a bounded
  aggregation — it OOMs regardless of query-size cap.**

- 2026-07-29 ~03:15Z — ✅ **ALL THREE PROJECTORS AT TIP** (63,696,834 =
  lake tip exactly; the 14-day sep41 zero-writer hole is CLOSED on the
  served tier). The three FULL compute-completeness runs launched
  sequentially (timer stopped during; log
  `/var/log/completeness-full-runs.log`; ~2h). SAC re-seed attempt 2
  past its predecessor's failure point and grinding. After both:
  supply-reconcile acceptance (PHO/AQUA/KALIEN) + route-sweep +
  coverage check close the §1 completeness + supply gates.

- 2026-07-29 ~03:25Z — 🔧 **SAC re-seed attempt 1 FAILED + fixed +
  relaunched.** `ClassifyTTLLiveness` batches 5,000 `unhex()` keys per
  IN-list ≈ 350 KiB of query text — over ClickHouse's 256 KiB
  `max_query_size` PARSE cap (a wire/text bound the 5,000-key batch
  sizing never accounted for). With the delete already executed this
  left seeded balances absent, so fixed immediately: users.d drop-in
  raising the default profile's max_query_size to 4 MiB (parse-stage
  text cap only — no memory/row/execution limits touched; hot-reloaded,
  verified live) + the SAME content codified in ansible
  (15-log-discipline.yml, tagged check `changed=0` = drift-clean
  byte-for-byte). Re-seed attempt 2 running. **Post-launch code note:**
  shrink the ClassifyTTLLiveness batch bound so the tool stays within
  default server caps (rides the next release — no more tags this
  session). — 🏆 **E1 GATE MET + the 39 supply alerts CLEARED.**
  1. **reconcile-balances 50/50: 0 MISMATCHES** (37 matched, 13
     merged-or-absent = accounts deleted on-chain, 0 errors) vs the
     19/50 (38%) pre-ordinal baseline — filed as
     `evidence/2026-07-29-reconcile-balances-50.md`. The C2-4c tie
     ambiguity is fixed end-to-end.
  2. **`supply_refresh_error_dominant` (was 39-40 firing) is GONE** —
     the CS-102 anchor fixes are working on live traffic.
  3. redstone AT TIP; sep41 ×2 within ~5k of tip at ~200k rows/cycle
     (`projector_lag_high` ×2 = that catch-up, self-clearing).
  4. New-alert triage: `ingestion_insert_errors{sep41_transfers}` =
     counter-window artifact from the catch-up burst (current cycles
     show 0 errors) — self-clears; `metrics_registry_absent`
     informational (likely a never-yet-incremented new counter) — noted.
  5. SAC TTL-gated re-seed running under the heavy wrapper.
  Remaining acceptance: sep41×2 at tip → FULL compute-completeness ×3
  → supply reconcile (PHO/AQUA/KALIEN) after the re-seed lands →
  updated route-sweep. — 🚀 **v0.21.2 DEPLOYED + the whole post-deploy
  chain is executing.**
  1. **Deploy verified**: workflow green, all 6 binaries v0.21.2,
     services active, `ops-ch` refreshed, ingest at tip, external smoke
     **13/13 PASS**.
  2. **Replays live**: redstone rewound to 63,624,934 → caught up
     ~1000 ledgers/cycle (2-3 decode_errors/cycle persist in the
     replayed range — the §2.4 undecodable class; completeness will
     re-judge). sep41: the projector-replay path hit the documented
     **new-cursor-inits-near-genesis trap** (cursors at ~73k, walking
     from genesis); fixed by stop-indexer → SQL fast-forward to the
     prior session's rebuild endpoints (supply 63,671,020 / transfers
     63,671,647) → start. Both now tailing their real gap (deadline-
     shrunk windows, tens of minutes to tip). NOTE: a live projector
     holds its cursor in memory — SQL fast-forward REQUIRES the
     stop/update/start sandwich or the row is clobbered.
  3. **SAC seeded-row DELETE executed (approved)**: sentinel predicate
     cross-checked exactly against provenance (54,863 rows / 38 assets
     == 38 provenance rows summing 54,863) → transactional DELETE of
     both → 0 sentinel rows remain. **TTL-gated re-seed running** under
     the heavy wrapper (`/var/log/sac-reseed.log`, v0.21.2 binary).
  4. 50-account reconcile baseline still running (serial).
  Acceptance batch when jobs land: supply reconcile (PHO toward
  tolerance, AQUA ~+0.18% preserved), sep41×2 + redstone FULL
  compute-completeness, route-sweep, alert clearing. — 🏷️ **v0.21.2 CUT** (promote 2687b3e1, guard-rail
  script green incl. verify.sh; one missing CHANGELOG entry added first
  — the ansible-drift restoration family). release.yml building; on
  completion the loop deploys all 6 binaries via deploy.yml (ZERO
  migrations in this tag — no schema risk), then: post-deploy battery →
  sep41 tail rebuild + redstone replay → SAC delete + re-seed
  (approved) → supply reconcile acceptance. 50-account
  reconcile-balances baseline still running (serial Horizon reads).

- 2026-07-29 ~02:15Z — ✅ **ANSIBLE APPLY + RULES SYNC DONE (INBOX #1
  cleared by the loop).**
  1. **Serving flip applied**: `stellarindex_clickhouse_serving_enabled:
     true` added to r1.yml (R1_INVENTORY_B64 re-synced); the CH-side
     profile turned out ALREADY provisioned (the 2026-07-27 13:03 apply
     — hours after my vault harvest checked, resolving that mystery).
     Live apply: 5 changed, 0 failed; all 3 services restarted; ingest
     advancing post-restart (no cursor regression); healthz OK.
  2. **Route-sweep: 21×5xx → 10×5xx** (`evidence/2026-07-29-route-sweep-
     post-serving-flip.txt`). The 11 recovered = the serving-auth class.
     The 10 remaining are the KNOWN slow-read timeout class (8s explorer
     budget; C-F1/C-F3) — fixes ride v0.21.2 + queued backfills. Not a
     regression: same routes failed in yesterday's baseline.
  3. **Prometheus rules were 3 WEEKS stale live** (2026-07-07 files,
     pre-org URLs, none of the campaign's alerts). Synced all 30
     `rules.r1/` files → promtool clean → SIGHUP (the lifecycle API is
     disabled — `systemctl reload`, not POST /-/reload; also
     `/etc/prometheus/rules.d/` is an INERT leftover dir, the real one
     is `rules.r1/`). Now 31 groups / 178 rules,
     `stellarindex_supply_assets_stale` LIVE — CS-102's monitoring hole
     closed in production.
  Next: v0.21.2 cut + deploy (session release budget unused). — ✅ **D3 VERIFY (bounded) + CUTOVER DONE.** The
  script's own verify OOM'd (FINAL×FINAL join of 4.5B×3.4B rows vs the
  18.6 GiB cap) — replaced with a bounded uniform key-hash sample
  (1/50,000): **66,539 keys, latest-ledger disagreement 0, change_type
  diff 109 (0.16%) = exactly the C2-4c tie-break corrections** (99 real
  ordinal corrections, 10 arbitrary-on-both-sides in un-ordinaled
  ranges). Cutover executed 01:44:16Z: `ledger_entries_current` is now
  the version-keyed table (pre-cutover tip 63,695,886), MV recreated,
  tip advancing (+6 in 30s), old table retained as `_old` (finalize
  DEFERRED). Also: the reconcile-balances first attempt misfired
  (`-config` is not a flag) and BOTH "still running" reads were pgrep
  self-matches — restarted correctly, running now. Chain next: ansible
  apply → v0.21.2. — 🎉 **D3 REPROJECT COMPLETE** (01:06:49Z,
  `reproject [38000000,63683991) complete`, ~29.5h wall). v2 table:
  3.448B rows, 96.7% with `intra_ledger_seq > 0`. **Cutover NOT yet
  done** — the script's phases are explicit (reproject → verify →
  cutover → finalize), and `verify` (v1-vs-v2 divergence sample) is now
  RUNNING (`/var/log/d3-verify.log`). On a clean verify the loop
  proceeds to `cutover` (drop MVs → double-RENAME → recreate MV →
  catch-up; revertible until `finalize`, which stays DEFERRED — it is
  the destructive DROP of old-v1). In parallel: the deferred
  **50-account reconcile-balances baseline** is running uncontended
  (`/var/log/reconcile-balances-50.log`) — acceptance target: 19/50
  pre-ordinal mismatches → ~0. Chain after: ansible apply → v0.21.2 →
  SAC delete+re-seed (approved). — 🔧 **Loop-reliability root cause found + fixed:
  the workstation was SLEEPING between turns.** Post-restart, no sleep
  assertion was held, so persistent Monitors died 4× (SSH tails killed
  by sleep) and one ScheduleWakeup was silently lost; the hourly cron
  guard was the only thing keeping the loop alive — exactly its designed
  role. Fix: `caffeinate -dims` now running
  (`PreventUserIdleSystemSleep 1` verified). **If resuming in a fresh
  session: start caffeinate FIRST or the loop's timers will silently
  die.** D3 progressing well: 58.0M/63.68M at 20:47Z (~3.2 min/window
  again), ETA ~00:00–01:00Z. No INBOX answers yet. — 📁 **Evidence pack STARTED (`evidence/`) + two
  alert dispositions + D3 un-stalled verdict.**

  1. **`docs/operations/evidence/` created** — index (gates → artifacts →
     honest gap list) + 4 filed artifacts: route-sweep pre-deploy
     baseline (full output this time, exit=21), soak-gate record,
     CS-102 red/green proof, supply-vs-Horizon baseline with the 3 FAILs
     dispositioned. Three plan generations called for these files; they
     now exist.
  2. **D3 is NOT stalled** — the hour-silent log alarmed me, but a live
     132s INSERT into `ledger_entries_current_v2` was in flight; the
     56M+ range is just much denser/slower per window. ETA slips past
     01:30Z, likely well into the morning. Monitor remains armed.
  3. **Two firing alerts are ONE known-pending item, not incidents:**
     `config_assertion_failed` = `FAIL compression_policies_applied`,
     and `timescale_compression_lag` is its downstream symptom — both
     exist because `add-missing-compression-policies.sql` is sequenced
     POST-D4 (§2.3.6). Considered pulling it forward (the backlog
     grows), but PG compression IO shares the ZFS pool with D3's writes
     and the sequencing decision was deliberate + recent — below my 70%
     bar to override. Both alerts will stand until the §2.3 batch.
  4. Root disk 80% (9.7G free) — watch-level; heavy-job guard trips at
     <2G; durable fix remains operator item #64. — 🔁 **Session restart recovery (PC reboot killed the
  ~13:40Z session) + three items closed.**

  Recovery was lossless via this file, as designed: cron guard re-armed
  (hourly), D3 completion monitor re-armed (the old one died with the
  session), services all active, D3 healthy — but slower than estimated:
  54.9M/63.68M at 19:15Z (~23 windows left, revised ETA **~01:30Z**).

  1. ✅ **Soak gate EXECUTED** (§2.5): 10 PASS / 0 FAIL past the 17:00Z
     deadline → snapshot destroyed, timer disabled. Time+evidence gate,
     no operator needed per contract.
  2. ✅ **The never-run CS-102 regression tests are now RUN — with a full
     red/green proof.** The dead session left the two storage files with
     the defect deliberately re-introduced (`PROBE(temporary)` markers) —
     an in-flight red-proof. Completed it: against the re-introduced
     defect both tests FAIL with precise diagnostics ("quiet asset anchor
     = 1000, want 5000 (the observer watermark)"); restored the fix →
     both PASS. The tests demonstrably guard the defect class, not just
     the happy path. Tree clean.
  3. ✅ **Pre-deploy route-sweep baseline re-captured**: ok=36
     client_4xx=37 **server_5xx=21** — byte-consistent with the known
     explorer outage (INBOX #1). This is the "before" for the
     post-deploy acceptance run.

  Next: parallel verification work while D3 runs; on D3 completion (the
  monitor fires) → the deferred 50-account reconcile-balances baseline
  uncontended, then D3 acceptance checks.

- 2026-07-28 ~13:40Z — 🛑 **DECIDED (auto, revertible): stop adding
  behaviour-changing code to v0.21.2; shift to verification + evidence.**

  26 behaviour-changing commits are already queued behind ONE undeployed
  release, several of which change what the API serves. Every further fix
  I add enlarges a blast radius that nobody has yet exercised against r1.
  Past some point "more fixes" stops reducing risk and starts adding it,
  and I judge we are past it. Remaining supply/eviction work is either
  parked on an operator DECISION (INBOX #5) or needs the heavy slot, so
  nothing on the critical path is being starved by this.

  Reversible: if a genuine blocker surfaces, it still gets fixed. This is
  about not shipping opportunistic improvements in the same tag as a
  supply-semantics change.

  **Executing instead: run the regression tests I WROTE TODAY BUT NEVER
  RAN.** `TestMinClassicComponentLedgerUsesObserverWatermark` and
  `TestMinSEP41ComponentLedgerUsesObserverWatermark` are build-tagged
  `integration`; I only ever `go vet`-ed them. A test that has never
  executed is not evidence — it is a claim. Running them now against real
  TimescaleDB via testcontainers.

- 2026-07-28 ~13:35Z — 🚀 **v0.21.2 DEPLOY PLAN — written now, because
  this is a much bigger deploy than a patch tag suggests: 26
  behaviour-changing commits, several of which change what the API
  SERVES.**

  **✅ ZERO migrations** (`git diff v0.21.1..HEAD -- migrations/` is
  empty). That removes the CS-099 hazard entirely — no
  old-binary-on-new-schema risk if the probe fails and the workflow rolls
  the binary back.

  **TWO deploy vectors, and missing the second is the easy mistake:**
  1. `gh workflow run deploy.yml` — the binaries.
  2. **`ansible-playbook … --tags stellarindex`** — `data-freshness.sh`
     and the Prometheus rules changed, so the new
     `stellarindex_supply_assets_stale` alert DOES NOT EXIST until ansible
     runs. Deploying binaries alone leaves the very blind spot that let
     CS-102 hide. This is also the apply that flips
     `stellarindex_clickhouse_serving_enabled` (INBOX #1) — one attended
     window can carry both.

  **What changes the moment binaries land:**
  - Classic + XLM supply UNFREEZE and are correct — those producers are
    alive.
  - SEP-41 (40 assets) publishes as `dormant` only, and re-freezes once
    the lag crosses 17,280 unless the projector catches up. **The sep41
    tail rebuild from 63,671,020 is part of the deploy, not a follow-up.**
  - redstone needs its replay from 63,624,934.
  - **Soroswap pairs whose entries are ARCHIVED stop reporting reserves.**
    This is user-visible and CORRECT (they were phantom depth), but it
    will look like data disappearing — expect it rather than treat it as
    a regression.

  **What does NOT change on deploy:** the SAC seed TTL filters. Seeds are
  manual, so nothing is re-seeded until someone runs one — and the phantom
  rows already in Postgres stay until the INBOX #5 DELETE is approved.
  Deploying does not silently rewrite supply.

  **Verification order after deploy** (each answers a different question):
  1. `bash scripts/dev/r1-smoke.sh` — 13 shape-asserted GETs.
  2. `bash scripts/ops/route-sweep.sh` — every OpenAPI GET; this is what
     catches a repeat of the 21/94 explorer outage.
  3. `bash scripts/ops/reconcile-supply-vs-horizon.sh` — expect EURC and
     KALE to come back inside tolerance (they are CS-102 casualties);
     **PHO stays +157% until the seed is re-run**, so do not read it as a
     failed deploy.
  4. `stellarindex-ops reconcile-balances -sample 50` — now ~2.4 s to
     sample instead of eating the budget. Run it WITHOUT piping through
     `tail`; its exit code is load-bearing.
  5. Watch `/v1/coverage` for redstone + sep41 ×2 going complete AFTER
     their replays — not before.

- 2026-07-28 ~13:00Z — 🧪 **Measured the TTL filters' blast radius BEFORE
  the deploy — they do NOT over-drop, and the AQUA risk I flagged points
  the opposite way to what I feared.**

  Why I checked: those filters change behaviour the moment v0.21.2 ships,
  and an over-broad filter would silently DELETE good balances — the same
  error class in the opposite direction. A first, naive sample looked
  alarming: 2,000 contract_data keys network-wide came back **2002
  archived / 0 live**.

  **That sample was misleading, not the filter.** Network-wide
  `contract_data` is dominated by expired TEMPORARY storage — among 50k
  recent TTL entries the median `live_until` is 63,164,824 and p90 is
  63,571,556, both well below tip. Most Soroban entries in history really
  are expired. SAC balances are PERSISTENT and behave nothing like that.

  Measured against AQUA's actual SAC contract (`CAUIKL3I…`):

  | | entries |
  |---|---|
  | total contract_data | 2,420 |
  | **archived** (dropped) | **757** |
  | **live** (kept) | **931** |
  | no TTL match (kept — fail-open) | 732 |

  So AQUA keeps **1,663 of 2,420**. Nothing like a wipe.

  **And the direction is favourable.** I had worried that dropping seeded
  rows would re-break AQUA. It should IMPROVE it: AQUA currently reconciles
  at **+0.18% OVERSTATED**, and its seeded SAC contribution (172.6M of
  100.1B ≈ 0.17%) is almost exactly that overstatement. Removing the
  ARCHIVED subset moves it toward 0, not away. The earlier caution —
  "seeded ≠ wrong, a blanket purge would break AQUA" — remains correct as
  stated: a *blanket* purge is wrong, a *TTL-gated* one is right, and
  those 931 live entries are precisely what a blanket purge would have
  destroyed.

  Still NOT a substitute for re-running the seed and re-reconciling after
  deploy; this is a pre-flight estimate, not the acceptance test.

- 2026-07-28 ~12:45Z — 📋 **v0.21.2 release prep: CHANGELOG curated and
  now complete.** The cut itself stays parked (one tag per session;
  v0.21.1 was this session's), but step 1 of /cut-release is done so the
  cut is mechanical when Ash approves.

  Audited `[Unreleased]` against `git log v0.21.1..HEAD`: **114 commits,
  30 of them code-bearing**, against 20 entries (5 Added / 15 Fixed —
  grouping is fine, no empty subsections).

  **Found one code commit with NO entry**, and a significant one:
  `9670ef29` — the galexie artifact-pin fix. During the 2026-07-27 live
  apply a stale pre-P27 binary was copied over the running v27.0.0 because
  the sha assert ran POST-install; that left us one galexie restart from
  re-running the P27 crash-loop SEV. Now documented.

  **How I nearly missed it:** my first pass was a keyword sweep that
  reported all 20 checks present. Three needles (`ttl`, `baseline`,
  `galexie`) were common enough to match unrelated text — and `galexie`
  did exactly that, hitting entries about config assertions and the pool
  trim. Printing the MATCH CONTEXT rather than trusting the boolean is
  what surfaced the gap. Same lesson as the rest of this session: a green
  check is only as good as what it actually covers.

  v0.21.2 contents when cut: CS-102 ×3 (`e21fa3d0`, `3f26b8db`,
  `aa0d08c2`), the sep41 zero-writer restart `ae7a082d` (**the
  load-bearing one for 40 assets**), redstone `9bfcf5da`, the TTL/archived
  work (`5471a05b`, `0fec76a3`, `7d9614c1`, `3bbb5085`), explorer 429
  `3422b150`, per-asset supply alert `22a8ac6d`, reconcile sampling
  `28405f0a`, plus the earlier seed/ansible/CI fixes.

- 2026-07-28 ~12:30Z — ⚡ **`reconcile-balances -sample N` unblocked**
  (`28405f0a`) — the §2.6 evidence pack needs samples larger than 8.

  **Diagnosed by measurement, not assumption.** The obvious suspects were
  both innocent: the per-account ClickHouse query answers in **1.7 s** and
  Horizon in **0.42 s** (and it already uses
  `argMax(balance, (ledger_seq, intra_ledger_seq))` — the composite
  tie-break, which is why the ordinal re-derive fixes THIS reader directly,
  independent of D3).

  The cost was the one-off **sampling** query: `GROUP BY account_id` across
  billions of raw change-log rows, then sorting every distinct id. It ate
  nearly the entire 900 s budget, which is why only 8 of 50 accounts were
  checked.

  Now sampled from the deduped current-state projection (~53.8M account
  rows, one per account): **~2.4 s**. The populations are **provably
  identical** — an account with ANY change above `minLedger` necessarily
  has its LAST change at or above it — so this is a pure speedup, not a
  narrower sample. Only account IDENTITIES come from current-state; each
  BALANCE is still read from the change log, so the C2-4c tie ambiguity in
  current-state values cannot leak into the verdict.

  **Deferred deliberately:** re-running the full 50-account baseline while
  D3 still holds ClickHouse. The fix removes the sampling cost, but the
  number is only worth filing once it can run uncontended.

- 2026-07-28 ~12:20Z — 🔎 **Swept every current-state reader of
  `contract_data` for the archived-entry class; found and fixed one more**
  (`3bbb5085`). The SAC seed was not the only exposure, and I checked
  rather than assumed.

  | reader | entry types | archived-entry risk |
  |---|---|---|
  | `asset_supply_reader` | trustline | none — classic entries have no TTL |
  | `account_state_reader` | account / offer / trustline | none — same |
  | `liquidity_pool_state_reader` | liquidity_pool | none — same |
  | `token_decimals_reader` | **contract_data** | **none in practice** — decimals are immutable, so an archived read still returns the right value |
  | `soroswap_pair_state_reader` | **contract_data** | **REAL — fixed** |

  Only Soroban `contract_data` / `contract_code` carry TTLs, so the classic
  readers cannot be affected at all. `TokenDecimals` reads a value that
  never changes, so staleness does not change the answer. **The one that
  mattered was Soroswap pair reserves**: a dead pool's final reserves were
  served as CURRENT depth. Archived pairs are now absent — which is already
  that reader's honest signal for "reserves unavailable", and its callers
  never read absence as zero.

  Also re-armed the D3 completion monitor: `TaskList` came back EMPTY, so
  the earlier one had expired despite `persistent: true` and I would have
  missed the reproject finishing. Worth checking TaskList rather than
  assuming a monitor is still alive.

  Soak now **9 PASS / 0 FAIL** — evidence well past the bar, waiting only
  on the 17:00Z clock.

- 2026-07-28 ~11:55Z — 📊 **D3 baseline: PARTIAL (8/50) but encouraging —
  and it points at the ordinal re-derive, not D3, as the fix.**

  The run was killed by its own 900 s cap after only **8 of 50** accounts:

  ```
  MATCHED 5   MISMATCH 0   NO_DATA 0   MERGED_OR_ABSENT 2   ERROR 1
  ```

  (The single ERROR is `context canceled` — an artifact of the timeout, not
  a real failure.)

  **0 mismatches where the pre-ordinal rate was 19/50 (38%).** At that rate
  8 clean accounts in a row is a ~2% outcome, so this is a real signal —
  but n=8, so it is a SIGNAL, NOT THE ACCEPTANCE TEST. Do not record the
  gate as passed on it.

  **Why this may already be fixed without D3:** `reconcile-balances` reads
  `stellar.ledger_entry_changes` directly (see its `-ch-addr` help), NOT
  the served `ledger_entries_current`. The ordinal re-derive populated
  `intra_ledger_seq` in exactly that table, so it could have resolved the
  C2-4c tie for this verifier already — while D3 is about the SERVED
  current-state table, a different consumer. If that holds, the two fixes
  address two different readers and both are still needed.

  **Why it was so slow (112 s/account):** almost certainly contention with
  the D3 reproject writing ClickHouse. **DECIDED (auto, revertible): defer
  the full 50-account baseline until D3 finishes** rather than fight it for
  the slot — the number is only meaningful if the run completes, and a
  contended run also risks slowing D3.

- 2026-07-28 ~11:58Z — ✅ **Corrected my own earlier call: the SAC seed's
  CURRENT-STATE path is now filtered too** (`7d9614c1`). I had left it,
  reasoning that filtering a streaming reader needed a server-side join of
  ~586M contract_data against ~586M ttl rows. **That reasoning was wrong.**
  The reader already narrows to WATCHED `Balance(Address)` keys in Go, so
  only that tiny fraction ever needs resolving — buffer the matched seeds
  in bounded batches and the existing bounded lookup handles it. No join,
  memory bounded, same fail-open contract. Both seed paths now agree, so
  which one an operator picks no longer decides whether archived balances
  get written.

- 2026-07-28 ~11:40Z — §2.6 evidence-pack progress + a gate re-verified.

  **Completeness gate re-checked live** (`/v1/coverage`): 17 sources, **3
  incomplete — redstone, sep41_supply, sep41_transfers**, all at watermark
  63,682,900. Exactly the three the plan already names, so §0 is CURRENT,
  not stale — and all three clear via the same v0.21.2 deploy + their
  replays. No new completeness problem exists.

  **D3 acceptance baseline capture started**: `reconcile-balances
  -sample 50`, the metric that must go 19 mismatches → 0 after cutover.
  Capturing it BEFORE cutover so the acceptance test has a documented
  before, rather than a remembered one. Not run under the heavy wrapper —
  it is a bounded 50-account sample, serial with a 250 ms Horizon delay,
  not the re-derive/backfill class the wrapper exists for, and D3 is
  CPU-bound on writes.

  Caveat on my own invocation: I piped it through `tail`, which masks the
  exit code — and this verifier's exit code is load-bearing (it fails when
  the ERROR fraction exceeds `-max-error-rate`, the C2-15 fail-open
  guard). Reading the mismatch count from the printed summary instead;
  the filed artifact should be produced WITHOUT the pipe.

- 2026-07-28 ~11:05Z — ✅ **Closed a launch-gate item: the explorer export
  no longer fails on our own rate limiter** (`3422b150`). This was the
  §1 "Launch mechanics" fragility — a launch-day rebuild could fail on a
  429, which is a bad way to discover it.

  Root of it: the code treated a 429 as a transport failure and charged it
  against the 5-attempt budget, so a throttled page burned everything in
  ~10 s of linear backoff and failed the export. **It passed on retry —
  the tell that nothing was broken, we were just asking too fast.**

  Now throttling has its own budget (8 waits) and does not consume
  transport attempts; it prefers the server's `Retry-After` (seconds or
  HTTP-date, capped at 60 s so a misconfigured value cannot stall a build
  for hours) and otherwise backs off exponentially to a 30 s cap **with
  jitter** — the export fans out many pages, and un-jittered backoff
  resynchronises them into the next window together. ~2 min total, longer
  than the anonymous window.

  Picked backoff over the two alternatives deliberately: an egress-IP
  exemption is infra that would ALSO mask a genuine limiter regression,
  and lowering build concurrency makes every build slower to dodge a
  problem that is really "we did not wait".

  `throttleDelayMs` exported + 8 unit tests (both Retry-After forms,
  elapsed date, hostile-value cap, unparseable fallback, exponential cap,
  jitter, and that total patience outlasts a window). Explorer gate
  108/108 + typecheck + lint clean; repo verify.sh green.
  Verified my own test actually RAN rather than trusting a green suite —
  the file was not visible in the truncated tail, so I re-ran it alone.

- 2026-07-28 ~10:55Z — 🚫 **No workaround exists for the sep41 stall —
  checked, so nobody burns time on it.** `projector-replay` looked like an
  unblock path; it is not. Its own help states it only *"Rewind the
  projector cursor … the projector tails forward to the live tip from
  here"* — it rewinds a cursor, it does not project. The deployed binary
  is **v0.21.1**, which still carries the zero-writer hole, so there is no
  registered sep41 source to tail forward and a rewind writes nothing.
  (Consistent with `ingestion_cursors` having no sep41 row to rewind.)

  **v0.21.2 is therefore the only path for the 40 SEP-41 assets.** No
  release cut: one tag per session and v0.21.1 was this session's, and a
  public tag is not cheaply revertible. Parked with the 13:30Z clock in
  OPERATOR INBOX #2 rather than acted on.

- 2026-07-28 ~10:45Z — ⏳ **CORRECTION + a 2.8-hour clock: the CS-102
  SEP-41 fix is NECESSARY BUT NOT SUFFICIENT, because the sep41 producer
  is genuinely DEAD.**

  I said earlier that the two fixes "cover all 48 watched assets". That is
  true of ANCHOR CORRECTNESS and overstated in practice. Measured:

  | | |
  |---|---|
  | live tip | 63,686,287 |
  | sep41 watermark | **63,671,020 — frozen** |
  | lag | 15,267 ledgers |
  | until the 17,280 dormancy horizon | **2,013 ledgers ≈ 2.8 h** |

  The watermark was byte-identical across two samples 90 s apart while tip
  advanced, and `ingestion_cursors` has **NO sep41 row at all** — the
  projector is not merely quiet, it was never running. That is the 14-day
  zero-writer hole (`ae7a082d`, undeployed), and 63,671,020 is exactly the
  documented rebuild start point, so the two agree.

  **What this means.** A correct anchor cannot conjure data a dead
  producer never wrote. Deploying CS-102 alone would let the 40 SEP-41
  assets publish as `dormant` only until the lag crosses 17,280 — about
  **13:30Z today** — after which they freeze again, and this time the gate
  is RIGHT to refuse: the producer really is stalled.

  So for SEP-41 the load-bearing fix is **restarting the projector
  (`ae7a082d`) + the tail rebuild from 63,671,020**, not the anchor. The
  anchor fix still matters — without it they would freeze even once the
  projector is healthy — but it is the second of two required changes, not
  the cure. The classic + XLM fixes ARE sufficient on their own, because
  those producers are alive (their watermarks sit 165 ledgers off tip).

  Recorded rather than acted on: both changes are already committed and
  ride v0.21.2. No release cut — one tag per session, and v0.21.1 was
  this session's.

- 2026-07-28 ~10:35Z — 🔎 **Swept for the CS-102 bug CLASS, and closed the
  monitoring gap that let it hide** (`22a8ac6d`).

  Having found the same defect three times, I audited every other
  freshness gate rather than assuming three was all of them. Five of the
  six `data-freshness.sh` domains are CORRECT — oracle / fx / trades /
  verdict / sep1 are keyed per PRODUCER, which is the right quantity, and
  a poller that stops writing genuinely is broken.

  **The sixth was the hole.** The `supply` domain measures
  `max(time)` across the WHOLE `asset_supply_history` table with no
  `GROUP BY`, so it only proves SOME asset is publishing. That is exactly
  why 37 of 48 assets could freeze — some for over two weeks — with this
  alert green the whole time: the handful of live assets kept the global
  max current. **An aggregate cannot see a partial freeze.**

  Added `stellarindex_supply_assets_stale` (count of assets with no
  snapshot in >30 h) + `stellarindex_supply_asset_max_age_seconds`, as two
  low-cardinality series rather than one per asset so the watched set can
  grow. Alert in BOTH rule trees + runbook.

  The runbook leads with the CS-102 triage shape — compare PRODUCER
  watermarks against per-entity last activity before concluding a writer
  is dead — and explicitly warns against "fixing" it by loosening the
  dormancy horizon, since that hides the defect and republishes unverified
  figures. It also carries this session's two measurement traps: read
  counter DELTAS not since-boot totals, and scope per-entity probes with
  an indexed predicate.

  **The doc-lint chain earned its keep**: it failed the push on a missing
  alerts-catalog entry AND an orphan runbook, both of which I had missed.
  verify.sh green after fixing (exit 0 + marker).

- 2026-07-28 ~10:05Z — ✅ **CS-102 SWEEP COMPLETE — all THREE supply
  algorithms fixed** (`aa0d08c2` closes the third leg).

  | # | algorithm | anchor was | assets | status |
  |---|---|---|---|---|
  | 1 | XLM | `MIN(last obs)` over SDF reserve accounts | XLM | ✅ `aa0d08c2` |
  | 2 | classic | per-ASSET last activity | 8 | ✅ `e21fa3d0` |
  | 3 | SEP-41 | per-CONTRACT last activity | 40 | ✅ `3f26b8db` |

  Same defect three times, found by following the population rather than
  stopping at the first fix: 5 assets → all 48 → "40 uninstrumented" →
  the SEP-41 path → and XLM, which is neither, was frozen too (last row
  04:46Z, 1 row in 6 h). SDF reserve accounts move every few days-to-weeks
  BY DESIGN, so `MIN(last observation)` across them is guaranteed to go
  stale — the anchor could not have worked.

  **The per-account probe is retained**, because it encodes a real
  precondition: every configured reserve account must actually be
  observed, or the reserve exclusion cannot be computed and the gate must
  stay permissive rather than bless a partial sum behind a healthy
  watermark. Only the freshness VALUE changed.

  **`MaxAccountObservationLedger` is a REQUIRED interface method, not an
  optional type-asserted one.** A missing delegate behind an optional
  interface degrades silently to the old anchor and looks exactly like
  healthy operation — the same silent-delegate class that produced the
  "entries = 0" bug. The compiler enumerated both production adapters
  instead of me guessing.

  Two existing tests asserted the per-account MINIMUM — they WERE the old
  specification, so they now assert the watermark, and the happy-path test
  fails loudly if the anchor ever regresses. Also deleted a stale doc
  comment still claiming "returns MIN(row.Ledger)": leaving it would have
  been the third doc-vs-implementation gap of the day, self-inflicted.

  verify.sh green (exit 0 AND the marker string). **Still pending the
  v0.21.2 deploy — until then 37 of 48 assets keep serving frozen
  supply.**

- 2026-07-28 ~09:10Z — ✅ **SEP-41 sibling FIXED** (`3f26b8db`).
  `MinSEP41ComponentLedger` now anchors on the producer watermark with the
  same shape as the classic fix: per-contract EXISTS decides
  *instrumented*, the watermark supplies the *value*, memoized on its own
  cache (separate from the classic one — the two producers advance
  independently, and one stalling must not hide behind the other).

  Empirically confirmed on r1 first, index-bounded (96 ms): watermark
  63,671,020; frozen contracts at 63,521,591 / 63,502,354 / 63,624,865
  (46k–169k behind); the one still-publishing contract's last event
  landing exactly ON the watermark. Perfect separation.

  **Together the two fixes now cover all 48 watched assets** — 8 classic +
  40 SEP-41. Before this, the fix I had shipped covered 8.

  Note the SEP-41 watermark (63,671,020) itself trails tip (63,684,077) by
  13,057 ledgers, because the sep41 projector was the 14-day zero-writer
  hole and its tail rebuild is still pending the v0.21.2 deploy. That is
  the gate working CORRECTLY — the producer really is behind, so it
  reports stale. It resolves when the tail rebuild runs, not by loosening
  anything.

  **Process note: I loaded r1's postgres with an unbounded join** (11 min,
  no output) before switching to index-bounded per-contract lookups that
  answered in 96 ms. Cancelled it via `pg_cancel_backend` rather than
  leaving it to compete with D3. Same class as the standing
  "no unbounded trade-scan queries" rule — it applies to
  `sep41_supply_events` too.

- 2026-07-28 ~08:55Z — ⚠️ **CS-102 HAS A SIBLING IN THE SEP-41 PATH, and my
  fix covers only 8 of 48 watched assets.** Found by widening my own
  verification from 5 assets to all 48 — the same narrow-slice trap I
  flagged earlier, this time in my own work.

  Running the FIXED anchor across every watched asset:

  | verdict | assets |
  |---|---|
  | FRESH (inside the 1000-ledger gate) | **8** |
  | UNINSTRUMENTED (gate skipped) | **40** |

  The 40 are the C-address Soroban/SEP-41 tokens. They have no rows in
  ANY classic component table, so `MinClassicComponentLedger` — the
  function I fixed — is not even on their path. They resolve through
  `StorageSEP41SupplyReader` → **`MinSEP41ComponentLedger`**, which is:

  ```sql
  SELECT COALESCE(MAX(ledger), 0) FROM sep41_supply_events
   WHERE contract_id = $1 AND ledger <= $2
  ```

  **Per-CONTRACT last activity — identical bug shape to CS-102.** A
  SEP-41 token with no recent mint/burn/clawback has a stale MAX(ledger),
  reads as a stalled producer, and freezes. The correct anchor is again
  the projector's watermark (how far `sep41_supply_events` has been
  written across ALL contracts), not one contract's last event.

  This matches the frozen population exactly: every asset frozen at
  2026-07-05 / 07-10 / 07-11 and the 30-asset batch at 2026-07-25 is a
  C-address. **The classic assets I fixed were the minority.**

  Code-level finding is CERTAIN (read from source). Empirical
  confirmation — projector watermark vs per-contract lag — is running;
  the join over `sep41_supply_events` is slow, so it is backgrounded
  rather than piled onto r1 while D3 writes.

- 2026-07-28 ~08:45Z — 🔗 **TTL filter WIRED into the SAC seed's
  full-history path** (`sacSeedReducer.dropArchived`, called after the
  window walk, before emit, judged at the lake's own tip).

  **Confirmed it is the right path first.** `sac_balance_seed_provenance`
  shows every existing seeded row came from **`full_history`**
  (seeded_at 2026-07-27 16:04:50) — the exact path now filtered. The
  ops command defaults to the OTHER path, so this was worth checking
  rather than assuming.

  `ClassifyTTLLiveness` now batches internally at 5,000 keys per IN list
  — a whole-network seed resolves tens of thousands (USDC alone carries
  48,505 seeded holders per the provenance table), which would not fit
  one list.

  **The current-state path (`StreamSACBalanceSeeds`) still has the gap,
  and I did NOT fix it blind.** It streams row-by-row instead of reducing
  to a key set, so filtering it wants a server-side join of ~586M
  contract_data against ~586M ttl rows — heavy-job class, and the slot is
  held by D3 for the rest of the day. Its doc comment claimed it returned
  "live" entries; it never did, and that claim is now corrected in place.
  **This is the SECOND doc-comment-vs-implementation gap found today**
  (the first was `MinClassicComponentLedger` promising "the slowest
  observer" while computing per-asset activity) — both were load-bearing
  falsehoods that hid a real defect, which is worth treating as a search
  pattern rather than a coincidence.

  Honest status: unit-tested + query-validated, **not exercised
  end-to-end** — re-running the seed is heavy-job class AND gated on the
  OPERATOR INBOX #5 DELETE decision.

- 2026-07-28 ~08:15Z — iteration close. **Soak gate: 8 PASS / 0 FAIL**
  (`/var/log/galexie-soak.log`, 18 sampled per run, 0 missing_in_cold, 0
  probe_errors). Evidence half MET; waiting only on the clock. Timer runs
  every 6 h at :20 — note the log stamps **CEST (+02:00)**, so the 17:00
  **UTC** gate is 19:00 CEST and two more checks (09:20Z, 15:20Z) land
  before it. Per the guardrail this gate is **auto-executable** (time +
  evidence, not operator), so at ≥17:00Z with 0 FAIL the loop destroys
  `data/minio@pre-trim-2026-07-26` without asking.
  D3 reproject at 39.5M / 63.68M, healthy, on track for ~22:20Z.

- 2026-07-28 ~08:10Z — 🛠️ **TTL-liveness mechanism IMPLEMENTED + validated
  against r1** (`internal/storage/clickhouse/ttl_liveness.go`).
  `ClassifyTTLLiveness` resolves each entry key to LIVE / ARCHIVED /
  UNKNOWN.

  It reads **`ledger_entries_current`**, not the raw change log — that
  table already holds 586,012,567 deduped `ttl` entries (≈1:1 with its
  586,390,435 contract_data), so liveness is a bounded lookup instead of
  a scan over all 1.15B TTL changes. Deliberately did NOT run the full
  586M×586M blast-radius join while the D3 reproject is writing to
  ClickHouse; that quantification waits for the heavy slot.

  **Validated end-to-end at the query level**: the exact generated SQL,
  run against `ledger_entries_current`, returned live_until values
  identical to the ones derived independently from the raw change log —
  `0a4970…` 56,252,151 (archived), `eb2333…` 54,431,750 (archived),
  `3b1c75…` 66,771,588 (live). Two sources, same answers.

  **Fails OPEN by construction.** A key with no TTL row, an undecodable
  key, or a TTLEntry whose decoded length is not exactly 48 bytes all
  return `TTLUnknown`, and the contract is that callers KEEP those.
  Dropping an entry merely because we failed to resolve it would
  understate supply — the same class of error as the phantom balances
  this removes, but far harder to notice than a residual over-count.
  Only a positive, parsed, lapsed `liveUntilLedgerSeq` justifies
  exclusion. Unit tests pin that guard, the sha256-over-DECODED-bytes
  derivation (hashing the base64 TEXT instead fails silently — every
  lookup just misses and the filter degrades to a no-op that reads as
  "nothing archived"), and the byte offsets.

  **Honest scope: this is the MECHANISM, not yet the fix.** It is not
  wired into `sac_balance_seed.go`, and no phantom row has been removed.
  Wiring + re-seed is gated on the OPERATOR INBOX #5 DELETE decision.

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

## 0. Verified current state — REFRESHED 2026-07-29 ~07:50Z (overnight autonomous run)

> **⭐ The overnight loop (2026-07-28 19:00Z → 2026-07-29 07:50Z) cleared
> the critical path.** Current verified state:
>
> | Gate | State |
> |---|---|
> | **D2+D3 (C2-4c)** | ✅ DONE end-to-end: reproject (29.5h) + bounded verify (66,539 keys, 0 ledger-diff) + cutover executed; **50-account reconcile vs Horizon: 0 mismatches** (was 38%). `_old` table retained (finalize deferred — it is the 40× investigation baseline) |
> | **v0.21.2** | ✅ cut + deployed + verified (smoke 13/13); `ops-ch` refreshed |
> | **Supply** | ✅ **7/8 vs Horizon** (PHO +157%→−0.0002%); 39 freeze alerts cleared; approved phantom-row DELETE executed (54,863 rows, provenance-verified); USDC +1.12% dispositioned (re-seed parked on the diagnosed CH blocker below) |
> | **Completeness** | ✅ **16/17 sources `complete=t`** (sep41×2 restored after the 14-day hole: projector at tip + full-substrate runs green). Sole exception: **redstone** — 866 undecodable events, decoder fix rides next release |
> | **Explorer routes** | 🔵 21×5xx → **10×5xx** (serving flip landed); residual = slow-read timeout class, see 40× investigation |
> | **⚠️ OPEN INVESTIGATION (top §2.4 item)** | The post-D3 `ledger_entries_current` costs **~40× more memory to read** than `_old` (measured: same probe 122 MiB vs 4.76 GiB; same ORDER BY, both merged). Blocks the SAC re-seed (classifier OOMs the client-pinned 10 GiB openRead cap) and is prime suspect for the 10 route timeouts |
> | **Alerts** | Residual: anomaly-freeze family (known [DECIDE]), dex informational ×6, cross-check ×3 (partial_wrap, dispositioned — Horizon-verified correct), compression pair (post-D4 item), metrics_registry_absent (informational), completeness (clears at next snapshot for sep41; redstone until decoder fix) |
> | **Loop infra** | caffeinate held (workstation sleep killed timers 4×); cron guard hourly; unattended-upgrades bounced PG 04:24Z (libc — routine, non-incident) |
>
> **Ash's three items stand** (top of OPERATOR INBOX): wire paging,
> book the security review, — and the SAC delete is now DONE/superseded.
> **Next-release queue (code, no more tags this session):** redstone
> decoder fix, TTL-classifier batch+settings fix, 40× table regression
> root-cause, /accounts snapshot reader.

<details><summary>(superseded 2026-07-28 §0 — kept for history)</summary>

> **⚠️ (historical) four blockers found 2026-07-27/28:**
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

</details>

## 1. Go-live gate (all must be true)

> **SUPERSEDED for planning by “THE PLAN” at the top of this file
> (refreshed 2026-08-15).** Kept for its rationale and execution recipes.
> Where this section and THE PLAN disagree on what is still outstanding,
> THE PLAN is right.


- [x] ✅ **Supply trustworthy** — DONE 2026-07-30: full reconcile vs
      Horizon **8/8 PASS** (evidence `2026-07-30-supply-reconcile-8of8.md`);
      the claimable seed, SAC full-history seed, and dormancy-anchor fix
      all landed. Residual: `supply_cross_check_divergence` ×3 (PHO/BLND/
      EURC, partial_wrap) dispositioned — our values Horizon-verified;
      external references count wrapped supply differently. Pre-launch:
      convert dispositions to annotated silences.
- [x] ✅ **Completeness green — 17/17, DONE 2026-08-01 08:33Z, first
      time ever** (evidence `2026-08-01-completeness-17of17.md`).
      Every source `complete=t` AND `lake_complete=t`, publicly served
      on `/v1/coverage`, with no carried claims — the final three
      (redstone/soroswap/aquarius) re-verified full-range within 24h
      and replay rewinds now force re-verification (dirty windows,
      proven in this very verdict).
- [x] ✅ **Prove-it battery passed** (§2.6) — every row filed as of 2026-07-30: reconcile-balances (0 mismatches), verify-lake/contiguity/hash-chain (genesis→tip), re-derive determinism (byte-identical), price vs 5 references (22/22), supply vs Horizon (**8/8**), `verify-usd-volume` calibrated (current pipeline exact; pre-07-23 re-stamp queued as data-quality follow-up). Remaining battery-adjacent: the SEV drill (paging-gated).
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
      ✅ **Export 429 fragility FIXED 2026-07-28** (`3422b150`). A 429 no
      longer spends a transport attempt — throttling has its own budget
      (8 waits), prefers the server's `Retry-After` (capped at 60 s so a
      bad value cannot stall a build for hours), else exponential to a
      30 s cap with jitter. ~2 min total patience, longer than the
      anonymous tier's window, so a build rides the window out instead of
      dying inside it. Chose backoff over the other two candidates: an IP
      exemption is infra that would also mask a real limiter regression,
      and lowering concurrency just makes every build slower to dodge a
      problem that is really "we did not wait".

## 2. Critical path (dependency-ordered)

> **SUPERSEDED for planning by “THE PLAN” at the top of this file
> (refreshed 2026-08-15).** Kept for its rationale and execution recipes.
> Where this section and THE PLAN disagree on what is still outstanding,
> THE PLAN is right.


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

### 2.5 ✅ Soak close-out — EXECUTED 2026-07-28 19:17Z

**Gate met and executed by the loop** (per the auto-executable contract):
10 PASS / 0 FAIL at 19:16Z (> 17:00 UTC), re-confirmed at execution time.
`data/minio@pre-trim-2026-07-26` destroyed (0 pre-trim snapshots remain);
`galexie-soak-check.timer` disabled + removed. Pool free 3.60 T (reclaim
lands asynchronously as ZFS frees the snapshot's unique blocks). Rollback
for cold-tier issues is now rehydrate-only (needs §2.2's archivewriter
cred fix — one more reason INBOX #1's ansible window matters).

<details><summary>(original gate text, for the record)</summary>


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
</details>

### 2.6 Prove correctness (Phase E — the go-live evidence pack)
**Artifacts now FILE under [`evidence/`](evidence/README.md)** (index
started 2026-07-28 with 4 artifacts + the honest gap list — first time
in three plan generations the files actually exist).
Run the confidence-campaign E-gate end to end and FILE the artifacts:
reconcile-balances (+ N random accounts/trustlines), verify-lake /
contiguity / hash-chain to genesis, compute-completeness all-green,
re-derive determinism proof, prices top-50 vs CoinGecko/Chainlink, supply
vs external truth **including the seeded SEP-41 genesis baselines (AQUA
overstated +15.7% in the 2026-07-07 test — verify the 2026-07-26 seed
doesn't serve that)**, first `verify-usd-volume -days 30` → calibrate the
C4-055/066 alert. Also: SEV-1/2 paging drill + rollback rehearsal —
evidence files have never been produced across three generations of plans.

### 2.6b Final pre-launch passes (added 2026-07-31, Ash's ask)
**Grounding incident (2026-07-31 ~09:45Z, Ash's live-site reports):**
CCTP page missing its visual suite + roster "0 events" + /network/
first-load "no operations in 24h" are ONE pattern: on-demand
analytics builds miss the request budget on cold/loaded paths
(`protocol bespoke build failed … context deadline exceeded` — the
suite IS in deployed v0.21.7), the API honestly omits the block, and
the frontend renders ABSENT as ZERO ("0 events", "no operations") —
a false empirical claim. Fix class, queued as the concrete start of
pass 3: (a) move every expensive block (protocol bespoke, network
op-mix, roster per-contract counts) to the established
prewarm/SWR/stale-serve pattern (CoverageCache / DEXTVLCache /
wealth-cache contract) so first loads are warm by construction;
(b) frontend honesty sweep — absent field renders "—", never 0/"no
X in 24h" (the agents' own omitted-not-zeroed principle, violated
by these panels). Re-verify bespoke build budgets on the quiet box
post-replay before concluding anything is intrinsically slow.
1. **Full audit pass** — one more cold adversarial sweep over the
   whole surface (post-all-the-July-changes; the last full audit was
   2026-07-01, and ~40 tags have shipped since).
2. **Visuals-opportunity pass** — walk every endpoint + explorer page
   asking "what useful chart/graph/pie is possible from data we
   already serve but don't visualize?"; build the winners.
3. **Complete-page load-time pass** — measure EVERY page to FULLY
   POPULATED (all panels, not just first paint), using COLD random
   addresses/assets/contracts drawn from the lake so no measurement
   is a cache hit; fix what misses budget.

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
1. **Apply DB migrations THROUGH 0143 before the binary swap** [V, audit-2026-08-14]:
   run `stellarindex-migrate up` (0142 = int4→bigint `derive_generation`; 0143 =
   `sessions.token_hash`) and confirm `schema_migrations.version = 143` and not
   dirty. THEN tag the launch release (SemVer) and deploy via the re-armed gate.
   The new binary carries `ExpectedSchemaVersion = 143`; REC-06's critical
   readiness check (`internal/api/v1/server.go:126`) fail-closes `/readyz` with a
   503 backend-drain if it starts against a ≤142 or dirty schema — so the ordering
   is enforced fail-closed, but the migration MUST actually be run first or the
   deploy will drain instead of serve.
2. Confirm `auth_mode=apikey_optional`; external SLA-probe smoke with a
   `sip_` key; outside-internet `make smoke` 13/13.
3. Status page + API docs + SLA/error-budget page current; F-0100
   counter-presence PromQL sanity; Grafana launch-watch board from
   `post-launch-queries.md` (refresh metric names first).
4. Announcement; open the first-24h watch (every alert = SEV-2 minimum).
   **EXPECT a one-time forced re-login of every dashboard user** in the first
   minutes after cutover [V, audit-2026-08-14]: migration 0143 leaves all
   pre-cutover `sessions` rows with `token_hash = NULL`, so the new binary (which
   authenticates by `sha256(cookie token)`) resolves none of them. This is the
   standard single-invalidation cost of a session-secret rotation, NOT an auth
   outage — do not escalate the resulting login spike / 401s on stale cookies.

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

> **SUPERSEDED for planning by “THE PLAN” at the top of this file
> (refreshed 2026-08-15).** Kept for its rationale and execution recipes.
> Where this section and THE PLAN disagree on what is still outstanding,
> THE PLAN is right.


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

> **SUPERSEDED for planning by “THE PLAN” at the top of this file
> (refreshed 2026-08-15).** Kept for its rationale and execution recipes.
> Where this section and THE PLAN disagree on what is still outstanding,
> THE PLAN is right.


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
