// Package archivecompleteness is the daemon side of the dual-archive completeness contract
// ([ADR-0017]), run by `stellarindex-ops archive-completeness check|fix|verify` and a daily timer
// (docs/operations/archive-completeness.md).
//
// Only the cross-anchor archive (`/srv/history-archive/`, anchored against SDF's signed view) is
// enforced: [CrossAnchorChecker.Check] lists missing checkpoints and is safe for concurrent calls.
// The primary galexie-archive scan is not implemented, so [Report.Primary] is always nil.
//
// [ADR-0017]: ../../docs/adr/0017-archive-completeness-invariants.md
package archivecompleteness
