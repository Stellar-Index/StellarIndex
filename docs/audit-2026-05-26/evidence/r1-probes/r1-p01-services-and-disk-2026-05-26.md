# R1-P01 / R1-P03 / R1-P04 / R1-P05 / R1-P11 — services + disk + listeners

Composite probe covering core health surfaces at audit-start.

## Subject

Confirm: (a) all production services active and on the deployed
version; (b) listening ports inventory matches expectations;
(c) disk usage; (d) load average; (e) stellar-core running only
as captive subprocess of Galexie (per CLAUDE.md 2026-04-23
removal of standalone stellar-rpc/stellar-core).

## When

`2026-05-26T21:45:53Z`

## Where

`root@136.243.90.96` (R1).

## Output (essential extracts)

```
Tue May 26 09:45:53 PM UTC 2026
 23:45:53 up 5 days,  8:08,  1 user,  load average: 23.46, 23.43, 23.13

---DF---
/dev/md1         49G   47G     0 100% /                       ← ROOT FULL
data/archive     13T   25G   13T   1% /srv/history-archive
data/minio       18T  5.3T   13T  31% /var/lib/minio
data/postgres    14T  889G   13T   7% /var/lib/postgresql
data/galexie     13T  9.0G   13T   1% /var/lib/galexie

---ROOT-USAGE---
504M	/var/log/journal/
2.2G	/root/
4.3G	/tmp/
14G	/var/log/           ← BIGGEST CONSUMER

---FAILED-UNITS---
● openipmi.service  (loaded failed failed)
● sla-probe.service (loaded failed failed)        ← FINDING F-0005

---ACTIVE-SVCS---
ratesengine-indexer:    active
ratesengine-aggregator: active
ratesengine-api:        active
galexie:                active
caddy:                  active
postgresql:             active
prometheus:             active
alertmanager:           inactive                  ← FINDING F-0004
loki:                   active
promtail:               active

---LISTENERS---
:22  sshd
:53  systemd-resolve (loopback)
:11726 stellar-core (captive subprocess of galexie ✓ per CLAUDE.md)
:6379  redis (localhost)
:5432  postgres (localhost)
:3000  ratesengine-api (localhost — fronted by Caddy)
:2019  caddy admin (localhost)
:9464  ratesengine-indexer metrics (localhost)
:9465  ratesengine-aggregator metrics (localhost)
:9000  minio (any iface)
:34143 promtail (ephemeral port)
:6061  galexie (any iface)
:3100  loki (any iface)
:443/:80 caddy

---STELLAR-CORE-PROC---
galexie  2333920  9.1  3.8 9133220 7581260 ?     Sl   May22 618:36
  \_ /usr/bin/stellar-core --conf /var/lib/galexie/captive-core/stellar-core.conf
     --console run --metadata-output-stream fd:3
```

## Interpretation

1. **Three core services active** (indexer/aggregator/api).
2. **`/dev/md1` root is 100% full** at 47G/49G — confirms F-0001.
   /var/log is the biggest consumer at 14G. Needs immediate
   investigation: is journalctl rotating? are application logs
   appearing here instead of /data partitions?
3. **`alertmanager` is INACTIVE** — this is a critical
   observability gap. Alerts from Prometheus may have no
   receiver, meaning Rules might be evaluating but no pages/
   tickets/emails get sent. Files F-0004.
4. **`sla-probe.service` failed** — SLA freshness measurements
   are not flowing to Prometheus textfile collector → no SLA
   alert can fire. Files F-0005.
5. **`openipmi.service` failed** — harmless on this hardware
   (no IPMI BMC); note-only.
6. **Load average 23.46** — high (one core per load unit
   typically). Likely from concurrent soroban-events fill walk
   + verify-archive bootstrap walk. NOTE: not an alert
   threshold today.
7. **Listening ports match expected shape**: stellar-core is
   captive (per CLAUDE.md 2026-04-23 invariant) — confirms W6
   check 13 (no standalone stellar-rpc / stellar-core process)
   ✓ promtail's ephemeral :34143 needs explanation in W18.
8. **Data partitions healthy**: postgres 7%, minio 31%, galexie
   and archive 1%. The audit's storage findings concentrate on
   root, not data.

## Disposition

- `claim-confirmed`: services live, captive-core only, data
  partitions healthy
- `claim-contradicted`: root disk full + alertmanager inactive
  + sla-probe failed → 3 findings (F-0001 sev `high`, F-0004
  sev `critical`, F-0005 sev `high`)

## Findings raised

- F-0001 (existing) — root partition 100% full
- F-0004 (NEW) — alertmanager inactive on R1
- F-0005 (NEW) — sla-probe.service failed
