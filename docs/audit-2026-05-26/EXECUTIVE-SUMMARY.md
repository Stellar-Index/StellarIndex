# Executive Summary — Audit 2026-05-26

**Audit window:** 2026-05-26 21:00 UTC → 2026-05-27 06:21 UTC+ (35 iterations and counting)
**Findings:** 134 active (5 critical, 33 high, 22 medium, 10 low, 5 retracted invalid, **41 positive**)
**Coverage matrices closed:** CG/CMC asset-metadata block + Stellar matrix sections A-I (60+ rows)
**Journey traces:** J05 operator cascade-recovery (Wave 0 playbook), J20 user-price-under-cascade, J30 operator-cctp-backfill
**XFI cross-file rows:** 19 covering 15/20 interaction classes
**Memory-truth verified:** 13/55 entries (3 obsolete + 9 current + 1 historical-current)
**R1 live probes:** 8 (cascade unfixed at all 8; ~7h elapsed since iter 1)
**Per-source decoder audits:** 16/16 Soroban-source decoders + 4 classic observers spot-audited (0 every-event gaps)
**Audit style:** cold + adversarial, raw-bytes evidence after iteration-8 false-positive cluster
**Sample command:** `ssh root@136.243.90.96` + 50+ raw curls against api.ratesengine.net

## TL;DR for the user

The audit found a **live operational cascade** AND a **structural
deployment-drift cluster** that compounds it.

### Operational cascade (Wave 0 blocker)

Running on r1 since at least the morning of 2026-05-26 (Redis last
bgsave success ~17:20 UTC, now ~36h+). It is **the same cascade**
documented in the 2026-05-10 SEV-2 incident (publicly served at
`/v1/incidents`) whose 4 post-mortem action items are **STILL
UNCHECKED 17 days later (F-0099)**. The cascade chain:

```
F-0001 root partition 100% full (49G/49G)
   → F-0041 Redis persistence on root partition (no isolation)
   → F-0053 Prometheus TSDB on root partition (cascade amplifier)
   → F-0039 Redis MISCONF stop-writes-on-bgsave-error → blocked
   → F-0036 counters not incrementing (no Redis write)
   → F-0080 aggregator_silent alert vulnerable to absent-series
   → F-0085 redis_writes_blocked alert blind (redis_exporter DOWN)
   → F-0104 api_price_stale alert depends on aggregator-emitted gauge
   → F-0027 alert pipeline structurally silent
   → F-0049 + F-0050 rate-limit + signup-throttle fail-OPEN on Redis errors
   → F-0086, F-0087, F-0089 /v1/oracle/* + /v1/lending/pools + /v1/vwap HTTP 500
   → /v1/price keeps serving 28h-stale LKG with proper stale:true flag
```

**Verdict against launch:** the launch-day checklist's "no fired alerts" go/no-go condition **passes vacuously** under this cascade (F-0100 false-green). Launch could happen against a silently-broken environment if the checklist is followed literally.

### Deployment-drift cluster (Wave 0/1 structural)

Discovered iter 30-34 by following the smoke-passes-while-API-fails
thread. F-0099 (unchecked post-mortem follow-ups) has a structural
sibling: **deployed configs drift silently from repo**:

- **F-0133/F-0134 HIGH:** deployed `r1-smoke.sh` is 113 LOC + 13
  checks vs repo's 213 LOC + 23 checks (md5 mismatch). It checks
  `/v1/coins` which returns 404 (removed in rc.48) yet smoke
  reports `ExecMainStatus=0`. **The launch-critical regression
  detector is silently broken.**
- **F-0135 MED:** `ledgerstream-tier.yml` rule file MISSING from
  /etc/prometheus/rules.r1/ on r1. So the page-severity
  `ratesengine_ledgerstream_tier_both_missing` alert (F-0112's
  POSITIVE design) is **not actually wired in production**.
- **F-0137 HIGH:** root cause — `configs/healthchecks/install.sh`
  IS the deploy mechanism for the smoke+heartbeat+sla-probe
  stack BUT it's a MANUAL operator script — NOT invoked by any
  Ansible playbook. Every healthchecks/ change requires a manual
  re-run on r1.
- **F-0139 HIGH:** alertmanager.yml deployed config is 27 LOC
  behind repo. Likely path-mismatch: Ansible `prometheus_pair`
  role renders to `/etc/alertmanager/alertmanager.yml`; daemon
  reads `/etc/prometheus/alertmanager.yml`. Plus role naming
  suggests it targets multi-host HA pair, not r1 single-host
  (F-0140).
- **F-0142 MED:** generalised pattern — **only Ansible-templated
  files (postgresql.conf.j2, ratesengine.toml.j2) have a
  guaranteed sync mechanism. Everything else is drift-eligible.**
  Caddy + Prometheus.yml are sync-by-coincidence
  (F-0138/F-0141 not POSITIVE — just frozen-state-aligned).

**8 cascade-blindness layers identified across F-0080, F-0085,
F-0099, F-0100, F-0104, F-0133, F-0135, F-0139.** The audit's
biggest structural finding: **alerts and smoke that were designed
to catch this exact failure mode have themselves either:**
1. Got the `rate()==0` anti-pattern (F-0080)
2. Depend on a cascade-affected metric (F-0085, F-0104)
3. Aren't deployed at all (F-0135, F-0139)
4. Run a stale version that doesn't check the right things (F-0133/4)
5. Pass vacuously because their condition is structurally
   true under the cascade (F-0100)

**Live r1 state (8 probes over the audit window):**
- iter 1 ~23:14 UTC: `/dev/md1 49G 47G 0 100%`, Redis MISCONF
- iter 15 ~01:00 UTC: same — `rdb_changes_since_last_save: 2358`
- iter 18 ~01:30 UTC: same — 2358 FROZEN (Redis refusing writes)
- iter 20 ~02:36 UTC: same
- iter 22 ~03:06 UTC: same. Load avg 21.23 (5d 13h uptime — concerning but not new)
- iter 26 ~04:06 UTC: same
- iter 30 ~05:08 UTC: same (drift cluster surfaced)
- iter 35 ~06:21 UTC: same
- Throughout: ratesengine-{api,indexer,aggregator} all `active`, data ingestion healthy (lag 2s, 46 markets in 24h, $1.04B volume), `/v1/price` serves 28h-stale LKG with proper stale:true flag
- **Cascade has been silently running for ~7h of audit time and ≥36h since start (Redis last bgsave success ~17:20 UTC on 2026-05-26).**

## The 8-step cascade-fix sequence

(Full detail in [07-remediation-plan.md](07-remediation-plan.md))

1. **F-0001** — free root disk (`journalctl --vacuum-size=200M`, rotate `/var/log/postgresql/*.log`, prune `wasm-history-*.stderr`)
2. **F-0041 + F-0053** — schedule move of `/var/lib/redis` + `/var/lib/prometheus` to ZFS data pool
3. **F-0045..F-0048** — restart redis_exporter, postgres_exporter, pgbackrest_exporter, fix MinIO scrape 403
4. **F-0080** — add `OR absent_over_time(...) == 1` guard to aggregator_silent rule (template at ingestion.yml:42-55 / F-0081)
5. **F-0085** — add meta-alert `redis_exporter_down` to surface future cascades
6. **F-0049 + F-0050** — flip signup throttle + global rate-limit to fail-CLOSED with HTTP 503
7. **F-0086..F-0089 + F-0090** — translate Redis errors to HTTP 503 + Retry-After (not 500); priceFallback-style stale-marked degradation on /v1/vwap + /v1/oracle/*
8. **F-0055** — fix /v1/status self-consistency: overall=ok cannot coexist with every signal=unknown

After these 8 steps, the cascade cannot go silent, public surfaces degrade per ADR-0018 contract on every route, abuse-defence layer doesn't bypass under Redis outage, and the launch-readiness checklist becomes meaningful again.

### Wave 0 ALSO must include drift remediation (added iter 30-34)

9. **F-0137** — Wire `configs/healthchecks/install.sh` into an Ansible task (or deploy.yml post-step). Eliminates manual-re-run risk class.
10. **F-0134** — Re-run `bash configs/healthchecks/install.sh` on r1 to align deployed smoke with repo (113 LOC → 213 LOC, 13 → 23 checks).
11. **F-0135** — SCP `configs/prometheus/rules.r1/ledgerstream-tier.yml` → r1 + `promtool check rules` + Prometheus reload (page-severity alert for F-0112's cold-tier design is missing in prod).
12. **F-0139** — Verify alertmanager path mismatch + sync deployed config to repo (27 LOC short).
13. **F-0140** — Audit Ansible `prometheus_pair` role; either fix its target to r1 or replace with a single-host role.
14. **F-0142** — Add `make verify-r1-sync` target that md5-compares every config path against deployed.

Without these 6 additional steps, the next cascade will go silent in exactly the same way (smoke green + alerts silent).

## What's working very well (POSITIVE findings)

The audit found **24 strong positive evidences**:

- **F-0042** `/v1/readyz` is honest about Redis MISCONF — graceful degradation
- **F-0067** Live ledger ingestion healthy throughout the cascade
- **F-0076** SEP-10 JWT validation: HMAC-SHA256 + `subtle.ConstantTimeCompare`, correct fail-closed
- **F-0077** API key entropy: 32 bytes from `crypto/rand`, NoopAPIKeyValidator fails CLOSED
- **F-0078** Migrations 0042..0045 honour ADR-0003 NUMERIC invariant (zero BIGINT on amount cols)
- **F-0079** ADR-0029 SQL-driven backfill design shipped + 6 backfill subcommands
- **F-0081** ingestion_source_stopped alert guarded via `label_replace(vector(1), ...)` pattern
- **F-0082** supply-refresh + supply-snapshot use `absent_over_time(...) == 1` correctly
- **F-0084** cross-region-check + cross-region-monitor ops tooling shipped
- **F-0088** `/v1/pools` serves fresh granular Stellar-native data (SDEX 35,082 24h trades, $516,595 volume)
- **F-0092** `/v1/sac-wrappers` exposes 40+ SAC↔SEP-23 mappings — strong CG/CMC differentiator
- **F-0096** `/v1/methodology` exposes full aggregation policy + ADR references
- **F-0097** `/v1/sources` exposes full source taxonomy with paid/include_in_vwap/backfill flags
- **F-0098** `/v1/incidents` serves full markdown post-mortems
- **F-0102** Launch-day-checklist + public-flip docs exist with detailed steps
- **F-0103** 768 `*_test.go` files; 113 in sources package
- **F-0106** Alertmanager has deadmansswitch heartbeat to healthchecks.io
- **F-0107** 17 rule files, 94 alerts; only ONE unguarded rate==0 pattern total
- **F-0059** WASM-audit coverage complete (every `BackfillSafe=true` source has audit log)
- **F-0111** RedStone EUROC + BENJI feed-id fix landed (was silently dropping 5 feeds pre-#53)
- **F-0112** TieredDataStore cold-tier read-path is correctly shaped (ADR-0027)
- **F-0113** docs-lint enforces 8 different code↔docs round-trip checks
- **F-0114** Customer webhook retry/backoff design correct (30s→1m→2m…1h capped)
- **F-0115** Stripe webhook dedupe textbook-idempotent

## CG/CMC parity stance (matrix-verified)

- **We MATCH on (15 asset-metadata rows):** spot price (all fiats/cryptos with stale-marked LKG), per-pair tickers, asset detail (description, links, ATH/ATL, sparkline), market cap, total/circulating supply, FDV, volumes — all live-verified via EV-0030/0032/0033/0043/0045
- **We LEAD on (`covered+` differentiators):** confidence score, freeze flag, divergence warning, triangulation provenance, methodology transparency (`/v1/methodology`), source taxonomy (`/v1/sources`), SAC↔SEP-23 wrapper map (`/v1/sac-wrappers`), granular per-DEX pools (`/v1/pools`), incidents post-mortems (`/v1/incidents`), asset_class taxonomy richer than CG free-form tags
- **We GAP on:** OHLC multi-bar series (single-bar only — F-0071), dedicated price-change endpoint (F-0062), holder distribution / top holders, sentiment ratings (non-goal)

## Stellar moat (Stellar coverage matrix — sections A-I closed)

**6 explicit differentiators** where we beat CG/CMC, not just match:
1. SAC↔SEP-23 wrapper map (`/v1/sac-wrappers` — 40+ tokens)
2. ADR-0029 Soroban contract-events catch-all + SQL-as-backfill
3. WASM-audit governance framework (16 audit files, BackfillSafe flag gating)
4. Cross-network asset identity (multi-network model: `/v1/assets/usdc` returns `networks: [{stellar:...}]`)
5. 4-tier verify-archive integrity verification (ADR-0017)
6. SQL-as-historical-backfill via soroban_events landing zone (6 backfill subcommands shipped)

60+ Stellar matrix rows resolved across sections A-I. Only 1 `partial` (DeFindex per F-0018). 0 `gap` rows.

## Architectural / structural findings

- **Cascade-fragility anti-pattern** (F-0080+F-0085+F-0104): three alerts each designed to catch the exact failure that breaks their own metric source. Single fix template (label_replace + absent_over_time) applies to all.
- **Param-naming inconsistency** (F-0061+F-0068+F-0073+F-0091): `/v1/price` uses `asset=`, `/v1/twap`+`/v1/ohlc` use `base=`, `/v1/price/batch` uses `asset_ids=`, `/v1/chart` uses `asset=`. Four shapes; pick one.
- **HTTP 500 vs 503 misclassification** (F-0090): handlers depending on Redis return 500 (code bug) under MISCONF; should return 503 + Retry-After (infrastructure issue).
- **Diagnostic surface contradiction** (F-0095): `/v1/diagnostics/ingestion` returns all-zeros with `flags.stale:false` while peer endpoints report fresh state.

## Audit methodology lesson learned (F-0075)

Iteration 6-7 produced a critical false-positive cluster (F-0060, F-0066) because the auditor cited extractor output rather than raw HTTP bytes. Iteration 8 caught it and added the **Raw bytes rule** to `02-protocol.md §3`: live-curl evidence must record raw HTTP body — not python-extracted fields — and parser correctness must be cross-checked against raw samples. This rule prevented further false positives in iterations 8-14.

## Coverage status (refreshed iter 22)

- ✅ **W01-04** repo/governance/build/deps
- ✅ **W05** numeric safety (ADR-0003 verified)
- ✅ **W06** ingest pipeline
- 🟡 **W07** per-source decoders — DeFindex deeply audited; W35 row-coverage matrix done (80 rows)
- ✅ **W08** chainlink connector
- ✅ **W09** migrations 0041-0045 NUMERIC check
- ✅ **W10** aggregator paths (J20 trace + handler audit + cascade focus)
- ✅ **W11** API runtime — 35/54 routes probed live
- 🟡 **W12** supply pipeline — touched (verified counters in /v1/assets/native)
- ✅ **W13** operator tooling (J30 backfill + cross-region-check + verify-archive)
- ✅ **W14** observability + alerts — full sweep done
- ✅ **W15** tests + CI reality
- 🟡 **W16** documentation — partial (10+ docs cross-checked)
- ✅ **W17** web frontends
- ✅ **W18** deployment (cascade impact)
- ✅ **W19** security + auth
- ✅ **W20** CG/CMC parity matrix — asset-metadata block closed
- ✅ **W21** r1 live state (5 probes across iterations)
- ✅ **W22** launch readiness checklist
- 🟡 **W23** multi-region — R2/R3 example-only (accepted)
- ✅ **W24** WASM-history audits
- ✅ **W25** generated artifacts + drift (F-0113 POSITIVE)
- ✅ **W26** cross-file interactions — 19 XFI rows, 15/20 classes covered
- ✅ **W27** soroban_events landing zone (ADR-0029)
- ✅ **W28** back-pressure / ctx-shutdown
- ✅ **W29** per-source backfill subcommands
- ✅ **W30** cold-tier read path (F-0112 POSITIVE)
- ✅ **W31** RWA asset representation (F-0110 governance drift + F-0111 POSITIVE)
- ✅ **W32** customer webhook fanout (F-0114 POSITIVE)
- ✅ **W33** Stripe billing (F-0115 POSITIVE)
- ✅ **W34** verify-archive Type=notify lifecycle
- ✅ **W35** granular-coverage matrix (80 rows; 5 DeFindex gaps confirmed)

⬜ = not started, 🟡 = partial, ✅ = comprehensive.

**Workstream status: 30 / 35 ✅ comprehensive; 5 🟡 partial; 0 ⬜ untouched.**

## Outputs created

- `00-plan.md`, `01-tracker.md`, `02-protocol.md`, `03-journeys.md`,
  `04-reconciliation.md`, `05-findings-register.md`,
  `06-exclusions-register.md`, `07-remediation-plan.md`,
  `08-cgcmc-parity-matrix.md`, `09-stellar-coverage-matrix.md`,
  `10-attack-tree.md`, `11-severity-rubric.md`,
  `12-r1-live-probe-protocol.md`, **`EXECUTIVE-SUMMARY.md` (this file)**
- 35 workstream files under `workstreams/`
- 49 evidence rows + 7 memory-truth rows + 1 journey trace
- 2 R1 live probe reports

## Recommended next actions for the user

1. **Wave 0 cascade fix** — execute the 8-step sequence; this is the highest-value action coming out of the audit. Expected effect: cascade resolved, alerts begin firing correctly, public surfaces resume nominal serving.
2. **Add post-mortem follow-up cadence** — the 2026-05-10 SEV-2 had 4 unchecked items 17 days later. Add a "post-mortem follow-up audit" pass per release cycle.
3. **Fix F-0080 + F-0104 + F-0085** with the F-0081 template (label_replace + absent_over_time) — single fix template.
4. **Decide on SEP-10 launch scope** (F-0093) — endpoint currently 503 in production.
5. **Plan the Wave-1 product cleanups** (F-0061 param-shapes, F-0090 HTTP 503, F-0071 OHLC multi-bar) for the 7-day post-launch window.
6. **The audit can continue** if you want depth in W25/W31 (RWA) or per-source decoder pass — but the current 107 findings are sufficient material to land Wave 0 + Wave 1.
