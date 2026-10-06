---
title: Validator Rollout — 1 → 3 Full Validators as one Tier-1 Organisation
last_verified: 2026-10-06
status: accepted for the validator-operations sequence (phases, quorum sets, key ceremony); not started — v1 ships archival-only per [ADR-0004](../../adr/0004-tier1-validator-aspiration.md). Region and replication shape come from [ADR-0050](../../adr/0050-multi-region-ha-architecture.md) and [ha-plan.md](../ha-plan.md).
---

# Validator Rollout — 1 → 3, as one Tier-1 Org

One Stellar Index organisation, three full validators in three regions,
brought up one at a time. The first node runs as a non-voting archival node
so problems surface without multi-node coordination; validators 2 and 3 add
no new shape. Each region is independent (ADR-0050 Model B): standing up a
validator in R2 is "deploy another region", never "join a cluster".

Changing the aspiration itself (for example, never promoting past
archival) needs a superseding ADR; this page cannot downgrade ADR-0004.

---

## 1. What a Tier-1 organisation is

Per SDF's `stellar-docs/docs/validators/tier-1-orgs.mdx`, one organisation
running **≥ 3 full validators** (voting, not watchers), **geographically
separated**, each publishing an **independent history archive**, voting as
one org. Our three validators are one org-vote, share one ceremony
procedure, and are listed once.

## 2. Scope boundary

No pricing or API property depends on validator status. A validator adds an
SCP vote, a public history archive and T1 eligibility; it adds no data to
aggregation, no API capability and no key beyond the validator key. If we
never promote, the API works identically.

---

## 3. Rollout phases

### Phase A — one archival node, non-voting

- `NODE_IS_VALIDATOR=false` on R1, quorum set mirroring SDF's recommended
  set for a non-validating node. Publish our history archive from day one.

**Exit criteria (A → B):**

- [ ] Live and synced for ≥ 7 consecutive days.
- [ ] Galexie and `stellarindex-indexer` ingest from it with zero gaps.
- [ ] Archive cross-checks against SDF and two other T1 orgs show hash
      parity.
- [ ] Memory, catchup duration and NVMe throughput measured against
      [archival-node-spec.md](archival-node-spec.md).

### Phase B — promote to validator, still one node

- Key ceremony (§5); `NODE_IS_VALIDATOR=true`; `NODE_SEED` resolved through
  the HSM signer daemon, never on disk (ADR-0004).
- Publish the public key in `stellarindex.io`'s `stellar.toml` and announce
  on SDF's `#validators` channel. One validator is not a T1 org.

**Exit criteria (B → C):**

- [ ] Voted correctly on 100 % of ledgers for 14 consecutive days.
- [ ] No incident involving the validator key or HSM.
- [ ] Archive cross-check green for 14 days.
- [ ] Rehearsed: HSM failure, validator-key rotation, core upgrade.

### Phase C — validator 2 in R2

Fresh key on a second HSM (never shared key material), same
`NODE_HOME_DOMAIN=stellarindex.io`, and validator 1 added to its org
sub-quorum. Exit criteria as for Phase B.

### Phase D — validator 3 in R3

Same pattern. The org now has three validators with independent archives.

### Phase E — apply for Tier-1 listing

After all three voted correctly with matching archives for ≥ 14 days, open
a PR to `stellar/stellar-docs` adding Stellar Index with the three public
keys and archive URLs. A refusal is not service-affecting; re-apply in 90
days.

### Phase F — steady state

Protocol upgrades one region at a time; yearly key rotation; quarterly HSM
backup audits.

---

## 4. Quorum set shape

- **A and B:** the same set we follow (SDF × 3 plus the T1 orgs, threshold
  67 %); in B we also vote.
- **C onward:** each of our validators nests a `stellarindex.io`
  sub-quorum holding our own validators. At two members it is weaker than a
  three-validator org, which is why the T1 application waits for Phase D.

The TOML for each phase lands in `configs/validators/` when that phase
ships.

---

## 5. Key ceremony

Once per validator, at Phases B, C and D.

**Materials:** a factory-reset YubiHSM 2; an offline ceremony laptop
imaged from a known-good ISO; two operators; a self-recorded witness
camera; pre-printed Shamir share forms.

1. Boot the laptop air-gapped.
2. Generate the ed25519 key on the HSM
   (`yubihsm-shell generate asymmetric ed25519`). Export only the public
   key; the private key never leaves the HSM.
3. Both operators sign off the public key and fingerprint.
4. Split the HSM backup into 3 Shamir shares, threshold 2, on sealed forms;
   store each in a separate safe at a different site.
5. Wipe the laptop; install the HSM in the validator host.
6. Point stellar-core's `NODE_SEED` at the signer daemon
   (`unix:///var/run/stellarindex-signer.sock`).
7. File the public key and ceremony log (no secret material) in
   `configs/validators/<name>/ceremony.txt`.

**HSM failure:** rebuild from any 2 of 3 shares onto a replacement HSM;
target under 48 h. **Suspected compromise:** announce on `#validators`, run
a new ceremony, update `stellar.toml` and quorum-set references.

---

## 6. Open questions

1. **HSM model:** YubiHSM 2, Nitrokey HSM 2 or a cloud KMS. Lead time is
   2–6 weeks (ADR-0004); buy a spare before Phase B so it is not on the
   critical path.

## 7. Launch-day definition of done

- [ ] Phase A complete (one archival node syncing and ingesting).
- [ ] Phase B complete — optional for public launch.
- [ ] Phases C–D queued with hosts ordered.
- [ ] Runbooks for HSM failure, key rotation and quorum-set change reviewed.
- [ ] Monitoring covers archive cross-check and per-validator correctness.

Launch can happen at the end of Phase A; B–F follow over the next months.
