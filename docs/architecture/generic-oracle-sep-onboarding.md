---
title: Generic oracle + SEP onboarding — investigation and design
last_verified: 2026-07-10
status: investigation
---

# Generic oracle + SEP onboarding

**Goal (stated by the operator):** "be able to pick up new oracles and
any other SEPs that contracts/protocols are using, so we're properly
able to interpret and translate them." This note investigates what
that capability would actually take, grounded in (1) a sweep of the
SEP registry for on-chain-relevant standards, (2) a live census of the
ClickHouse raw lake for SEP-40-shaped contracts we don't ingest today,
and (3) a design sketch evaluated against this repo's binding
constraints (ADR-0035 identity gating, the every-event principle,
Galexie-not-RPC ingest).

This is an investigation deliverable, not an implementation. It does
**not** edit [ADR-0045](../adr/0045-sep40-oracle-read-adapter.md) —
ADR-0045 (Accepted 2026-07-06) already covers the narrower "SEP-40
state-read adapter" question and its deferral is reaffirmed, not
overturned, by what follows. This note broadens the question ADR-0045
answered (SEP-40 reads specifically) to the operator's actual ask
(any SEP a protocol might implement) and adds fresh lake evidence.

## 1. SEP inventory — which SEPs standardize an on-chain interface

Full sweep of `stellar/stellar-protocol/ecosystem` (59 SEPs: 17
Active, 7 Final, 26 Draft, 3 Abandoned, as of 2026-07-10). Most SEPs
are anchor/wallet/off-chain protocols (SEP-6/24 deposit-withdrawal,
SEP-10 auth, SEP-12 KYC, SEP-31 cross-border, SEP-38 RFQ, …) — those
are out of scope for a *decoder*; they never appear as Soroban
contract events or InvokeContract calls. The ones that DO define an
on-chain Soroban contract surface:

| SEP | Title | Status | Defines a concrete on-chain interface? | Generically decodable? |
| --- | --- | --- | --- | --- |
| **SEP-40** | Oracle Consumer Interface | Draft | Yes — `lastprice`/`price`/`prices`/`x_last_price`/`assets`/`base`/`decimals`/`resolution` reads. **No events mandated.** | Partially. Method *names* are stable, but it's a **read** interface (contract storage), not an event schema — see §2/§3. Optional methods (`x_last_price`, `resolution`) are NOT universal: Reflector v3 ships none of them (CLAUDE.md, confirmed again in the reflector README Q2). A generic reader can only safely assume `lastprice`/`prices`/`assets`/`decimals` exist. |
| **SEP-41** | Soroban Token Interface | Draft | Yes — `transfer`/`mint`/`burn`/`clawback`, each with a stable topic[0] symbol. | **Yes, for detection.** `internal/canonical/discovery` already sniffs topic[0] against the 4 SEP-41 symbols and records sightings — this is the existing proof that "detect a SEP-shaped contract generically" works. Full *decode* is NOT generic: CAP-67 (post-P23) added a 4th `sep0011_asset` topic and shifted `mint`/`clawback` argument positions (legacy SAC vs CAP-67 — CLAUDE.md), so even this best-case SEP needs shape-aware, per-revision decode logic, not one universal decoder. |
| **SEP-50** | Non-Fungible Tokens | Draft (2025-03-10) | Yes — `NonFungibleToken` trait: `balance()`/`owner_of()`/`transfer()`/`transfer_from()`/`approve()`/`approve_for_all()`/`name()`/`symbol()`/`token_uri()`; event topics `transfer`/`approve`/`approve_for_all`. | Same shape as SEP-41: detectable by topic[0] (with collision risk — `transfer`/`approve` are generic words other protocols also use), not yet observed on our mainnet lake (see §2). No decoder exists; out of current scope but the closest future analogue to SEP-41 discovery. |
| **SEP-56** | Tokenized Vault Standard | Draft | Yes — `TokenizedVault` trait: `deposit(assets, receiver, from, operator)`/`redeem(shares, receiver, owner, operator)`/preview & conversion methods; `Deposit`/`Withdraw` events. | Symbol collision risk is HIGH — `deposit`/`withdraw` are already used by Blend (lending) and other protocols with different semantics and different topic arity. Directly relevant to DeFindex (see below) but not safely generic on topic[0] alone. |
| **SEP-46** | Contract Meta | Active | No — describes the contract's *build*, embedded in a Wasm custom section, read off-chain from the binary, not observable on the event/call stream. | No. Feeds tooling (docs/operations/wasm-audits), not ingest. |
| **SEP-47** | Contract Interface Discovery | Draft | No on-chain mechanism — a contract *self-declares* which SEPs it implements via a `sep` Wasm-meta entry. Per the spec itself: "not available to contracts to inspect" and unverified. | No — and importantly, MUST NOT be trusted for gating. A contract can falsely claim `sep=40`. Useful only as an operator-triage hint (see §3), never as an attribution source. |
| **SEP-48** | Contract Interface Specification | Active | Descriptive only — `contractspecv0` Wasm custom section lists a contract's actual function/event signatures, extracted at build time. Per spec: "does not support a contract claiming to implement any specific interface" and is unenforced/self-declared. | No, same caveat as SEP-47 but with more useful detail (real signatures, not just a SEP-number claim) — a legitimate input to a *human* WASM audit (`docs/operations/wasm-audits/`), never to automated attribution. |
| SEP-49 | Upgradeable Contracts | Draft | Notable non-finding: it explicitly declines to define an application event for upgrades, instead pointing at the Soroban host's automatic **system** event on `update_contract`. Tangential to oracle onboarding, but directly relevant to CLAUDE.md's "Soroban DeFi contracts upgrade in place" trap: our own capture path (`internal/dispatcher/census.go::captureEligible`, mirrored by `sorobanevents.Capture`) explicitly requires `ContractEventTypeContract` and drops `ContractEventTypeSystem` — so even if we wanted to detect upgrades generically off this host system event, we do not currently capture it. Flagged for a future note; out of scope here. | N/A |
| SEP-39 | NFT interop recommendations | Active | No — SEP-1 `stellar.toml` metadata fields only (`url`/`url_sha256`), explicitly "not a standard." | No |
| SEP-8 | Regulated Assets | Final | No — an off-chain approval-server protocol over classic authorization flags; the spec never mentions Soroban. | No |
| SEP-45, SEP-57 | Contract-account web auth; T-REX regulated-exchange tokens | Draft | Plausible future on-chain surfaces (auth flows, compliance-gated transfers) but niche/unconfirmed on Stellar mainnet today; not investigated further here — flag for a future sweep if either shows real deployment. | Unknown |

**Verdict:** exactly two SEPs are load-bearing for a *generic on-chain
interpretation* capability today — **SEP-40** (oracles, the
operator's direct ask) and **SEP-41** (tokens, already generically
*detected*, though not generically *decoded*, via
`internal/canonical/discovery`). **SEP-50** (NFTs) and **SEP-56**
(vaults) are the next-most-plausible future analogues if/when real
mainnet deployments appear — they're structurally identical to the
SEP-41 case (stable method names, real events, but topic-symbol
collision risk and revision drift). SEP-46/47/48's self-declared
metadata is corroborated as **untrustworthy for attribution by their
own spec text** ("not available to contracts to inspect" / "does not
support a contract claiming to implement any specific interface") —
this is the same failure mode CLAUDE.md already documents twice from
direct experience: Reflector v3 lacking the documented `twap`/`x_*`
methods, and DeFindex's decoder originally targeting a schema
(`paltalabs/defindex` tag `1.0.0`) that "mainnet never deployed"
(`internal/sources/defindex/README.md`). **Standardization of method
names does not imply standardization of deployed behavior** — every
generic-interface design below has to route around that fact, not
around it.

## 2. Lake census — are we missing a live SEP-40 oracle today?

### Methodology

Queried r1's ClickHouse `stellar.contract_events` table directly over
the HTTP interface (`:8123`, read-only `SELECT`), via file+scp SQL —
no operator binary run, no write path touched. Table stats at query
time: **12,393,496,593 rows** (`system.parts` sum, active parts only),
ledger range **2 → 63,407,342** (full history since genesis to
2026-07-10 02:11 UTC).

`topic_0_sym` is a decoded plaintext column (populated at ingest —
see `internal/storage/clickhouse/recognition.go`'s `DistinctTopicShapes`,
the same substrate the ADR-0033 recognition-gap scan uses), so a
`WHERE topic_0_sym IN (...)` scan is a cheap single-narrow-column read
even at this row count — no wide XDR columns touched. Full,
unwindowed query against 28 oracle-suggestive symbols (`price`,
`prices`, `lastprice`, `last_price`, `x_last_price`, `set_price`,
`update_price`, `price_update`, `new_price`, `oracle`/`Oracle`/`ORACLE`,
`feed`, `PriceData`, `resolution`, `write_prices`, `relay`,
`force_relay`, `REFLECTOR`, `REDSTONE`, `rate`, `rates`, `set_rate`,
`symbol_rates`, `StandardReference`, `update`, `base`, `decimals`,
`assets`), grouped by `(contract_id, topic_0_sym)`, ran in 2m06s under
the same bounded settings production code uses for this class of scan
(`max_threads=2, max_memory_usage=8GiB`). Follow-up point queries
pulled representative event bodies (`topics_xdr`/`data_xdr`) for every
candidate contract not already known, to distinguish real oracle
shapes from false positives. All scratch SQL files were removed from
r1's `/tmp` after use.

**Known contract IDs excluded from "candidate" status** (already
ingested by their own decoder): Reflector DEX
`CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M`, Reflector
CEX `CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN`,
Reflector FX `CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC`,
RedStone Adapter `CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG`,
Band StandardReference `CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M`
(the census correctly found zero events for Band's contract ID or for
the `relay`/`force_relay`/`StandardReference` symbols anywhere in the
lake — independent confirmation of the README's "Band emits zero
events" claim from a different angle than the original 2026-04-22/24
source-code grep).

### Candidate table

All contract IDs the scan surfaced that are NOT one of the five known
oracle contracts above, with what inspection of a representative
event actually showed:

| Contract ID | Topic(s) seen | Event count | First seen (UTC) | Last seen (UTC) | What it actually is |
| --- | --- | --- | --- | --- | --- |
| `CCWKKEQTMGBNLHDKSYWFOA4IFFR2GT6FRYSHIXQQGNVB64AQHCFXLL4S` | `update` | 2,237 | 2026-02-15 | 2026-07-08 (**still active**, 2 days before this census) | **Not an oracle.** Body decodes to `(doc_id: String, ipfs_cid: String, u64, bytes32)`, e.g. `doc_id="DFID-BEEF-BR-2026-001159-207711"` and an IPFS CID `Qm...`. A supply-chain / certification anchor (Brazilian beef traceability, by the naming), unrelated to price data. Highest-volume, most-recent candidate the naive `topic_0_sym` filter produced — and a clean false positive. Included to show the discovery technique's precision limits (§3 design implications). |
| `CAHDGXF64LG4PA45PPCDFQYRYWH3X33G7JJFGJTKMFMQIFUKNBONLGHD` | `update` | 250 | 2025-11-18 | 2025-11-25 (dormant ~7.5 mo) | Same family as above — IDs like `BR3013976106920` + IPFS CIDs. Not an oracle. |
| `CDFMV3EI2FEGKHQYZXFSKPBEXO2MXKRMQRXK5SM4DVWWHMGEJTH6JVK2` | `update` | 151 | 2025-05-14 | 2025-05-23 (dormant ~14 mo) | Informal price-string publisher — body is a raw **string**, sometimes bare (`"2613.91"`), sometimes `"{BTC/USD: 10237830000000}"` — not a typed `PriceData`, not SEP-40 shaped. Short-lived, abandoned. |
| 11 distinct contracts (`CAWGFI3C…`, `CBXYAETT7…`, `CBS4NRL3…`, `CACB35527…`, `CA7WWEYQ…`, `CDFDN4XHJ…`, `CCJTHJHLA…`, `CBXKEJQLS…`, `CC6YGNNNK…`, `CAC4VDKR7…`, `CCCDKLH3D…`) | `REDSTONE` | 2–4 each (27 total) | 2025-09-08 | 2026-05-13 | **RedStone-Adapter-shaped, but not the production Adapter.** Sampled exemplar (`CAWGFI3C…`) decodes to the exact documented Adapter body — `Map{updated_feeds: Vec<Map{package_timestamp, price, write_timestamp}>, updater}` — but from a contract ID that is not `CA526Y2N…`. Every one of the 11 fires 2–4 times then goes permanently dark: the signature of a one-off test deployment of RedStone's open-source Adapter code (`redstone-finance/redstone-public-contracts`, referenced in `internal/sources/redstone/README.md`), not a second production feed. |
| `CB7R2RMTDXJBZUNI55ISXCRBFRP77IJMO7UKQIYBSJH5KNTDQT6HWSLG` + `CBZDVCNBYFUHP4DUDAEYU7BRTRK2UF4VT7L5WAUFWNNERHTFU7AIFLKQ` | `price_update` | 1 each | 2026-06-04 10:31 | 2026-06-04 12:05 (~1.5h apart, both single-shot) | The closest genuine SEP-40-*adjacent* shape found: `Map{price_num, price_den}` — a rational price, structurally reasonable, but fired exactly once per contract by two different contract IDs 1.5 hours apart. Reads as a tutorial/demo pair, not infrastructure. |
| `CCHRZE2K5TCERZLDO5IXDUWUKLRPVE72DI3TDF2RP6EQKEW6BNOMQRMU` + `CCSRDUCAQ2SWUBC35BHADMWACAOKNJDHKHVAK2H37RGTZA342ZMGLC7U` | `asset` then `oracle` | 2–3 each | 2026-03-18 (contract 2) / 2026-06-15 (contract 1) | 2026-03-19 / 2026-06-16 | Same developer, two deployments ~3 months apart (both register the **identical** two Stellar-asset pubkeys via `asset`/`added`/`enabled` events before emitting 1–2 `oracle`-topic events). Looks like an iterative small/hobby oracle build. No sustained cadence in either deployment. |

### Verdict

**No currently-active, sustained SEP-40-shaped oracle exists in the
lake that we don't already ingest.** The highest-volume, most-recent
candidate (2,237 events, active through the day before this census)
is conclusively not an oracle on inspection. Every genuinely
oracle-shaped candidate found is either a dead test deployment of
RedStone's own open-source code, or a single-shot demo/tutorial
contract with no sustained cadence. **This reaffirms ADR-0045's
"defer — no concrete target exists" conclusion with fresh evidence as
of 2026-07-10**, not just the 2026-07-06 reasoning it shipped with.

**Scope caveat, stated plainly:** this census can only see
**event-emitting** candidates, because `topic_0_sym` is the only
plaintext-searchable column the raw lake exposes for this kind of
sweep. `stellar.operations.body_xdr` (where an event-less oracle's
`InvokeHostFunction` function name like Band's `relay` lives) is raw
XDR with no plaintext function-name column anywhere in ClickHouse —
there is no SQL-only way to search for "which contracts are being
called with a SEP-40-shaped function name." A second Band-alike
oracle that emits zero events, exactly like Band itself, is
**structurally invisible** to this technique and to every other
census mechanism in the repo today (see §3 gap analysis). Given Band
is precedent that this pattern happens on Stellar, that blind spot is
worth closing independent of whether this particular census found
anything in it.

## 3. Design options

Three shapes, each evaluated against the binding constraints: ADR-0035
contract-identity gating (fail-closed, never topic-only attribution),
the every-event principle (CLAUDE.md: never ship a partial-event
decoder), and Galexie-not-RPC ingest (ADR-0001 — no `stellar-rpc` in
the production path).

### (a) Generic SEP-40 poll/read adapter — the ADR-0045 original shape

A source that, for a **curated** list of oracle contracts, reads
`lastprice`/`prices`/`assets`/`decimals` from contract storage at
query time or via a `LedgerEntryChangeDecoder` on the oracle's
`PriceData` storage entries — riding [ADR-0039](0039-soroban-contract-state-reader.md)'s
lake-native state reader (read-time decode, no backfill worker), the
same substrate Blend pool state already uses.

- **Can capture:** any registered SEP-40 oracle, including
  event-less ones (state reads don't need an event to trigger on —
  they need either a periodic snapshot or an entry-change delta,
  neither of which requires the emitter to publish anything).
- **Cannot capture / open problems:** no event-driven trigger exists
  for a pure-storage oracle without either (1) a periodic poll
  cadence (adds latency + lake read cost, and needs a schedule
  independent of ledger close) or (2) a `LedgerEntryChangeDecoder`
  keyed to the oracle's exact storage key layout — which is
  contract-specific and currently undocumented for any un-ingested
  oracle, because none exists to document against (§2). Building the
  storage-key logic blind repeats the DeFindex tag-1.0.0-vs-mainnet
  mistake. Also needs its own completeness model — "did we read every
  state change" is a different claim than ADR-0033's event-coverage
  claim, and isn't designed yet.
- **Constraint fit:** compliant with Galexie-not-RPC (reads the lake,
  not `stellar-rpc`) and with ADR-0035 IF the oracle registry is
  contract-identity-gated (curated list, fail-closed for anything not
  seeded) rather than "any contract that answers `lastprice()`."
- **Status:** this is exactly what ADR-0045 already scoped and
  deferred. §2's census gives no reason to reverse that — there is
  still no concrete integration target to design the storage-key /
  completeness pieces against.

### (b) SEP-shaped discovery pipeline — extend the existing sniffer

Generalize `internal/canonical/discovery` (today: SEP-41 topic-symbol
sniffing → `discovered_assets` table → `stellarindex-ops discovery`
CLI) to also flag SEP-40-shaped (and, later, SEP-50/SEP-56-shaped)
**candidates** for operator review, with fast onboarding via the
existing per-source decoder framework once a candidate is confirmed
real. Two concrete gaps this closes, both evidenced above:

1. **Event-shaped discovery** — the manual §2 census (write SQL,
   scp to r1, eyeball results) is exactly the workflow
   `discovery.Sniff` already automates for SEP-41. Extending
   `Sniff`'s symbol classification (or adding a parallel
   `SniffOracle`) to record `(contract_id, topic_0_sym, ledger)` hits
   for an oracle-suggestive symbol set makes this continuous instead
   of an ad hoc investigation, with the SAME "record a sighting, never
   attribute a price" discipline `discovered_assets` already has —
   this is the load-bearing property, not a new one: discovery has
   never fed price/trade data, only an operator worklist, and that
   must not change.
2. **Event-less discovery — a genuine gap, not just a nice-to-have.**
   `dispatchOne`'s discovery hook only runs on the event path
   (`internal/dispatcher/dispatcher.go` — confirmed by reading the
   call sites: `discoverySink.Push` is called once, inside
   `dispatchOne`; the `ContractCallContext` path that feeds
   `ContractCallDecoder`s, used by Band today, has no equivalent
   hook). Band proves event-less oracles are a real Stellar pattern.
   A second Band-alike is invisible to §2's census AND to today's
   discovery mechanism. Closing this means adding a symmetric
   discovery hook on the `ContractCallContext` path: sniff
   `(contract_id, function_name)` against a small oracle-suggestive
   allow-list (`lastprice`, `price`, `prices`, `relay`,
   `force_relay`, `write_prices`, `x_last_price`) and record hits the
   same way. This is new code, but small and precedented — it is the
   *same* pattern already proven for events, moved one seam over.

Both halves are pure observation: **record a sighting, do not decode,
do not attribute, do not emit `canonical.OracleUpdate`.** Fail-closed
by construction — a discovered contract sits in an
`discovered_assets`-shaped table until an operator confirms it's real
(exactly the false-positive rate §2 demonstrated: 3 of the ~7 distinct
candidate patterns found were false positives on a first look, and
the review step is precisely what catches that). Once confirmed, the
existing five/six-file on-chain-source recipe
(`docs/contributing/add-onchain-source.md`) is the onboarding path —
same contract-identity gating discipline as reflector/redstone/band,
same every-event completeness requirement, no shortcuts.

SEP-47/48's self-declared metadata (§1) is a reasonable **enrichment**
here, not a trust source: when a discovery hit needs operator triage,
pulling the candidate's `contractspecv0` function list (already
exposed by WASM-audit tooling) gives a faster first read on "does this
look like `lastprice`/`prices`/`assets`" without granting it any
attribution authority — the operator still verifies against real
ledger behavior, per the DeFindex/Reflector-v3 lesson in §1.

- **Constraint fit:** fully compliant — no RPC (runs off the same
  dispatcher/lake stream), no bypass of ADR-0035 (discovery ≠
  attribution, by the same design as today's SEP-41 sniffer), and
  every-event stays intact because discovery never partially decodes
  — it either records a sighting or a real decoder handles the whole
  schema once onboarded.
- **Cost:** small. The event-path half is a copy of an existing,
  production-proven pattern. The ContractCall-path half is new but
  narrow — one new hook + one new sniffer function, no new storage
  shape (reuse `discovered_assets` or a sibling table with a
  `discovery_path` column to distinguish event-sourced from
  call-sourced hits).

### (c) Full generic decode-by-interface

A decoder that, given a contract self-declaring (SEP-47) or exposing
(SEP-48) a SEP-40/41/50/56 interface, automatically decodes its
events/state into canonical types without a human-written,
per-protocol decoder package.

- **Rejected.** Every piece of evidence in this note argues against
  it directly:
  - SEP-47 and SEP-48 are **self-declared and explicitly unverified**
    by their own spec text — a malicious or merely careless contract
    can claim an interface it doesn't correctly implement. Gating
    ingest on a self-declaration is the opposite of ADR-0035's trust
    model (factory-anchored, never topic/claim-based).
  - Even genuine implementations drift from the documented interface:
    Reflector v3 (real, trusted, production) lacks `twap`/`x_*`
    despite being "SEP-40-compliant"; DeFindex's real mainnet
    contracts don't match the schema the upstream repo's own tagged
    release implied. A generic decoder trusting the *declared* shape
    over *verified on-chain* behavior would repeat both mistakes
    mechanically, at the scale of every future contract, instead of
    catching them once per protocol the way a human-audited decoder
    does.
  - It also fights the every-event principle: a truly generic decoder
    either handles every field of every declared interface correctly
    (which is exactly the WASM-version-aware, per-revision audit work
    `docs/operations/wasm-audits/` already does by hand, just
    automated badly) or silently produces partial/wrong data at
    volume — worse than not decoding at all.

## 4. Recommendation

**Build (b), not (a) or (c).** Rationale, tying back to both
deliverables:

- §2's census found **zero** live, sustained, un-ingested SEP-40
  oracles — so (a)'s hardest problems (storage-key layout,
  state-read completeness) still have no concrete target to design
  against, exactly as ADR-0045 said. Building it now would still be
  speculative infrastructure. **No change to ADR-0045's deferral.**
- §2 also demonstrated, empirically, that discovering a NEW protocol
  today is a manual, ad hoc, SQL-writing exercise — the exact
  workflow this note just performed by hand. That's a real capability
  gap the operator's stated goal names directly ("pick up new
  oracles... so we're properly able to interpret and translate
  them") and it's inexpensive to close: `internal/canonical/discovery`
  already IS the pattern, proven in production for SEP-41 since its
  original ship date, needing only (i) a broader/parallel symbol set
  for oracle-shaped events and (ii) the missing symmetric hook on the
  `ContractCallContext` path to stop being blind to Band-alikes.
- (c) is not merely "not needed yet" — it's actively contraindicated
  by this repo's own incident history (Reflector v3 docs vs. reality,
  DeFindex tag-1.0.0 vs. mainnet). Building it would institutionalize
  the exact failure mode two prior audits caught by hand.

Once (b) is live and flags a genuine candidate (an event pattern or
ContractCall pattern that survives operator review — sustained
cadence, correct SEP-40 method names, real asset registrations, not a
one-off test or a false positive like the beef-traceability contract
in §2), onboarding it is NOT a new architectural problem: it's the
existing `add-onchain-source` recipe, same six files, same
contract-identity gating, same every-event discipline reflector /
redstone / band already went through. (b) shortens *time to notice*;
it deliberately does not shortcut *how carefully we onboard*.

## References

- [ADR-0045](../adr/0045-sep40-oracle-read-adapter.md) — SEP-40 read
  adapter decision (Accepted, deferred); this note reaffirms it with
  fresh 2026-07-10 lake evidence and broadens scope to SEPs generally.
- [sep40-oracle-read-adapter.md](sep40-oracle-read-adapter.md) — the
  original investigation this note builds on for the state-read
  substrate details.
- [ADR-0039](../adr/0039-soroban-contract-state-reader.md) — the
  lake-native state reader option (a) would ride.
- [ADR-0035](../adr/0035-factory-anchored-contract-gating.md) /
  [ADR-0040](../adr/0040-completing-contract-gating.md) — contract
  identity gating; the non-negotiable constraint on all three options.
- [ADR-0033](../adr/0033-completeness-verification-model.md) — the
  recognition-gap model `DistinctTopicShapes` implements; the natural
  home for a broadened discovery symbol set.
- `internal/canonical/discovery/` (`doc.go`, `sniffer.go`, `sink.go`,
  `recorder.go`) — the existing SEP-41 discovery pattern this note
  proposes extending.
- `internal/dispatcher/dispatcher.go` — `dispatchOne` (event-path
  discovery hook, present) vs. the `ContractCallContext` path (no
  discovery hook — the gap §3(b) closes).
- `internal/sources/{reflector,redstone,band}/README.md` — the three
  oracle ingests; Band is the load-bearing precedent for event-less
  on-chain sources.
- `internal/sources/defindex/README.md` — "mainnet never deployed
  that" — the DeFindex tag-1.0.0-vs-mainnet lesson cited against
  option (c).
- `docs/operations/wasm-audits/README.md` — the per-WASM-hash audit
  procedure a discovered candidate ultimately still needs before
  `BackfillSafe` flips true.
- `docs/contributing/add-onchain-source.md` — the onboarding recipe a
  confirmed discovery candidate follows.
- External specs: SEP-40 (Oracle Consumer Interface), SEP-41 (Soroban
  Token Interface), SEP-46 (Contract Meta), SEP-47 (Contract Interface
  Discovery), SEP-48 (Contract Interface Specification), SEP-49
  (Upgradeable Contracts), SEP-50 (Non-Fungible Tokens), SEP-56
  (Tokenized Vault Standard) — all at
  `github.com/stellar/stellar-protocol/tree/master/ecosystem`.
