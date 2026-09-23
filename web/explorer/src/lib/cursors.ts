/**
 * The ingestion_cursors `source` namespaces that hold a LIVE position.
 * Mirrors `liveCursorSources` in internal/storage/timescale/cursors.go
 * (pinned by cursors.test.ts). Every other namespace is a one-shot job's
 * shard cursor — `backfill`, `census-backfill`, `projected-rebuild`, … —
 * whose last_ledger is the end of a historical range, not the tip.
 * An allowlist, because job namespaces are added far more often.
 */
export const LIVE_CURSOR_SOURCES: readonly string[] = [
  'ledgerstream',
  'projector',
];

export function isLiveCursorSource(source: string): boolean {
  return LIVE_CURSOR_SOURCES.includes(source);
}
