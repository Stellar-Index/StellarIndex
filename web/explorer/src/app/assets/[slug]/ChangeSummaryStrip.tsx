'use client';

import {
  ChevronDown,
  ChevronsDown,
  ChevronsUp,
  ChevronUp,
  MoveRight,
  TrendingDown,
  TrendingUp,
  type LucideIcon,
} from 'lucide-react';

import { useChangeSummary, type ChangeSummary } from '@/api/hooks';
import { DirectionPill, FreshnessMarker } from '@/components/primitives';
import { Badge } from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { compareDecimalStrings, formatPriceSmall } from '@/lib/format';

type DeltaWindow = {
  label: string;
  /** Percent change: 5.2 means +5.2%. Null = no data. */
  deltaPct: number | null;
};

type Acceleration = 'increasing' | 'flat' | 'decreasing';

// The chevron points where momentum is heading, not where price is.
const ACCELERATION: Record<string, { label: string; icon: LucideIcon }> = {
  'up:increasing': { label: 'accelerating up', icon: ChevronsUp },
  'up:flat': { label: 'steady up', icon: MoveRight },
  'up:decreasing': { label: 'decelerating up', icon: ChevronDown },
  'down:increasing': { label: 'accelerating down', icon: ChevronsDown },
  'down:flat': { label: 'steady down', icon: MoveRight },
  'down:decreasing': { label: 'recovering', icon: ChevronUp },
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
        <div className="grid w-full grid-cols-4 gap-2">
          {windows.map((w) => (
            <div key={w.label} className="flex flex-col items-start gap-1">
              <span className={PANEL_LABEL_CLASS}>{w.label}</span>
              <DirectionPill deltaPct={w.deltaPct} compact />
            </div>
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
        <Badge
          tone="neutral"
          title={`Lowest USD value observed since this asset entered the index${summary.atl_at ? ` (${summary.atl_at.slice(0, 10)})` : ''} — not an all-time low over full market history.`}
        >
          low since indexed
          <span className="font-mono tabular-nums">
            ${formatPriceSmall(atl)}
          </span>
        </Badge>
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
  return (
    <Badge tone={direction}>
      <Icon className="h-3 w-3" aria-hidden />
      {days} {days === 1 ? 'day' : 'days'} {direction}
    </Badge>
  );
}

/** First derivative (direction) × second (acceleration): one icon and its name. */
export function AccelerationArrow({
  direction,
  acceleration,
}: {
  direction: 'up' | 'down';
  acceleration: Acceleration;
}) {
  const mark = ACCELERATION[`${direction}:${acceleration}`];
  if (!mark) return null;
  const Icon = mark.icon;
  // Momentum is not price direction, so it never borrows the up/down colours.
  return (
    <span
      className="text-ink-muted inline-flex items-center gap-1 text-xs"
      title="Momentum over the streak"
    >
      <Icon className="h-3 w-3" aria-hidden />
      {mark.label}
    </span>
  );
}
