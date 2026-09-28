---
title: SEP-1 stellar.toml resolution — operational reference
last_verified: 2026-05-03
status: living procedure
---

# SEP-1 stellar.toml resolution — operational reference

This doc covers the **runtime** + **on-call** concerns of SEP-1
resolution:

- HTTP failure-mode handling per home-domain
- Refresh cadence and forcing a re-fetch
- SSRF guard playbook
- Operator-facing instructions

The implementation lives in `internal/metadata/`; see that
package's [`doc.go`](../../internal/metadata/doc.go) for the
in-code overview.

## Resolution flow

Nothing fetches a stellar.toml on the API request path. The
`sep1-refresh.timer` resolves issuers in the background and the API
serves what it persisted:

```
sep1-refresh.timer →
  stellarindex-ops sep1-refresh (internal/ops/ingest/sep1_refresh.go)
    for each due issuer, sequentially:
      metadata.Resolver.Resolve(home_domain)
        ↓ HTTP GET https://<home_domain>/.well-known/stellar.toml
            ↓ DNS resolve → SSRF guard (private/loopback/link-local rejected)
            ↓ TLS handshake (5s timeout)
            ↓ HTTP read (10s total budget)
            ↓ TOML parse
        ↓ on success: write issuers.sep1_payload, stamp sep1_resolved_at,
          clear the retry ladder
        ↓ on failure: stamp the attempt, advance the retry ladder

asset request →
  v1/assets/{id} handler →
    issuers.sep1_payload for the asset's issuer (Postgres)
```

The resolver's timeouts and SSRF deny-list live in
`internal/metadata/sep1.go`; the rotation size and cadence live in
`sep1-refresh.service.j2` / `sep1-refresh.timer`.

## Failure modes

### 404 from upstream

The home-domain server returns 404 for `/.well-known/stellar.toml`.
Common causes:

- Issuer hasn't published a SEP-1 file (most cases — many small
  issuers don't bother).
- Issuer rotated the file path (very rare; SEP-1 mandates the
  `.well-known` location).

**Resolver behaviour:** returns `ErrSEP1NotFound`. **Refresh
behaviour:** nothing is persisted and the issuer's retry ladder
advances (see §"Refresh cadence").
**Handler behaviour:** asset overlay degrades cleanly — fields
that come from SEP-1 are reported as `null`, the `home_domain`
field on the asset response stays populated from the `AccountEntry`,
the `sep1_status` field is set to `"unreachable"`.

**On-call action:** none if a single asset is affected; investigate
if many home-domains report `not_found` simultaneously (suggests a
network egress problem on our side, not the issuers').

### TLS / connection error

Common causes:

- Issuer's TLS cert expired
- Hosting provider DNS down
- HTTPS-redirect loop
- Cert issued for a different name (CN mismatch)

**Resolver behaviour:** returns `ErrSEP1HTTP` wrapping the network
error. **Handler behaviour:** same as 404 — overlay degrades,
`sep1_status` becomes `"unreachable"`.

**On-call action:** scoped to that asset. The `host-down` and
`scrape-failing` runbooks don't apply here — this is third-party
infrastructure.

### Timeout

`metadata.ResolverConfig.HTTPTimeout` (default 10s) is the total
read budget. Slow issuers can blow this; we don't tune it per
host because that defeats the bound.

**Resolver behaviour:** `ErrSEP1Timeout`. **Refresh:** retry ladder advances.
**Alert:** `stellarindex_metadata_resolver_timeout_total` increases
beyond baseline (P3 alert, designed but not yet shipping at v1).

### TOML parse error

The fetched bytes don't parse as valid TOML. Causes seen in the wild:

- Issuer's CMS injected HTML around the TOML
- BOM byte at the start (not always handled by the TOML parser)
- Trailing garbage
- Invalid UTF-8 sequences

**Resolver behaviour:** `ErrSEP1MalformedTOML`. **On-call action:**
report the issuer; we don't try to "fix" their published file.

### SSRF rejection

The `metadata.Resolver` resolves DNS first, then checks the
resolved IP against a private-range deny-list:

- IPv4 RFC 1918: 10/8, 172.16/12, 192.168/16
- IPv4 loopback: 127/8
- IPv4 link-local: 169.254/16 (catches AWS instance metadata
  service — `169.254.169.254`)
- IPv4 multicast: 224/4
- IPv6 RFC 4193 (private): fc00::/7
- IPv6 loopback: ::1
- IPv6 link-local: fe80::/10

**Resolver behaviour:** `ErrSEP1PrivateIP`. **On-call action:**
malicious-issuer event — investigate. The `home_domain` value on
the affected asset's account is operator-supplied but came from
the chain (issuer set it via `SetOptionsOp`); the offender is the
issuer, not us. Report to the security mailing list per
[security.md](../../SECURITY.md).

## Refresh cadence

The `sep1-refresh.timer` re-resolves the watched set on a rotation —
**hourly at :12 UTC**, 750 issuers a run, so a domain that answers is
re-fetched roughly every two days. (It was 500 once a day, which over
76,658 domains was a 153-day cycle; the arithmetic is in
`sep1-refresh.service.j2`.) A domain that fails climbs a retry ladder —
1d, 2d, 4d, 8d, 16d, then a 30-day cap — and returns to the fast
cadence on its first success, so the budget goes to domains that
answer.

### Forcing a re-fetch for one issuer

When an issuer publishes a corrected stellar.toml and wants it
visible before the rotation reaches it, refresh that issuer alone;
`-issuer` bypasses the staleness queue:

```sh
stellarindex-ops sep1-refresh -config /etc/stellarindex/api.toml \
  -issuer <G-strkey>
```

A `home_domain` change on chain needs no action: the next refresh of
that issuer resolves the new domain.

## Operator-facing tasks

### Adding a curated issuer → home-domain mapping

The API reads an issuer's on-chain `AccountEntry.HomeDomain` from
`account_observations` for the accounts listed in
`[metadata].watched_issuer_accounts` (never add an issuer to
`[supply].sdf_reserve_accounts` for this: that subtracts its XLM from
circulating supply). For issuers with no observation, a curated map
supplies the home-domain:

```toml
# /etc/stellarindex.toml
[metadata]
watched_issuer_accounts = ["GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"]

[metadata.issuer_home_domains]
"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" = "centre.io"
```

An observation always wins over the curated map, including an
observation that the account has no home_domain (cleared by
`SetOptions`, or the account merged): the API then serves no
home_domain rather than the curated value. On `/v1/assets/{id}` and
`/v1/assets/{id}/metadata`, an unobserved issuer's live ClickHouse
account state is read before the curated map, and wins when it
carries a home_domain.

The map's only job is supplying `home_domain` for the SEP-1
resolver lookup; the resolver then fetches the issuer's
`.well-known/stellar.toml` and the rest of the asset-detail
fields (name, description, image, conditions, max_supply…) come
from there. There is **no** broader "override the SEP-1 fields
themselves" knob today — fixing per-field metadata requires the
issuer to publish a corrected stellar.toml at the home-domain.

### Removing a stale mapping

Edit the same `[metadata.issuer_home_domains]` table; delete the
issuer's row; re-deploy the binary (the config is parsed at boot
in `internal/config/load.go`, not hot-reloaded).

### Tracing a specific asset's resolution

A future `stellarindex-ops sep1-trace -domain <home_domain>`
subcommand (not in `cmd/stellarindex-ops/main.go`'s switch today)
would dump the full resolution path: DNS, IP, SSRF check
result, HTTP status, parsed fields. Until it lands the manual
playbook is:

```sh
# 1. Confirm what the API sees
curl -sf https://api.stellarindex.io/v1/assets/<asset_id> | jq .

# 2. Confirm what the last refresh persisted
psql -d stellarindex -c "SELECT sep1_resolved_at, sep1_payload
  FROM issuers WHERE g_strkey = '<G-strkey>'"

# 3. Hit the issuer directly
curl -sfL "https://<home_domain>/.well-known/stellar.toml"

# 4. If 2 and 3 disagree, re-fetch (see §"Forcing a re-fetch"):
stellarindex-ops sep1-refresh -config /etc/stellarindex/api.toml \
  -issuer <G-strkey>
```

## Metrics + alerts

`internal/metadata` emits no Prometheus metrics. `sep1-refresh` logs
each per-issuer failure and judges its own run: a failure fraction
high enough to indicate an outage on our side unwinds the ladder step
it just applied (see the job's doc comment). The data-freshness
watchdog reads `max(issuers.sep1_resolved_at)`, which a failed attempt
stamps as well as a success, so it cannot tell a working refresh from
a failing one. No per-issuer resolver error-rate alert ships today.

## References

- Refresh job: `internal/ops/ingest/sep1_refresh.go`; persisted
  columns `issuers.sep1_payload` / `issuers.sep1_resolved_at`
- Supply policy: [ADR-0011](../adr/0011-supply-algorithm.md) (uses
  SEP-1 `[[CURRENCIES]].max_supply` as the off-chain max-supply
  source)
- Package: `internal/metadata/`
- SEP-1 spec: <https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0001.md>
