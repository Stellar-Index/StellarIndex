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

/** The window's UTC day strings (YYYY-MM-DD), oldest → today. */
export function windowDaysUTC(windowDays: number, today = new Date()): string[] {
  const out: string[] = [];
  const end = Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate());
  for (let i = windowDays - 1; i >= 0; i--) {
    out.push(new Date(end - i * 86_400_000).toISOString().slice(0, 10));
  }
  return out;
}

export function ReasonHeatmap({ cells, windowDays }: { cells: HeatCell[]; windowDays: number }) {
  const clean = cells.filter((c) => c.day && c.reason && Number.isFinite(c.count) && c.count > 0);
  if (clean.length === 0) {
    return <p className="text-sm text-ink-muted">No freezes in the last {windowDays} days.</p>;
  }
  const days = windowDaysUTC(windowDays);
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
