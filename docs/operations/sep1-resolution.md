---
title: SEP-1 stellar.toml resolution — operational reference
last_verified: 2026-10-21
status: living procedure
---

# SEP-1 stellar.toml resolution — operational reference

Runtime and on-call concerns of SEP-1 resolution. Code: `internal/metadata/`
(overview in [`doc.go`](../../internal/metadata/doc.go)).

## Resolution flow

Nothing fetches a stellar.toml on the API request path. `sep1-refresh.timer`
resolves issuers in the background; the API serves what was persisted.

```
sep1-refresh.timer →
  stellarindex-ops sep1-refresh (internal/ops/ingest/sep1_refresh.go)
    for each due issuer, sequentially:
      metadata.Resolver.Resolve(home_domain)
        ↓ HTTP GET https://<home_domain>/.well-known/stellar.toml
            ↓ DNS resolve → SSRF guard (non-public ranges rejected; port 443 only)
            ↓ TLS handshake (5s timeout)
            ↓ HTTP read (10s total budget)
            ↓ TOML parse
        ↓ success: write issuers.sep1_payload, stamp sep1_resolved_at, clear retry ladder
        ↓ failure: stamp the attempt, advance the retry ladder

v1/assets/{id} handler → issuers.sep1_payload for the asset's issuer (Postgres)
```

Timeouts: `internal/metadata/sep1.go`; SSRF deny-list: `internal/nettools/ssrf.go`.
Rotation size and cadence: `sep1-refresh.service.j2` / `sep1-refresh.timer`.

## Failure modes

On any failure the refresh persists nothing and advances the retry ladder (see
§"Refresh cadence"). The asset overlay degrades cleanly: SEP-1-derived fields
are `null`, `home_domain` stays populated from the `AccountEntry`, and
`sep1_status` is `"unreachable"`.

| Mode | Resolver error | Causes | On-call action |
|---|---|---|---|
| HTTP status | `ErrHTTPStatus` | Non-200, 404 above all: issuer never published a SEP-1 file (most cases); path rotated (rare) | None for one asset. Investigate if many domains report `http` at once (our egress). |
| TLS / connection | net / x509 error (labels `dns`, `connect`, `tls`); `ErrRedirectRefused` for redirects (cross-host, https downgrade, over 5 hops) | Expired cert, DNS down, CN mismatch | Scoped to that asset; third-party infra, so `host-down` / `scrape-failing` runbooks don't apply. |
| Timeout | context deadline / net timeout (label `timeout`) | Slow issuer exceeding `metadata.Options.Timeout` (default 10s, total budget; not tuned per host) | None; ladder advances. |
| TOML parse | `ErrTOMLParse`; also `ErrTOMLTooLarge` (over 100 KiB), `ErrTOMLTooDeep`, `ErrInvalidDomain` | HTML injected by CMS, BOM, trailing garbage, invalid UTF-8 | Report the issuer; we don't fix their file. |
| SSRF | `ErrSSRFBlocked` | Resolved IP in deny-list | Malicious-issuer event: investigate. The `home_domain` came from chain (`SetOptionsOp`); the offender is the issuer. Report per [SECURITY.md](../../SECURITY.md). |

SSRF deny-list (checked after DNS resolution, `nettools.IsBlockedIP`): loopback,
unspecified, link-local (covers `169.254.169.254`), multicast, RFC 1918 and
fc00::/7, plus CGNAT 100.64/10, 192.0.0/24, 198.18/15, 0/8, 240/4 and reserved
or transition IPv6 ranges (full list: `extraBlockedNets`). Any port but 443 is
also refused.

## Refresh cadence

`sep1-refresh.timer` rotates the watched set: **hourly at :12 UTC**, 750
issuers a run, so a domain that answers is re-fetched about every two days
(arithmetic in `sep1-refresh.service.j2`). A failing domain climbs a retry
ladder: 1d, 2d, 4d, 8d, 16d, then a 30-day cap; the first success returns it
to the fast cadence.

### Forcing a re-fetch for one issuer

`-issuer` bypasses the staleness queue:

```sh
stellarindex-ops sep1-refresh -config /etc/stellarindex.toml \
  -issuer <G-strkey> -write
```

Without `-write` the run is a preview and persists nothing. A chain `home_domain` change needs no action: the next refresh resolves the
new domain.

## Operator-facing tasks

### Adding a curated issuer → home-domain mapping

The API reads an issuer's on-chain `AccountEntry.HomeDomain` from
`account_observations` for accounts in `[metadata].watched_issuer_accounts`
(never add an issuer to `[supply].sdf_reserve_accounts` for this: that
subtracts its XLM from circulating supply). For issuers with no observation, a
curated map supplies the home-domain:

```toml
# /etc/stellarindex.toml
[metadata]
watched_issuer_accounts = ["GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"]

[metadata.issuer_home_domains]
"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" = "centre.io"
```

An observation always wins over the curated map, including one showing no
home_domain (cleared by `SetOptions`, or account merged): the API then serves
none. On `/v1/assets/{id}` and `/v1/assets/{id}/metadata`, an unobserved
issuer's live ClickHouse account state is read before the curated map and wins
when it carries a home_domain.

The map only supplies `home_domain`; name, description, image, conditions,
max_supply etc. come from the fetched stellar.toml. There is no knob to
override SEP-1 fields: the issuer must publish a corrected file.

### Removing a stale mapping

Delete the issuer's row in `[metadata.issuer_home_domains]` and re-deploy (the
config is parsed at boot in `internal/config/load.go`, not hot-reloaded).

### Tracing a specific asset's resolution

No `stellarindex-ops sep1-trace` subcommand exists. Manual playbook:

```sh
# 1. Confirm what the API sees
curl -sf https://api.stellarindex.io/v1/assets/<asset_id> | jq .

# 2. Confirm what the last refresh persisted
psql -d stellarindex -c "SELECT sep1_resolved_at, sep1_payload
  FROM issuers WHERE g_strkey = '<G-strkey>'"

# 3. Hit the issuer directly
curl -sfL "https://<home_domain>/.well-known/stellar.toml"

# 4. If 2 and 3 disagree, re-fetch (see §"Forcing a re-fetch"):
stellarindex-ops sep1-refresh -config /etc/stellarindex.toml \
  -issuer <G-strkey> -write
```

## Metrics + alerts

`sep1-refresh` logs each per-issuer failure and stamps every run's counters
into `/var/lib/node_exporter/textfile_collector/sep1_refresh.prom`: issuers by
outcome, the `reached` denominator (domains that had served a toml before), the
`regressed` count and a `failures{reason,reached}` split (timeout, dns,
connect, tls, http, redirect, parse, too_large, too_deep, ssrf, invalid_domain, payload,
domain, lake, write, other). Three ticket alerts read it, each held for three
hourly runs: `stellarindex_sep1_refresh_timeouts_high` and
`stellarindex_sep1_refresh_failing` at 30% of reached, and
`stellarindex_sep1_refresh_stale` when no run has stamped for 3 h
([runbook](runbooks/sep1-refresh.md)). The run also judges itself: a failure
fraction high enough to indicate an outage on our side (90% of reached) unwinds
the ladder step it just applied and fails the unit. The data-freshness watchdog
reads `max(issuers.sep1_payload_fetched_at)`, stamped only by a successful
fetch, so a run that fails every domain goes stale. It is an aggregate: one dead
domain among many live ones does not trip it.

Every successful write diffs the held payload against the new one and appends
a row to `issuer_identity_history` (migration 0190) for each changed
`OrgName`, `ORG_URL`, `ORG_LOGO` or currency `Image` on an issuer that already
had a payload. Nothing alerts on those rows yet.

## References

- Refresh job: `internal/ops/ingest/sep1_refresh.go`; columns
  `issuers.sep1_payload` / `issuers.sep1_resolved_at`
- Supply policy: [ADR-0011](../adr/0011-supply-algorithm.md) (SEP-1
  `[[CURRENCIES]].max_supply` is the off-chain max-supply source)
- Package: `internal/metadata/`
- SEP-1 spec: <https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0001.md>
