import { compareDecimalStrings } from '@/lib/format';

export type EnvelopeBar = { t: string; h: string; l: string };
export type EnvelopePoint = { time: number; upper: string; lower: string };

/**
 * trailingEnvelope — per bar, the highest high and lowest low over the bars
 * that open inside the trailing `windowSec` ending with it, selected exactly
 * over the served decimal strings. `bars` must be ascending. A point is
 * emitted only once the window lies wholly inside the fetched series, and
 * skipped when any bar in its window carries a non-decimal high or low, so
 * the band never understates the range. Missing buckets inside a window are
 * no-trade intervals and contribute nothing.
 */
export function trailingEnvelope(
  bars: readonly EnvelopeBar[],
  windowSec: number,
  grainSec: number,
): EnvelopePoint[] {
  const times = bars.map((b) => Math.floor(new Date(b.t).getTime() / 1000));
  const out: EnvelopePoint[] = [];
  let start = 0;
  for (let i = 0; i < bars.length; i++) {
    if (times[i] + grainSec - windowSec < times[0]) continue;
    while (times[start] <= times[i] - windowSec) start++;
    const p = windowExtremes(bars, start, i);
    if (p) out.push({ time: times[i], ...p });
  }
  return out;
}

function windowExtremes(
  bars: readonly EnvelopeBar[],
  from: number,
  to: number,
): { upper: string; lower: string } | null {
  let upper = bars[from].h;
  let lower = bars[from].l;
  for (let j = from; j <= to; j++) {
    const hi = compareDecimalStrings(bars[j].h, upper);
    const lo = compareDecimalStrings(bars[j].l, lower);
    if (hi === null || lo === null) return null;
    if (hi > 0) upper = bars[j].h;
    if (lo < 0) lower = bars[j].l;
  }
  return { upper, lower };
}
