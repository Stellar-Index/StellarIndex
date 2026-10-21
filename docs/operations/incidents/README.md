---
title: Incidents — public-facing customer comms (moved)
last_verified: 2026-10-21
status: redirect
---

# Incidents — moved

The customer-facing incident posts live at
**[`internal/incidents/data/`](../../../internal/incidents/data/)**, where the
stellarindex-api binary embeds them (`go:embed`) and serves the parsed corpus via
`GET /v1/incidents`. The public status page (stellarindex.io/status) reads that endpoint.

`go:embed` cannot traverse upward (`..`), so the source of truth lives under the importing
package rather than being mirrored by a build rule.

## File naming + workflow

Files use `<YYYY-MM-DD>-<short-slug>.md` with the YAML frontmatter of
[`internal/incidents/data/_template.md`](../../../internal/incidents/data/_template.md).
Declare, update and resolve per [`sev-playbook.md`](../sev-playbook.md).

## API surface

`GET /v1/incidents` returns the corpus as JSON, newest `started_at` first. Files starting with
`_` (templates) are skipped; a file that fails to parse is logged and skipped.

## Internal postmortems

Internal post-mortems live separately at [`docs/operations/postmortems/`](../postmortems/).
