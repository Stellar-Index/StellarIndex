---
title: Runbook — tls-cert-expiring-soon
last_verified: 2026-05-28
status: draft
severity: P2
---

# Runbook — `stellarindex_tls_cert_expiring_soon`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_tls_cert_expiring_soon` |
| Severity | P2 (ticket) |
| Detected by | `deploy/monitoring/rules/api.yml` + `configs/prometheus/rules.r1/api.yml` |
| Typical MTTR | 15–60 min |
| Impact | TLS handshake fails when cert expires. `api.stellarindex.io` would 5xx every request; customer integrations break. 14-day head room means there's plenty of time to manually renew before customer-visible impact. |

## Symptoms

- `stellarindex_tls_cert_not_after_unix{host=<H>} - time() < 14 * 24 * 3600`
- Sustained for ≥ 1 h (one missed probe is tolerated; sustained drift is not)
- Caddy's `caddy.log` may show recent renewal-attempt errors

The probe runs from the API binary every 6 h — see
`internal/api/v1/tls_probe.go::RunTLSCertProbe`. A 14-day
threshold gives 56 successful probes' head room.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@<host>

# 1. Confirm the gauge value vs. NOW.
curl -sS localhost:3000/metrics | grep stellarindex_tls_cert_not_after_unix

# 2. Read the actual on-disk cert Caddy is serving.
openssl x509 -in /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/api.stellarindex.io/api.stellarindex.io.crt -noout -enddate

# 3. Check Caddy's renewal log for the most recent attempt.
journalctl -u caddy --since "30d ago" | grep -iE "renew|certificate"
```

## Likely causes

1. **ACME rate limit hit.** Let's Encrypt enforces 5 duplicate
   cert/week and 50 certs/account/week. Look for `429` /
   `tooManyCertificatesPerName` in caddy.log.
2. **DNS-01 challenge failing.** If using DNS-01 (we don't by
   default, but operators may have configured it), the renewal
   gets stuck on the TXT-record propagation.
3. **HTTP-01 challenge failing.** Port 80 reachability broken
   — firewall change, Caddy not bound to :80, Cloudflare
   proxying interfering.
4. **Caddy disk full** (F-0001 cluster). `/var/lib/caddy` on
   the root partition; if `/` is full, Caddy can't write the
   new cert.
5. **Caddy stopped / crashed.** Renewal needs Caddy alive
   sometime during the 30-day pre-expiry window.

## Remediation

### Force a manual Caddy renewal

```sh
# Trigger renewal without touching the cert. Caddy responds to SIGUSR1
# but the safer path is the JSON-RPC admin endpoint.
curl -X POST 'http://localhost:2019/load' --data @/etc/caddy/Caddyfile.api -H 'Content-Type: text/caddyfile'

# Or restart (renewals attempt at startup):
systemctl restart caddy

# Watch the renewal attempt:
journalctl -u caddy -f
```

### If Caddy can't renew (rate limited, etc.)

Fall back to certbot's standalone mode or use ZeroSSL via Caddy's
ACME alternate config (`acme_ca_root https://acme.zerossl.com/v2/DV90`).
See `docs/operations/r1-deployment-state.md` for the full TLS
provisioning sequence.

### Verify post-fix

```sh
# Probe runs every 6h; force a probe by restarting the API binary:
systemctl restart stellarindex-api
sleep 30

curl -sS localhost:3000/metrics | grep stellarindex_tls_cert_not_after_unix
# Should show a NotAfter ~90 days in the future for Let's Encrypt.
```

The alert clears after `for: 1h` elapses with the new gauge value.

## Related

- `internal/api/v1/tls_probe.go::RunTLSCertProbe` — probe
  implementation
- `docs/reference/metrics/README.md#stellarindex_tls_cert_not_after_unix` — metric reference
- F-0051 audit finding (audit-2026-05-26) — origin
- F-0001 cluster — root disk full could starve Caddy's renewal
