// Shared honesty helpers for chart time series.
//
// Two fabrication classes live here so every chart fixes them ONCE:
//
// 1. The partial trailing daily bucket: a daily-grain series whose last
//    point is TODAY's (UTC) still-accumulating bucket plots as if the day
//    were complete — a phantom cliff at the right edge, and "Latest"
//    headlines promote the partial figure. The server is growing its own
//    exclusion; the frontend stays robust regardless by dropping the
//    trailing today-point from daily series (hourly intraday series keep
//    the live day — that's their whole point).
//
// 2. The epoch-0 fallback: mapping a non-parsable date to time 0 feeds
//    lightweight-charts a 1970 point (and two bad dates feed it duplicate
//    ascending-order-violating times, which throws). Non-parsable dates
//    yield null here and callers drop the point instead.

/** Today's UTC calendar date as YYYY-MM-DD. */
export function todayUTC(now: Date = new Date()): string {
  return now.toISOString().slice(0, 10);
}

/**
 * isPartialTodayDate — true when `date` is a DAILY-grain stamp
 * (YYYY-MM-DD) for today's still-accumulating UTC day. Hourly stamps
 * (YYYY-MM-DDTHH:MM, the 24h-window grain) are never "partial-day":
 * intraday charts are meant to show the live day.
 */
export function isPartialTodayDate(
  date: string | null | undefined,
  now: Date = new Date(),
): boolean {
  return !!date && date.length === 10 && date === todayUTC(now);
}

/**
 * dropPartialTrailingDay — drop a trailing point whose date is today's
 * UTC day from a daily-grain series. A no-op for hourly series and for
 * series that already end on a complete day. Only the LAST point is ever
 * dropped — an interior today-point would indicate out-of-order data,
 * which is not this helper's problem to hide.
 */
export function dropPartialTrailingDay<T extends { date?: string | null }>(
  points: T[],
  now: Date = new Date(),
): T[] {
  if (points.length === 0) return points;
  const last = points[points.length - 1];
  return isPartialTodayDate(last.date ?? null, now) ? points.slice(0, -1) : points;
}

/**
 * seriesPointTime — tolerant point date → unix seconds, shared by the
 * daily ("YYYY-MM-DD") and hourly ("YYYY-MM-DDTHH:MM") point shapes.
 * Returns null for a non-parsable date so callers DROP the point —
 * never the old epoch-0 fallback, which plotted bad dates at 1970 and
 * could feed lightweight-charts duplicate time:0 entries (an
 * ascending-order assertion crash).
 */
export function seriesPointTime(date: string): number | null {
  const iso =
    date.length === 10
      ? `${date}T00:00:00Z`
      : date.length === 16
        ? `${date}:00Z`
        : date;
  const ms = Date.parse(iso);
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : null;
}
