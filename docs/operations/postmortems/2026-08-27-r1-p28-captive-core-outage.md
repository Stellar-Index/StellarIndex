---
title: Post-mortem — r1 ingestion outage from a raw-apt captive-core upgrade
date: 2026-08-27
status: resolved
severity: P1 (~22 min of halted mainnet ingestion; no served-data loss — API/prices served from cache throughout)
author: ops (Claude-assisted)
---

# r1 P28 captive-core upgrade → ingestion outage (2026-08-27)

## Summary

While clearing the `stellarindex_stellar_stack_lagging` alert (r1's captive-core
was protocol-27 `27.1.0`; P28 needs `>= 28.0.1` before the ~2026-09-16 upgrade
ledger), I upgraded the core with a **raw `apt install stellar-core=28.0.1…`
plus `systemctl restart galexie`**, instead of the ansible galexie role. That
halted r1 ingestion for **~22 minutes**. The public API and served prices kept
working the whole time (they serve from cache + the ClickHouse lake), so there
was **no served-data corruption or customer-visible price outage** — but the
ingest tip froze at ledger 64147354 and lag climbed to ~1250 s before recovery.

## Root cause

galexie's captive-core **is** the system `stellar-core` package: `galexie.toml`
sets `stellar_core_binary_path = /usr/bin/stellar-core`, and `run_stellar_core:
false` means r1 runs no standalone core service. The `apt` operation on the
`stellar-core` package **reset `/etc/stellar/captive-core-galexie.cfg` from
`stellar:galexie 0640` to `stellar:stellar 0640`** (the package's default owner).
galexie runs as user **`galexie`, which is not in the `stellar` group**, so it
lost read access to its own captive-core config:

```
Could not load configuration: Failed to load captive-core-toml-path file:
open /etc/stellar/captive-core-galexie.cfg: permission denied
```

→ captive-core never started → no ledgers exported → ingest tip frozen.

**The ansible galexie role does not have this bug.** `07-galexie.yml` does the
*versioned* apt install itself and templates the cfg back to `stellar:galexie
0640` in the same run (ansible's `template` re-asserts owner/group/mode even when
content is unchanged). A **role-driven** upgrade would have restored the group
automatically. Raw apt outside the role does not.

## Why a blip became a 22-minute outage

A galexie restart on r1 forces a **captive-core cold catchup on mainnet's large
state**, which is inherently slow (~9 minutes):

1. Download + verify + apply **~14.7 GB** of bucket state for the checkpoint.
2. `Startup state load took 94.6 s` (in-memory Soroban state).
3. Bind overlay, authenticate to peers, follow live SCP consensus.
4. `Waiting for trigger ledger: <next checkpoint>` — buffer synced ledgers until
   the next checkpoint boundary, then flush the backlog to galexie.

I restarted galexie **~5 times** during triage (the P28 restart, the core
rollback, a wrapper bypass, the chgrp fix). **Each restart re-triggered the full
~9-min cold catchup from scratch**, so it never completed — that is what turned a
brief blip into a 22-min outage, far more than the config bug itself.

I also lost time on a **red herring**: `galexie-append.sh`'s resume-point
preamble runs `mc ls --recursive live/galexie-live/` (lists the whole hot bucket)
and is slow on restart — but it was never the blocker.

## Resolution

1. `chgrp galexie /etc/stellar/captive-core-galexie*.cfg` (restored the role's
   intended `stellar:galexie 0640`).
2. Rolled the core back to the known-good `27.1.0` (`apt install
   --allow-downgrades stellar-core=27.1.0-…`) to de-risk recovery — the P28
   bump is not urgent (weeks of runway).
3. **Stopped touching galexie** and let the one clean catchup complete
   uninterrupted. Tip resumed at 64147354, flushed the ~260-ledger backlog, and
   lag collapsed to ~1 s. Ingestion healthy.

(A temporary edit to `galexie-append.sh` that hardcoded the resume ledger to
bypass the slow `mc ls` was made during triage and must be reverted — the real
fix was the cfg group, not the preamble.)

## Prevention / action items

1. **Protocol/core upgrades on r1 go through the ansible galexie role, never raw
   apt.** Procedure: pin `stellar_core_version` in `r1.yml` → run the role (or
   its galexie task) → verify `stellar-core version` and that
   `/etc/stellar/captive-core-galexie*.cfg` is `stellar:galexie`. Do it in a
   maintenance window and **expect ~10 min of ingest catchup**. See
   `docs/operations/protocol-upgrades.md`. The role support (versioned install +
   `group: galexie` cfg template) landed on the network-params branch (PR #203);
   ensure it is on `main` before the next r1 core bump.
2. **Never restart galexie on r1 more than once for a change.** One restart, then
   wait ~10 min and watch the tip — do not re-restart mid-catchup.
3. **Defensive option (not yet applied):** add `galexie` to the `stellar` group
   (or have the role re-assert the cfg group immediately after any apt task) so a
   stray package operation can't lock galexie out. Weigh against giving galexie
   broader `stellar`-group read.
4. A stalled ingest tip is **not** a served-data outage (cache + lake keep
   serving) — diagnose calmly; don't panic-restart.
