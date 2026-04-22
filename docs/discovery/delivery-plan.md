# 10-week delivery plan

**Status:** ✅ Plan locked — week-by-week calendar with explicit gates,
dependencies, and risk register. **Owned by:** `@ash`, refined with
each weekly review.

**Scope:** maps the [proposal's Phase 1–7](../ctx-proposal.md) against
a concrete 10-week calendar, anchored to the
[RFP requirements matrix](rfp-requirements-matrix.md) and the
[repo structure plan](repo-structure-plan.md).

---

## 1. Calendar assumptions

| Anchor | Date |
| ------ | ---- |
| T+0 — first award payment (nominal) | **2026-04-22** |
| Week 1 window | Apr 22 – Apr 28 |
| Week 10 window | Jun 24 – Jun 30 |
| **Production deadline** | **2026-06-30** |

If the actual payment date slips, **every date below shifts by the
same delta** but the *shape* of the plan does not change. We do not
compress phases in response to a slipped start — we negotiate a new
end date with the customer.

---

## 2. Team & roles

| Role | Owner | Weekly commitment |
| ---- | ----- | ----------------- |
| Tech lead / architect | **@ash** | 100 % |
| Backend engineer | **@alex** | 100 % |
| DevOps / SRE | **@alex** (dual-hatted) | 30 % |
| Docs / DX | **@ash** (shared) | 10 % |
| Customer / stakeholder liaison | **@ash** | 5 % |
| Security review | external (TBD) | spot-hours pre-launch |
| Smart-contract / ecosystem review | **@ash** + community (Discord) | ad-hoc |

Two engineers is lean. The plan assumes no sickness, no parallel
obligations, and a 45-hour commit per week. **Every slip below is
already against this ambitious baseline.**

If a third engineer joins mid-delivery, re-plan §7 parallel tracks
to absorb them.

---

## 3. Week-by-week calendar

### Week 1 — Foundation (Apr 22 – Apr 28)

Proposal Phase 1 (*"Discovery, Architecture Finalization and
Environment Setup"*) + §15 repo-migration of
[repo-structure-plan.md](repo-structure-plan.md).

**We are effectively mid-week-1 on plan landing day.** Most of
"discovery" is done in `docs/discovery/`. What remains:

| Deliverable | Owner | Day | Blocks |
| ----------- | ----- | --- | ------ |
| Initialise `github.com/ctx/ratesengine` | @ash | Day 1 | everything |
| Port `docs/discovery/` → `docs/_archive/discovery/` (verbatim) | @ash | Day 1 | — |
| Extract 5 ADRs (0001 Horizon, 0002 MinIO, 0003 i128, 0004 Tier-1, 0005 monorepo) | @ash | Day 1 | — |
| `Makefile`, `.golangci.yml`, `.github/workflows/ci.yml` | @ash | Day 1 | any PR |
| First `internal/canonical/` package (Trade, Price, Asset, Pair, Amount-as-big.Int) with 95 % coverage | @alex | Days 1–2 | every source module |
| HA infrastructure design docs (10 files under `docs/architecture/infrastructure/`) | @ash | Days 2–3 | Phase 2 deploy prep |
| API spec (`openapi/ctx-rates.v1.yaml` skeleton) | @ash | Days 3–4 | Phase 5 |
| Send [proposal-corrections.md](proposal-corrections.md) to customer | @ash | Day 2 | customer sign-off on Week 2 design |
| Procurement: MinIO cluster sizing PO; cloud account (AWS or GCP pick); domain registration (`ratesengine.ctx.io`); TLS (Let's Encrypt) | @alex | Days 3–5 | Weeks 8–9 |
| Set up secret manager (HashiCorp Vault or equivalent on colo + parallel cloud KMS) | @alex | Day 4–5 | Week 8 |
| Stellar Discord `#validators` + `#api-producers` introductions | @ash | Day 5 | validator track (post-launch) |
| Week-1 customer demo: repo public, docs live, canonical types building | — | Fri | gate → Week 2 |

**Exit gate — Week 1 closes when all are true:**

- [ ] Repo public at `github.com/ctx/ratesengine`, Apache-2.0.
- [ ] `go test ./...` passes on `internal/canonical/`.
- [ ] Docs site preview live at `docs.ratesengine.ctx.io` (staging).
- [ ] Customer acknowledged receipt of proposal corrections.
- [ ] 5 ADRs landed.
- [ ] Cloud account + domain + TLS provisioned.
- [ ] Colo hardware confirmed (existing R640 + plan for #2).

### Week 2 — Ingestion part 1 (Apr 29 – May 5)

Proposal Phase 2a.

| Deliverable | Owner |
| ----------- | ----- |
| **stellar-core watcher** running on colo R640 (`CATCHUP_RECENT`, postgres backend, stellar-core-postgres Debian package) | @alex |
| **Galexie** running alongside, writing to local MinIO single-node | @alex |
| **stellar-rpc** running alongside, reading captive-core | @alex |
| **MinIO single-node** on colo with bucket `galexie-pubnet/` | @alex |
| **TimescaleDB single-node** with migrations scaffold | @alex |
| `internal/consumer/` — the Source interface + its orchestration loop | @ash |
| `internal/extract/` — thin wrapper over `withObsrvr/stellar-extract` v0.1.2 | @ash |
| `internal/sources/sdex/` — SDEX ClaimAtom extraction, 3 variants + 5 op types | @ash |
| First fixture set ported from `stellar-etl/testdata/trades/` | @alex |
| CI integration test harness (testcontainers-go for Postgres + Redis + MinIO) | @alex |

**Exit gate — Week 2:**

- [ ] Live pubnet ledger → Galexie → MinIO → consumer → `trades` table.
- [ ] 24-h backfill range replayed end-to-end without crash.
- [ ] Golden-file SDEX fixtures pass in CI.

### Week 3 — Ingestion part 2 (May 6 – May 12)

Proposal Phase 2b.

| Deliverable | Owner |
| ----------- | ----- |
| `internal/sources/soroswap/` — factory + pair events; swap+sync correlation | @ash |
| `internal/sources/aquarius/` — variable-arity event decoder, plane batch-reader | @ash |
| `internal/sources/phoenix/` — 8-events-per-swap correlator | @ash |
| `internal/sources/comet/` — swap/join/exit decoder | @ash |
| `internal/sources/blend/` — auction + supply events | @ash |
| `internal/sources/reflector/` — REDSTONE-event subscription + 3-contract polling | @ash |
| `internal/sources/redstone/` — Adapter event subscription | @ash |
| `internal/sources/band/` — poll loop (no events) | @ash |
| First external connector: `internal/sources/external/cex/binance/` — WS ticker ingest | @alex |
| `internal/supply/` — classic + SEP-41 supply running sums | @alex |
| Asset-metadata table seeded from SEP-1 stellar.toml resolution | @alex |

**Exit gate — Week 3:**

- [ ] Eight on-chain source packages + one CEX package operating.
- [ ] `trades` hypertable ingesting > 10 trades/sec sustained from
      live pubnet (ballpark of the actual rate at time of test).
- [ ] i128 regression fixtures (including KALIEN-incident case) pass.
- [ ] Supply for XLM, USDC, and three random Soroban tokens within
      1 stroop of an independent calculation (e.g. stellar.expert).

### Week 4 — Aggregation part 1 (May 13 – May 19)

Proposal Phase 3a.

| Deliverable | Owner |
| ----------- | ----- |
| `internal/aggregate/` — VWAP over rolling window | @ash |
| `internal/aggregate/outlier.go` — deviation-based outlier rejection | @ash |
| `internal/aggregate/triangulation.go` — path selection through BTC / USD / XLM / USDC | @ash |
| `internal/aggregate/fallback.go` — TWAP fallback when VWAP thresholds not met | @ash |
| Liquidity-eligibility rules (min USD volume, min source count, spread) | @ash |
| Configurable thresholds via `configs/defaults.yaml` | @ash |
| Per-pair aggregation storage in Redis hot cache | @alex |
| `internal/divergence/` — CoinGecko + CMC + Chainlink-HTTP cross-check with configurable thresholds | @alex |

**Exit gate — Week 4:**

- [ ] VWAP + TWAP + fallback operating on XLM/USD, USDC/USD,
      BTC/USD at minimum.
- [ ] Divergence detector flagging when our price diverges > 2 %
      from CoinGecko/CMC for validation assets.
- [ ] Deterministic replay: same input trades → same aggregated output.

### Week 5 — Aggregation part 2 (May 20 – May 26)

Proposal Phase 3b.

| Deliverable | Owner |
| ----------- | ----- |
| 1000-pair aggregation (all indexed Soroban tokens + classic assets in SDEX) | @ash |
| Outlier / divergence rules tuned against 72-h replay | @ash |
| Cross-asset triangulation validated against direct-pair prices (unit: <0.1 % error on the triangulated path vs. direct) | @ash |
| `internal/sources/external/fx/` — ExchangeRatesApi + ECB as redundant FX anchors | @alex |
| Additional CEX connectors: Coinbase, Kraken, Bitstamp (beyond Binance) | @alex |
| Chaos-test 1: kill a random source, verify aggregation continues with degraded flag | @alex |

**Exit gate — Week 5:**

- [ ] All indexed pairs producing aggregate prices within 30 s of
      latest contributing trade.
- [ ] Per-source P50 contribution latency < 5 s behind ledger tip.

### Week 6 — Historical OHLC (May 27 – Jun 2)

Proposal Phase 4.

| Deliverable | Owner |
| ----------- | ----- |
| TimescaleDB continuous aggregates for 1m / 15m / 1h / 4h / 1d / 1w / 1mo granularities | @alex |
| Retention policies per [stellar-rfp](../stellar-rfp.md): lower granularities capped, 1h+ indefinite | @alex |
| Backfill orchestrator (`ctx-ops backfill --from <ledger> --to <ledger>`) | @ash |
| **Since-inception historical backfill** for top-20 pairs (XLM + major stables + top Soroban tokens). Run in parallel with continuing work — expect multi-day compute | @alex |
| Per-pair historical data published to API-staging for internal testing | @ash |
| OHLC correctness cross-check against Hubble's `history_trades` table for 3 specific ledger ranges | @ash |

**Exit gate — Week 6:**

- [ ] OHLC tables populated for top-20 pairs back to asset inception
      (or as early as data exists on pubnet).
- [ ] Continuous aggregates auto-roll on new trades.
- [ ] API-internal OHLC query < 100 ms for any timeframe.

### Week 7 — API layer (Jun 3 – Jun 9)

Proposal Phase 5.

| Deliverable | Owner |
| ----------- | ----- |
| `internal/api/v1/` — REST handlers for every endpoint in `openapi/ctx-rates.v1.yaml` | @ash |
| `internal/auth/` — API-key auth with per-key quota | @ash |
| `internal/ratelimit/` — Redis-backed token bucket, 1000 req/min default | @ash |
| `internal/api/streaming/` — SSE for near-real-time price subscription | @ash |
| Batch endpoint (bulk asset lookup) | @ash |
| Generated API reference → `docs.ratesengine.ctx.io/reference/api/` | @alex |
| Go client SDK in `pkg/client/` | @alex |
| Self-service onboarding page at `docs.ratesengine.ctx.io/getting-started` | @alex |
| CDN caching for historical endpoints (CloudFront or equivalent) | @alex |

**Exit gate — Week 7:**

- [ ] Public staging URL responds at p95 < 100 ms in dev-load test.
- [ ] OpenAPI spec + published reference + Go SDK all consistent.
- [ ] Customer (Stellar + Freighter) given staging access to test
      against our API.

### Week 8 — Infrastructure hardening (Jun 10 – Jun 16)

Proposal Phase 6a.

| Deliverable | Owner |
| ----------- | ----- |
| Promote TimescaleDB to primary + sync replica (colo) + async replica (cloud) | @alex |
| Redis cluster 3-node + Sentinel | @alex |
| MinIO 4-drive erasure-coded on colo + async replicated to S3 cloud | @alex |
| Second stellar-core + Galexie + stellar-rpc in cloud tier | @alex |
| Traefik reverse proxy + TLS (Let's Encrypt + rotation) | @alex |
| Prometheus + Grafana + Alertmanager deployed + dashboards committed | @ash |
| Structured logging via Loki; alert rules wired to runbooks | @ash |
| Per-endpoint SLO definitions + burn-rate alerts | @ash |
| Backup: Timescale WAL archive to MinIO + off-site; MinIO snapshot to cloud S3 | @alex |

**Exit gate — Week 8:**

- [ ] Kill-a-Postgres drill: replica promotes within 30 s, API unaffected.
- [ ] RPO ≤ 1 h demonstrable via restore test.
- [ ] All alerts have matching runbooks.

### Week 9 — SLA validation (Jun 17 – Jun 23)

Proposal Phase 6b.

| Deliverable | Owner |
| ----------- | ----- |
| k6 `api_steady_state.js` — 1000 req/min × 100 keys × 30 min. Pass: p95 ≤ 200 ms, p99 ≤ 500 ms, error < 0.1 % | @alex |
| k6 `api_ramp_to_saturation.js` — find the cliff. Document cliff location for capacity plan. | @alex |
| k6 `api_spike.js` — 10× burst, recover < 60 s | @alex |
| k6 `ingest_peak_ledger.js` — 5× normal event rate for 1 h | @alex |
| Chaos suite pass: all scenarios in [repo-structure-plan §9](repo-structure-plan.md) | @alex |
| Security review (external or community) on the full stack | @ash |
| SEV-1 / SEV-2 playbook dry-run (kill something, time the response) | @ash |
| Public status page at `status.ratesengine.ctx.io` | @ash |
| Documented SEV-1 / SEV-2 response guarantees + escalation chain | @ash |

**Exit gate — Week 9:**

- [ ] Four load-test scenarios pass acceptance.
- [ ] Chaos scenarios all pass.
- [ ] Security review sign-off (no unaddressed CRITICAL or HIGH).
- [ ] SEV-1 drill response time within RFP bounds.

### Week 10 — Finalization (Jun 24 – Jun 30)

Proposal Phase 7.

| Deliverable | Owner |
| ----------- | ----- |
| Documentation sweep: every runbook verified, every ADR accurate, every config option documented | @ash |
| Published release notes (CHANGELOG.md moved `[Unreleased]` → `[2026.06.30.1]`) | @ash |
| Public announcement + social media | @ash |
| Community channels: Stellar Discord announcement, dev mailing list | @ash |
| Sign-off demo with customer | — |
| **Production cutover** — flip DNS, enable public rate-limit tier | @alex |
| First 24-h post-launch watch (rotating shifts) | @ash + @alex |

**Exit gate — delivery closed:**

- [ ] Public API serving requests.
- [ ] Customer sign-off letter received.
- [ ] All release artefacts tagged, signed, published.
- [ ] Post-launch 24-h incident-free.

---

## 4. Critical path

Dependencies that gate the ship date:

```
Week 1 foundation
    ├── Repo init (→ all PRs)
    ├── canonical types (→ all source packages)
    ├── ctx-proposal corrections → customer (→ any design conflict)
    └── procurement (MinIO, cloud) (→ Week 2 deploy)

Week 2 ingest-1 (SDEX)  ──►  Week 3 ingest-2 (Soroban + CEX)  ──►
    ──►  Week 4-5 aggregation  ──►  Week 6 OHLC  ──►
    ──►  Week 7 API  ──►  Week 8 HA  ──►  Week 9 SLA  ──►  Week 10 ship
```

**Five single points of failure on the critical path**:

1. **Repo init** — blocks everything. Mitigated by doing it Day 1.
2. **Canonical types** — blocks every source package. Mitigated by
   sharing the design across week 1 Day 1–2 so both engineers
   understand it before work spreads.
3. **Galexie backfill throughput** — if it runs slower than expected
   on our hardware, week-6 since-inception backfill slips. Mitigated
   by starting backfill early (runs in background from end of
   Week 2).
4. **CEX API stability** — a Binance or Coinbase breaking change
   during delivery stalls a connector. Mitigated by building ≥ 3
   CEX connectors so any one failing is survivable.
5. **Pubnet protocol upgrade landing mid-delivery** — Stellar could
   ship a new protocol during our window. Mitigated by using SDK
   helpers throughout (the SDK abstracts protocol-version
   differences) and by watching
   `stellar-docs/docs/networks/software-versions.mdx` for
   announcements.

---

## 5. Phase gates — what must be true to cross each week boundary

Summarised from the per-week exit gates:

| From | To | Gate |
| ---- | -- | ---- |
| W1 | W2 | Repo + canonical types + procurement |
| W2 | W3 | SDEX end-to-end + fixtures green |
| W3 | W4 | All 8 on-chain sources + 1 CEX + supply working |
| W4 | W5 | VWAP/TWAP/fallback + divergence detector |
| W5 | W6 | 1000-pair aggregation + triangulation verified |
| W6 | W7 | OHLC + backfill for top-20 pairs |
| W7 | W8 | Staging API + customer test-access |
| W8 | W9 | HA deployment + backup verified |
| W9 | W10 | SLA load + chaos + security review pass |
| W10 | ship | Customer sign-off + 24-h incident-free |

If any gate fails, the options are **narrow scope** (cut a feature),
**extend timeline** (negotiate with customer), or **increase team**
(unlikely given the short window). We do not proceed to the next
week on a red gate.

---

## 6. Risk register

Ordered by probability × impact.

| # | Risk | Prob | Impact | Mitigation |
| - | ---- | ---- | ------ | ---------- |
| R1 | Solo engineer unavailable for a week | Med | High | Swap weeks 4 ↔ 6 (aggregation is smaller than OHLC infra); catch up with overtime + weekend |
| R2 | Stellar protocol upgrade ships mid-delivery | Med | Med | SDK helpers already abstract versions; pin our tested-against version explicitly in CHANGELOG; add upgrade test to CI |
| R3 | Galexie backfill slower than hoped | Med | High | Start backfill early (end of W2); mirror SDF's public GCS bucket as bootstrap before running our own |
| R4 | CEX API breaking change during delivery | Med | Med | ≥ 3 CEX connectors so any single failure is survivable; pin vendor API version in the connector; smoke-test weekly |
| R5 | Load-test p95 fails target | Low | High | Pre-W9 capacity modelling; add cache tier or shard Timescale if needed; worst case request 1-week extension for tuning |
| R6 | Security review raises CRITICAL finding post-W9 | Low | High | Run security audit in parallel throughout (not only W9); use community Discord for early review |
| R7 | Dependency regression (go-stellar-sdk, stellar-extract) | Med | Low | SHA-pin via VERSIONS.md; Dependabot PRs held behind integration-test green |
| R8 | HSM / validator-seed vendor lead time | N/A | — | Validator track is POST-LAUNCH; no blocker on 10-week delivery |
| R9 | Customer requests scope changes mid-delivery | Med | Med | Proposal-corrections note sent W1 establishes "here's what we committed, here's what we're updating." Further changes documented as written amendments |
| R10 | Open-source contributor influx before hardening | Low | Low | Repo public Day 1 but `PROJECT_STATUS.md` banner: "Pre-v1; breaking changes allowed; not yet production-ready" |
| R11 | MinIO + Galexie S3-compat doesn't Just Work | Low | Med | Smoke test W1 Day 5 (before deeper work commits). Fall back: single-node MinIO + cron `s3 sync` to cloud |
| R12 | stellar-rpc SQLite performance at retention > 7 d | Med | Low | Plan already routes historical reads to Timescale, not stellar-rpc; keep RPC at default retention |
| R13 | Customer sign-off held up for reasons outside delivery | Low | High | Weekly check-in touchpoint; flag blockers in customer review 2 weeks out |

**Weekly risk review**: every Friday, walk the table, re-rate
probabilities, add/remove rows, note any newly-discovered risks.

---

## 7. Parallel tracks (not on the critical path)

Work that progresses alongside the week-by-week calendar.

### 7.1. Customer relationship

- **Week 1:** proposal corrections sent.
- **Weekly:** Friday check-in call; share progress notes, risk
  register deltas, any blocking questions.
- **Week 7:** give customer staging API access.
- **Week 10:** sign-off demo + letter.

### 7.2. Validator track (POST-LAUNCH)

Mentioned in [decisions.md](decisions.md) as a 3-6 month post-launch
goal, not part of the 10-week delivery. What starts in parallel now:

- HSM vendor research (YubiHSM 2 vs Nitrokey HSM 2 vs AWS CloudHSM
  vs Google Cloud HSM). Lead times 2-6 weeks.
- Geographic region #2 and #3 for the three-validator trio.
  Research data-centre options (OVH, Equinix, Hetzner, Latitude.sh).
- Discord `#validators` participation — listen, learn norms before
  we apply for inclusion in quorum sets.
- SEP-20 stellar.toml `[[VALIDATORS]]` draft; keep in source control,
  publish only when validators actually come up.

### 7.3. Open-source community

- **Week 1:** repo public with Apache-2.0.
- **Week 2:** Stellar Discord announcement: "we're building this,
  public repo, contributions welcome pre-v1 but no stability
  commitments."
- **Week 6:** blog post on DEX indexing patterns, fixture corpus
  we're using, invite code review.
- **Week 10:** release announcement + call for contributors
  (specifically CEX connector additions).

### 7.4. Procurement

- **Week 1:** cloud account, domain, TLS, MinIO storage, secret
  manager, Slack/Discord for ops.
- **Week 3:** monitoring provider (PagerDuty or OpsGenie) for SEV
  paging.
- **Week 8:** CDN contract if not bundled with cloud provider.

---

## 8. Buffer / contingency

**We have roughly 1.5 weeks of buffer** across the 10-week window:

- Week 1 has overlap with discovery work we've already done
  (~2 days of capacity not allocated to new work).
- Week 10 is mostly wrap; actual "new work" is 0.5-1 day.
- The risk register's R1 scenario (one engineer unavailable for a
  week) is the dominant buffer consumer; any multi-risk collision
  starts eating the actual schedule.

**Buffer deployment rules**:

1. Never use buffer for new scope.
2. Never commit buffer against a probability < 30 %.
3. If W7 exit gate slips, convene a re-plan; don't silently absorb.

---

## 9. Launch readiness checklist (Week 10, Day 1)

- [ ] All runbooks reviewed + verified within last 7 days.
- [ ] On-call rotation set up; first week is @ash + @alex split.
- [ ] Status page published and auto-populated from Prometheus.
- [ ] Post-mortem template written.
- [ ] Public release tag cut, signed, published.
- [ ] docs.ratesengine.ctx.io serving stable URLs with correct
      redirects.
- [ ] API reference auto-regenerated on build.
- [ ] SDK (Go) published on `pkg.go.dev`.
- [ ] Customer sign-off demo scheduled.
- [ ] Pre-announcement blog post reviewed + scheduled.
- [ ] Launch-day social-media posts queued.
- [ ] 24-h post-launch watch plan written with explicit escalation
      chain.

---

## 10. Post-launch (week 11+)

Not part of the 10-week contract but flagged so we don't lose
momentum.

- **Week 11 (Jul 1–7):** stabilisation. Triage any post-launch
  issues. Resist feature work.
- **Week 12+:** validator track begins (Phase-3 per
  [decisions.md](decisions.md)). HSM procurement completes; testnet
  validator spun up.
- **Quarter 3 2026:** three-validator trio on pubnet; apply for
  Tier 1 inclusion in leading quorum sets.
- **Ongoing:** quarterly doc hygiene sweep
  ([repo-structure-plan §6.5](repo-structure-plan.md)), protocol-
  upgrade testing (as Stellar ships new versions), SDK additions
  (JS, Python) per customer demand.

---

## 11. Success criteria (what "done" looks like)

### Contractual (from the RFPs)

- [ ] API uptime ≥ 99.9 % over first 30 post-launch days.
- [ ] p95 latency ≤ 200 ms sustained under 1000 req/min per key
      load.
- [ ] Price data staleness ≤ 30 s for supported assets.
- [ ] Historical endpoints return correct OHLC for every RFP
      timeframe × granularity cell.
- [ ] Since-inception data available for XLM + USDC (minimum).
- [ ] Full repo public, Apache-2.0, self-hostable via documented
      `docker-compose up`.

### Architectural (our standards)

- [ ] Every RFP requirement ticked in
      [rfp-requirements-matrix.md](rfp-requirements-matrix.md).
- [ ] Every audit doc's claims still verifiable against the
      production code.
- [ ] Every ADR still current or explicitly superseded.
- [ ] No CRITICAL or HIGH security findings open.
- [ ] Load-test artefacts (k6 results) committed to
      `test/load/results/<date>.json` and published.

### Reputational

- [ ] At least one external contributor landed a merged PR
      pre-launch.
- [ ] At least one Stellar ecosystem participant publicly endorsed
      the launch.
- [ ] No production incident in first 72 h post-launch.

---

## 12. Next steps after this doc lands

Per the sequence in our earlier plan:

1. ✅ Discovery (`docs/discovery/`).
2. ✅ Repo structure plan ([repo-structure-plan.md](repo-structure-plan.md)).
3. ✅ Delivery plan (this doc).
4. **Next: HA infrastructure design round** — 10 sub-docs in
   `docs/architecture/infrastructure/` (scaffold already at
   [infrastructure/README.md](infrastructure/README.md)).
5. **Then: API endpoint specification round** — `openapi/ctx-rates.v1.yaml`
   skeleton + endpoint-by-endpoint design doc.
6. **Then: Day 1 of Week 1** per this plan — repo initialisation.

The plans (3 + 4 + 5) land **before** code. Every line of production
code ships against a concrete design.

---

## 13. Living document — rules

- **Every Friday:** update this doc with actual vs planned, risk
  deltas, gate-status ticks.
- **On every gate slip:** re-plan and note the adjustment here.
- **At W10 end:** archive the final version to
  `docs/_archive/delivery-plan-2026-q2.md` with post-mortem notes.

Audit trail: `git log` on this file is the project's chronological
ground truth.
