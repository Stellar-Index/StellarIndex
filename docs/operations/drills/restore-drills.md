---
title: Restore-drill evidence log
status: living — appended by scripts/ops/restore-drill.sh
---

# Restore drills (ADR-0043)

Append-only evidence that our backups actually restore. Each entry is
written by `scripts/ops/restore-drill.sh` (non-destructive scratch
restore + verification on r1) and committed by the operator. A month
with no entry here is itself a finding.

