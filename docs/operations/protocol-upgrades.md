---
title: Stellar protocol upgrades
last_verified: 2026-08-27
status: current
---

# Stellar protocol upgrades

Stellar bumps its ledger protocol version periodically (Futurenet leads, then
Testnet, then Mainnet). Each upgrade can add XDR types, operations, or event
shapes an indexer must decode. Two things must be ready **before** a network
crosses the upgrade ledger:

1. **Captive stellar-core ≥ the new protocol.** galexie embeds captive-core to
   replay ledgers; a core that predates the protocol **refuses to sync past the
   upgrade ledger** → ingestion halts. The role installs core with
   `state: present`, which never upgrades an existing binary — so a bump is a
   deliberate `stellar_core_version` pin (below), not automatic.
2. **A `go-stellar-sdk` / `go-xdr` that defines the new XDR types.** An unknown
   union arm fails to unmarshal → the indexer errors on every affected ledger.

## The core-upgrade procedure

`stellar-core-auto-upgrade.timer` does this unattended every 6 hours: when
apt.stellar.org offers a newer core it stops galexie, installs it, restores the
captive-core config group, and waits for galexie to write past its stop tip,
reinstalling the previous version if it does not
(`stellarindex_stellar_core_autoupgrade_failed` pages). Core 29.0.0 reached apt
2026-09-24 with the mainnet P29 vote on 2026-10-01, so a week is the runway to
plan for. The manual procedure below is for a pinned host, a rollback, or a
hold (`touch /var/lib/stellarindex/stellar-core-auto-upgrade.stop`).

```sh
# 1. Pin the target apt version in the region inventory:
#    stellar_core_version: "28.0.1-3508.947aad841.noble"
# 2. Apply (targeted — reinstalls core, restarts galexie onto it):
ansible-playbook -i inventory/<region>.yml playbooks/archival-node.yml --tags galexie
# 3. Confirm: stellar-core version  → >= the new protocol
#    and galexie resumes exporting (no "unsupported ledger version").
```

Do the core bump in a maintenance window a few days AHEAD of the upgrade ledger;
galexie's captive core can run a newer protocol against an older live network
safely (forward-compatible), so there's no downside to being early.

## Readiness checklist (every protocol bump)

Work top to bottom; each line is a yes/no, and a "no" blocks the mainnet window.

1. Read the release notes and CAP list; mark each change as XDR shape, operation,
   event shape or host-function-only.
2. `go-stellar-sdk` on `main` defines every new XDR arm (an unknown union arm
   fails to unmarshal on every affected ledger).
3. Every `switch` over a changed XDR enum falls through gracefully (grep the
   enum name under `internal/`).
4. Captive core ≥ the new protocol on every test net, then on r1 (procedure above).
5. Run the drill below on a test net that has already crossed the upgrade.
6. A decoder, event-schema or feed change found by the drill follows the replay
   rule in [ingest-pipeline.md](../architecture/ingest-pipeline.md#the-replay-decision-rule).
7. Record the result in a section like the ones below.

## Upgrade drill: golden ledgers, green vs live

`scripts/ops/protocol-upgrade-drill.sh` compares a **green** stack against the
**live** one on a list of golden ledgers and prints a pass/fail diff report.
Neither side is touched: it only reads `/ledgers/{seq}`,
`/ledgers/{seq}/transactions` and `/ledgers/{seq}/operations` from both, drops
request-time fields (`as_of`, `flags`) and diffs the rest. A non-200 on either
side, an unparsable body, a `truncated` list or an empty golden list is a failure or a refusal,
never a pass.

1. **Pick the ledgers** (one sequence per line, `#` comments allowed): the
   upgrade ledger, the one after it, and ledgers holding the new operation or
   XDR shapes. On a test net, take the upgrade ledger from the
   network's upgrade announcement, or page `/v1/ledgers?before=<seq>` and read
   each row's `protocol_version` to find where it changes.
2. **Bring up green.** Run the new build (and, for a core bump, the new captive
   core) against a *separate* database set, replay the golden range into it
   (`projector-replay` for projected sources, `ch-rebuild` otherwise), and serve
   it on a private address. Green must never take the public name. One test net
   at a time: a whole-stack blue-green needs a second host, so the first drills
   use the spare capacity of the test-net VMs, not r1.
3. **Run the drill** against live and green:

   ```sh
   LIVE_URL=https://<testnet-api>/v1 GREEN_URL=http://<green-host>:<port>/v1 \
   DRILL_MIN_PROTOCOL=29 bash scripts/ops/protocol-upgrade-drill.sh golden.txt
   ```

4. **Read the report.** Each check prints `PASS`/`FAIL`; a `FAIL` on a route
   includes the first 40 lines of the diff. The exit code is the number of
   failed checks. Promote green only at exit 0, and keep the report with the
   release notes. `scripts/ops/protocol-upgrade-drill-test.sh` proves the
   drill itself fails on a mismatch, an HTTP error and an old protocol.

**Limit:** a drill needs a live network that has already crossed the change. It
cannot rehearse a mainnet-only amendment such as CAP-0076; for those, rely on
the checklist and a decode-failure watch after the vote.

## Protocol 29 — readiness

Core `29.0.0` reached apt 2026-09-24; the mainnet vote is 2026-10-01.

- [ ] Checklist above, steps 1–4. Step 1 for P29 has not been written up here;
  fill the change table (as in the P28 section) when done.
- [ ] Drill run on one test net (testnet or futurenet), report attached.
- [ ] CAP-0076 is mainnet-only and cannot be drilled; watch for decode failures
  and the stack-version probe after the vote instead.

## Protocol 28 "Adapter" — readiness (reviewed 2026-08-26)

Timing: **Testnet 2026-08-27 17:00 UTC · Mainnet 2026-09-16 17:00 UTC**.

Breaking changes and how StellarIndex handles them:

| Change | Handling |
| --- | --- |
| **CAP-83** — new `StellarValue` arm `STELLAR_VALUE_EMPTY_TX_SET` (empty-txset ledgers) | `go-stellar-sdk v0.7.2` decodes it. We never switch on `StellarValueType`; an empty-txset ledger reads as a zero-transaction ledger, already a normal case. **No change needed.** |
| **CAP-85** — new `ContractExecutable` arm `CONTRACT_EXECUTABLE_EXTERNAL_REF` | SDK decodes it. All three `ContractExecutableType` switches (`wasm_lake_reader.go`, `wasm_history.go`, `state_snapshot.go`) fall through gracefully → an external-ref contract reports as "unresolved wasm" / isn't tallied as wasm|sac. No crash. **No change needed** (enriching external-ref indexing is optional, non-blocking). |
| **CAP-86** — sparse-map host functions | WASM host functions only; no XDR/decode impact. |
| Validator clock sync (NTP) | We run captive-core only, not a validator. N/A. |

**Test nets:** ready — captive core `28.0.1` (fresh installs pulled it), now pinned
via `stellar_core_version`; SDK `v0.7.2` on `main`.

**Mainnet (r1) — DONE 2026-08-27** (was: action required before 2026-09-16):
- r1's captive core `27.1.0` → **`28.0.1-3508.947aad841.noble`** (protocol 28),
  pinned via `stellar_core_version` in `r1.yml` and applied with
  `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags galexie`.
  Dry-run first (`--check --diff`, clean: changed=6, failed=0), then applied.
- **Verified post-apply:** `stellar-core version` → `28.0.1 … ledger protocol
  version: 28`; `/etc/stellar/captive-core-galexie.cfg` perms → `stellar:galexie`.
  The role **re-templates the cfg with correct group ownership**, which is why
  the role path is safe where raw `apt install` is not — raw apt resets the cfg
  to `stellar:stellar` and galexie (user `galexie`) loses read access (the
  2026-08-27 raw-apt outage). Always use the `--tags galexie` role apply.
- **Expected during the apply:** galexie restarts ~2× (systemd unit change +
  the Restart-galexie handler), and EACH restart re-triggers the ~9-min cold
  catchup. This is normal — do NOT manually restart during the catchup (each
  manual restart resets the catchup from scratch; that turned the 2026-08-27
  incident into a 22-min outage). One apply, then watch the tip recover.
- `r1.yml` is `.gitignore`d, so the pin is not version-controlled — the
  **running core on r1 is authoritative** (`state: present` never downgrades an
  existing binary, so a future apply without the pin won't revert it).
- TODO: confirm the r1 API/indexer binary (currently `v0.44.9`, built from
  `main`) carries SDK `v0.7.2` so the new P28 XDR arms decode once mainnet
  crosses the upgrade ledger on 2026-09-16.
