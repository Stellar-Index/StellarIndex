---
title: Runbook — api-security alerts
last_verified: 2026-10-05
status: draft
---
# API security alerts

Alerts from `configs/prometheus/rules.r1/api-security.yml` (mirror: `deploy/monitoring/rules/api-security.yml`).

## At a glance

- [`stellarindex_api_cors_wildcard_in_prod`](#stellarindex_api_cors_wildcard_in_prod)
- [`stellarindex_apikey_lineage_write_failures`](#stellarindex_apikey_lineage_write_failures)

## stellarindex_api_cors_wildcard_in_prod

**Severity** P3 (ticket). MTTR 10-30 min (config change + redeploy).
**Trigger** `sum(increase(stellarindex_api_cors_decisions_total{outcome="allowed_wildcard"}[15m])) > 0` for 5m. `STELLARINDEX_ALLOWED_ORIGINS=*` is set on a production api instance and is matching real cross-origin requests. If `auth_mode` is credentialed, any origin can read authenticated responses. Outcomes: `no_origin`, `allowed_origin`, `allowed_wildcard`, `denied`. Boot-time twin: `warnOpenCORS` in `cmd/stellarindex-api/main.go`; decision code in `internal/api/v1/middleware/cors.go`.

**Diagnose**

```sh
# Confirm the running config
curl -fs http://localhost:9464/debug/config 2>/dev/null | grep -i allowed_origins || true
echo "$STELLARINDEX_ALLOWED_ORIGINS"

# Confirm which outcome is firing and at what rate
curl -fs http://localhost:9464/metrics | grep stellarindex_api_cors_decisions_total
```

**Fix**

1. If credentials (cookies / API keys tied to a session) are in play for browser traffic, replace the wildcard with an explicit allow-list of the known frontend origin(s) and redeploy.
2. If the API is meant to be publicly, anonymously readable with no credentialed cross-origin flow, the wildcard may be intentional. Downgrade to an acknowledged exception by adjusting the alert's severity/routing; do not silence it or widen `.gitleaksignore`-style suppression lists.
3. Verify: `allowed_wildcard` stops incrementing and (if origins were pinned) `allowed_origin` picks up the same traffic.

## stellarindex_apikey_lineage_write_failures

**Severity** P3 (ticket). MTTR 10-30 min (apply one role).
**Trigger** `sum by (op) (increase(stellarindex_apikey_lineage_write_failures_total[90m])) >= 3` for 30m. Lineage writes for `op` (`mint`, `slide` or `delete`) have kept failing. No request fails and no authentication decision changes, but [lineage records](#lineage-records-step-2) go unwritten (mint, slide) or undeleted (delete). Code: `internal/auth/apikey_lineage_record.go`.

**Diagnose**

Check first that the ACL grant was applied. The usual cause is a binary deployed without the `redis-sentinel` role run after it, so every lineage write is NOPERM. Run as the admin user (prints key patterns only):

```sh
redis-cli ACL GETUSER stellarindex | grep -F 'apikey-lineage'
```

If the grant is there, look for Redis refusing writes (`used_memory` against `maxmemory`, OOM errors in the API log) or timing out.

**Fix**

1. Grant missing: apply the `redis-sentinel` role to the host.
2. Then run `stellarindex-ops apikey-lineage-backfill -config /etc/stellarindex.toml -dry-run` and `-write` as in [Lineage records](#lineage-records-step-2): it creates the records the failed mints and slides missed, and deletes the records and the `apikey-lineage:owner:` sets that failed deletes left (an owner set goes once none of its members has a lineage record or a credential).
3. Verify: the counter stops increasing and the alert resolves within 90 minutes.

## Operator-issued API keys

Mint a key for a partner, or change a key's rate limit. Both write an audit log row and need a `-reason`. The customer's plaintext key keeps working after an upgrade; the new budget applies on the next request.

```sh
stellarindex-ops mint-key -config /etc/stellarindex.toml -identifier <id> -label <label> -reason "<why>" -rate-limit-per-min <N> -write
stellarindex-ops upgrade-key -config /etc/stellarindex.toml -key-id <kid> -rate-limit-per-min <N> -reason "<why>" -write
```

Record every delegated key's ancestor chain before deploying a release that enforces lineage. The release that ships `apikey-lineage-backfill` does not enforce lineage yet, and it does not keep ancestors alive: using a child refreshes only the child's idle window. An unused middle key still idles out after 90 days, and the backfill reports its subtree as orphaned. Review the orphaned KeyIDs the dry run lists: enforcement will refuse them, and the account owner may need a fresh key.

Order, per host:

1. Run `-write` only once every host runs the new binary. An older binary rewrites a record without the ancestor hashes, so a backfill written before it is gone loses them again.
2. Run `-dry-run` again just before deploying the enforcement release. It must report 0 newly resolved records (`would record the chain of 0 keys`); any other count means a record lost its chain since the write, so run `-write` again and repeat this step.

```sh
stellarindex-ops apikey-lineage-backfill -config /etc/stellarindex.toml -dry-run
stellarindex-ops apikey-lineage-backfill -config /etc/stellarindex.toml -write
```

### Lineage records (step 2)

Each key also has an `apikey-lineage:<hash>` record (KeyID and owner only, no key material). A key's own use and its descendants' use slide it, at most once an hour per key per API process, to the 90-day idle window plus 2 hours, so an ancestor nobody calls directly still stands while its children are in use. Only an EXPIRE that cannot create a record slides it. No authentication decision reads it yet. The ancestor's credential record still idles out on its own use, as before. A write that fails is counted in `stellarindex_apikey_lineage_write_failures_total{op}` and changes nothing else: a mint still succeeds, and the backfill writes the missing record later.

When a lineage record is deleted:

- A revoke deletes the lineage records of the revoked key, of every key it removes, and of idled-out keys between them, read from the removed keys' stored chains. An erasure deletes those of the account's keys, of their idled-out ancestors, and of every key in the account's `apikey-lineage:owner:<sha256>` set, so it reaches an account with no credential left. Only records naming the same account are deleted, and never while the key's own credential exists, so a revoke that fails part-way keeps the records of the keys it left live.
- These deletes are best-effort and run after the credential delete. A failed one never fails the revoke or the erasure, and is counted as `op="delete"`; `account-erase` also prints `some api-key lineage records were not deleted`. What it leaves expires at most 2 hours after its own and every descendant's credential have idled out. A persistent key's record never expires, nor does its account's owner set (its name derives from the account identifier); `apikey-lineage-backfill -write` deletes both.
- A record no revoke reaches expires the same way: an idled-out key revoked with no live descendant, for example.
- A revoke or an erasure run by a binary older than this one deletes no lineage record, and one left for a persistent key never expires. Before enforcement ships, run the backfill after the last old binary is gone. It reports `would delete N orphaned lineage records` (no credential, no live descendant), and `-write` deletes them.

Order, per host. Do not ship lineage enforcement until the last step passes:

1. Deploy the binary.
2. Apply the `redis-sentinel` role, which grants the app user `~apikey-lineage:*`. Until then every lineage write is NOPERM. Check the grant (prints key patterns only): `redis-cli ACL GETUSER stellarindex | grep -F 'apikey-lineage'`, run as the admin user.
3. Run `apikey-lineage-backfill -dry-run`. It prints `would create the lineage record of N keys`. If it says `not checked, the Redis ACL denies apikey-lineage:*` and exits non-zero, go back to step 2.
4. Run `apikey-lineage-backfill -write`.
5. Run `-dry-run` again. It must print `would create the lineage record of 0 keys`, `would delete 0 orphaned lineage records` and `would delete 0 orphaned lineage owner sets`.

## Related

- [api](api.md): API-side alerts.
