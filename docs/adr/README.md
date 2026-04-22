# Architecture Decision Records

Every significant architectural choice in Rates Engine is captured
here as an **ADR** — a numbered, dated, immutable record.

## Rules

1. **Immutable.** Once an ADR is `Accepted`, its body is never
   edited for content. Typo fixes and formatting are allowed; rationale
   is not.
2. **Supersede, don't rewrite.** If a decision changes, write a new
   ADR that supersedes the old one, and add one line to the old ADR's
   metadata:

   ```yaml
   superseded_by: 0017
   ```

3. **Numbered sequentially.** ADR-0001, ADR-0002, … No gaps, no
   renumbering.
4. **One topic per ADR.** Don't bundle.
5. **Every PR that makes an architectural decision includes an ADR.**
   Review-gate: ADR + code together or neither.

## Status values

- `Proposed` — under discussion; safe to move to `Accepted` only
  after the PR lands.
- `Accepted` — in force. Code adheres.
- `Superseded` — replaced by a later ADR; pointer in metadata.
- `Rejected` — proposed and explicitly turned down; kept so we don't
  re-litigate the same idea.

## Template

See [_template.md](_template.md) for the boilerplate.

## Index

| # | Status | Title | Landed |
| - | ------ | ----- | ------ |
| [0001](0001-horizon-deprecated.md) | Accepted | Horizon is not in the architecture | 2026-04-22 |
| [0002](0002-minio-s3-compat-storage.md) | Accepted | Self-hosted storage is S3-compatible, not local filesystem | 2026-04-22 |
| [0003](0003-i128-no-truncation.md) | Accepted | i128 / u128 preserved end-to-end; never int64 | 2026-04-22 |
| [0004](0004-tier1-validator-aspiration.md) | Accepted | Tier-1 three-validator aspiration | 2026-04-22 |
| [0005](0005-monorepo.md) | Accepted | Monorepo with one Go module | 2026-04-22 |

## Related

- [docs/discovery/decisions.md](../discovery/decisions.md) — the
  Phase-1 decisions log these ADRs are extracted from. Read the
  discovery doc for the narrative; read the ADRs for the binding
  commitment.
- [docs/discovery/engineering-standards.md](../discovery/engineering-standards.md)
  §5.5 — why decisions live in ADRs, not scattered architecture
  docs.
