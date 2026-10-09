import { AssetText } from '@/components/AssetLink';

export type GanttEvent = {
  asset_id?: string;
  quote_id?: string;
  frozen_at?: string;
  recovered_at?: string | null;
  firing?: boolean;
  reason?: string;
};

export type GanttSpan = {
  leftPct: number;
  widthPct: number;
  firing: boolean;
  title: string;
};

export type GanttRow = { asset: string; quote: string; spans: GanttSpan[] };

// A span narrower than this would vanish; it still marks that a freeze happened.
const MIN_WIDTH_PCT = 0.6;

/** Lays freezes out per pair over [earliest frozen_at, now]; still-firing spans run to now. */
export function ganttRows(
  events: readonly GanttEvent[],
  nowMs: number,
  maxRows = 12,
): { rows: GanttRow[]; startMs: number } | null {
  const valid = events
    .map((e) => {
      const start = new Date(e.frozen_at ?? '').getTime();
      const end = e.firing ? nowMs : new Date(e.recovered_at ?? '').getTime();
      return { e, start, end };
    })
    .filter(
      (x) =>
        x.e.asset_id &&
        x.e.quote_id &&
        Number.isFinite(x.start) &&
        Number.isFinite(x.end) &&
        x.end >= x.start,
    );
  if (valid.length === 0) return null;
  const startMs = Math.min(...valid.map((x) => x.start));
  const span = Math.max(1, nowMs - startMs);

  const byPair = new Map<string, { row: GanttRow; latest: number }>();
  for (const { e, start, end } of valid) {
    const key = `${e.asset_id}\u0000${e.quote_id}`;
    let entry = byPair.get(key);
    if (!entry) {
      entry = {
        row: { asset: e.asset_id!, quote: e.quote_id!, spans: [] },
        latest: start,
      };
      byPair.set(key, entry);
    }
    entry.latest = Math.max(entry.latest, start);
    entry.row.spans.push({
      leftPct: ((start - startMs) / span) * 100,
      widthPct: Math.max(MIN_WIDTH_PCT, ((end - start) / span) * 100),
      firing: !!e.firing,
      title: `${e.reason ?? 'freeze'}: ${e.frozen_at} → ${
        e.firing ? 'still firing' : e.recovered_at
      }`,
    });
  }
  const rows = [...byPair.values()]
    .sort((a, b) => b.latest - a.latest)
    .slice(0, maxRows)
    .map((x) => x.row);
  return { rows, startMs };
}

export function FreezeGantt({
  events,
  nowMs,
}: {
  events: readonly GanttEvent[];
  /** The fetch time: still-firing spans run to it. */
  nowMs: number;
}) {
  const g = ganttRows(events, nowMs);
  if (!g) return null;
  return (
    <div
      role="img"
      aria-label={`Freeze timeline for the ${g.rows.length} most recently frozen pairs`}
      className="space-y-1.5"
    >
      {g.rows.map((r) => (
        <div
          key={`${r.asset}/${r.quote}`}
          className="grid grid-cols-[9rem_1fr] items-center gap-3 text-xs"
        >
          <span className="text-ink-body truncate font-mono">
            <AssetText canonical={r.asset} />/<AssetText canonical={r.quote} />
          </span>
          <span className="bg-surface-muted relative block h-3 rounded-sm">
            {r.spans.map((s, i) => (
              <span
                key={i}
                title={s.title}
                className={`absolute inset-y-0 rounded-sm ${
                  s.firing ? 'bg-down-strong' : 'bg-warn-500'
                }`}
                style={{
                  left: `${s.leftPct}%`,
                  width: `${Math.min(s.widthPct, 100 - s.leftPct)}%`,
                }}
              />
            ))}
          </span>
        </div>
      ))}
      <div className="text-ink-muted flex justify-between pl-[calc(9rem+0.75rem)] text-[10px]">
        <span>
          {new Date(g.startMs).toISOString().slice(0, 16).replace('T', ' ')}Z
        </span>
        <span>now</span>
      </div>
    </div>
  );
}
