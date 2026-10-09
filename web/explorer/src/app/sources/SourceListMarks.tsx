// Same thresholds as CursorAgo: <60s ok, <10min warn, else bad.
export function lagTone(lagSeconds: number): 'ok' | 'warn' | 'bad' {
  return lagSeconds < 60 ? 'ok' : lagSeconds < 600 ? 'warn' : 'bad';
}

const DOT_BG = {
  ok: 'bg-ok-500',
  warn: 'bg-warn-500',
  bad: 'bg-bad-500',
} as const;

export function LagDot({ lagSeconds }: { lagSeconds: number }) {
  const tone = lagTone(lagSeconds);
  return (
    <span
      role="img"
      aria-label={`ingest lag ${tone}`}
      data-tone={tone}
      className={`mr-1.5 inline-block h-2 w-2 rounded-full ${DOT_BG[tone]}`}
    />
  );
}

/** Thin bar scaled against the largest count in the group. */
export function CountBar({ value, max }: { value: number; max: number }) {
  if (!(max > 0) || !(value > 0)) return null;
  const pct = Math.max((value / max) * 100, 2);
  return (
    <div
      role="img"
      aria-label={`${value.toLocaleString('en-US')} of ${max.toLocaleString('en-US')} group max`}
      className="bg-surface-subtle mt-1 ml-auto h-1 w-20 rounded-full"
    >
      <div
        className="bg-brand-500 h-1 rounded-full"
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}
