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
