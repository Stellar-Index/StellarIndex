'use client';

import { TrendingDown, TrendingUp } from 'lucide-react';

import { useChangeSummary, type ChangeSummary } from '@/api/hooks';
import { DirectionPill, FreshnessMarker } from '@/components/primitives';
import { compareDecimalStrings, formatPriceSmall } from '@/lib/format';

type DeltaWindow = {
  label: string;
  /** Percent change: 5.2 means +5.2%. Null = no data. */
  deltaPct: number | null;
};

type Acceleration = 'increasing' | 'flat' | 'decreasing';

const ACCELERATION: Record<string, { glyph: string; label: string }> = {
  'up:increasing': { glyph: '↗↗', label: 'accelerating up' },
  'up:flat': { glyph: '↗→', label: 'steady up' },
  'up:decreasing': { glyph: '↗↘', label: 'decelerating up' },
  'down:increasing': { glyph: '↘↘', label: 'accelerating down' },
  'down:flat': { glyph: '↘→', label: 'steady down' },
  'down:decreasing': { glyph: '↘↗', label: 'recovering' },
};

/**
 * unwrapChangeSummary — useChangeSummary returns the bare row; the
 * /v1/changes/{entity_type}/{id} envelope ({data, as_of, flags}) is
 * accepted too, for a caller holding the raw response. Exported for tests.
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
  const atl =
    summary.atl_value != null &&
    compareDecimalStrings(summary.atl_value, '0') != null
      ? summary.atl_value
      : null;
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
      <FreshnessMarker flags={q.data?.flags} />
      {hasAny && (
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5 text-xs">
          {windows.map((w) => (
            <span key={w.label} className="inline-flex items-center gap-1">
              <span className="text-ink-muted">{w.label}:</span>
              <DirectionPill deltaPct={w.deltaPct} compact />
            </span>
          ))}
        </div>
      )}
      {streakOk && (
        <StreakChip
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
      {atl != null && (
        <span
          className="bg-surface-subtle text-ink-muted inline-flex items-center gap-1 rounded-sm px-2 py-0.5 font-mono text-xs tabular-nums"
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

export function StreakChip({
  direction,
  days,
}: {
  direction: 'up' | 'down';
  days: number;
}) {
  const Icon = direction === 'up' ? TrendingUp : TrendingDown;
  const tone =
    direction === 'up'
      ? 'bg-up-subtle/50 text-up-strong'
      : 'bg-down-subtle/50 text-down-strong';
  return (
    <span
      className={`inline-flex items-center gap-1 rounded-sm px-2 py-0.5 text-xs font-medium ${tone}`}
    >
      <Icon className="h-3 w-3" aria-hidden />
      {days} {days === 1 ? 'day' : 'days'} {direction}
    </span>
  );
}

/** First derivative (direction) × second (acceleration) as one glyph. */
export function AccelerationArrow({
  direction,
  acceleration,
}: {
  direction: 'up' | 'down';
  acceleration: Acceleration;
}) {
  const a = ACCELERATION[`${direction}:${acceleration}`];
  const tone = direction === 'up' ? 'text-up-strong' : 'text-down-strong';
  return (
    <span
      className={`text-xs font-bold tabular-nums ${tone}`}
      aria-label={a.label}
      title={a.label}
    >
      {a.glyph}
    </span>
  );
}
