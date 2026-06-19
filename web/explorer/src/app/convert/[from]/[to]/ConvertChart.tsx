'use client';

import { useEffect, useState } from 'react';
import dynamic from 'next/dynamic';

import { Panel } from '@/components/reveal';
import { API_BASE_URL, asExample } from '@/api/client';

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-[260px]" /> },
);

type TF = '1w' | '1mo' | '1y' | 'all';
const TIMEFRAMES: { key: TF; label: string; granularity: string }[] = [
  { key: '1w', label: '7d', granularity: '1h' },
  { key: '1mo', label: '30d', granularity: '4h' },
  { key: '1y', label: '1y', granularity: '1d' },
  { key: 'all', label: 'All', granularity: '1d' },
];

interface ChartPoint {
  t: string;
  p?: string | null;
}

/**
 * ConvertChart — the {from}/{to} forex rate over time, as a
 * lightweight-charts line (FX is a single-rate series, so line not
 * OHLC). Served by /v1/chart?asset=fiat:{from}&quote=fiat:{to}, which
 * triangulates the cross-rate via USD. Degrades to a quiet note when
 * the pair has no history.
 */
export function ConvertChart({ from, to }: { from: string; to: string }) {
  const [tf, setTf] = useState<TF>('1mo');
  const [data, setData] = useState<{ time: number; value: number }[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const spec = TIMEFRAMES.find((t) => t.key === tf) ?? TIMEFRAMES[1];
    const controller = new AbortController();
    setLoading(true);
    setError(null);
    const url = `${API_BASE_URL}/v1/chart?asset=fiat:${encodeURIComponent(from)}&quote=fiat:${encodeURIComponent(to)}&timeframe=${tf}&granularity=${spec.granularity}`;
    fetch(url, { signal: controller.signal })
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.json() as Promise<{ data?: { points?: ChartPoint[] } }>;
      })
      .then((env) => {
        const pts = (env.data?.points ?? [])
          .map((p) => ({
            time: Math.floor(new Date(p.t).getTime() / 1000),
            value: p.p != null ? Number(p.p) : NaN,
          }))
          .filter((p) => Number.isFinite(p.value));
        setData(pts);
        setLoading(false);
      })
      .catch((err: Error) => {
        if (err.name === 'AbortError') return;
        setError(err.message);
        setLoading(false);
      });
    return () => controller.abort();
  }, [from, to, tf]);

  return (
    <Panel
      title={`${from}/${to} rate history`}
      source={asExample('/v1/chart', { asset: `fiat:${from}`, quote: `fiat:${to}`, timeframe: tf })}
      bodyClassName="space-y-3"
    >
      <div className="flex flex-wrap items-center gap-1 text-xs">
        {TIMEFRAMES.map((o) => (
          <button
            key={o.key}
            type="button"
            onClick={() => setTf(o.key)}
            aria-pressed={tf === o.key}
            className={`rounded px-2 py-0.5 font-mono uppercase tracking-wider ${
              tf === o.key ? 'bg-brand-600 text-white' : 'text-ink-muted hover:bg-surface-subtle'
            }`}
          >
            {o.label}
          </button>
        ))}
      </div>
      {loading && <div className="h-[260px]" />}
      {error && !loading && (
        <div className="flex h-[260px] items-center justify-center text-sm text-ink-muted">
          {error === 'HTTP 404'
            ? 'No rate history for this pair + window yet.'
            : `Chart unavailable (${error}).`}
        </div>
      )}
      {!loading && !error && data.length === 0 && (
        <div className="flex h-[260px] items-center justify-center text-sm text-ink-muted">
          No rate history for this pair + window yet.
        </div>
      )}
      {!loading && !error && data.length > 0 && (
        <LineChart data={data} height={260} ariaLabel={`${from} to ${to} exchange rate over the ${tf} window`} />
      )}
    </Panel>
  );
}
