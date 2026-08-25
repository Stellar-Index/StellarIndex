'use client';

import { useEffect, useRef } from 'react';
import {
  CandlestickSeries,
  createChart,
  HistogramSeries,
  LineStyle,
  type CandlestickData,
  type HistogramData,
  type IChartApi,
  type IPriceLine,
  type ISeriesApi,
  type Time,
} from 'lightweight-charts';

import { localTickMarkFormatter, localCrosshairTimeFormatter } from './localTime';
import { readChartTheme, baseChartOptions, type ChartTheme } from './chartTheme';

export type CandlePoint = {
  /** Unix epoch seconds */
  time: number;
  open: number;
  high: number;
  low: number;
  close: number;
  /** Optional per-bar volume — renders a histogram in the pane below. */
  volume?: number;
};

export type CandleChartProps = {
  data: CandlePoint[];
  height?: number;
  className?: string;
  /**
   * Live tip price (asset in the chart's quote units). When set, it
   * drives the right-axis current-price line so the label TICKS with
   * each trade instead of freezing at the last closed candle's close.
   * The series' own static last-value label is hidden while a live
   * price is present, so there's exactly one price line. Null/absent →
   * the static last-close label is shown as before.
   */
  livePrice?: number | null;
  /**
   * Text alternative for the canvas-rendered chart (WCAG 1.1.1).
   * lightweight-charts paints to a <canvas> with no DOM text, so
   * screen readers get nothing without this.
   */
  ariaLabel?: string;
};

/**
 * CandleChart — TradingView Lightweight Charts wrapper: OHLC candlesticks with
 * an optional VOLUME histogram in a separate pane **below** the price pane
 * (v5 native multi-pane, ~75/25 split), sharing one time axis. Colours are
 * driven by the dark design tokens via ./chartTheme (no hardcoded literals).
 *
 * The component owns the chart lifecycle: create on mount, dispose on unmount.
 * Data updates push via setData rather than tearing down the chart.
 */
export function CandleChart({
  data,
  height = 360,
  className,
  livePrice,
  ariaLabel,
}: CandleChartProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const seriesRef = useRef<ISeriesApi<'Candlestick'> | null>(null);
  const volumeRef = useRef<ISeriesApi<'Histogram'> | null>(null);
  const priceLineRef = useRef<IPriceLine | null>(null);
  const themeRef = useRef<ChartTheme | null>(null);

  const hasVolume = data.some((p) => p.volume != null && Number.isFinite(p.volume));

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const theme = readChartTheme();
    themeRef.current = theme;

    const chart = createChart(container, {
      ...baseChartOptions(theme, { timeVisible: true }),
      timeScale: {
        timeVisible: true,
        secondsVisible: false,
        borderColor: theme.border,
        // Local-time axis labels — the default UTC reads as "stale".
        tickMarkFormatter: localTickMarkFormatter,
      },
      localization: {
        timeFormatter: localCrosshairTimeFormatter,
      },
      rightPriceScale: {
        borderColor: theme.border,
        scaleMargins: { top: 0.1, bottom: 0.08 },
      },
      width: container.clientWidth,
      height,
    });
    chartRef.current = chart;

    const series = chart.addSeries(CandlestickSeries, {
      upColor: theme.up,
      downColor: theme.down,
      wickUpColor: theme.up,
      wickDownColor: theme.down,
      borderVisible: false,
    });
    seriesRef.current = series;

    if (hasVolume) {
      // Volume in its own pane (index 1), below the price pane.
      const volume = chart.addSeries(
        HistogramSeries,
        { priceFormat: { type: 'volume' }, priceLineVisible: false, lastValueVisible: false },
        1,
      );
      volume.priceScale().applyOptions({ scaleMargins: { top: 0.15, bottom: 0 } });
      volumeRef.current = volume;
      // Split ~75% price / ~25% volume.
      const panes = chart.panes();
      if (panes.length > 1) {
        panes[0].setStretchFactor(3);
        panes[1].setStretchFactor(1);
      }
    }

    const ro = new ResizeObserver((entries) => {
      for (const e of entries) {
        chart.applyOptions({ width: e.contentRect.width });
      }
    });
    ro.observe(container);

    return () => {
      ro.disconnect();
      chart.remove();
      chartRef.current = null;
      seriesRef.current = null;
      volumeRef.current = null;
      // The price line is owned by the disposed series; drop the handle so
      // the live-price effect recreates it against the fresh series rather
      // than calling applyOptions on a dead one.
      priceLineRef.current = null;
    };
  }, [height, hasVolume]);

  // Push new data on prop changes (and initial mount) without destroying the chart.
  useEffect(() => {
    const theme = themeRef.current;
    // Adaptive price precision (2026-08-05): lightweight-charts
    // defaults to 2 decimals, which renders XLM as a flat "$0.17" and
    // any sub-cent asset as "$0.00". Scale the axis/crosshair/legend
    // precision to the series' actual magnitude.
    const precision = pricePrecisionFor(data);
    seriesRef.current?.applyOptions({
      priceFormat: { type: 'price', precision, minMove: 10 ** -precision },
    });
    seriesRef.current?.setData(toSeries(data));
    if (theme) volumeRef.current?.setData(toVolume(data, theme));
    chartRef.current?.timeScale().fitContent();
  }, [data]);

  // Live current-price line (RT-2): mirror the headline's live tip onto the
  // chart's right axis so the current-price label ticks with each trade
  // instead of freezing at the last closed candle. While a live price is
  // present we hide the series' own static last-value label (else two labels
  // stack on the axis) and drive a single price line that we recolour +
  // reprice on every tick. No live tip → restore the static label.
  useEffect(() => {
    const series = seriesRef.current;
    if (!series) return;
    const hasLive =
      livePrice != null && Number.isFinite(livePrice) && livePrice > 0;

    series.applyOptions({
      lastValueVisible: !hasLive,
      priceLineVisible: !hasLive,
    });

    if (!hasLive) {
      if (priceLineRef.current) {
        series.removePriceLine(priceLineRef.current);
        priceLineRef.current = null;
      }
      return;
    }

    // Colour by where the live tick sits vs the last closed candle — up
    // (green) above, down (red) below — the same direction semantics the
    // headline price flashes on.
    const theme = themeRef.current;
    const lastClose = data.length ? data[data.length - 1].close : null;
    const color =
      lastClose != null && (livePrice as number) < lastClose
        ? (theme?.down ?? '#f6465d')
        : (theme?.up ?? '#31c48d');
    const opts = {
      price: livePrice as number,
      color,
      lineWidth: 1 as const,
      lineStyle: LineStyle.Solid,
      axisLabelVisible: true,
      title: '',
    };
    if (priceLineRef.current) {
      priceLineRef.current.applyOptions(opts);
    } else {
      priceLineRef.current = series.createPriceLine(opts);
    }
  }, [data, livePrice]);

  return (
    <div
      ref={containerRef}
      className={className}
      style={{ width: '100%', height }}
      role="img"
      aria-label={
        ariaLabel ??
        `Candlestick price chart${data.length ? ` with ${data.length} bars` : ''}`
      }
    />
  );
}

// pricePrecisionFor picks the axis decimal count from the series'
// magnitude: enough significant digits that intraday movement is
// visible (XLM at ~$0.17 gets 6 decimals, not "0.17"), without
// rendering BTC-scale values with absurd tails. Exported for tests.
export function pricePrecisionFor(points: CandlePoint[]): number {
  let max = 0;
  for (const p of points) {
    if (p.close > max) max = p.close;
    if (p.high > max) max = p.high;
  }
  if (max === 0) return 2;
  if (max >= 1000) return 2;
  if (max >= 10) return 4;
  if (max >= 0.01) return 6;
  if (max >= 0.0001) return 8;
  return 10;
}

function toSeries(points: CandlePoint[]): CandlestickData<Time>[] {
  return points.map((p) => ({
    time: p.time as Time,
    open: p.open,
    high: p.high,
    low: p.low,
    close: p.close,
  }));
}

// Volume bars, tinted to the bar's direction (up when close ≥ open) at low
// opacity so they read as context, not foreground.
function toVolume(points: CandlePoint[], theme: ChartTheme): HistogramData<Time>[] {
  return points
    .filter((p) => p.volume != null && Number.isFinite(p.volume))
    .map((p) => ({
      time: p.time as Time,
      value: p.volume as number,
      color: p.close >= p.open ? theme.volUp : theme.volDown,
    }));
}
