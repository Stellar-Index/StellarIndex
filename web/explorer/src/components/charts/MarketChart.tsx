'use client';

import { useState } from 'react';
import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { API_BASE_URL } from '@/api/client';
import { Segmented } from '@/components/ui';
import {
  isFrameStale,
  useLedgerFollow,
  useLiveClock,
  useTipStream,
} from '@/lib/live/hooks';
import type { components } from '@/api/types';

/** A tip tick drives the chart's live price line while fresher than this
 * (producer window ~5s; 30s of silence = wedged stream / backgrounded tab
 * → drop the live line, fall back to the last-closed-candle label). Mirrors
 * LiveAssetPrice's TIP_LIVE_STALE_MS so the chart and headline agree. */
const TIP_LIVE_STALE_MS = 30_000;

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
  liveTip = false,
}: {
  base: string;
  quote: string;
  baseLabel: string;
  quoteLabel: string;
  height?: number;
  defaultTimeframe?: Win;
  /**
   * Opt in to a live current-price line on the right axis, fed by the
   * shared /v1/price/tip/stream for THIS pair (same source the headline
   * LiveAssetPrice uses). Off by default so multi-chart boards don't each
   * open an SSE tip connection — turn it on for single-pair/asset pages.
   */
  liveTip?: boolean;
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

  // Live (RT-2): refresh the active window's candles on each ledger close so
  // the forming bar advances instead of freezing at page load. Prefix key
  // matches every grain/limit for this pair.
  useLedgerFollow(['/v1/ohlc', base, quote]);
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

  // Live current-price line: subscribe to this pair's tip stream (same
  // multiplexed source as the headline LiveAssetPrice) so the right-axis
  // price label ticks with each trade. Quoted in the chart's OWN quote so
  // the tip value shares the candles' price scale. Disabled unless liveTip
  // is set; a wedged/quiet stream (or a withheld pair) simply yields null →
  // the static last-closed-candle label is shown instead.
  const tip = useTipStream(liveTip ? base : null, quote);
  const clock = useLiveClock();
  const tipFresh =
    tip != null && !isFrameStale(clock, tip.receivedAt, TIP_LIVE_STALE_MS);
  const tipNum = tipFresh ? Number(tip.data.data?.price) : NaN;
  const livePrice = Number.isFinite(tipNum) && tipNum > 0 ? tipNum : null;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-xs">
        <Segmented
          ariaLabel="Chart window"
          options={WINDOWS.map((w) => ({ label: w.label, value: w.key }))}
          value={winKey}
          onChange={(k) => selectWindow(k as Win)}
        />
        <Segmented
          ariaLabel="Candle interval"
          options={win.grains.map((g) => ({ label: g, value: g }))}
          value={activeGrain}
          onChange={setGrain}
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
            livePrice={livePrice}
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

// FEC A3-F6.2 (2026-08-24): the private ToggleGroup (brand-600 + `subtle`
// active variants) folded onto ui/Segmented — the quiet bg-surface active
// style + WindowPills' a11y semantics won for in-card switches.
