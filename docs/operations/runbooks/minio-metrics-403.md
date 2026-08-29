---
title: Runbook — MinIO Prometheus scrape returns 403
last_verified: 2026-08-29
status: current
severity: P2
---

# Runbook — MinIO Prometheus scrape returns 403

## At a glance

| Field | Value |
| ----- | ----- |
| Symptom | Prometheus targets API shows `minio` job `down` with `lastError: server returned HTTP status 401 Unauthorized` or `403 Forbidden`. MinIO answers 401 for a missing/empty bearer file and 403 for a token whose service account lacks `admin:Prometheus`; both land here. |
| Severity | P2 (ticket) |
| Detected by | `stellarindex_minio_exporter_down` (`deploy/monitoring/rules/meta.yml` + the r1 overlay: `up{job="minio"} == 0 OR absent_over_time(up{job="minio"}[5m]) == 1`, `for: 2m`, **`severity: page`**). Its `runbook_url` points at [exporter-down.md](exporter-down.md) — that is the alert's first-response page; come here for the token-provisioning procedure. |
| Typical MTTR | 10 minutes (provision token + restart Prometheus) |
| Impact | MinIO observability gap: no bucket-usage, replication, or write-latency metrics scraped. Operator can't alert on disk exhaustion of the MinIO data partition until the token is wired. |

## Why this happens

MinIO's `/minio/v2/metrics/cluster` endpoint requires a bearer
token by default. `configs/prometheus/prometheus.r1.yml` already
points Prometheus at the right URL with
`bearer_token_file: /etc/prometheus/minio.token`, but the token
file isn't created automatically — it's an operator-mint step. If
no token has ever been provisioned, every scrape is rejected
(401 with no/empty token, 403 with an under-privileged one) and
the job stays `down`.

Because MinIO is where galexie writes ledger metadata and the
indexer reads it back (ADR-0002), a `down` MinIO target also makes
every alert that depends on MinIO cluster metrics silently blind —
which is why the detecting alert is a page, not a ticket. See
`stellarindex_minio_exporter_down`'s own description in
`deploy/monitoring/rules/meta.yml`.

This is finding F-0045 / task #38 of audit-2026-05-26.

## Provisioning procedure

### 1. Mint a service account on MinIO

SSH to r1 and run `mc` against the local MinIO server. Replace
the placeholder host alias `local` with whatever
`/root/.mc/config.json` calls it (default `local`).

```sh
ssh root@136.243.90.96
mc admin user svcacct add local "<MINIO_ROOT_USER>" \
  --policy prometheus-read \
  --name "prometheus-metrics-scrape" \
  --description "Bearer-token scrape for /minio/v2/metrics/cluster"
```

If the `prometheus-read` policy doesn't yet exist, create it:

```sh
cat > /tmp/prometheus-policy.json <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["admin:Prometheus"],
      "Resource": ["arn:aws:s3:::*"]
    }
  ]
}
EOF
mc admin policy create local prometheus-read /tmp/prometheus-policy.json
rm /tmp/prometheus-policy.json
```

The `svcacct add` command prints a `Secret Key` — that's the
bearer token. Copy it; you only see it once.

### 2. Write the token file

```sh
# On r1, as root.
umask 077
echo "<the-secret-key>" > /etc/prometheus/minio.token
chown prometheus:prometheus /etc/prometheus/minio.token
chmod 0400 /etc/prometheus/minio.token
```

The token file is `0400` owned by `prometheus:prometheus` so
only Prometheus can read it. Confirm:

```sh
ls -l /etc/prometheus/minio.token
# -r-------- 1 prometheus prometheus 40 May 28 09:00 /etc/prometheus/minio.token
```

### 3. Reload Prometheus

```sh
systemctl reload prometheus
```

Wait one scrape interval (15-30 s) and check the target health:

```sh
curl -sS http://localhost:9090/api/v1/targets \
  | jq '.data.activeTargets[] | select(.labels.job=="minio") | {health, lastError}'
```

Expected:

```json
{
  "health": "up",
  "lastError": ""
}
```

### 4. Confirm metrics flow

```sh
curl -sS http://localhost:9090/api/v1/query \
  --data-urlencode 'query=minio_cluster_capacity_usable_total_bytes' \
  | jq '.data.result | length'
# Expect: > 0
```

If the result is `0`, MinIO accepted the token (no 403) but isn't
returning metrics — usually means the `prometheus-read` policy
needs an additional permission. Re-check the policy JSON above
against the upstream MinIO docs.

## Failure modes

- **Still 403 after token file written.** The token doesn't
  match the service account that was minted, OR the
  service-account policy doesn't include `admin:Prometheus`. Re-mint
  the svcacct (it's free to do — just keep one alive at a time)
  and try again. Confirm the policy is attached via
  `mc admin user svcacct info local <svcacct-access-key>`.
- **Prometheus permission denied on the token file.** Symptom:
  `lastError: error reading bearer token file ... permission
  denied`. Fix: ownership (`chown prometheus:prometheus`) and
  mode (`0400`).
- **Scrape times out.** MinIO under heavy load can take >5 s to
  emit the metrics page. Bump `scrape_timeout` on the `minio`
  job in `prometheus.r1.yml`; reload Prometheus.
- **Service account revoked / token rotated.** Mint a new one and
  re-run steps 2-3.

## Long-term: Ansible

This procedure is currently manual: **no Ansible task owns
`/etc/prometheus/minio.token`.** Verified 2026-08-29 —
`configs/ansible/roles/archival-node/tasks/09-minio.yml` never
mentions the token, and nothing else in `configs/ansible/`
templates it. (The scrape stanza in `prometheus.r1.yml` used to
claim otherwise; that comment was corrected in the same pass.) The
same gap is recorded in
[credential-rotation.md](../credential-rotation.md#prometheus-bearer-token-regen-minio-root-rotation-only),
which notes the 2026-07-03 incident where rotating the MinIO root
password invalidated the bearer token by hand.

When the gap closes, the token-mint step lives in `09-minio.yml`
and the manual procedure here becomes a fall-back.

Until then, `make verify-r1-sync` will surface any drift between
the `prometheus.r1.yml` scrape config and the running daemon's
view — but it can't generate the token file itself. That's an
operator step every time the MinIO svcacct rotates.

## Related

- ADR-0002 — self-hosted storage is S3-compatible (MinIO is the
  default).
- F-0045 (audit-2026-05-26) — original finding.
- `configs/prometheus/prometheus.r1.yml` — the `job_name: minio` scrape stanza.
- F-0152 closure — sibling exporters (redis / postgres /
  pgbackrest) now installed; MinIO is the last one waiting on
  this manual token step.
- [exporter-down.md](exporter-down.md) — where
  `stellarindex_minio_exporter_down` routes; its per-exporter notes
  carry the day-to-day `Authorization: Bearer $(cat
  /etc/prometheus/minio.token)` probe.
- [credential-rotation.md](../credential-rotation.md) — regenerating the
  bearer token after a MinIO **root** rotation.

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): "Detected by"
  named operator inspection / a bare `up{job="minio"} == 0` — the real detector
  is the P1 `stellarindex_minio_exporter_down` (page, `for: 2m`, with an
  `absent_over_time` arm), whose `runbook_url` routes to `exporter-down.md`;
  the symptom is 401 **or** 403 depending on whether the token is missing or
  under-privileged; the Ansible-gap section re-confirmed (no task owns
  `/etc/prometheus/minio.token`) and the stale "rendered by ansible" comment in
  `configs/prometheus/prometheus.r1.yml` corrected in the same pass.
- 2026-05-28 — initial draft (F-0045 procedure documentation).
