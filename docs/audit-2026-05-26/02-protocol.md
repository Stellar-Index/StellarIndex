# Execution Protocol

## 1. Zero-Trust Rule

Docs are not facts. For any claim from markdown, architecture
notes, prior audits (04-29, 05-02, 05-12), discovery artifacts,
runbooks, ADRs, RFP/proposal text, CHANGELOG entries, CLAUDE.md,
agent-memory files, or PR descriptions:

1. Locate the live code path (file + line).
2. Locate the test or runtime wiring that exercises it.
3. Record whether the doc is true, stale, partial, or contradicted.

Past audit artifacts list **what someone looked at then**, not
**what is true now**. Their findings (open or closed) are
re-tested cold here.

Agent memory under `~/.claude/projects/.../memory/` is *not*
authoritative. Each memory entry is a tip about prior incidents
or operator preferences; the audit verifies each against current
code. Obsolete entries become findings.

## 2. Adversarial Frame

For every component, three additional questions beyond correctness:

1. **What's the abuse vector?** What does an unauthenticated user,
   a low-tier paying user, a hostile peer in the validator
   network, or a misbehaving upstream feed get if they push this
   surface?
2. **What's the silent-failure mode?** What does this surface
   return when its dependency is degraded — wrong number, stale
   number, no number?
3. **What's the trust boundary?** Where does external data first
   enter our system? Is it validated, scaled, classed, and clocked
   correctly? Could it impersonate our internal data?

Findings from this frame go in
[05-findings-register.md](05-findings-register.md) with severity
per [11-severity-rubric.md](11-severity-rubric.md).

## 3. Evidence Discipline

Nothing is accepted from memory or prior audits. Every material
claim must cite at least one of:

- a local file reference with line anchor (`internal/foo/bar.go:123`)
- a generated inventory artifact in this directory
- a command output captured in `evidence/log.md` or
  `evidence/r1-probes/*.md`
- a test file and the *behaviour it actually asserts*
- an OpenAPI excerpt referenced by line
- a SQL migration referenced by line
- a Prometheus rule expression referenced by file + rule name
- an ADR referenced by ID + invariant text

When two pieces of evidence disagree, both are recorded; the
disagreement itself becomes a finding.

**Raw bytes rule (added 2026-05-27 after F-0075):** Live-curl
evidence must record raw HTTP body bytes — not derived
extractor output (python dicts, jq filters, etc.). When a
parser is used for readability, the parser's correctness
must be cross-checked against at least one raw-byte sample
in the same iteration. Iteration 6's F-0060 (then-critical)
was caused by an extractor reading top-level `price` instead
of `data.price`, producing all-null output and a false
cascade-victim finding that took two iterations to retract.

## 4. Evidence Log Format and ID Taxonomy

Evidence is split across ledgers, each with its own ID prefix.

| Prefix | Ledger | Used for |
| --- | --- | --- |
| `EV-####` | [evidence/log.md](evidence/log.md) | code/test/runtime observations |
| `CMD-####` | [evidence/commands.md](evidence/commands.md) | shell command transcripts (exact output) |
| `XFI-####` | [evidence/cross-file-interactions.md](evidence/cross-file-interactions.md) | every material seam |
| `J-####` | [journeys-traces/J##-*.md](journeys-traces/) | one ID per completed journey trace |
| `R1-####` | [evidence/r1-probes/](evidence/r1-probes/) | live R1 SSH probes |
| `XFI-CLASS-####` | same as XFI but for *interaction classes* | covers W26's class-roll-up gate |
| `MEM-####` | inline in evidence/log.md | memory-truth verification entries |

Each row records: prefixed ID, ISO-8601 UTC date, claim or
observation, source refs (file:line or transcript path),
workstream(s) (W01..W35), notes (≤200 chars).

IDs are monotonic per ledger. Do not reuse IDs. Invalidated
entries: mark `superseded by <NEW-ID>` rather than deleting.

A finding can cite IDs from any ledger; cross-ledger citations
are encouraged.

## 5. Per-File Audit Loop

For every tracked file in `inventory/file-coverage.tsv`:

1. **Read the file directly.** No second-hand summaries.
2. **Classify file role** using the controlled vocabulary
   (`file_kind` column in the TSV): `runtime`, `test`, `fixture`,
   `migration`, `config`, `deploy`, `workflow`, `script`,
   `documentation`, `generated`, `frontend`, `asset`, `policy`,
   `unknown`. The role determines the rest of the loop's emphasis.
3. **Identify inbound dependencies.** Imports, callers, routes,
   workflows, scripts, docs, generated inputs.
4. **Identify outbound dependencies.** Imports, commands,
   network calls, database objects, cache keys, files, env vars,
   metrics, alerts, docs.
5. **Identify trust boundaries.** External input, user input,
   ledger data, third-party API, DB, Redis, filesystem, CI
   secret, SSH host, browser, Cloudflare, systemd.
6. **Identify invariants.** Precision, idempotency, consistency,
   freshness, auth, ordering, schema, source attribution, rate
   limit, timeout, privacy.
7. **Identify tests that exercise it.** Direct unit tests,
   integration coverage, or no test at all? What do the tests
   actually assert?
8. **Identify docs that describe it.** ADR, architecture note,
   runbook, README, comment block; classify doc truth.
9. **Update status.** Terminal: `done` / `excluded` / `blocked`.
10. **Record evidence refs.** Cite at least one ID per ledger
    (not all required — pick what proves the claim).

A file marked `done` must have at least one evidence ref. `done`
means *reviewed with evidence* — not bug-free.

## 6. Per-Decoder Audit Loop

For every source decoder in `internal/sources/<source>/`:

1. **Claim surface.** What event topics / op kinds / contract IDs
   / methods does this decoder claim to handle?
2. **Decode entry function(s).** Trace from dispatcher routing
   to the decoder's first XDR read.
3. **Malformed-input handling.** Construct a malformed payload
   in test or by inspection; verify graceful failure (no panic,
   typed error, drop counter increment).
4. **Storage / consumer integration.** Where does the decoded
   trade/observation go? What table? What sink interface?
5. **Fixture realism.** Are golden files in `test/fixtures/`
   captured from real ledgers? When was the last capture (file
   mtime + git log on the fixture)?
6. **Tests vs actual risk.** What is asserted? What is left
   unproven?
   - happy-path-only: a finding
   - no malformed-input test: a finding
   - no WASM-version dispatch test (when source has multiple
     deployed versions): a finding
   - no decoder/sink integration test: a finding for
     production-routed decoders
7. **WASM audit status.** `BackfillSafe` flag in
   `internal/sources/external/registry.go` vs
   `docs/operations/wasm-audits/<source>.md` evidence trail.
8. **Surprise list compliance.** CLAUDE.md "Things that will
   surprise you" includes per-source caveats. Each caveat is a
   test claim; verify a test asserts it.
9. **Every-event coverage (W35).** Cross-reference the source's
   on-chain event surface (contract `event.rs` or equivalent
   docs) with the decoder's `classify()` switch. Any unclaimed
   event is a W35 finding even if the decoder is otherwise
   correct.
10. **Backfill subcommand (if applicable).** If the source has a
    matching `ratesengine-ops <source>-backfill` subcommand
    (W29), verify the contract+topic filter matches the
    decoder's claim surface exactly.

## 7. Per-Migration Audit Loop

For every `migrations/####_*.up.sql` and matching `.down.sql`:

1. **Up + down symmetry.** Does down actually reverse up?
2. **Concurrent-safe DDL.** `CREATE INDEX CONCURRENTLY`,
   `ADD COLUMN ... DEFAULT NULL`, etc., where applicable.
3. **Hypertable / continuous-aggregate semantics.** Window
   intervals, refresh policy, compression policy, retention.
4. **NUMERIC vs BIGINT.** ADR-0003 invariant: any column that
   stores i128 amounts must be NUMERIC.
5. **PK includes partition key.** TimescaleDB TS103 — the PK
   must include the time partition column (migration 0041 fixed
   this for soroban_events; verify every other hypertable
   complies).
6. **Index coverage.** Hottest queries hit indexes? (cross-ref
   with `internal/storage/timescale/*.go` queries.)
7. **Trigger / view drift.** Materialized views and triggers
   that depend on this migration.
8. **Reader correspondence.** Find the `internal/storage/timescale/`
   reader/writer that uses this table. Drift = finding.
9. **ON CONFLICT shape match.** If the table has a unique
   constraint that the writer relies on for idempotency, verify
   the writer's `ON CONFLICT` column list matches exactly (the
   rc.78→rc.79 incident: migration shipped one PK shape, writer
   targeted another, every insert returned 42P10).

## 8. Per-Route Audit Loop

For every HTTP route registered in `internal/api/v1/server.go`:

1. **OpenAPI presence.** Route + method present in
   `openapi/rates-engine.v1.yaml`? Parameters, schemas, status
   codes match?
2. **Envelope conformance.** Response uses
   `internal/api/v1/envelope.go` (`data`, `as_of`, `flags`,
   `pagination`)? Errors are RFC 7807?
3. **Auth gate.** Public, API-key, or admin?
4. **Rate-limit identity.** Per-key or per-IP? Exempt internal?
5. **Cache headers.** ETag, max-age, Vary, Cache-Control.
6. **Pagination.** Cursor-based per ADR-0018? Stable across
   inserts?
7. **Empty / not-found shape.** 200 + empty data vs 404 — which?
8. **Latency budget.** Target p50/p95/p99 from
   `docs/operations/sla-probe.md` and ADR-0009; live R1
   measurement.
9. **Test coverage.** Unit handler test + integration test?
   `pkg/client/endpoints_test.go` covers the wire shape?
10. **Removed-route hygiene.** If the route was deleted, no
    internal callers remain, deprecation headers were emitted
    prior to removal, examples + Postman + explorer no longer
    reference it.
11. **Prewarm/handler drift.** If a route has a prewarm path
    (`coins_cache.go`, `markets_cache.go`, `history_cache.go`,
    `sources_stats_cache.go`, `coverage_cache.go`,
    `asset_detail_cache.go`), the prewarm call MUST pass
    byte-identical args to the handler's cached reader
    (`feedback_prewarm_handler_drift`).

## 9. Per-External-Source Audit Loop

For every adapter in `internal/sources/external/<vendor>/`:

1. **Vendor truth.** API docs URL, current rate limits,
   redistribution licence.
2. **Auth hygiene.** Keys come from env (`internal/config`),
   never from code; key absence = source disabled, not panic.
3. **Normalization.** Amount scale 10^8 (per CLAUDE.md), pair
   normalisation matches our canonical Pair.
4. **Retry + backoff + jitter.** Exponential, capped, jittered.
5. **Clock skew.** Vendor `ts` vs our wall clock — guard rails.
6. **Class.** `ClassExchange` / `ClassAggregator` / `ClassOracle`
   / `ClassAuthoritySanity` per
   `internal/sources/external/registry.go`.
7. **Inclusion policy.** Aggregator-feeding by default, or
   divergence-only?
8. **Backfill safety.** `BackfillSafe` true requires WASM audit
   (for on-chain) or vendor history determinism (for off-chain).
9. **Failure-mode coverage.** Vendor 5xx, 429, 401, network
   timeout — tests assert each.

## 10. Per-Alert Audit Loop

For every Prometheus rule under `deploy/monitoring/rules/<area>.yml`
AND `configs/prometheus/rules.r1/<area>.yml` (multi-host +
single-host overlay must stay paired):

1. **Expression provability.** Does the metric name actually
   exist? (Cross-ref `internal/obs/metrics.go` + per-binary
   metric registration.)
2. **Threshold defensibility.** Does the threshold derive from
   ADR / SLA / runbook, or is it a guess?
3. **Severity tier.** Maps to
   `configs/alertmanager/alertmanager.r1.yml` route?
4. **Runbook link.** `runbook_url` annotation points to a
   `docs/operations/runbooks/<name>.md` that exists?
5. **Runbook content.** Runbook describes diagnosis steps,
   dashboard links, escalation, postmortem template?
6. **Firing test.** Has this alert ever fired in R1 (verify in
   Alertmanager history)? If never, is the alert reachable in
   principle, or is it dead?
7. **Multi-host ↔ R1 pairing.** Multi-host rule and R1 overlay
   must both exist (CI's `monitoring-rules` job validates this).

## 11. Per-Doc Audit Loop

For every doc file:

1. **Frontmatter age.** `last_verified` <90 days = ok, 90-180 =
   warn, >180 = stale.
2. **Truth claims.** Identify every factual claim; trace each
   to code/test/runtime evidence.
3. **Stale references.** Removed packages, removed routes,
   removed tools, replaced libraries.
4. **Contradiction.** Does it disagree with another doc or ADR?
   If so, both go to findings.

## 12. Findings Rules

Use [05-findings-register.md](05-findings-register.md).

Each finding needs:

- stable ID (`F-####`)
- severity per [11-severity-rubric.md](11-severity-rubric.md)
- concise title (≤80 chars)
- affected surface (file paths + line numbers)
- evidence refs (any prefix per §4)
- workstream(s) (W01..W35)
- adversarial vector (if applicable)
- disposition

**Finding status taxonomy:**

| Status | Meaning |
| --- | --- |
| `open` | reviewed, real, not yet remediated |
| `needs_evidence` | suspected; evidence incomplete |
| `needs_owner` | confirmed real, awaiting wave/owner |
| `accepted` | risk explicitly accepted; requires `note` entry |
| `wontfix` | will not be fixed; requires reasoning |
| `closed-by-PR-####` | code/docs/infra change merged + verify rerun |
| `duplicate` | cross-references prior finding |
| `invalid` | retracted after deeper review |

Severity: `critical` / `high` / `medium` / `low` / `note`.

## 13. Exclusions Rules

Use [06-exclusions-register.md](06-exclusions-register.md).

Any skipped scope must record:

- exact excluded thing (path or behaviour)
- reason (impossible / out-of-scope / requires-external-access)
- temporary or permanent for this audit
- evidence needed to re-enter scope

## 14. Test Interpretation Rules

Tests prove asserted behaviour only.

For each important suite, record:

- what it actually asserts
- what it leaves unproven
- whether CI runs it (gate vs informational)
- whether live runtime wiring could still break despite green tests

## 15. Live R1 Probe Rules

Live probes via SSH (`root@136.243.90.96`) are encouraged. Each
probe is captured as a transcript in
`evidence/r1-probes/<topic>-<YYYYMMDD>.md` with:

- time of probe (UTC)
- exact command(s)
- raw output (truncated to relevant lines only)
- claim being tested
- finding ID if a discrepancy is observed

**Anti-pattern from prior sessions:** never inline `$$..$$` in
SQL over SSH — `$$` expands to the shell PID and corrupts the
query. Use `psql -f /tmp/<query>.sql` after `scp`'ing the file.

Probe protocol details: [12-r1-live-probe-protocol.md](12-r1-live-probe-protocol.md).

## 16. CG/CMC Parity Matrix Rules

Use [08-cgcmc-parity-matrix.md](08-cgcmc-parity-matrix.md). Each
row is one feature. Mark:

- `covered` — we ship it, with proof
- `partial` — we ship some of it; specify the gap
- `gap` — we don't ship it; finding required
- `non-goal` — explicit product decision; cite the decision
- `n/a` — feature is structurally impossible for our scope

## 17. Stellar Depth Matrix Rules

Use [09-stellar-coverage-matrix.md](09-stellar-coverage-matrix.md).
Same scoring as the CG/CMC matrix, but rows describe surfaces
where we *must* be deeper than CG/CMC. Each `gap` row is a
launch-quality finding.

## 18. Docs-Truth Rules

When docs disagree with code:

- do not silently trust either side
- log evidence from both sides
- state whether the doc overstated, understated, or contradicted
  reality
- prefer changing the doc over weakening the code unless the doc
  describes an *intent* the code never reached

## 19. Cross-File Interaction Log

Use [evidence/cross-file-interactions.md](evidence/cross-file-interactions.md).

Record seams such as:

- binary → package wiring (`cmd/* main.go` → `internal/*`)
- package → package interfaces
- handler → storage adapter
- decoder → sink → storage
- aggregator → Redis → API
- workflow → script → generated artifact
- runbook or proposal text → code path it purports to describe
- ADR text → import-boundary lint rule
- live R1 traffic pattern → handler → storage query
- soroban_events.Capture → AsyncSink → InsertSorobanEventsBatch
  (the new ADR-0029 seam)
- soroban_events row → Reconstruct → events.Event → decoder →
  Insert<source>Event (the new SQL-backfill seam)

Each row should make it possible for a reviewer to find both ends
of the seam without searching.

## 20. Memory-Truth Rules

Each agent-memory entry under
`~/.claude/projects/-Users-ash-code-ratesengine/memory/` is a
hypothesis about the system, not a fact. For each:

1. Read the entry.
2. Identify the claim (incident lesson, operator preference,
   architecture note).
3. Verify the claim against current code/runtime.
4. Log `MEM-####` in evidence/log.md with disposition:
   `still-true`, `obsolete`, `superseded-by-PR-####`,
   `contradicted-by-code`.
5. Obsolete or contradicted memories become findings (severity
   `low` or `note` depending on impact).

Examples already known:
- `feedback_reenable_trades_compression` — already false:
  job 1000 is `scheduled=t` per psql query 2026-05-26.
- `feedback_quiet_checksum_was_a_noop` — claims rc.72 fix
  didn't work; rc.77 fd-2 wrap actually fixed it. Verify rc.77
  fix still holds in the binary on r1.

## 21. Final Condition

The audit is complete only when the control docs, evidence logs,
inventory, findings, exclusions, and remediation plan can be
followed without relying on undocumented context.

A reviewer should be able to:

1. Open this directory cold
2. Read README → 00-plan → tracker
3. Locate every claim's evidence
4. Re-walk every workstream in their own session
5. Re-test every finding without needing the original auditor

If any of those five steps requires asking the original auditor
a question, the audit is not done.
