'use client';

import { decimalOrNull } from '@/lib/format';

interface Bucket {
  hour: string;
  volume_usd: string;
}

/**
 * SourceSparkline — mini 24h bar chart for /v1/sources?include=stats,sparkline
 * volume_history_24h. 24 vertical bars, height proportional to that
 * hour's USD volume. No labels — pure visual hint of trend.
 */
export function SourceSparkline({
  buckets,
  width = 80,
  height = 24,
}: {
  buckets?: Bucket[];
  width?: number;
  height?: number;
}) {
  if (!buckets || buckets.length === 0) {
    return <span className="text-ink-faint font-mono text-[10px]">—</span>;
  }
  // An absent or unparsable hour draws no bar — a gap, not a zero hour.
  const values = buckets.map((b) => decimalOrNull(b.volume_usd));
  const known = values.filter((v): v is number => v !== null);
  if (known.length === 0) {
    return <span className="text-ink-faint font-mono text-[10px]">—</span>;
  }
  const max = Math.max(...known);
  if (max === 0) {
    return <span className="text-ink-faint font-mono text-[10px]">no vol</span>;
  }
  const barWidth = width / buckets.length;
  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className="inline-block"
    >
      {values.map((v, i) => {
        if (v === null) return null;
        const h = (v / max) * height;
        const x = i * barWidth;
        const y = height - h;
        return (
          <rect
            key={i}
            x={x + 0.5}
            y={y}
            width={Math.max(0.5, barWidth - 1)}
            height={Math.max(0.5, h)}
            className="fill-brand-500/70"
          />
        );
      })}
    </svg>
  );
}
