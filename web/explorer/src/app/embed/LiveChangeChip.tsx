'use client';

import { useChangeSummary, type ChangeEntityType } from '@/api/hooks';

const WINDOW_FIELD = {
  '1h': 'h1_delta_pct',
  '24h': 'h24_delta_pct',
  '7d': 'd7_delta_pct',
} as const;

type Window = keyof typeof WINDOW_FIELD;

// Reads one delta field off a bare row (what useChangeSummary returns)
// or the {data, as_of, flags} envelope — same either-shape tolerance as
// the asset-sidebar's unwrapChangeSummary, scoped to the single field a chip needs.
function unwrapDeltaPct(raw: unknown, field: string): number | null {
  if (raw == null || typeof raw !== 'object') return null;
  const maybe = raw as Record<string, unknown>;
  const row =
    maybe.data != null && typeof maybe.data === 'object'
      ? (maybe.data as Record<string, unknown>)
      : maybe;
  const v = row[field];
  return typeof v === 'number' ? v : null;
}

/**
 * LiveChangeChip — embed-widget change pill, hydrated from the live
 * change-summary worker (GET /v1/changes/{entity_type}/{id}) instead
 * of staying pinned to the build-time percentage the widget baked at
 * deploy (K061: the pair/asset embeds already refresh their headline
 * price every 60s via LivePrice, but the change chip beside it never
 * refreshed, so the two could silently disagree for as long as the
 * iframe stayed open). Falls back to the baked figure until the
 * worker has a row, and stays on it if the worker never computes one.
 */
export function LiveChangeChip({
  entityType,
  entityID,
  window,
  initialPct,
}: {
  entityType: ChangeEntityType;
  entityID: string;
  window: Window;
  initialPct: number | null;
}) {
  const q = useChangeSummary(entityType, entityID);
  const livePct = unwrapDeltaPct(q.data, WINDOW_FIELD[window]);
  const pct = livePct ?? initialPct;

  if (pct == null || !Number.isFinite(pct)) return null;
  const cls =
    pct > 0
      ? 'bg-up-subtle text-up'
      : pct < 0
        ? 'bg-down-subtle text-down'
        : 'bg-surface-subtle text-ink-body';
  return (
    <span
      className={`rounded-sm px-1.5 py-0.5 font-mono text-[11px] tabular-nums ${cls}`}
    >
      {pct > 0 ? '+' : ''}
      {pct.toFixed(2)}% {window}
    </span>
  );
}
