---
title: Site quality audit — findings register
last_verified: 2026-07-03
status: current
---

# Findings register

Severity / status legend in PLAN.md. Root-cause layer: UI (explorer
code), API (endpoint gap), DATA (index/backfill gap), IA (information
architecture / labels).

| # | Sev | Where | What the visitor experiences | Root cause (hypothesis → verified) | Wave | Status |
|---|-----|-------|------------------------------|-------------------------------------|------|--------|
| S-001 | P0 | nav "DEX/AMM" | Clicking "DEX/AMM" lands on /protocols — label promises a venue view, destination is protocol verification pages | IA: label/destination mismatch | B | open |
| S-002 | P1 | /assets | Shows 11 assets under the heading "Assets" — the verified catalogue presented as the asset universe | UI+API: page consumes the catalogue listing; the full census (every traded/trustlined asset) exists in the store but has no browse surface | C | open |
| S-003 | P0 | /issuers → /issuers/GA5Z…/ | The site's own issuer list links to a detail that says "Issuer not found" (Circle!) | Suspected trailing-slash param pollution in the static-export client fetch, AND/OR list/detail source mismatch (list = curated+sep1, detail = trades/ChangeTrust-derived). Verify both | B | open |
| S-004 | P2 | /transactions | A bare paginated list — no op-type mix, no fee stats, no volume context, no "what's moving" | UI: the API already serves OperationTypeStats + NetworkThroughput; the page never consumes them | D | open |
| S-005 | P2 | /operations, /ledgers | Same bare-list shape as S-004 | UI (same) | D | open |
| S-006 | P1 | /contracts/{id} | Top contracts lack cached/rendered code; the reported example (CDSOP5Y4…RG4R) shows no code AND no code history despite Phase-C claims | DATA/API: verify whether ledger_entry_changes holds the instance+code for this contract and whether ContractWasm/ContractCodeHistory query the tables Phase C actually filled | B | open |
| S-007 | P2 | /contracts/{SAC} | SACs render as anonymous contracts — no name, no "wrapped USDC" tag, no explainer, no link to the classic asset | API+UI: SACClassicAssetName reader shipped 2026-07-03 — wire it into contract detail + directory rows | C | open |
| S-008 | P3 | site-wide | (placeholder for the consistency-grid findings — dates, amounts, addresses, names) | — | E | open |

## Same-day incident notes (2026-07-03, related operational state)

- `minio_exporter_down`: MinIO root rotation invalidated the
  Prometheus bearer JWT (`/etc/prometheus/minio.token`). Fixed —
  regenerated from the new credentials; scrape up. Follow-up: add
  token regeneration to the rotation runbook + a config-assertion.
- `completeness_incomplete`: stale sdex verdict (mismatch healed
  13:00, verdict computed 05:31). Manual recompute kicked.
- `ingest_gap_detector_silent`: restart churn during rotation/staged
  applies; cycles healthy again (3 clean runs/source).
