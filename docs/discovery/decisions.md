# Firm Decisions

A running log of decisions that shape Phase 1 audits and Phase 2
architecture. Each decision is **binding unless explicitly revisited**
with a dated note here. Anything tentative belongs in an audit doc's
"Open items" section, not here.

Format: each decision gets a date, a one-line verdict, and a short
justification so future-us can tell whether the reasoning still holds.

---

## 2026-04-22 — Horizon is deprecated; we will not use it

**Verdict:** ❌ **Horizon is not a component of the CTX Rates architecture.**
We do not ingest from Horizon, we do not run Horizon, and we do not
proxy any client request to Horizon.

**Why:** User directive, captured during Phase 1 discovery. Aligns with
the broader Stellar ecosystem direction — `stellar/go` (the monorepo
that housed Horizon's canonical code path) was archived on 2025-12-16,
Horizon has moved to its own dedicated repo, and the CDP / Galexie /
Ingest-SDK / stellar-rpc stack is now the supported ingestion path for
new builders.

**How to apply:**

- Any audit doc referencing Horizon must mark it explicitly as "not in
  our architecture" and do not describe any integration work against it.
- Data sources for us are: **Galexie + our own stellar-rpc + our own
  captive-core + Reflector / Redstone on-chain reads + external HTTP
  feeds (CEX, FX, reference)**. Horizon is not on that list.
- If a downstream consumer of our API historically relies on Horizon
  conventions, we document our endpoint differences but we do not
  re-implement Horizon-compat on top.
- The hosted Horizon API operated by SDF (horizon.stellar.org) may
  still be **mentioned** in discovery notes when relevant for context
  (e.g. a third-party indexer uses it), but we treat such indexers as
  candidates for replacement, not for adoption.

**Revisit if:** SDF were to reverse course and publicly re-position
Horizon as the primary ingest path; or if a critical protocol feature
ships only through Horizon first. Neither is the case at time of
writing.

---

## 2026-04-22 — Self-hosted storage uses MinIO (or S3-compat), not local filesystem

**Verdict:** 🧪 **Working decision** — for any pipeline that writes
`LedgerCloseMeta` files (Galexie, possibly our own exporters), the
production target is an S3-compatible object store we run ourselves.

**Why:** Verified from `go-stellar-sdk/support/datastore/filesystem.go`
that the local-filesystem datastore:

1. Silently ignores the 9-key per-object metadata Galexie attaches
   (`start-ledger`, `end-ledger`, `start-ledger-close-time`,
   `end-ledger-close-time`, `protocol-version`, `core-version`,
   `network-passphrase`, `compression-type`, `version`).
2. Warns in its own docstring that "concurrent writes to the same file
   path are not safe and may result in data corruption."
3. Is documented by SDF as intended for dev/test only.

MinIO on our colo hardware gives us the S3 API (which SDF do support
for production via `endpoint_url`), full metadata support, atomic
conditional puts, and multi-machine access — at zero AWS/GCP bill.

**How to apply:**

- Galexie `datastore_config.type = "S3"` with `endpoint_url` pointing at
  our MinIO cluster.
- Credentials via env vars standard AWS SDK supports.
- Keep filesystem mode as a **local dev convenience only**; do not use
  it in any staging or prod deployment.

**Revisit if:** We hit a hard MinIO scaling or consistency issue in
load testing; or if operational complexity of running MinIO clusters
outweighs the benefits vs. a managed S3. Both unlikely given our
existing baremetal operating experience.

---

## 2026-04-22 — i128 / u128 must survive end-to-end with no truncation

**Verdict:** ✅ **All token amounts, balances, reserves, swap amounts,
and any other Soroban-originating numeric quantity are carried as full
128-bit values end-to-end.** Never as `int64` / `uint64` / `float64`
in storage, in code, or on the wire.

**Why:** Soroban stores token quantities as `i128` (two 64-bit words,
`hi` and `lo`). At 7 decimals, anything above ~922 billion tokens (i64
max divided by 10⁷) overflows `int64`. A real production incident
(shared by the user, 2026-04-22) confirmed the blast radius:

> "KALIEN balance is stored as `i128` (two 64-bit words). The actual
> value 40,000,005,972,900,000,000 exceeds i64 max (~9.2×10¹⁸), so it's
> stored with `high=2, low=3106517825480896768`. Stellar Expert is
> only reading the low 64 bits, displaying 310,651,782,548.0896768
> instead of the real 4,000,000,597,290. … Both the SAC `balance()`
> call and the pair's `get_reserves()` agree on ~4T KALIEN and ~40M
> KALE — exactly matching the original deposit."

Stellar Expert's own response, verbatim:

> "Our analytics DB doesn't support 128-bit integers natively. Before
> Soroban all balances were stored as `int64`, and that's what's
> stored in our db. … The team has proposed a few different solutions,
> but it will take a few months to address this problem."

We are not going to ship that bug. If the largest Stellar explorer
can't render a real on-chain balance, anyone relying on us for
portfolio / pricing / liquidity data is going to get wrong numbers
unless we get this right from day one.

We have also **verified** one instance of exactly this bug in
withObsrvr's `cdp-pipeline-workflow/processor/processor_soroswap_router.go`
(lines 155–177): `entry.Val.I128.Lo` is read, `.Hi` is ignored. Any
swap on a pair where either side > ~922 B tokens is silently
mis-recorded. This is one more reason not to fork that repo (see
[data-sources/withobsrvr-cdp-pipeline-workflow.md](data-sources/withobsrvr-cdp-pipeline-workflow.md)).

**How to apply:**

### Storage (TimescaleDB / PostgreSQL)

- All amount columns: `NUMERIC` (arbitrary precision). Never `BIGINT`,
  never `DOUBLE PRECISION`.
- Reserves, supplies, swap inputs/outputs, offer amounts, trade
  amounts, balances, TTL durations that originate from `i128` — all
  `NUMERIC`.
- If we compute ratios (prices) they are stored as `NUMERIC(precision,
  scale)` with enough room (e.g. `NUMERIC(38, 18)`), not as `DOUBLE`.

### Go in-memory types

- Parse `xdr.Int128Parts` / `xdr.UInt128Parts` via a helper that
  returns `*big.Int` (or `decimal.Decimal` with precision ≥ 38). The
  two's-complement sign bit on `Hi` must be handled correctly for
  `Int128Parts`. See `stellar-extract/scval_converter.go` —
  `int128ToString()` is a good reference implementation. Lift it or
  depend on it directly.
- Price math uses `big.Rat` or `shopspring/decimal` with explicit
  precision rules, not `float64`.

### Wire format (API / JSON)

- JSON numbers are IEEE 754 doubles — **53 bits of precision**. Any
  amount > 2⁵³ ≈ 9.007×10¹⁵ loses bits in any JS client. Therefore:
- All numeric amounts in API responses are **strings**, not JSON
  numbers. This mirrors the Horizon convention.
- Prices may be numbers *only if* we clamp to a precision we can
  promise fits in float64. Safer to always stringify.

### Regression tests

- One fixture per i128-corner-case: exactly at i64 max, one token over
  i64 max, two-word high-bit set, two-word negative (i128 specifically).
- An integration test that round-trips a Soroban swap event with
  amounts > 2⁶⁴ through our full pipeline and verifies the exact
  recovered value.

**Revisit if:** Never. This is a correctness invariant, not an
ergonomic trade-off. Any code that holds an amount in `int64` or
`float64` is a bug, full stop.

---

## 2026-04-22 — We aim to become a Tier 1 org running three full archival validators

**Verdict:** ✅ **Strategic direction.** Phase-1 deployment is a watcher
(non-validating) stellar-core + Galexie + stellar-rpc trio. Post-launch,
we stand up three geographically-separated Full Validators and pursue
Tier 1 inclusion.

**Why:** User directive, 2026-04-22. Consistent with CTX Rates'
existing network-participation posture and with our product story —
being *both* the canonical pricing API *and* a contributing
infrastructure operator strengthens our position materially.

SDF's `tier-1-orgs.mdx` spells out the shape:

- Three Full Validators per org ("Tier 1"), geographically dispersed,
  each publishing a separate public history archive.
- Quorum set coordinated with other Tier 1 orgs; `HIGH` quality
  declaration in the config.
- SEP-20 self-verification via home-domain `stellar.toml`.
- Active participation in `#validators` (Discord / Keybase
  `stellar.public`) and the `stellar-validators` mailing list.
- Tier 1 membership is *emergent* — not granted by SDF. Enough other
  orgs must include us in their quorum sets.

**How to apply:**

### Phase 1 (first 10 weeks post-award, scope of this delivery)

- **One watcher / non-validating** stellar-core on our co-lo R640.
- **One stellar-rpc** on the same (or nearby) machine.
- **One Galexie** exporting to our MinIO data lake.
- **No validator key.** We do not sign ledgers in this phase.
- We do, however, already run everything *capable* of becoming a
  validator: correct versions, correct configs, correct hardware class.

### Phase 2 (post-launch, 3–6 months out)

- Stand up two **geographically separate** sites (beyond Vancouver).
- Each site runs its own stellar-core + history archive.
- Configure the first validator key + quorum set, join testnet
  validator pool first.
- Promote to pubnet Basic Validator, observe uptime and quorum health.

### Phase 3 (6–12 months out)

- Promote all three to **Full Validator** (add history archive
  publishing).
- Publish SEP-20 stellar.toml on our root domain naming the three
  validators.
- Join the validators email list + Discord #validators, coordinate
  with existing Tier 1 operators.
- Begin proving reliability: target 99.95 %+ uptime per node, respond
  promptly to protocol-upgrade votes.

### Operational standards we commit to from day one

- **Validator keys never on disk unencrypted.** HSM or equivalent
  airgapped signing before any production seed is generated. Decide
  HSM vendor / pattern before Phase 2 cutover.
- **Three archives, not one.** Don't cross-mount. Each validator
  writes to its own blob store (MinIO-behind-nginx on its own box, or
  distinct S3 buckets).
- **No shared data center** for all three. At least two distinct
  regions, ideally three.
- **Protocol-upgrade voting** is a named human responsibility on-call,
  not a cron job.

**Revisit if:** Tier 1 acceptance proves to have material operational
costs we hadn't scoped (e.g. mandatory 24/7 staffed NOC); or if the
SDF changes the quorum-set mechanics materially. Running three Full
Validators is not revisitable based on cost alone — it's the *product*
story as well as a network contribution.

**Related audits:** [data-sources/archival-nodes.md](data-sources/archival-nodes.md)
captures the hardware, ports, catchup modes, tier-1 mechanics in detail.

---
