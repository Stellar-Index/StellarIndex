import type { LinePoint } from './LineChart';

/**
 * Served daily points → chart geometry, with every missing day made
 * EXPLICIT.
 *
 * The RWA surfaces omit a day they could not measure. Handing those
 * points straight to the chart would join the two sides of the hole
 * with a straight line — the fabrication the payloads refuse on the
 * wire, reintroduced in pixels and drawn at the same confidence as the
 * days that were real. So the missing days are emitted as gap points
 * (null value) and the line breaks at them.
 *
 * This lives here rather than in either panel because BOTH of them need
 * it and a hole is not a thing two charts should be allowed to disagree
 * about.
 *
 * Values are parsed to JS numbers for the y-coordinate ONLY. The wire
 * carries exact decimal strings (ADR-0003) and every figure a panel
 * PRINTS comes from those; a pixel is allowed to be a float.
 */
export const DAY_SECONDS = 86_400;

/**
 * Ceiling on how many day slots one series may occupy. The server caps
 * the POINT count; this caps the SPAN, so a series with two points a
 * decade apart cannot make the gap filling below allocate thousands of
 * slots. Past it the points are plotted as they came, and the only
 * casualty is that the holes are drawn joined.
 */
export const MAX_DAY_SLOTS = 4096;

/** Unix seconds for a served RFC 3339 day, or null when unparsable. */
export function dayTime(t: string): number | null {
  const ms = Date.parse(t);
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : null;
}

/**
 * Build a gap-aware line from points that carry an RFC 3339 day.
 *
 * `value` reads the y figure off a point; `volume`, when given, reads
 * the figure for the pane below the line.
 */
export function toDailyLine<T extends { t: string }>(
  points: T[],
  value: (p: T) => number,
  volume?: (p: T) => number,
): LinePoint[] {
  const byDay = new Map<number, T>();
  for (const p of points) {
    const t = dayTime(p.t);
    if (t != null) byDay.set(t - (t % DAY_SECONDS), p);
  }
  const days = [...byDay.keys()].sort((a, b) => a - b);
  if (days.length === 0) return [];

  const plot = (t: number, p: T): LinePoint => ({
    time: t,
    value: value(p),
    ...(volume ? { volume: volume(p) } : {}),
  });
  const first = days[0];
  const last = days[days.length - 1];
  if ((last - first) / DAY_SECONDS + 1 > MAX_DAY_SLOTS) {
    return days.map((t) => plot(t, byDay.get(t) as T));
  }
  const out: LinePoint[] = [];
  for (let t = first; t <= last; t += DAY_SECONDS) {
    const p = byDay.get(t);
    out.push(p ? plot(t, p) : { time: t, value: null });
  }
  return out;
}

/**
 * Hues keyed to a STABLE identity, never to rank.
 *
 * The window switcher re-ranks the lines: over a year one instrument
 * leads, over a month another may. Assigning the palette by row number
 * would repaint every survivor on that switch, so a reader who learned
 * "USTRY is amber" is misled by their own filter. Hues are therefore
 * handed out over the keys SORTED, which does not move when the values
 * do, while the caller keeps whatever draw order it wants.
 *
 * `palette` is consumed in fixed order and never cycled: the caller
 * must have already bounded the series count to its length.
 */
export function hueByIdentity(
  keys: string[],
  palette: string[],
): Map<string, string> {
  const hue = new Map<string, string>();
  [...keys]
    .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0))
    .forEach((k, i) => hue.set(k, palette[i]));
  return hue;
}
