'use client';

import { useMemo, useState } from 'react';
import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { API_BASE_URL, timeoutSignal } from '@/api/client';
import { useHistory, useSources, type Source } from '@/api/hooks';
import { Button, Segmented } from '@/components/ui';
import {
  isFrameStale,
  useLedgerFollow,
  useLiveClock,
  useTipStream,
} from '@/lib/live/hooks';
import type { components } from '@/api/types';
import { scaleBaseUnits } from '@/lib/format';
import { downloadText, toCsv } from '@/lib/export';
import { trailingEnvelope } from './envelope';

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
type Bar = {
  time: number;
  open: number;
  high: number;
  low: number;
  close: number;
  /** Quote-asset units; absent when the bar states no scale. */
  volume?: number;
};

/**
 * A series bar's quote volume in quote-asset units. `v_quote` is a
 * smallest-unit sum at the scale of the venues in THAT bucket (7 on-chain,
 * 8 CEX, 6 FX), so it is divided by the bar's own `v_quote_decimals`; a bar
 * that states none plots no volume rather than a guessed divisor. The
 * sub-smallest-unit fraction `v_quote` can carry is dropped before the
 * BigInt-safe scale (ADR-0003).
 */
export function seriesQuoteVolume(b: OHLCBar): number | undefined {
  const d = b.v_quote_decimals;
  if (d === null || !Number.isInteger(d) || d < 0) return undefined;
  const whole = /^(\d+)(?:\.\d*)?$/.exec(b.v_quote.trim());
  if (!whole) return undefined;
  return scaleBaseUnits(whole[1], d) ?? undefined;
}

export function toChartBar(b: OHLCBar): Bar {
  return {
    time: Math.floor(new Date(b.t).getTime() / 1000),
    open: Number(b.o),
    high: Number(b.h),
    low: Number(b.l),
    close: Number(b.c),
    volume: seriesQuoteVolume(b),
  };
}

// Interval → seconds, used to size the request (limit = span ÷ interval, capped
// at the API's 1000-bar/request ceiling). /v1/ohlc serves this full grain set,
// but 1m/5m/30m come off the minute aggregate, which a deployment MAY bound to
// a 90-day retention window (migration 0156) — where it does, those three
// return no bars beyond it. The window→grain table below never asks for one
// outside its window, so this is a note for whoever edits that table, not a
// live constraint.
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

const OHLC_CSV_COLUMNS = [
  't',
  'o',
  'h',
  'l',
  'c',
  'v_base',
  'v_quote',
  'v_base_decimals',
  'v_quote_decimals',
  'n',
] as const;

/** The served series as CSV, every value verbatim from /v1/ohlc. */
export function ohlcCsv(bars: readonly OHLCBar[]): string {
  return toCsv(OHLC_CSV_COLUMNS, bars);
}

export function ohlcExportName(
  base: string,
  quote: string,
  interval: string,
  bars: readonly OHLCBar[],
  ext: 'csv' | 'json',
): string {
  // Asset ids carry ':' (code:issuer), which some filesystems reject.
  const safe = (s: string) => s.replace(/[^A-Za-z0-9._-]+/g, '_');
  const stamp = (t: string | undefined) => safe((t ?? '').replace(/[-:]/g, ''));
  const span = `${stamp(bars[0]?.t)}-${stamp(bars[bars.length - 1]?.t)}`;
  return `stellarindex-ohlc-${safe(base)}-${safe(quote)}-${interval}-${span}.${ext}`;
}

// Window → the granularities that make sense for it (bar count in [~24, cap]),
// with a sensible default (the finest that's dense-but-performant). Per the
// chart-data recon: the API accepts any grain for any window, so this offer set
// is a client-side bar-budget choice — showing ALL usable variants per window.
// One server-side bound can apply, and it is why the minute-derived grains stop
// at the 7d row: 1m/5m/30m are served from the minute aggregate, which a
// deployment may bound to 90 days, so offering 5m on the 90d/1y/all windows
// would render an empty chart rather than a coarse one.
type Win = '24h' | '7d' | '30d' | '90d' | '1y' | 'all';
const WINDOWS: {
  key: Win;
  label: string;
  spanSec: number;
  grains: string[];
  def: string;
}[] = [
  {
    key: '24h',
    label: '24h',
    spanSec: 86_400,
    grains: ['5m', '15m', '30m', '1h'],
    def: '5m',
  },
  {
    key: '7d',
    label: '7d',
    spanSec: 604_800,
    grains: ['15m', '30m', '1h', '4h'],
    def: '15m',
  },
  {
    key: '30d',
    label: '30d',
    spanSec: 2_592_000,
    grains: ['1h', '4h', '1d'],
    def: '1h',
  },
  {
    key: '90d',
    label: '90d',
    spanSec: 7_776_000,
    grains: ['4h', '1d'],
    def: '4h',
  },
  {
    key: '1y',
    label: '1y',
    spanSec: 31_536_000,
    grains: ['1d', '1w'],
    def: '1d',
  },
  {
    key: 'all',
    label: 'All',
    spanSec: 157_680_000,
    grains: ['1w', '1mo'],
    def: '1w',
  },
];

// Trailing high/low envelope windows. Only windows longer than the candle are
// offered: a one-candle envelope is just that candle's wicks.
const BAND_WINDOWS = [
  { key: '1h', sec: 3600 },
  { key: '4h', sec: 14_400 },
  { key: '24h', sec: 86_400 },
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
 * than the requested window.
 */
export function MarketChart({
  base,
  quote,
  baseLabel,
  quoteLabel,
  height = 380,
  defaultTimeframe = '7d',
  liveTip = false,
  volatilityBand = false,
  sourceOverlay = false,
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
  /** Offer a trailing high/low envelope (1h/4h/24h) over the candles. */
  volatilityBand?: boolean;
  /** Offer a picker that layers one source's trades over the candles. */
  sourceOverlay?: boolean;
}) {
  const [winKey, setWinKey] = useState<Win>(defaultTimeframe);
  const win = WINDOWS.find((w) => w.key === winKey) ?? WINDOWS[1];
  const [grain, setGrain] = useState<string>(win.def);
  // Guard: if the window changed and the current grain isn't valid for it, snap
  // to the window default (keeps the two controls consistent).
  const activeGrain = win.grains.includes(grain) ? grain : win.def;
  const limit = limitFor(win.spanSec, activeGrain);
  const grainSec = INTERVAL_SEC[activeGrain] ?? 3600;
  const bandOptions = volatilityBand
    ? BAND_WINDOWS.filter((b) => b.sec > grainSec)
    : [];
  const [bandKey, setBandKey] = useState('off');
  // Looked up in the module constant, not bandOptions, so the band memo below
  // depends on a value the compiler knows is never mutated.
  const activeBand =
    (volatilityBand &&
      BAND_WINDOWS.find((b) => b.key === bandKey && b.sec > grainSec)) ||
    null;

  const [overlaySource, setOverlaySource] = useState('');
  const overlayTrades = useHistory(
    overlaySource ? base : undefined,
    quote,
    1000,
    { source: overlaySource, windowSec: win.spanSec },
  );
  const overlay = useMemo(
    () =>
      overlaySource && sourceOverlay
        ? overlayPoints(overlayTrades.data ?? [])
        : null,
    [overlaySource, sourceOverlay, overlayTrades.data],
  );

  const selectWindow = (key: Win) => {
    const next = WINDOWS.find((w) => w.key === key);
    setWinKey(key);
    if (next) setGrain(next.def);
  };

  // Live (RT-2): refresh the active window's candles on each ledger close so
  // the forming bar advances instead of freezing at page load. Prefix key
  // matches every grain/limit for this pair.
  useLedgerFollow(['/v1/ohlc', base, quote]);
  const query = useQuery<OHLCBar[], Error>({
    queryKey: ['/v1/ohlc', base, quote, activeGrain, limit],
    queryFn: async ({ signal }) => {
      const url = `${API_BASE_URL}/v1/ohlc?base=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}&interval=${activeGrain}&limit=${limit}`;
      const r = await fetch(url, { signal: timeoutSignal(undefined, signal) });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const env = (await r.json()) as { data?: { intervals?: OHLCBar[] } };
      return env.data?.intervals ?? [];
    },
  });

  // Chart numbers are lossy doubles; export reads the raw strings instead.
  const raw = query.data;
  const data = useMemo(() => (raw ?? []).map(toChartBar), [raw]);
  // Envelope selection runs over the served strings; only the plotted
  // edges become doubles. Memoized: a new array would make CandleChart re-fit
  // and reset the user's zoom on every clock tick.
  const bandSec = activeBand?.sec;
  const band = useMemo(
    () =>
      raw && bandSec
        ? trailingEnvelope(raw, bandSec, grainSec).map((p) => ({
            time: p.time,
            upper: Number(p.upper),
            lower: Number(p.lower),
          }))
        : null,
    [raw, bandSec, grainSec],
  );
  const loading = query.isLoading;
  const error = query.error ? query.error.message : null;

  const coverageNote = coverageCaption(data, win.spanSec);

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
        {bandOptions.length > 0 && (
          <Segmented
            ariaLabel="Volatility band"
            options={[
              { label: 'No band', value: 'off' },
              ...bandOptions.map((b) => ({
                label: `${b.key} band`,
                value: b.key,
              })),
            ]}
            value={activeBand?.key ?? 'off'}
            onChange={setBandKey}
          />
        )}
        {sourceOverlay && (
          <SourceOverlayPicker
            value={overlaySource}
            onChange={setOverlaySource}
            failed={!!overlaySource && overlayTrades.isError}
          />
        )}
        <span className="text-ink-faint ml-auto font-mono tracking-wider uppercase">
          {baseLabel} / {quoteLabel}
        </span>
        {!error && raw && raw.length > 0 && (
          <div
            role="group"
            aria-label="Download chart data"
            className="flex items-center gap-1"
          >
            <Button
              variant="ghost"
              size="sm"
              onClick={() =>
                downloadText(
                  ohlcExportName(base, quote, activeGrain, raw, 'csv'),
                  'text/csv;charset=utf-8',
                  ohlcCsv(raw),
                )
              }
            >
              CSV
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() =>
                downloadText(
                  ohlcExportName(base, quote, activeGrain, raw, 'json'),
                  'application/json',
                  JSON.stringify(raw, null, 2),
                )
              }
            >
              JSON
            </Button>
          </div>
        )}
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
        <ChartMessage height={height}>
          No price history for this pair + window yet.
        </ChartMessage>
      )}
      {!loading && !error && data.length > 0 && (
        <>
          <CandleChart
            data={data}
            height={height}
            livePrice={livePrice}
            band={band}
            overlay={overlay}
            ariaLabel={`${baseLabel}/${quoteLabel} OHLC candlestick chart with volume, ${activeGrain} candles${activeBand ? `, ${activeBand.key} high/low band` : ''}${overlaySource ? `, ${overlaySource} trades overlaid` : ''}`}
          />
          {coverageNote && (
            <p className="text-ink-faint font-mono text-[11px]">
              {coverageNote}
            </p>
          )}
        </>
      )}
    </div>
  );
}

type HistoryTrade = NonNullable<ReturnType<typeof useHistory>['data']>[number];

// Plots the served decimal price; rows without one (zero-amount legs) are skipped.
export function overlayPoints(
  rows: readonly HistoryTrade[],
): { time: number; value: number }[] {
  const out: { time: number; value: number }[] = [];
  for (const r of rows) {
    const value = Number(r.price);
    const time = Date.parse(r.ts) / 1000;
    if (
      r.price &&
      Number.isFinite(value) &&
      value > 0 &&
      Number.isFinite(time)
    ) {
      out.push({ time, value });
    }
  }
  return out;
}

// /v1/history serves on-chain trades only, so CEX venues are not offered.
export function selectableSources(sources: readonly Source[] | undefined) {
  return (sources ?? []).filter((s) => s.selectable && s.on_chain);
}

function SourceOverlayPicker({
  value,
  onChange,
  failed,
}: {
  value: string;
  onChange: (v: string) => void;
  failed: boolean;
}) {
  const { data } = useSources();
  const options = selectableSources(data);
  if (options.length === 0) return null;
  return (
    <label className="text-ink-muted flex items-center gap-2 font-mono">
      Overlay trades
      <select
        aria-label="Overlay trades from one source"
        className="bg-surface border-border text-ink rounded border px-2 py-1"
        value={value}
        onChange={(e) => onChange(e.target.value)}
      >
        <option value="">None</option>
        {options.map((s) => (
          <option key={s.name} value={s.name}>
            {s.name}
          </option>
        ))}
      </select>
      {failed && <span role="status">Overlay unavailable</span>}
    </label>
  );
}

// A short series has two indistinguishable causes — a young pair or history
// not yet loaded — so the caption states the start date and claims neither.
export function coverageCaption(data: Bar[], spanSec: number): string | null {
  if (data.length === 0) return null;
  const first = data[0].time;
  const last = data[data.length - 1].time;
  const covered = last - first;
  // Missing more than ~15% of the requested span at the start means the
  // series is coverage-limited rather than genuinely flat.
  if (covered < spanSec * 0.85) {
    const from = new Date(first * 1000).toISOString().slice(0, 10);
    return `History from ${from}.`;
  }
  return null;
}

function ChartMessage({
  height,
  children,
}: {
  height: number;
  children: React.ReactNode;
}) {
  return (
    <div
      className="text-ink-muted flex items-center justify-center text-sm"
      style={{ height }}
    >
      {children}
    </div>
  );
}

// ToggleGroup is ui/Segmented — the quiet bg-surface active
// style + WindowPills' a11y semantics won for in-card switches.
