# getEvents v2 RPC proposal — Stellar discussion #1872

**Source:** <https://github.com/orgs/stellar/discussions/1872>
**Scope this file captures:** the in-flight `getEvents` v2 design and
why it matters to *our* Soroban-event-based price indexing (Soroswap
swaps, Blend events, Aquarius, etc.). This is **not** the final API —
the discussion is live and contested.

> ⚠️ The content here is drawn from a single AI summary pass over the
> discussion on 2026-04-22. Before we build against any specific detail
> (endpoint shape, cursor format, topic filter syntax) we should re-read
> the discussion directly and look at the linked RPC PRs. Treat this as
> orientation, not specification.

## Why we care

Our Soroban price signals come from contract events — `Swap`, `Deposit`,
`Withdraw` on Soroswap pairs; `new_auction` / `fill_auction` on Blend;
Aquarius pool events; and potentially SEP-40 oracle updates. The
current Stellar-RPC `getEvents` has two show-stoppers for a production
price index:

1. **7-day retention** on the default deployment. We need since-inception
   or at minimum multi-month for the "since inception" OHLC endpoint.
2. **Poor ergonomics** — clients cannot express "give me all `transfer`
   events for contract X" without scanning ledger-by-ledger and merging.

The v2 proposal addresses both.

## Participants (for context)

- **Maintainers:** tamirms (author), mootz12, tomerweller, leighmcculloch,
  urvisavla, kalepail, Shaptic.
- **External voices:** teddav (privacy-pools use case, pushed for full
  history), steven-tomlinson, FrankSzendzielarz, tupui.

## Three query modes (as proposed)

Each request uses exactly one of:

- `RangeQuery` — `min`/`max` ledger bounds. Ascending requires `min`;
  descending works without bounds (captures "latest N" stably across
  RPC-operator retention policies).
- `TransactionQuery` — fetch events within a single tx hash.
- `CursorQuery` — continuation of a prior query; only `{ cursor, limit? }`
  are required.

Cursors are **opaque**. The RPC encodes full query state inside them,
which is good for us — we can persist a cursor per contract and resume
after restart without re-negotiating filters.

## Topic filter semantics

**Positional, Ethereum-style** (compared to `eth_getLogs` in the
discussion). Up to 4 positions, up to 3 values per position. `null`
means wildcard. An array at a position expresses OR within that
position.

**Concern raised (mootz12, tomerweller):** expressing "transfers to OR
from address A" requires two queries — same trade-off Ethereum makes.
For our case (most queries will be "all `transfer`/`swap` events for
contract X within a ledger range") this is fine; we'll build the client
abstraction on top.

**Input formats:** base64-XDR and JSON `SCVal` both accepted. That lets
us send filters in a human-readable form during development and XDR in
production.

## Pagination / DoS guards (as proposed)

- Max 3 contractIds per request.
- Max 4 topic positions, max 3 values per position.
- Max 1,000 events per response.
- Internal 10k-ledger scan window — responses may come back partial
  with `hasMore`, `scannedLedger`, and `availableLedgers` metadata, so
  callers page with cursors. Importantly this means the server gives
  **predictable response times** rather than timing out on sparse
  queries.

For us: a cold backfill of Soroswap swaps since pair inception will
involve a lot of paginated calls but will at least be well-behaved. We
shouldn't block the live hot path on getEvents scans — feed fresh
events via subscription/streaming as they close, and use getEvents
only for targeted historical fetches and backfill.

## Ordering

`order: "desc"` reverses the **full** `(ledger, tx_index, op_index,
event_index)` tuple, not just the ledger dimension (tamirms
indicated as intuitive default; final behavior pending perf review).

## Full-history retention — explicit goal

teddav pushed for this as a hard requirement; tamirms confirmed the v2
design is compatible with RPC nodes that retain full event history. In
practice, this means **operators (us included) can choose retention**.
Stellar's hosted RPC may still be 7-day; our self-hosted RPC can be
configured for full history, which is the whole reason we're
self-hosting.

## Design tensions that may still move

1. **Single `filters` array** (tomerweller) vs three-mode enum. A single
   flat array would let callers combine "all transfers involving me" in
   one request but reintroduces illegal-state possibilities that the
   current enum prevents.
2. **GitHub-style search syntax** (leighmcculloch): `"contract:C... topic0:{symbol:transfer}"`.
   Readable but weaker type safety. Less likely to win — current
   direction favours structured fields.
3. **Incremental fix to existing `getEvents`** rather than a new
   endpoint (leighmcculloch). Would reduce breaking changes. If this
   wins, the v2 API shape may not land at all; instead we get opaque
   cursors + `order: desc` + `hasMore` on the existing endpoint.

Any of these would change the client code we write, so we should not
code against a specific shape until the discussion converges.

## Linked issues (for later deep-dives)

- `stellar/stellar-rpc#426`, `#575` — original pain points.
- `stellar/stellar-rpc#23` — requiring startLedger upfront.
- `stellar/stellar-docs#2250` — event ID sort-ability documentation.

## Implications for our architecture

1. **Self-host stellar-rpc with long retention** — we already planned
   this; the discussion confirms it's the only way to get since-inception
   event data reliably. See [archival-nodes.md](../data-sources/archival-nodes.md)
   (to be written).
2. **Client abstraction layer** — since the v2 shape will change, we
   build a thin internal adapter around whatever RPC is current, and
   keep all our processor code talking to a stable internal interface
   (contract ID + topic pattern + ledger range → stream of events).
3. **Prefer streaming for live, getEvents for backfill** — once events
   get beyond the hot-path window, we use getEvents pagination to
   backfill. The 10k-ledger scan window is fine for batch jobs.
4. **Avoid getEvents-based live hot path** — at the 30-second RFP SLA,
   polling any public getEvents endpoint is neither low-latency nor
   ourselves; we'll ingest from captive-core directly or via a
   subscription channel (stellar-rpc does not currently push, so we
   poll our **own** rpc at close-cadence + ledger-notification-channel
   from our own core).

## Open items for follow-up

- [ ] Watch #1872 for convergence; revisit before Phase 2 Soroban work.
- [ ] Read `stellar/stellar-rpc` open PRs referenced by tamirms to see
      which direction is actually being implemented.
- [ ] Confirm retention config key in self-hosted stellar-rpc
      (`HISTORY_RETENTION_WINDOW` or similar) and what hardware/disk
      footprint full retention implies. Capture in archival-nodes.md.
- [ ] Pin a compatibility target: we'll support whatever ships as the
      final v2 shape in the stellar-rpc release our Horizon-era
      alternative runs against, plus a fallback path on legacy v1 for
      nodes on older releases.
