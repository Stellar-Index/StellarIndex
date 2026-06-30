---
title: Audit 1 — Cold system/site audit — PLAN
status: planning (pass 1)
---

# Audit 1 — Cold system/site audit — PLAN

Goal: an adversarial, file-by-file, interaction-by-interaction audit of the
whole Stellar Index system — code, data integrity, infrastructure, CI/CD,
documentation, and the two web surfaces — that **exceeds** every prior audit.

See [README](README.md) for severity rubric, finding-ID scheme (`CS-###`), and
the adversarial stance.

## How this audit EXPANDS the prior passes

Prior audits (2026-04-29, 05-02, 05-12(+codex), 05-26, 06-11, 06-14, page-06-19,
seo-06-20) are the baseline. This pass must add net-new coverage, specifically
in the angles prior passes under-weighted. _(Filled from baseline-gap analysis —
see "Baseline + gap analysis" below.)_

Expansion principles:
- **Re-derive, don't trust.** Where a prior audit marked something "fixed,"
  re-verify against current `main` (remediations drift; reopened bugs are real).
- **Go one layer deeper.** Prior passes mostly audited Go correctness. This pass
  adds: frontend (web/explorer) security + correctness, CI/CD supply chain,
  secret/dependency posture, SSE/streaming lifecycle, rate-limit correctness
  under concurrency, the ClickHouse lake DDL, ansible role correctness, and
  cross-package data-flow integrity end-to-end.
- **Hunt the recurring root-cause classes** the prior audits named (coarse-PK,
  config-tag drift, dead alerts, doc drift, test rot) — plus new classes this
  pass defines.

## Baseline + gap analysis

Nine prior audits (2026-04-29 → 06-14 + page/SEO) reviewed. Full synthesis in the
pass-4 notes; the operative conclusions:

### Already WELL-covered → re-VERIFY only (don't re-litigate)
i128/ADR-0003 numeric discipline; secrets-in-tree (vault-encrypted, gitleaks);
on-chain decoder *event coverage* (every-event TSV); aggregation math
(VWAP/TWAP/outlier/stablecoin late-bind); API-contract chain (OpenAPI↔handler↔
client↔smoke, drift-linted); auth/SEP-10 *primitives* (32-byte keys, constant-
time compare, SETNX replay guard, GETDEL single-use); webhook SSRF/HMAC/queue-
race; Stripe idempotency; migration PK-grain (0057-0060 + lint); dead-alert
wiring (lint-metric-refs); dependency/CVE posture. → For these, this pass only
**re-verifies the fix still holds on current `main`** (remediations drift) and
hunts the recurring classes; it does NOT re-deep-audit.

### UNDER- or NEVER-covered → THIS PASS OWNS (priority order)
1. **CF Pages Edge Functions** (`web/explorer/functions/*`, shipped 2026-06-24
   *after every prior code audit) — server-side edge code doing live-price fetch +
   OG render + asset proxy = **never audited** SSRF/injection/cache-poison surface.
   → folds into A26/A30. **TOP PRIORITY.**
2. **ClickHouse lake round-trip + DDL** — ADR-0034 source-of-truth with **ZERO
   integration coverage** (06-14 A20, 06-11 F-1349); `explorer_reader.go` CH reader
   untested. → A11. **P0.**
3. **Multi-tenant authorization / IDOR** — `internal/platform`(+postgresstore,
   ~0 tests) + dashboard + `/account/admin` never systematically tested for
   cross-tenant read/write. → A18. **P0.**
4. **Data-correctness AT SCALE** — prior Q4 was *one sample* and already found
   **XLM circulating supply +48.1% / market-cap +47.9% vs CoinGecko**. "Code-
   correct ≠ data-correct" is the biggest under-mined seam. → A34 + a new live-
   reconciliation sweep (per-asset supply, 24h vol/protocol, OHLC vs exchanges).
5. **Dynamic techniques nobody ran:** adversarial XDR **decoder fuzzing** (A2/A4/
   A5; permanently excluded by every prior audit), **SSE slow-consumer/leak tests**
   (A21), **k6 + chaos against the CURRENT ClickHouse-era build** (A34/A35).
6. **Per-source Soroban decode *fidelity*** — only DeFindex was deep-audited; the
   other ~11 sources were row-classified. → A4/A5.
7. **Classic/SAC supply observers** (`accounts/trustlines/claimable_balances/
   liquidity_pools/sac_balances`) — repeatedly punted. → A6.
8. **Accessibility (WCAG)** — one unverified pass (06-14 Q3: 1 Critical + 7
   Serious, remediation untracked); **zero frontend tests exist**. → Audit 2 a11y +
   A30 (confirm Q3 fixes landed).

### Recurring root-cause classes (the 14 → mapped onto CC-1…CC-7)
Prior audits named 14 recurring classes; they compress into this plan's cross-
cutting hunts: positional/shape assumptions (CC-4/CC-5), coarse-PK silent-loss
(A12), config-tag drift (CC-2/A23), split-brain stores (CC-1), dead observability
(A27), doc/comment drift (CC-7/A32), PR-only-CI-vs-commit-to-main (A28), test rot
(A34), recover/coverage asymmetry (CC-3), copy-paste correctness logic (new: add
to CC-1 hunt), fail-open under outage (CC-3/A17), durability ordering (CC-1/A1),
code≠data (new CC: see priority 4), silent-gaps-vs-declared-exclusions → **this
audit keeps an explicit exclusions register** (treat prior deferral lists as a
checklist of unprobed surfaces).

## Area decomposition (Pass 2 — reconciled)

34 areas. Built by **merging two independent decompositions** — the surface-
inventory mapper's 32 + the auditor's own list (`_auditor-independent-areas.md`)
— then adding the angles neither covered alone. Real counts from the mapper:
~95 internal packages (CLAUDE.md lists ~30 — see CS-005), 100 route registrations
vs 88 OpenAPI paths, 69 migrations, 6 ansible roles, 23 systemd units, 75 explorer
routes, 10 CI workflows. **Coverage map** (every dir → ≥1 area) at the end.

Priority key: **P0** = headline correctness/security/data-integrity; **P1** =
important; **P2** = hardening. Attack lists are deliberately concrete (a finding
needs a failing input).

### Code / ingest / decode correctness
- **A1 — Ledger ingest & streaming.** `internal/ledgerstream`, `cmd/…-indexer`. *Attack:* cursor-resume gaps/regression (recurred 2026-06-01), archive-vs-live divergence, dropped/duplicated ledgers, SDK error swallowing, back-pressure. **P0**
- **A2 — Dispatcher & decoder routing.** `internal/dispatcher`(+statsflush), `consumer`, `events`, `scval`. *Attack:* topic-vs-contract-identity gating (ADR-0035), ContractCall/OpArgs hook (Band/Redstone), `MustI128` panics. **P0**
- **A3 — Projector one-writer invariant.** `internal/projector`, `pipeline/sink.go::IsProjectedEvent`. *Attack:* a projected source on two write paths or registry↔IsProjectedEvent mismatch → double-count/silent-drop; replay idempotency; cursor genesis init. **P0**
- **A4 — On-chain DEX decoders.** `sources/{soroswap,phoenix,comet,aquarius,sdex,soroswap_router}`. *Attack:* Phoenix 8-event grouping, Soroswap Sync correlation, Comet no-factory anchor, CAP-67 vs SEP-41 topic shape, pre-P23 classic gap. **P0**
- **A5 — Lending/bridge/oracle decoders.** `sources/{blend,blend_backstop,cctp,rozo,defindex,reflector,redstone,band}`. *Attack:* Reflector 3-contract split, Band E18/E9 scale, Redstone feed_id zip mismatch, contract-upgrade schema drift on backfill. **P1**
- **A6 — Supply pipeline.** `internal/supply`, `sources/{accounts,trustlines,claimable_balances,liquidity_pools,sac_balances,sep41_supply,sep41_transfers}`. *Attack:* 3-algorithm NUMERIC math, mint/burn/clawback counterparty topic-index (the CAP-67 loss), double-count. **P0**
- **A7 — External CEX/FX connectors.** `sources/external/*` (11)+`forex`,`frankfurter`. *Attack:* non-uniform decimals (10^8 vs FX 10^6), class-gating leak into VWAP, outbound SSRF/secret leak, `BackfillSafe`. **P1**
- **A8 — Aggregation math.** `internal/aggregate`+7 subpkgs. *Attack:* VWAP/TWAP/outlier correctness, `min_usd_volume=0` dust manipulation, stablecoin fiat-proxy + the new self-peg arm (depeg hiding), closed-bucket contract (ADR-0015), MEV dedup. **P0**
- **A9 — Canonical types & money / i128 sweep (ADR-0003).** `canonical`(+discovery), `xdrjson`, all `Int128Parts` parse sites. *Attack:* i128→int64 truncation end-to-end; NUMERIC vs BIGINT columns; JSON precision >2^53; **the claimed golangci i128 analyzer + BIGINT/DOUBLE migration lint that don't exist** (P4-6). **P0**

### Data integrity / storage / completeness
- **A10 — TimescaleDB served tier.** `storage/timescale` (75 files). *Attack:* SQL injection, cagg refresh races, NUMERIC coercion, hypertable lock sizing, chunk-count perf cliff. **P0**
- **A11 — ClickHouse raw lake.** `storage/clickhouse`, `deploy/clickhouse/tier1_schema.sql`. *Attack:* substrate contiguity + hash-chain claim, FINAL dedup correctness, lake-vs-served drift, topic_0_sym='' undercount. **P0**
- **A12 — Migrations & retention.** `migrations/` (69). *Attack:* rogue `trades` retention drift; PK granularity (0053) double-count/loss; event-index uniqueness (0054-0060). *(Mapper's "0031/0040 down re-adds retention" = FALSE — downs are deliberate NO-OPs; see register.)* **P1**
- **A13 — Completeness verification.** `completeness`, `archivecompleteness`, `hashdb`(dead), ops `compute-completeness`/`verify-archive`. *Attack:* reconcile soundness, watermark overwrite-not-max, childgate staleness, "100% coverage" provenance, hashdb unwired. **P1**
- **A14 — Divergence cross-check.** `internal/divergence`. *Attack:* false-negative when CoinGecko/Chainlink down (CG is down now), silent staleness. **P1**
- **A15 — Ops mutation CLI.** `cmd/stellarindex-ops` (57 files, key minting + lake mutation w/ prod creds). *Attack:* writes bypassing projector, archive trim/rehydrate safety, key-mint authz. **P1**

### Security / auth / platform
- **A16 — API authn/authz & middleware.** `api/v1/middleware` (20), `auth`(+sep10). *Attack:* auth bypass, SEP-10 challenge replay/expiry, RequireEmailVerified bypass, keypolicy escalation, trusted-proxy/XFF trust. **P0**
- **A17 — Keys & quota/rate-limit.** `ratelimit`, `usage`, `cachekeys`, `keypolicy`, `monthly_quota`. *Attack:* token-bucket race/over-admit, cache-key poisoning, quota underflow, key-prefix split-brain (rek_/sip_). **P0**
- **A18 — Customer dashboard & platform store.** `api/v1/{dashboardauth,dashboardkeys,dashboardwebhooks}`, `platform`(+postgresstore — ~0 tests, CS-003). *Attack:* IDOR on keys/webhooks, admin-route access control, untested store correctness. **P0**
- **A19 — Webhook delivery & Stripe.** `customerwebhook`, `/webhooks/stripe`. *Attack:* HMAC signing/verify, SSRF to customer URLs, Stripe signature verification, retry storms. **P1**
- **A20 — Email / magic-link.** `notify` (Resend), magic-link tokens (0065). *Attack:* email/header injection, token brute-force/replay, nil-Now panic class. **P1**
- **A21 — SSE / streaming.** `api/streaming`(+redispub), `api/streampublish`, 5 stream endpoints. *Attack:* unauth fan-out, slow-loris/backpressure, Redis pub/sub key leakage, goroutine leak on disconnect. **P1**
- **A22 — Verified-currency & SEP-1 trust.** `internal/currency` (seed.yaml), `internal/metadata` (SEP-1 TOML). *Attack:* seed tampering, SEP-1 fetch SSRF, unverified-collision bypass, JSON-LD injection downstream. **P1**
- **A23 — Secrets & config.** `internal/config`, ansible inventory, `.gitleaks.toml`, working-tree creds (CS-001). *Attack:* validate()-on-copy panic class, config-tag drift, default foot-guns (sep41 projector default), plaintext/working-tree secrets. **P1**

### Infra / IaC / deploy / observability
- **A24 — Ansible IaC.** `configs/ansible/roles/*` (6) + playbooks, patroni/redis-sentinel HA. *Attack:* Jinja string-"false"-truthy class, idempotency, vault handling, failover correctness, the Discord apply.sh render. **P1**
- **A25 — systemd & host hardening.** `deploy/systemd/` (23), `docker/` (6). *Attack:* missing sandboxing (User/NoNewPrivileges/ReadWritePaths), root containers, unpinned base images, textfile perms. **P2**
- **A26 — Edge / TLS / CDN.** `configs/caddy`, CF Pages, `web/status/{_headers,wrangler.toml}`. *Attack:* TLS/cipher, CSP/security headers, edge cache poisoning, CF Functions secret exposure. **P1**
- **A27 — Monitoring & alerting.** `deploy/monitoring/rules` (21) vs `configs/prometheus/rules.r1` (22), alertmanager, `obs`, runbooks. *Attack:* rule drift between dirs, alert→runbook gaps, dead alerts (no evaluator), metric cardinality blowup, Discord-degrade-silently, deadmansswitch. **P1**

### CI/CD / supply chain
- **A28 — CI pipeline & lint gates.** `.github/workflows/{ci,api-audit,release-validate}.yml`, `scripts/ci/*`. *Attack:* lint bypass, import-boundary escape via baseline suppression, gitleaks allowlist over-broad (just widened — re-check), docs-drift guards that silently drifted (postman, web types). **P1**
- **A29 — Release & deploy automation.** `release.yml`, `deploy.yml` (SSH+Ansible), `cut-release.sh`. *Attack:* artifact integrity (SHA256SUMS), SSH key/secret scope, rollback path, no container signing, tag→build trust. **P1**

### Frontend / API contract / docs / resilience
- **A30 — Explorer frontend security.** `web/explorer` (75 routes), `/dashboard/admin`, `/embed/*`. *Attack:* JSON-LD/stored-XSS via SEP-1 fields, client-side admin/auth gating, embed iframe XSS + clickjacking, API keys in static bundle, /auth/callback open-redirect, CF Pages Function SSRF. **P0**
- **A31 — API contract & spec drift.** `openapi/…v1.yaml` (88) vs 100 code routes, `pkg/client`, dual-shape `/v1/assets/{slug}`. *Attack:* undocumented/auth-missing routes, shape-discriminator confusion, generator drift (types.ts/postman not drift-guarded). **P1**
- **A32 — Docs/ADR integrity.** `docs/`, CLAUDE.md (3× package undercount, CS-005), ADRs. *Attack:* docs that lie about current behavior (doc-hygiene class — 6 found this session), last_verified staleness, ADR invariants claimed-but-unenforced (i128 analyzer). **P1**
- **A33 — Pricing read-paths & query performance.** `api/v1/{price*,vwap,twap,oracle,observations,chart,ohlc}`. *Attack:* non-sargable WHERE (`func(col)` — the 50→400ms incident), fallback serving stale as fresh (`flags.stale`), prewarm cache-key drift, alias resolution. **P0**
- **A34 — Cross-package data-flow & resilience.** end-to-end `consumer.Event`→sink arms, canonical round-trips storage→API→SDK; `test/{integration,load,chaos}` adequacy. *Attack:* an Event type with no sink arm (silent drop), precision loss at a boundary, thin chaos coverage of real failure modes. **P1**

### Coverage map (every top-level dir → area)

`cmd/`→A1,A15 · `internal/`→A1-A23,A33,A34 · `pkg/`→A31 · `migrations/`→A12 ·
`configs/`→A23,A24,A26,A27 · `deploy/`→A11,A25,A26,A27,A29 · `web/`→A26,A30 ·
`scripts/`→A28,A29 · `test/`→A34 · `docs/`→A32 · `openapi/`→A31 · `examples/`→A31
· `.github/`→A28,A29. _No unaudited white space._

## Execution protocol

1. **Fan-out review.** Each area gets an independent adversarial reviewer with
   the attack list. Reviewers return candidate findings with a concrete failing
   input/scenario (no repro → downgrade).
2. **Adversarial verification.** Every Critical/High candidate gets an
   independent skeptic that tries to REFUTE it (read the actual code path).
   Survivors only.
3. **De-dup + synthesize** into `01-cold-system-findings.md` (CS-###), ranked
   by severity, each with: location, failing scenario, blast radius, fix.
4. **Coverage proof.** Append the directory→area map showing no white space.

## Cross-cutting hunts (thread through EVERY area)

These are failure *classes*, not areas — each reviewer applies all of them to
their area. They come from this system's own incident history + the Pass-3
self-review, and are where a new model should out-find the prior audits:

- **CC-1 Silent error swallowing.** `continue`/`return nil` on error that drops a
  row/event (seen in decoder skips + `tryStablecoinFiatProxy`). Grep `err != nil`
  blocks that neither log+metric nor propagate. → silent data loss.
- **CC-2 Zero-value / nil-time handling.** `.IsZero()`/`time.Time{}` /nil-pointer
  guards. This codebase has shipped ≥4 such bugs (magic-link nil-Now panic,
  last-used 2055, rek_/sip_ split-brain, validate()-on-copy). Audit every
  time/optional field on a serving path.
- **CC-3 Concurrency & goroutine lifecycle.** Run targeted packages under `-race`;
  hunt goroutine leaks on SSE disconnect, worker shutdown, ctx cancellation;
  token-bucket/cache races.
- **CC-4 Backfill-vs-live divergence.** Any decoder/reader that behaves
  differently on historical replay (old WASM, missing landing zone, retention-
  scoped served tier) vs live. The "served tier verified only within what it
  holds" caveat is where false "complete" claims hide.
- **CC-5 Precision/scaling at boundaries.** i128 (CC of A9), the 10^8/10^6/per-
  asset decimals, NUMERIC↔string↔JSON, E18/E9 oracle scales — at every
  storage→API→SDK hop.
- **CC-6 Trust-boundary / input validation.** Every external input: SEP-1 TOML,
  vendor JSON, customer webhook URL, query params, XDR — SSRF, injection, unbounded
  decode, allow-list bypass.
- **CC-7 Doc/claim vs code.** For every "verified/complete/safe/enforced" claim
  in a doc/comment/ADR, find the code that proves or breaks it (6 such drifts
  already found this session; the i128 "analyzer" claim is the prime suspect).

## Pass-3 self-review additions (areas the first cut under-weighted)

- **A35 — Disaster recovery / restore actually tested.** Extends A11/A25.
  *Attack:* backups exist (pgBackRest verified healthy this session) but is a
  **restore** ever drilled? A never-restored backup is schrödinger's backup.
  Verify `archival-node-bringup.md` + DR triage tree against reality; MinIO/
  galexie-archive + ClickHouse lake recovery, not just Postgres. **P1**
- **A36 — Licensing / data-redistribution compliance.** *Attack:* Apache-2.0 dep
  license compatibility (scan go.mod); CEX/vendor data redistribution
  restrictions (CLAUDE.md notes some venues restrict redistribution) — are we
  re-serving data we're not licensed to? **P2**
- **Accessibility (a11y)** of the explorer is logged to **Audit 2** (product
  quality), not here — but flagged so it isn't lost.

## Pass log (refinement passes over THIS plan)

- **Pass 1:** methodology, expansion strategy, execution protocol, templates.
- **Pass 2:** folded the surface-inventory mapper + the auditor's independent
  decomposition into 34 areas with attack lists + a directory coverage map (no
  white space); banked 4 verified recon findings + cleared 2 false positives.
- **Pass 3:** adversarial self-review → added 7 cross-cutting failure-class hunts
  (CC-1…CC-7) drawn from this system's incident history, plus A35 (DR/restore
  actually tested) and A36 (licensing/redistribution); routed a11y to Audit 2.
- **Pass 4 (done — plan FROZEN):** folded the 9-audit baseline-gap analysis;
  split areas into "re-verify only" (well-covered) vs "this-pass-owns" (under/
  never-covered); kept an explicit exclusions register; sequenced execution waves.

## Execution waves (frozen)

Effort is concentrated on NEW territory; well-covered areas get a cheap
re-verification only.

- **Wave 1 — NEW territory deep-dives (highest marginal value):** CF Pages Edge
  Functions (A26/A30), ClickHouse lake round-trip + DDL (A11), multi-tenant IDOR
  (A18), pricing read-paths + non-sargable scan (A33), SSE lifecycle (A21).
- **Wave 2 — data-correctness at scale:** live reconciliation of served values vs
  ground truth (supply, 24h vol, OHLC) — extends prior Q4's single sample (A34 +
  A6/A8). Decoder fidelity for the ~11 row-classified Soroban sources (A4/A5).
- **Wave 3 — cross-cutting sweeps (CC-1…CC-7) over the whole tree** + the
  re-verification of well-covered fixes (auth, webhook, Stripe, PK-grain, alerts)
  on current `main`.
- **Wave 4 — infra/CI/docs/frontend:** ansible-vs-actual-R1 reconciliation (A24),
  systemd/Docker hardening (A25), CI supply-chain + drift guards (A28/A29),
  docs/ADR integrity (A32), DR-restore-actually-tested (A35), licensing (A36),
  a11y (→ Audit 2).
- **Dynamic (opportunistic, flagged for operator if heavy):** decoder fuzzing,
  k6/chaos against current build — scoped but may need a runtime budget.

Each wave: independent adversarial reviewers → refute-pass verification →
register (`01-cold-system-findings.md`). Status: **EXECUTING.**

## Exclusions register (explicit — what this pass does NOT probe)

- Live R2/R3 (don't exist), WASM bytecode disassembly, multi-week fuzz campaigns,
  Galexie/stellar-core binary internals, vendor portal state (Stripe/Resend/CF
  merchant dashboards), and **live destructive chaos** (scenarios read, not
  executed against prod) — carried forward from prior audits' deferrals. Anything
  found *behind* these is logged as an exclusion, not a silent gap.
