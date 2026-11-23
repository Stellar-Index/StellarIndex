---
title: Runbook — api-security alerts
last_verified: 2026-10-05
status: draft
---
# API security alerts

Alerts from `configs/prometheus/rules.r1/api-security.yml` (mirror: `deploy/monitoring/rules/api-security.yml`).

## At a glance

- [`stellarindex_api_cors_wildcard_in_prod`](#stellarindex_api_cors_wildcard_in_prod)

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

Each key also has an `apikey-lineage:<hash>` record (KeyID and owner only, no key material). A key's own use and its descendants' use slide it, at most once an hour per key per API process, to the 90-day idle window plus 2 hours, so an ancestor nobody calls directly still stands while its children are in use. Only an EXPIRE that cannot create a record slides it, and a revoke or an account erasure deletes it. No authentication decision reads it yet. The ancestor's credential record still idles out on its own use, as before. A write that fails is counted in `stellarindex_apikey_lineage_write_failures_total{op}` and changes nothing else: a mint still succeeds, and the backfill writes the missing record later.

Order, per host. Do not ship lineage enforcement until the last step passes:

1. Deploy the binary.
2. Apply the `redis-sentinel` role, which grants the app user `~apikey-lineage:*`. Until then every lineage write is NOPERM. Check the grant (prints key patterns only): `redis-cli ACL GETUSER stellarindex | grep -F 'apikey-lineage'`, run as the admin user.
3. Run `apikey-lineage-backfill -dry-run`. It prints `would create the lineage record of N keys`. If it says `not checked, the Redis ACL denies apikey-lineage:*` and exits non-zero, go back to step 2.
4. Run `apikey-lineage-backfill -write`.
5. Run `-dry-run` again. It must print `would create the lineage record of 0 keys`.

## Related

- [api](api.md): API-side alerts.
