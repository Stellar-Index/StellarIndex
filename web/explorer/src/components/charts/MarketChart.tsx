'use client';

import { useState } from 'react';
import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { API_BASE_URL } from '@/api/client';
import type { components } from '@/api/types';

// CandleChart pulls in lightweight-charts (~155 KB). Lazy-load it so the
// surrounding page renders without paying the bundle tax up front.
const CandleChart = dynamic(
  () => import('@/components/charts/CandleChart').then((m) => m.CandleChart),
  { ssr: false, loading: () => <div className="h-[360px]" /> },
);

type OHLCBar = components['schemas']['OHLCSeriesBar'];
type Bar = { time: number; open: number; high: number; low: number; close: number; volume: number };

// Interval → seconds, used to size the request (limit = span ÷ interval, capped
// at the API's 1000-bar/request ceiling). /v1/ohlc serves this full grain set.
const INTERVAL_SEC: Record<string, number> = {
  '1m': 60,
  '5m': 300,
  '15m': 900,
  '30m': 1800,
  '1h': 3600,
  '4h': 14400,
  '1d': 86400,
  '1w': 604800,
  '1mo': 2592000,
};
const OHLC_CAP = 1000;

// Window → the granularities that make sense for it (bar count in [~24, cap]),
// with a sensible default (the finest that's dense-but-performant). Per the
// chart-data recon: the API accepts any grain for any window, so this offer set
// is a client-side bar-budget choice — showing ALL usable variants per window.
type Win = '24h' | '7d' | '30d' | '90d' | '1y' | 'all';
const WINDOWS: {
  key: Win;
  label: string;
  spanSec: number;
  grains: string[];
  def: string;
}[] = [
  { key: '24h', label: '24h', spanSec: 86_400, grains: ['5m', '15m', '30m', '1h'], def: '5m' },
  { key: '7d', label: '7d', spanSec: 604_800, grains: ['15m', '30m', '1h', '4h'], def: '15m' },
  { key: '30d', label: '30d', spanSec: 2_592_000, grains: ['1h', '4h', '1d'], def: '1h' },
  { key: '90d', label: '90d', spanSec: 7_776_000, grains: ['4h', '1d'], def: '4h' },
  { key: '1y', label: '1y', spanSec: 31_536_000, grains: ['1d', '1w'], def: '1d' },
  { key: 'all', label: 'All', spanSec: 157_680_000, grains: ['1w', '1mo'], def: '1w' },
];

function limitFor(spanSec: number, interval: string): number {
  const isec = INTERVAL_SEC[interval] ?? 3600;
  return Math.min(OHLC_CAP, Math.ceil(spanSec / isec) + 2);
}

/**
 * MarketChart — the canonical price chart across every market / pair / exchange
 * surface: real OHLC candlesticks with a volume histogram in a pane below,
 * served by GET /v1/ohlc. Two controls: a lookback **window** and an adaptive
 * **granularity** that offers every candle size usable for that window (default
 * = the finest dense one). A coverage caption surfaces when history is shorter
 * than the requested window (backfill still filling in).
 */
export function MarketChart({
  base,
  quote,
  baseLabel,
  quoteLabel,
  height = 380,
  defaultTimeframe = '7d',
}: {
  base: string;
  quote: string;
  baseLabel: string;
  quoteLabel: string;
  height?: number;
  defaultTimeframe?: Win;
}) {
  const [winKey, setWinKey] = useState<Win>(defaultTimeframe);
  const win = WINDOWS.find((w) => w.key === winKey) ?? WINDOWS[1];
  const [grain, setGrain] = useState<string>(win.def);
  // Guard: if the window changed and the current grain isn't valid for it, snap
  // to the window default (keeps the two controls consistent).
  const activeGrain = win.grains.includes(grain) ? grain : win.def;
  const limit = limitFor(win.spanSec, activeGrain);

  const selectWindow = (key: Win) => {
    const next = WINDOWS.find((w) => w.key === key);
    setWinKey(key);
    if (next) setGrain(next.def);
  };

  const query = useQuery<Bar[], Error>({
    queryKey: ['/v1/ohlc', base, quote, activeGrain, limit],
    queryFn: async ({ signal }) => {
      const url = `${API_BASE_URL}/v1/ohlc?base=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}&interval=${activeGrain}&limit=${limit}`;
      const r = await fetch(url, { signal });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const env = (await r.json()) as { data?: { intervals?: OHLCBar[] } };
      return (env.data?.intervals ?? []).map((b) => ({
        time: Math.floor(new Date(b.t).getTime() / 1000),
        open: Number(b.o),
        high: Number(b.h),
        low: Number(b.l),
        close: Number(b.c),
        volume: Number(b.v_quote),
      }));
    },
  });

  const data = query.data ?? [];
  const loading = query.isLoading;
  const error = query.error ? query.error.message : null;

  // Coverage: if the earliest returned bar starts well inside the requested
  // window, history is truncated (backfill in progress) — say so honestly.
  const coverageNote = coverage(data, win.spanSec);

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-xs">
        <ToggleGroup
          options={WINDOWS.map((w) => ({ key: w.key, label: w.label }))}
          value={winKey}
          onSelect={(k) => selectWindow(k as Win)}
        />
        <ToggleGroup
          options={win.grains.map((g) => ({ key: g, label: g }))}
          value={activeGrain}
          onSelect={setGrain}
          subtle
        />
        <span className="ml-auto font-mono uppercase tracking-wider text-ink-faint">
          {baseLabel} / {quoteLabel}
        </span>
      </div>
      {loading && <ChartMessage height={height}>Loading…</ChartMessage>}
      {error && !loading && (
        <ChartMessage height={height}>
          {error === 'HTTP 404'
            ? 'No price history for this pair + window yet.'
            : `Chart unavailable (${error}).`}
        </ChartMessage>
      )}
      {!loading && !error && data.length === 0 && (
        <ChartMessage height={height}>No price history for this pair + window yet.</ChartMessage>
      )}
      {!loading && !error && data.length > 0 && (
        <>
          <CandleChart
            data={data}
            height={height}
            ariaLabel={`${baseLabel}/${quoteLabel} OHLC candlestick chart with volume, ${activeGrain} candles`}
          />
          {coverageNote && (
            <p className="font-mono text-[11px] text-ink-faint">{coverageNote}</p>
          )}
        </>
      )}
    </div>
  );
}

function coverage(data: Bar[], spanSec: number): string | null {
  if (data.length === 0) return null;
  const first = data[0].time;
  const last = data[data.length - 1].time;
  const covered = last - first;
  // If we're missing more than ~15% of the requested span at the start, the
  // series is coverage-limited rather than genuinely flat.
  if (covered < spanSec * 0.85) {
    const from = new Date(first * 1000).toISOString().slice(0, 10);
    return `History begins ${from} — earlier data still backfilling.`;
  }
  return null;
}

function ChartMessage({ height, children }: { height: number; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-center text-sm text-ink-muted" style={{ height }}>
      {children}
    </div>
  );
}

// A small segmented toggle — mono uppercase, low radius, brand active state.
function ToggleGroup({
  options,
  value,
  onSelect,
  subtle,
}: {
  options: { key: string; label: string }[];
  value: string;
  onSelect: (key: string) => void;
  subtle?: boolean;
}) {
  return (
    <div className="inline-flex gap-0.5 rounded-md border border-line bg-surface p-0.5">
      {options.map((o) => {
        const active = value === o.key;
        return (
          <button
            key={o.key}
            type="button"
            onClick={() => onSelect(o.key)}
            aria-pressed={active}
            className={`rounded-sm px-2 py-0.5 text-[11px] font-mono uppercase tracking-wider transition-colors ${
              active
                ? subtle
                  ? 'bg-surface-muted text-brand-400'
                  : 'bg-brand-600 text-white'
                : 'text-ink-muted hover:bg-surface-subtle hover:text-ink-body'
            }`}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}
