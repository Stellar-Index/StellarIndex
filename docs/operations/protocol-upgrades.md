---
title: Stellar protocol upgrades
last_verified: 2026-08-27
status: current
---

# Stellar protocol upgrades

Each ledger protocol bump (Futurenet, then Testnet, then Mainnet) can add XDR
types, operations or event shapes. Two things must be ready **before** a
network crosses the upgrade ledger:

1. **Captive stellar-core ≥ the new protocol.** galexie embeds captive-core; a
   core that predates the protocol **refuses to sync past the upgrade ledger**
   and ingestion halts. The role installs core with `state: present`, which
   never upgrades an existing binary, so a bump is a deliberate
   `stellar_core_version` pin, not automatic.
2. **A `go-stellar-sdk` / `go-xdr` that defines the new XDR types.** An unknown
   union arm fails to unmarshal and the indexer errors on every affected ledger.

## The core-upgrade procedure

`stellar-core-auto-upgrade.timer` does this unattended every 6 hours: when
apt.stellar.org offers a newer core it stops galexie, installs it, restores the
captive-core config group, waits for galexie to write past its stop tip, and
reinstalls the previous version if it does not
(`stellarindex_stellar_core_autoupgrade_failed` pages). Core 29.0.0 reached apt
2026-09-24 with the mainnet P29 vote on 2026-10-01, so plan for a week of
runway. The manual procedure is for a pinned host, a rollback, or a hold
(`touch /var/lib/stellarindex/stellar-core-auto-upgrade.stop`).

```sh
# 1. Pin the target apt version in the region inventory:
#    stellar_core_version: "28.0.1-3508.947aad841.noble"
# 2. Apply (targeted — reinstalls core, restarts galexie onto it):
ansible-playbook -i inventory/<region>.yml playbooks/archival-node.yml --tags galexie
# 3. Confirm: stellar-core version  → >= the new protocol
#    and galexie resumes exporting (no "unsupported ledger version").
```

Bump in a maintenance window a few days AHEAD of the upgrade ledger; a newer
captive core against an older live network is forward-compatible.

## Readiness checklist (every protocol bump)

Each line is yes/no; a "no" blocks the mainnet window.

1. Read the release notes and CAP list; mark each change as XDR shape, operation,
   event shape or host-function-only.
2. `go-stellar-sdk` on `main` defines every new XDR arm.
3. Every `switch` over a changed XDR enum falls through gracefully (grep the
   enum name under `internal/`).
4. Captive core ≥ the new protocol on every test net, then on r1 (procedure above).
5. Run the drill below on a test net that has already crossed the upgrade.
6. A decoder, event-schema or feed change found by the drill follows the replay
   rule in [ingest-pipeline.md](../architecture/ingest-pipeline.md#the-replay-decision-rule).
7. Record the result in a section like the ones below.

## Upgrade drill: golden ledgers, green vs live

`scripts/ops/protocol-upgrade-drill.sh` compares a **green** stack against the
**live** one on golden ledgers and prints a pass/fail diff report. It only
reads `/ledgers/{seq}`, `/ledgers/{seq}/transactions` and
`/ledgers/{seq}/operations` from both, drops request-time fields (`as_of`,
`flags`) and diffs the rest. A non-200 on either side, an unparsable body, a
`truncated` list or an empty golden list is a failure or refusal, never a pass.

1. **Pick the ledgers** (one sequence per line, `#` comments allowed): the
   upgrade ledger, the one after it, and ledgers holding the new operation or
   XDR shapes. On a test net, take the upgrade ledger from the network's
   announcement, or page `/v1/ledgers?before=<seq>` and read each row's
   `protocol_version` to find where it changes.
2. **Bring up green.** Run the new build (and, for a core bump, the new captive
   core) against a *separate* database set, replay the golden range into it
   (`projector-replay` for projected sources, `ch-rebuild` otherwise), and serve
   it on a private address. Green must never take the public name. One test net
   at a time; first drills use spare test-net VM capacity, not r1.
3. **Run the drill** against live and green:

   ```sh
   LIVE_URL=https://<testnet-api>/v1 GREEN_URL=http://<green-host>:<port>/v1 \
   DRILL_MIN_PROTOCOL=29 bash scripts/ops/protocol-upgrade-drill.sh golden.txt
   ```

4. **Read the report.** Each check prints `PASS`/`FAIL`; a route `FAIL` includes
   the first 40 lines of the diff. Exit code = number of failed checks. Promote
   green only at exit 0 and keep the report with the release notes.
   `scripts/ops/protocol-upgrade-drill-test.sh` proves the drill fails on a
   mismatch, an HTTP error and an old protocol.

**Limit:** a drill needs a live network that has already crossed the change. It
cannot rehearse a mainnet-only amendment such as CAP-0076; rely on the
checklist and a decode-failure watch after the vote.

## Protocol 29 — readiness

Core `29.0.0` reached apt 2026-09-24; the mainnet vote is 2026-10-01.

- [ ] Checklist above, steps 1–4. Step 1 is the table below. Still not
  evidenced in the repo: the SDK arm check (step 2) and the r1 core version
  after the vote.

| Change | Handling |
| --- | --- |
| **No CAP is labelled Protocol 29.** The stellar-protocol [core/README.md](https://github.com/stellar/stellar-protocol/blob/master/core/README.md) lists CAP-0083/85/86 at 28 and CAP-0081/84/87/88 at "TBD"; none at 29. Whether any CAP shipped in P29 is **unconfirmed**. | Nothing to map to decoders. |
| **No XDR change.** `git compare v28.0.1...v29.0.0` in stellar-core touches no `.x` file ([compare](https://github.com/stellar/stellar-core/compare/v28.0.1...v29.0.0)); stellar-horizon [#234](https://github.com/stellar/stellar-horizon/pull/234) states "No XDR change and no SDK bump in Protocol 29". | `go-stellar-sdk v0.7.3` (go.mod) already decodes everything. **No decoder, `ledgerstream` or meta change needed.** |
| Core behaviour changes ([v29.0.0 notes](https://github.com/stellar/stellar-core/releases/tag/v29.0.0)): DEX offer-crossing accuracy fix, pool hops not counted against the limit, lower max message size in protocol 29, over-limit messages dropped early, hash-of-hash optimization removed. | Consensus and overlay behaviour inside core. DEX offer crossing may shift SDEX trade results at the vote ledger; whether it does is **unconfirmed** (the notes give no detail). No code change. |
| Soroban host (custom-section and BrTable cost accounting raising rent fees): reported by a third party only, not confirmed from a primary source. | **Unconfirmed.** Fee fields, not shapes. |

Primary sources read: stellar-core [v29.0.0](https://github.com/stellar/stellar-core/releases/tag/v29.0.0), stellar-rpc [v29.0.0](https://github.com/stellar/stellar-rpc/releases/tag/v29.0.0) ("Support for Protocol 29", no CAPs named), stellar-galexie [#96](https://github.com/stellar/stellar-galexie/pull/96) (bundled core `29.0.0-3589.4eb833373`).

- Mainnet activated P29 at ledger 64717645. Core `29.0.0` is on apt and
  `stellar-core-auto-upgrade` installs a newer apt core with tip verification
  and rollback. galexie is pinned to `galexie-v29.0.0` (2026-10-02, VERSIONS.md).
- [ ] Drill run on one test net (testnet or futurenet), report attached.
  No P29 drill is recorded in the repo or its git history.
- [ ] Watch for decode failures and the stack-version probe after the vote.
  (The earlier "CAP-0076 is mainnet-only" note does not apply: stellar-protocol
  lists CAP-0076 at protocol 24.)

## Protocol 28 "Adapter" — readiness (reviewed 2026-08-26)

Testnet 2026-08-27 17:00 UTC · Mainnet 2026-09-16 17:00 UTC.

| Change | Handling |
| --- | --- |
| **CAP-83** — new `StellarValue` arm `STELLAR_VALUE_EMPTY_TX_SET` (empty-txset ledgers) | `go-stellar-sdk v0.7.2` decodes it. We never switch on `StellarValueType`; an empty-txset ledger reads as a zero-transaction ledger. **No change needed.** |
| **CAP-85** — new `ContractExecutable` arm `CONTRACT_EXECUTABLE_EXTERNAL_REF` | SDK decodes it. All three `ContractExecutableType` switches (`wasm_lake_reader.go`, `wasm_history.go`, `state_snapshot.go`) fall through gracefully: an external-ref contract reports as "unresolved wasm" and isn't tallied as wasm\|sac. **No change needed** (enriching external-ref indexing is optional). |
| **CAP-86** — sparse-map host functions | WASM host functions only; no XDR/decode impact. |
| Validator clock sync (NTP) | We run captive-core only, not a validator. N/A. |

**Test nets:** captive core `28.0.1`, pinned via `stellar_core_version`; SDK
`v0.7.2` on `main` (go.mod now `v0.7.3`).

**Mainnet (r1) — DONE 2026-08-27:**
- Captive core `27.1.0` → **`28.0.1-3508.947aad841.noble`** (protocol 28),
  pinned via `stellar_core_version` in `r1.yml`, applied with
  `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags galexie`
  (`--check --diff` first: changed=6, failed=0).
- **Verified:** `stellar-core version` → `28.0.1 … ledger protocol version: 28`;
  `/etc/stellar/captive-core-galexie.cfg` perms → `stellar:galexie`. The role
  **re-templates the cfg with correct group ownership**; raw `apt install`
  resets it to `stellar:stellar` and galexie (user `galexie`) loses read access
  (the 2026-08-27 raw-apt outage). Always use the `--tags galexie` role apply.
- **Expected during the apply:** galexie restarts ~2× (unit change +
  Restart-galexie handler), and EACH restart re-triggers the ~9-min cold
  catchup. Do NOT manually restart during the catchup: each manual restart
  resets it from scratch (that made the 2026-08-27 incident a 22-min outage).
  One apply, then watch the tip recover.
- `r1.yml` is `.gitignore`d, so the pin is not version-controlled; the
  **running core on r1 is authoritative** (`state: present` never downgrades).
- TODO: confirm the deployed r1 API/indexer binary carries SDK ≥ `v0.7.2` so
  P28 XDR arms decode (mainnet crossed the upgrade ledger 2026-09-16).
