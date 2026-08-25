'use client';

import { useChangeSummary, type ChangeSummary } from '@/api/hooks';
import {
  AccelerationArrow,
  MultiWindowDelta,
  StreakIndicator,
  type DeltaWindow,
} from '@/components/primitives';
import { formatPriceSmall } from '@/lib/format';

/**
 * unwrapChangeSummary — the /v1/changes/{entity_type}/{id} endpoint
 * returns the standard envelope ({data, as_of, flags}); useChangeSummary
 * currently hands back the raw JSON. Accept either the envelope or a
 * bare row so the strip keeps working if the hook later unwraps.
 * Exported for tests.
 */
export function unwrapChangeSummary(raw: unknown): ChangeSummary | null {
  if (raw == null || typeof raw !== 'object') return null;
  const maybe = raw as { data?: unknown; entity_id?: unknown };
  if (maybe.data != null && typeof maybe.data === 'object') {
    return maybe.data as ChangeSummary;
  }
  return maybe.entity_id != null ? (raw as ChangeSummary) : null;
}

/**
 * buildChangeWindows — the canonical 1h/24h/7d/30d strip from a
 * change-summary row. Windows the worker hasn't computed stay null so
 * the pill renders "—", never a fabricated zero. Exported for tests.
 */
export function buildChangeWindows(s: ChangeSummary): DeltaWindow[] {
  return [
    { label: '1h', deltaPct: s.h1_delta_pct ?? null },
    { label: '24h', deltaPct: s.h24_delta_pct ?? null },
    { label: '7d', deltaPct: s.d7_delta_pct ?? null },
    { label: '30d', deltaPct: s.d30_delta_pct ?? null },
  ];
}

/**
 * ChangeSummaryStrip — the live multi-window delta strip for one asset,
 * from GET /v1/changes/coin/{id} (the 5-minute change-summary worker).
 * Progressive enhancement over the build-time change pills: renders
 * nothing while loading and nothing on 404/503 (worker hasn't computed
 * a row / reader unwired) rather than an empty frame.
 *
 * The low-water chip is labelled "since indexed", not "ATL": per the
 * endpoint contract, atl_value is the lowest value observed since the
 * entity entered the index — NOT an all-time low over full history.
 * (The page's real ATH from prices_1d renders separately in the price
 * card, which is why this strip omits the endpoint's ath_value.)
 */
export function ChangeSummaryStrip({
  assetID,
  className,
}: {
  assetID: string;
  className?: string;
}) {
  const q = useChangeSummary('coin', assetID);
  const summary = unwrapChangeSummary(q.data);
  if (!summary) return null;

  const windows = buildChangeWindows(summary);
  const hasAny = windows.some((w) => w.deltaPct != null);
  const atl = summary.atl_value != null ? Number(summary.atl_value) : null;
  const streakOk =
    summary.streak_direction != null &&
    summary.streak_direction !== 'flat' &&
    (summary.streak_days ?? 0) > 0;

  if (!hasAny && atl == null && !streakOk) return null;

  return (
    <div
      className={`border-line-subtle flex flex-wrap items-center gap-x-3 gap-y-2 border-t pt-3 ${className ?? ''}`}
      data-testid="change-summary-strip"
    >
      {hasAny && <MultiWindowDelta windows={windows} compact wrap />}
      {streakOk && (
        <StreakIndicator
          kind="streak"
          direction={summary.streak_direction as 'up' | 'down'}
          days={summary.streak_days ?? 0}
        />
      )}
      {streakOk && summary.acceleration != null && (
        <AccelerationArrow
          direction={summary.streak_direction as 'up' | 'down'}
          acceleration={summary.acceleration}
        />
      )}
      {atl != null && Number.isFinite(atl) && (
        <span
          className="bg-surface-subtle text-ink-muted inline-flex items-center gap-1 rounded-full px-2 py-0.5 font-mono text-xs tabular-nums"
          title={`Lowest USD value observed since this asset entered the index${summary.atl_at ? ` (${summary.atl_at.slice(0, 10)})` : ''} — not an all-time low over full market history.`}
        >
          <span className="text-[10px] tracking-wider uppercase">
            low since indexed
          </span>
          ${formatPriceSmall(atl)}
        </span>
      )}
    </div>
  );
}
