# R1 Live Probe Protocol

Live probes are first-class evidence. Each probe is a transcript
that proves a claim about the running system at a specific
moment in time.

## Access

- SSH: `ssh root@136.243.90.96` (per memory `reference_r1_ssh` —
  hostname doesn't resolve; login as root not ash)
- All probes recorded under
  `evidence/r1-probes/<topic>-<YYYYMMDD>.md` using the template
  at `evidence/r1-probes/_template.md`

## Discipline

1. **Timestamp every probe.** UTC, ISO-8601. The R1 state changes;
   without a timestamp, the evidence is unverifiable.
2. **Exact command + raw output.** No paraphrase. Truncate
   irrelevant noise but never the relevant lines.
3. **State the claim being tested.** Before the command, write
   the claim. After the command, write the outcome.
4. **Per memory `feedback_r1_sql_quoting`:** for any SQL, `scp`
   the file to `/tmp/<query>.sql` then `psql -f /tmp/<query>.sql`.
   Never inline `$$..$$` over SSH — `$$` expands to the shell
   PID and corrupts the query.
5. **Per memory `feedback_no_pipe_through_tail`:** for any
   command whose exit code matters, capture it separately. Don't
   pipe a gate through `tail` — tail's exit 0 masks a real
   failure.
6. **Per memory `feedback_verify_bg_exit_lies`:** background
   tasks report "exit 0" even when the script inside failed.
   Always grep for an explicit success marker.

## Probe Catalogue (Mandatory)

### R1-P01 — Service health

- Subject: are all production services active and on the
  expected version?
- Command (recorded in transcript):
  ```sh
  systemctl is-active ratesengine-indexer ratesengine-aggregator ratesengine-api
  for b in ratesengine-indexer ratesengine-aggregator ratesengine-api ratesengine-ops; do
    /usr/local/bin/$b -version 2>&1 | head -1
  done
  ```
- Claim: all four binaries are at the version recorded in the
  most recent successful deploy.yml run.

### R1-P02 — Timers

- Subject: which timers are armed; when do they next fire; have
  any failed since last reboot?
- Command:
  ```sh
  systemctl list-timers --all
  systemctl --failed --no-pager
  journalctl --boot --no-pager --since="-24h" -p err -t systemd | tail -50
  ```

### R1-P03 — Disk usage

- Subject: any partition >80% used; any growth trend
  abnormal?
- Command:
  ```sh
  df -h
  df -i
  du -sh /var/log/journal/ /var/lib/postgresql/data/pg_wal/ /var/lib/minio/ /var/lib/galexie/
  ```
- **Known at audit-start (2026-05-26 23:14 UTC):** `/dev/md1`
  (root partition, 49G) is 100% full. The data partitions are
  fine.

### R1-P04 — Memory and load

- Subject: memory pressure; swap usage; load average vs CPU
  count.
- Command:
  ```sh
  free -g
  uptime
  cat /proc/loadavg
  cat /proc/swaps
  ps auxf | sort -nrk 3,3 | head -10  # top CPU
  ps auxf | sort -nrk 4,4 | head -10  # top RSS
  ```

### R1-P05 — Listening ports

- Subject: every listener is intentional; nothing extra.
- Command:
  ```sh
  ss -tnlp | sort -k4
  ss -unlp | sort -k4
  ```

### R1-P06 — Postgres health

- Subject: backend count, WAL position, replication state, slot
  state.
- Command:
  ```sh
  set -a; source /etc/default/ratesengine; set +a
  psql "$RATESENGINE_POSTGRES_DSN" -c "SELECT count(*), state FROM pg_stat_activity GROUP BY state;"
  psql "$RATESENGINE_POSTGRES_DSN" -c "SELECT pg_size_pretty(pg_database_size('ratesengine')) AS size;"
  psql "$RATESENGINE_POSTGRES_DSN" -c "SELECT * FROM pg_replication_slots;"
  psql "$RATESENGINE_POSTGRES_DSN" -c "SELECT pg_current_wal_lsn();"
  ```

### R1-P07 — Postgres hypertables + cagg

- Subject: hypertable chunk count, compression coverage,
  retention policy state, continuous-aggregate refresh state.
- Command (via `scp`'d sql file):
  ```sh
  scp <transcript>/hypertable-state.sql root@r1:/tmp/
  ssh root@r1 'psql -f /tmp/hypertable-state.sql'
  ```
- The SQL covers:
  ```sql
  SELECT hypertable_name, num_chunks, compression_enabled
  FROM timescaledb_information.hypertables;
  SELECT job_id, scheduled, last_run_status, next_start
  FROM timescaledb_information.jobs WHERE proc_name LIKE '%compression%';
  SELECT view_name, refresh_lag, last_run_status
  FROM timescaledb_information.continuous_aggregates;
  ```

### R1-P08 — Redis health

- Subject: connected clients, memory usage, eviction policy.
- Command:
  ```sh
  redis-cli INFO clients
  redis-cli INFO memory
  redis-cli CONFIG GET maxmemory-policy
  ```

### R1-P09 — MinIO health

- Subject: bucket sizes, free space, replication state.
- Command:
  ```sh
  mc admin info local
  mc du local/galexie-archive
  mc du local/galexie-live
  ```

### R1-P10 — Galexie health

- Subject: is the writer running? what's the current tip?
- Command:
  ```sh
  systemctl status galexie.service --no-pager
  ls -la /var/lib/galexie/ | head
  curl -s localhost:6061/metrics | grep -E 'galexie_tip|galexie_write'
  ```

### R1-P11 — stellar-core captive process

- Subject: is captive-core running under galexie? (per
  CLAUDE.md: stellar-core only runs as captive subprocess of
  galexie since 2026-04-23, not as a standalone service)
- Command:
  ```sh
  ps auxf | grep -E 'stellar-core' | grep -v grep
  systemctl status stellar-core 2>&1 | head -5  # should NOT be loaded
  ```

### R1-P12 — Caddy / TLS / Cloudflare

- Subject: cert expiry; trusted-proxy list; access log shape.
- Command:
  ```sh
  systemctl status caddy --no-pager -n 5
  cat /etc/caddy/Caddyfile | head -40
  curl -sI https://api.ratesengine.net/v1/healthz | head -10
  echo | openssl s_client -servername api.ratesengine.net \
    -connect api.ratesengine.net:443 2>/dev/null \
    | openssl x509 -noout -dates
  ```

### R1-P13 — Backfill / ingest state

- Subject: which cursors are active; which lag.
- Command:
  ```sh
  scp <transcript>/cursor-inventory.sql root@r1:/tmp/
  ssh root@r1 'set -a; source /etc/default/ratesengine; set +a;
    psql -f /tmp/cursor-inventory.sql'
  ```
- SQL:
  ```sql
  SELECT source, sub_source, last_ledger,
         NOW() - updated_at AS age
  FROM ingestion_cursors
  ORDER BY updated_at DESC LIMIT 30;
  ```

### R1-P14 — soroban_events state (NEW)

- Subject: row count + ledger range; any drop warnings; the
  ON CONFLICT shape works.
- Command:
  ```sh
  ssh root@r1 'journalctl -u soroban-events-fill.service --no-pager -q \
    --since="-24h" | grep -E "dropped|rows dropped|chunk complete" | tail -30'
  ```

### R1-P15 — verify-archive state (NEW)

- Subject: state file high-water-mark; in-progress chunks; last
  successful run.
- Command:
  ```sh
  ssh root@r1 'cat /var/lib/ratesengine/verify-archive-state.json'
  ssh root@r1 'systemctl status verify-archive-tier-a.timer --no-pager -n 3'
  ssh root@r1 'journalctl -u verify-archive-tier-a.service --since="-24h" --no-pager -q | tail -20'
  ```

### R1-P16 — Prometheus + Alertmanager state

- Subject: scrape success per job; active alerts; silenced
  alerts.
- Command:
  ```sh
  curl -s localhost:9090/api/v1/targets | jq '.data.activeTargets[] | {job: .labels.job, health: .health, lastError: .lastError}'
  curl -s localhost:9093/api/v2/alerts | jq '.[] | {labels: .labels.alertname, state: .status.state, startsAt: .startsAt}'
  curl -s localhost:9093/api/v2/silences | jq '.[] | {matchers: .matchers, startsAt: .startsAt, endsAt: .endsAt}'
  ```

### R1-P17 — Loki + log volume

- Subject: log retention; recent error rate.
- Command:
  ```sh
  curl -s localhost:3100/ready
  du -sh /var/lib/loki/
  journalctl --disk-usage
  ```

### R1-P18 — Live API trace

- Subject: real-traffic patterns; 404 rate; p50/p95/p99
  latency over a sample window.
- Command:
  ```sh
  journalctl -u caddy.service --since="-1h" --no-pager -q \
    | awk '{print $NF}' | sort | uniq -c | sort -nr | head -20
  ```

### R1-P19 — TLS cert auto-renew freshness

- Subject: has the cert been renewed in the last 90 days?
- Command:
  ```sh
  ls -la /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/
  journalctl -u caddy.service --since="-30d" --no-pager -q | grep -i "renew" | tail
  ```

### R1-P20 — Customer webhook health (NEW)

- Subject: subscription count; delivery success rate.
- Command:
  ```sh
  scp <transcript>/customer-webhook-state.sql root@r1:/tmp/
  ssh root@r1 'psql -f /tmp/customer-webhook-state.sql'
  ```
- SQL:
  ```sql
  SELECT count(*) AS total_subscriptions FROM customer_webhook_subscriptions;
  SELECT outcome, count(*) FROM customer_webhook_deliveries
   WHERE created_at > now() - interval '24 hours'
   GROUP BY outcome;
  ```

### R1-P21 — Per-source row counts (NEW)

- Subject: are the new per-source tables actually populating
  from live ingest?
- Command:
  ```sh
  scp <transcript>/per-source-counts.sql root@r1:/tmp/
  ssh root@r1 'psql -f /tmp/per-source-counts.sql'
  ```
- SQL:
  ```sql
  SELECT 'soroban_events' AS t, count(*) FROM soroban_events
  UNION ALL SELECT 'cctp_events', count(*) FROM cctp_events
  UNION ALL SELECT 'rozo_events', count(*) FROM rozo_events
  UNION ALL SELECT 'comet_liquidity', count(*) FROM comet_liquidity
  UNION ALL SELECT 'soroswap_skim_events', count(*) FROM soroswap_skim_events
  UNION ALL SELECT 'phoenix_liquidity', count(*) FROM phoenix_liquidity
  UNION ALL SELECT 'phoenix_stake_events', count(*) FROM phoenix_stake_events
  UNION ALL SELECT 'blend_positions', count(*) FROM blend_positions
  UNION ALL SELECT 'blend_emissions', count(*) FROM blend_emissions
  UNION ALL SELECT 'blend_admin', count(*) FROM blend_admin;
  ```

### R1-P22 — Migration application (NEW)

- Subject: has every checked-in migration been applied?
- Command:
  ```sh
  ssh root@r1 'psql -At "$RATESENGINE_POSTGRES_DSN" -c \
    "SELECT version, dirty FROM schema_migrations;"'
  ls migrations/*.up.sql | wc -l
  ```

## Reporting

Each probe gets one file in `evidence/r1-probes/`. Filename:
`r1-p<NN>-<YYYYMMDD>.md`.

A probe is `done` when:
- the transcript is complete
- the claim is either confirmed or contradicted (with a finding
  ID if contradicted)
- evidence is linked back from the relevant workstream

The audit cannot close without at least one full probe transcript
covering each numbered probe above.
