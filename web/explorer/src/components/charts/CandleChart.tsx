'use client';

import { useEffect, useRef } from 'react';
import {
  CandlestickSeries,
  createChart,
  HistogramSeries,
  type CandlestickData,
  type HistogramData,
  type IChartApi,
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
export function CandleChart({ data, height = 360, className, ariaLabel }: CandleChartProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const seriesRef = useRef<ISeriesApi<'Candlestick'> | null>(null);
  const volumeRef = useRef<ISeriesApi<'Histogram'> | null>(null);
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
    };
  }, [height, hasVolume]);

  // Push new data on prop changes (and initial mount) without destroying the chart.
  useEffect(() => {
    const theme = themeRef.current;
    seriesRef.current?.setData(toSeries(data));
    if (theme) volumeRef.current?.setData(toVolume(data, theme));
    chartRef.current?.timeScale().fitContent();
  }, [data]);

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
