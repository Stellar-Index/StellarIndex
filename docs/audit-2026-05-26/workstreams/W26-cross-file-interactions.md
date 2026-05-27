# W26 — Cross-file interactions (audit-blocking gate)

## Scope

W26 owns
[evidence/cross-file-interactions.md](../evidence/cross-file-interactions.md)
and the canonical interaction-class taxonomy. **W26 cannot close**
until every other workstream W01..W35 has terminal status AND
every required class in the taxonomy has at least one fully-traced
`XFI-####` row.

## Required interaction classes (extended from baseline)

| Class | Example seam |
| --- | --- |
| XFI-CLASS-001 | binary → config → pkg (`cmd/* main.go` → `internal/config` → `internal/*`) |
| XFI-CLASS-002 | decoder → sink → store (each per-source decoder → pipeline/sink → timescale) |
| XFI-CLASS-003 | workflow → script → artifact (GH Actions → release.sh → SHA256SUMS) |
| XFI-CLASS-004 | alert → runbook → service (Prometheus rule → runbook md → systemd unit) |
| XFI-CLASS-005 | route → handler → store (api/v1/* → store + redis read) |
| XFI-CLASS-006 | metric → registration → scrape → rule (obs.metrics → binary register → Prometheus scrape → rule eval) |
| XFI-CLASS-007 | ADR text → import-boundary lint rule (ADR-0001 → lint-imports.sh) |
| XFI-CLASS-008 | migration → reader → writer (migrations/NNNN → storage/timescale/*.go) |
| XFI-CLASS-009 | cache key → builder → consumer (cachekeys → handler/aggregator) |
| XFI-CLASS-010 | live R1 traffic pattern → handler → store query |
| XFI-CLASS-011 | RFP row → code path (RFP/proposal text → handler/source/aggregate) |
| XFI-CLASS-012 | runbook step → operator subcommand (runbook ops cmd → ratesengine-ops subcommand) |
| XFI-CLASS-013 | systemd unit → binary subcommand (deploy/systemd/*.service → cmd/* binary) |
| XFI-CLASS-014 | proposal feature → API endpoint → SDK method |
| **XFI-CLASS-015 (NEW)** | soroban_events.Capture → AsyncSink → InsertSorobanEventsBatch (ADR-0029 seam) |
| **XFI-CLASS-016 (NEW)** | soroban_events row → Reconstruct → events.Event → decoder → Insert<source>Event (SQL-backfill seam) |
| **XFI-CLASS-017 (NEW)** | ledgerstream.TolerateTrailingMissing → parse error → walk-complete (rc.81 seam) |
| **XFI-CLASS-018 (NEW)** | WASM audit doc → BackfillSafe flag → per-source backfill subcommand allowed |
| **XFI-CLASS-019 (NEW)** | Stripe webhook signature → idempotency-key → DB tier-upgrade → rate-limit middleware |
| **XFI-CLASS-020 (NEW)** | customer webhook subscription → fanout → SSRF check → HMAC sign → delivery |

## Closure criteria

- Every class has at least one fully-traced `XFI-####` row in
  `evidence/cross-file-interactions.md`.
- Every other workstream W01..W35 has terminal status.
- Findings on any class with no traced row, or any seam where
  the connection is implicit / undocumented.
