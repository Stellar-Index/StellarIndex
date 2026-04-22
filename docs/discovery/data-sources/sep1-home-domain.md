# SEP-1 / stellar.toml / home-domain resolution

**Status:** ✅ **Design locked.** Closes the Freighter V1 `home_domain`
field gap and the `max_supply` metadata source for classic assets.

**Related RFP fields:**
- Freighter V1 `Home Domain` (nullable, classic-only).
- Freighter V1 asset `name` / description / image metadata.
- Supply `max_supply` override source for
  [supply-data.md](supply-data.md).

## What SEP-1 defines

SEP-1 specifies the **`stellar.toml`** file hosted at
`https://<home_domain>/.well-known/stellar.toml` for any
organisation wanting to publish verified information about its
accounts, assets, validators, and services. Key sections for us:

- `[[CURRENCIES]]` — per-asset records. Fields include `code`,
  `issuer`, `display_decimals`, `name`, `desc`, `conditions`,
  `image`, `max_supply`, `is_asset_anchored`, `anchor_asset`,
  `regulated`, `redemption_instructions`, `collateral_addresses`,
  `status`.
- `[[ACCOUNTS]]` — operational account listing.
- `[[VALIDATORS]]` — validator self-verification (SEP-20).
- `NETWORK_PASSPHRASE`, `FEDERATION_SERVER`, `HORIZON_URL`,
  `AUTH_SERVER` — various service endpoints.

For our RFP surface we care about `[[CURRENCIES]]` and
`[[VALIDATORS]]` (the latter for our own Tier-1 publication per
[decisions.md](../decisions.md)).

## Resolution flow

```
┌──────────────────┐   step 1: load AccountEntry from Galexie-ingested state
│ AccountEntry     │
│   home_domain    │
└────────┬─────────┘
         │
         ▼                step 2: HTTPS GET
  https://<domain>/.well-known/stellar.toml
         │
         ▼                step 3: parse TOML
    [[CURRENCIES]]        step 4: filter by (code, issuer) match
     code, issuer,        step 5: cache + refresh
     name, image,
     max_supply, …
```

## Caching strategy

Home-domain resolution is external HTTP — we don't want it in the
hot path of our API. Two-tier cache:

1. **Persistent cache** in Postgres `asset_metadata` table, keyed
   by `(asset_key, home_domain)`. Refreshed on a schedule (default:
   every 6h) plus on-demand when we see a new asset.
2. **In-memory / Redis cache** per API node, TTL 15 min. Falls
   back to persistent cache on miss.

Staleness tolerance: Freighter asset-detail views can happily
serve 6-hour-old metadata. If a classic asset has no home_domain or
the TOML fetch fails, return `null` for all metadata fields and
record `metadata_status: "unavailable"`.

## Fetch semantics

- HTTPS only. Reject `http://`.
- Timeout: 10 s per fetch.
- Follow up to 3 redirects (some operators put the TOML on a
  subdomain or CDN).
- Respect `Cache-Control: max-age` if provided.
- Max file size: 100 KB. Anything larger we reject with
  `metadata_status: "too_large"`.
- User-Agent: `ctx-rates/<version> (+https://ratesengine.ctx.io)`.

## Parsing

Use a standard TOML parser (`github.com/pelletier/go-toml/v2` is
already a transitive dep via Galexie). Fields we extract from each
`[[CURRENCIES]]` entry:

| TOML field | Our column | Use |
| ---------- | ---------- | --- |
| `code` + `issuer` | `asset_key` | Lookup key |
| `name` | `display_name` | Freighter asset detail |
| `desc` | `description` | Freighter asset detail |
| `image` | `image_url` | Freighter asset detail |
| `display_decimals` | `display_decimals` | UI rendering hint |
| `max_supply` | `declared_max_supply` (NUMERIC) | `supply-data.max_supply` override — flagged `self_declared: true` |
| `is_asset_anchored` | `is_anchored` | RWA flag |
| `anchor_asset_type`, `anchor_asset` | `anchor_*` | Anchor metadata |
| `status` | `status` | "live" / "dead" / "private" / "test" — we skip assets with `status: dead` |
| `conditions`, `regulated` | informational | Pass-through |

Fields we **ignore** on first pass: `collateral_addresses`,
`redemption_instructions` — outside our pricing scope.

## Trust model

**Home domains are self-asserted.** An issuer can publish any
values they want. We do NOT treat `max_supply` from a stellar.toml
as authoritative — we use it as a *display* value and flag it
`self_declared: true` in our API response. Our
`total_supply` / `circulating_supply` from on-chain data remain
the ground truth.

One exception: **validator self-verification via SEP-20** uses
stellar.toml as the canonical source for validator public keys —
that's by design (see [decisions.md](../decisions.md) — our own
Tier-1 publication).

## Federation (separate concern)

SEP-1 also references federation server discovery (`FEDERATION_SERVER`).
Federation lets users resolve `name*domain.com` → `G…` accounts. This
is **not** something our pricing API needs. We don't do name
resolution. Ignored.

## Pre-resolution verification

Some issuers publish multiple `[[CURRENCIES]]` entries for assets
they don't actually control. We verify each entry by matching the
`issuer` address against the `AccountEntry.HomeDomain` we loaded
in step 1:

```
toml_entry_is_valid =
  toml.code == observed_asset.code
  AND toml.issuer == observed_asset.issuer
  AND observed_asset.home_domain == request_domain
```

If the TOML claims currencies for issuers whose home_domain
doesn't match, we ignore those entries.

## Schema

```sql
CREATE TABLE asset_metadata (
  asset_key         TEXT PRIMARY KEY,
  home_domain       TEXT,
  display_name      TEXT,
  description       TEXT,
  image_url         TEXT,
  display_decimals  INT,
  declared_max_supply NUMERIC,
  is_anchored       BOOLEAN,
  anchor_asset_type TEXT,
  anchor_asset      TEXT,
  status            TEXT,
  raw_toml          JSONB,        -- for audit / later-added fields
  fetched_at        TIMESTAMPTZ,
  fetch_status      TEXT,         -- "ok" | "unreachable" | "invalid_toml" | "no_match" | "too_large"
  error_detail      TEXT
);
```

## Soroban tokens — no home domain

SEP-41 Soroban tokens have no `home_domain` field because they
aren't accounts — they're contracts. The Freighter RFP lists
`Home Domain` as "classic assets only; nullable." So our response
for SEP-41 tokens: `home_domain: null`.

For SEP-41 tokens we get display metadata from the contract's own
methods: `name()`, `symbol()`, `decimals()`. That's already in the
SEP-41 interface per [notes/sep-41-token-events.md](../notes/sep-41-token-events.md).

No stellar.toml resolution path for SEP-41; on-chain only.

## Refresh strategy

- **Scheduled full refresh**: every 6 hours, re-fetch every
  `asset_metadata` row with `fetch_status = "ok"` and
  `fetched_at > 6h ago`.
- **Failed-entry retry**: rows with `fetch_status != "ok"` retry
  on an exponential backoff (1 h, 4 h, 16 h, 1 d, then daily).
- **New-asset on-demand**: first time we see a classic asset in
  our trade stream, kick off a resolution immediately (async).

Rate limit per-domain: max 1 fetch per minute per `home_domain`
regardless of how many assets point to it. Cache the TOML as a
whole, not per-asset.

## Failure modes

- Home domain resolves to `127.0.0.1` or `localhost` → reject,
  `fetch_status = "suspicious"`.
- TOML parse fails → `fetch_status = "invalid_toml"`, retain
  previous good row if any.
- TLS cert expired → `fetch_status = "tls_error"`.
- 404 → `fetch_status = "unreachable"`.
- Rate limited (429 from the domain) → treat as transient, retry
  with backoff.

API response always includes a metadata-status indicator so
consumers know when values are stale / unavailable.

## Open items

- [ ] Pick the TOML parser. `pelletier/go-toml/v2` vs
      `BurntSushi/toml`. Both are already transitive deps; pick
      one and pin.
- [ ] Decide SSRF protections: block private IP ranges (RFC1918,
      link-local, etc.) on the DNS resolution step so a malicious
      domain can't point at `169.254.169.254` (cloud metadata) or
      internal services.
- [ ] Robots.txt handling — we probably ignore it (stellar.toml is
      a well-known standard endpoint, not crawlable content). But
      respect for good citizenship.
- [ ] Handle `stellar.toml` files that are >100 KB — some big
      anchors might legitimately exceed this. Tune the limit after
      reviewing real-world examples.
- [ ] Decide if we expose raw TOML in API response (`raw_toml`
      field) — useful for debugging, but enlarges response size.
      Default off; toggle per-request.

## Related

- [supply-data.md](supply-data.md) — uses `declared_max_supply` as
  metadata override.
- [../rfp-requirements-matrix.md §B1](../rfp-requirements-matrix.md)
  — this doc closes the home-domain gap.
- [../decisions.md](../decisions.md) — SEP-20 validator self-
  verification (related SEP).
