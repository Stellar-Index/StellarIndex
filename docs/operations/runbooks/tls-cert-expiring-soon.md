---
title: Runbook — tls-cert-expiring-soon
last_verified: 2026-08-29
status: current
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

The shipped expression (both trees):

```promql
stellarindex_tls_cert_not_after_unix - time() < 14 * 24 * 3600
and stellarindex_tls_cert_not_after_unix > 0
```

- Sustained for ≥ 1 h (one missed probe is tolerated; sustained drift is not)
- Caddy's journal (`journalctl -u caddy`) may show recent renewal-attempt errors

The `and … > 0` arm is a defensive floor: without it a zero-valued sample would
compute `0 - time()` and fire permanently. With today's producer the gauge is
only ever set from a real leaf `NotAfter`, so the guard never changes the
verdict — but it is part of the shipped rule and belongs in the expression you
paste into Prometheus.

**This alert does NOT cover a dead probe.** `TLSCertNotAfterUnix` is a
`GaugeVec`: it has no series until the first successful probe, and a FAILING
probe deliberately keeps the last-known value rather than zeroing or dropping it
(`internal/obs/metrics.go`). So a probe that is timing out leaves this alert
quiet while the gauge slowly ages into the 14-day window. The liveness signal is
the companion counter:

```promql
sum by (host, outcome) (rate(stellarindex_tls_cert_probe_total{outcome!="ok"}[1h]))
```

The probe runs from the API binary every 6 h
(`TLSCertProbeInterval`, `internal/api/v1/tls_probe.go::RunTLSCertProbe`, with
one immediate probe at startup). A 14-day threshold gives 56 successful probes'
head room.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@<host>

# 1. Confirm the gauge value vs. NOW.
curl -sS localhost:3000/metrics | grep stellarindex_tls_cert_not_after_unix

# 2. Read the actual on-disk cert Caddy is serving.
openssl x509 -in /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/api.stellarindex.io/api.stellarindex.io.crt -noout -enddate

# 3. Check Caddy's renewal log for the most recent attempt.
journalctl -u caddy --since "30d ago" --no-pager | grep -iE "renew|certificate"
```

## Likely causes

1. **ACME rate limit hit.** Let's Encrypt enforces 5 duplicate
   cert/week and 50 certs/account/week. Look for `429` /
   `tooManyCertificatesPerName` in Caddy's journal.
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

The live config on r1 is `/etc/caddy/Caddyfile` — rendered by
`configs/ansible/roles/archival-node/templates/Caddyfile.j2` (19-caddy.yml).
`Caddyfile.api` is a *repo* filename (`configs/caddy/Caddyfile.api`) and does
not exist on the host; posting it would 404.

```sh
# Trigger renewal without touching the cert. Caddy responds to SIGUSR1
# but the safer path is the JSON-RPC admin endpoint.
curl -X POST 'http://localhost:2019/load' --data-binary @/etc/caddy/Caddyfile -H 'Content-Type: text/caddyfile'

# Or reload/restart (renewals attempt at startup):
caddy validate --config /etc/caddy/Caddyfile && systemctl reload caddy

# Watch the renewal attempt:
journalctl -u caddy -f
```

### If Caddy can't renew (rate limited, etc.)

Fall back to certbot's standalone mode or use ZeroSSL via Caddy's
ACME alternate config. The directive that selects an alternate ACME
**directory** is `acme_ca`, inside the site's `tls` block:

```caddyfile
tls {
    acme_ca https://acme.zerossl.com/v2/DV90
}
```

(`acme_ca_root` is a different directive — it supplies a trusted root PEM for
a private ACME endpoint, and pointing it at a directory URL does nothing.)
Codify any such change in `Caddyfile.j2`, not just on the host.
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
- `configs/ansible/roles/archival-node/templates/Caddyfile.j2` — the config
  that renders to `/etc/caddy/Caddyfile`
- `docs/reference/metrics/README.md#stellarindex_tls_cert_not_after_unix` — metric reference
- F-0051 audit finding (audit-2026-05-26) — origin
- F-0001 cluster — root disk full could starve Caddy's renewal

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): the reload
  path is `/etc/caddy/Caddyfile` (ansible `Caddyfile.j2`), not
  `Caddyfile.api`, which only exists in the repo; the symptom expression was
  missing the shipped `and stellarindex_tls_cert_not_after_unix > 0` guard;
  `acme_ca_root` → `acme_ca` (the former is a root-PEM directive, not a
  directory selector); `caddy.log` → the systemd journal; added the
  "this alert does not cover a dead probe" note — probe failures KEEP the
  last-known gauge value (`internal/obs/metrics.go`), so
  `stellarindex_tls_cert_probe_total{outcome!="ok"}` is the liveness signal.
