'use client';

// ReasonHeatmap — the /anomalies day×reason calendar: one row per
// freeze reason, one column per UTC day of the served window, cell
// intensity = freeze count (sequential single-hue: the brand tone at
// stepped alpha — magnitude is one hue light→dark, never a rainbow).
// Identity is never color-alone: rows are labeled, every cell carries
// a native title with the exact (day, reason, count), and the feed
// table below remains the accessible fallback.
//
// Honesty: render this ONLY when the API returned a non-null `daily`
// block (`?include=daily` served). Inside a served window, a day with
// no cells IS a real zero — the reader scanned the whole window — so
// empty cells are drawn as empty, not omitted. Callers must not feed
// this component a fabricated window.

export type HeatCell = { day: string; reason: string; count: number };

// Alpha steps for count buckets 1..4+ of the max — a 4-step
// sequential ramp on one hue.
const LEVELS = [0.25, 0.5, 0.75, 1];

export function levelFor(count: number, max: number): number {
  if (count <= 0 || max <= 0) return 0;
  const idx = Math.min(LEVELS.length - 1, Math.ceil((count / max) * LEVELS.length) - 1);
  return LEVELS[idx];
}

/**
 * servedDaysUTC — the day axis (YYYY-MM-DD, oldest → newest) derived from
 * the SERVED cells' date range, never the client clock: a skewed client
 * clock (or a server window not ending today) previously minted columns
 * the reader never scanned — fabricated zeros — and could push served
 * days off the grid entirely. Days BETWEEN served cells are real zeros
 * (the reader scanned the whole served window), so the range is
 * enumerated densely; `maxDays` caps the axis at the window length
 * (ending on the newest served day) so one garbage far-past date can't
 * explode the grid.
 */
export function servedDaysUTC(cells: { day: string }[], maxDays: number): string[] {
  const days = cells
    .map((c) => c.day)
    .filter((d) => /^\d{4}-\d{2}-\d{2}$/.test(d))
    .sort();
  if (days.length === 0) return [];
  const endMs = Date.parse(`${days[days.length - 1]}T00:00:00Z`);
  const startMs = Math.max(
    Date.parse(`${days[0]}T00:00:00Z`),
    endMs - (maxDays - 1) * 86_400_000,
  );
  const out: string[] = [];
  for (let ms = startMs; ms <= endMs; ms += 86_400_000) {
    out.push(new Date(ms).toISOString().slice(0, 10));
  }
  return out;
}

export function ReasonHeatmap({ cells, windowDays }: { cells: HeatCell[]; windowDays: number }) {
  const clean = cells.filter((c) => c.day && c.reason && Number.isFinite(c.count) && c.count > 0);
  if (clean.length === 0) {
    return <p className="text-sm text-ink-muted">No freezes in the last {windowDays} days.</p>;
  }
  const days = servedDaysUTC(clean, windowDays);
  // Row order: heaviest reason first (stable, entity-bound — not
  // repainted by filters; there is no categorical hue to preserve).
  const totals = new Map<string, number>();
  for (const c of clean) totals.set(c.reason, (totals.get(c.reason) ?? 0) + c.count);
  const reasons = [...totals.keys()].sort((a, b) => (totals.get(b) ?? 0) - (totals.get(a) ?? 0));
  const byKey = new Map(clean.map((c) => [`${c.reason}|${c.day}`, c.count]));
  const max = Math.max(...clean.map((c) => c.count));

  return (
    <div className="space-y-2">
      <div className="overflow-x-auto">
        <table
          role="img"
          aria-label={`Freeze events per day and reason over the last ${windowDays} days; darkest cell is ${max} freezes`}
          className="border-separate border-spacing-[2px]"
        >
          <tbody>
            {reasons.map((reason) => (
              <tr key={reason}>
                <td className="whitespace-nowrap pr-2 text-right align-middle font-mono text-[11px] text-ink-body">
                  {reason}
                </td>
                {days.map((day) => {
                  const count = byKey.get(`${reason}|${day}`) ?? 0;
                  const level = levelFor(count, max);
                  return (
                    <td key={day} className="p-0">
                      <span
                        title={`${day} · ${reason}: ${count} freeze${count === 1 ? '' : 's'}`}
                        className="block h-4 w-4 rounded-xs bg-surface-muted"
                      >
                        {level > 0 && (
                          <span
                            aria-hidden
                            className="block h-full w-full rounded-xs"
                            style={{ backgroundColor: 'var(--color-brand-500)', opacity: level }}
                          />
                        )}
                      </span>
                    </td>
                  );
                })}
              </tr>
            ))}
            <tr>
              <td />
              <td colSpan={days.length}>
                <div className="flex justify-between pt-1 font-mono text-[10px] text-ink-faint">
                  <span>{days[0]}</span>
                  <span>{days[days.length - 1]}</span>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div className="flex items-center gap-1.5 text-[10px] text-ink-muted">
        <span>0</span>
        <span className="inline-block h-3 w-3 rounded-xs bg-surface-muted" />
        {LEVELS.map((l) => (
          <span
            key={l}
            className="inline-block h-3 w-3 rounded-xs"
            style={{ backgroundColor: 'var(--color-brand-500)', opacity: l }}
          />
        ))}
        <span>{max}</span>
        <span className="pl-1">freezes / day</span>
      </div>
    </div>
  );
}
