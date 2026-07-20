'use client';

import { useEffect, useRef } from 'react';
import {
  AreaSeries,
  createChart,
  HistogramSeries,
  type HistogramData,
  type IChartApi,
  type ISeriesApi,
  type LineData,
  type Time,
} from 'lightweight-charts';

import { localTickMarkFormatter, localCrosshairTimeFormatter } from './localTime';
import { readChartTheme, baseChartOptions, type ChartTheme } from './chartTheme';

export type LinePoint = {
  /** Unix epoch seconds */
  time: number;
  value: number;
  /** Optional per-bar volume — renders a histogram in the pane below. */
  volume?: number;
};

export type LineChartProps = {
  data: LinePoint[];
  height?: number;
  className?: string;
  /**
   * Tone the line up/down based on the overall trend. Default derives
   * from first→last sign; pass an explicit boolean to override.
   */
  positive?: boolean;
  /** Text alternative for the canvas-rendered chart (WCAG 1.1.1). */
  ariaLabel?: string;
  /**
   * When false, force the area-fill off and render a thin line only
   * (used for dense count-series like throughput). Default true.
   */
  area?: boolean;
  /**
   * Show time-of-day on the x-axis (for intraday/hourly series).
   * Default false (daily series read better without it).
   */
  timeVisible?: boolean;
  /**
   * When set, render a crosshair-following legend showing the hovered
   * point's line value (and volume, when present).
   */
  legend?: {
    valueLabel: string;
    volumeLabel?: string;
    formatValue?: (n: number) => string;
    formatVolume?: (n: number) => string;
  };
};

/**
 * LineChart — TradingView Lightweight Charts wrapper for scalar (time, value)
 * series, with an OPTIONAL volume histogram in a separate pane **below** (v5
 * native multi-pane). Colours come from the dark design tokens via ./chartTheme.
 * Companion to [CandleChart]; use CandleChart when you have OHLC.
 */
export function LineChart({
  data,
  height = 320,
  className,
  positive,
  ariaLabel,
  area = true,
  timeVisible = false,
  legend,
}: LineChartProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const seriesRef = useRef<ISeriesApi<'Area'> | null>(null);
  const volumeRef = useRef<ISeriesApi<'Histogram'> | null>(null);
  const themeRef = useRef<ChartTheme | null>(null);
  const legendRef = useRef<HTMLDivElement>(null);
  const legendCfgRef = useRef(legend);
  useEffect(() => {
    legendCfgRef.current = legend;
  });
  const legendEnabled = !!legend;

  const isUp = positive ?? trendUp(data);
  const hasVolume = data.some((p) => p.volume != null && Number.isFinite(p.volume));

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const theme = readChartTheme();
    themeRef.current = theme;

    const chart = createChart(container, {
      ...baseChartOptions(theme, { timeVisible }),
      timeScale: {
        timeVisible,
        secondsVisible: false,
        borderColor: theme.border,
        ...(timeVisible ? { tickMarkFormatter: localTickMarkFormatter } : {}),
      },
      ...(timeVisible
        ? { localization: { timeFormatter: localCrosshairTimeFormatter } }
        : {}),
      rightPriceScale: {
        borderColor: theme.border,
        scaleMargins: { top: 0.1, bottom: 0.1 },
      },
      width: container.clientWidth,
      height,
    });
    chartRef.current = chart;

    const lineColor = isUp ? theme.up : theme.down;
    const fillColor = area ? (isUp ? theme.upFill : theme.downFill) : 'rgba(0,0,0,0)';
    const series = chart.addSeries(AreaSeries, {
      lineColor,
      topColor: fillColor,
      bottomColor: 'rgba(0,0,0,0)',
      lineWidth: 2,
      priceLineVisible: false,
    });
    seriesRef.current = series;

    if (hasVolume) {
      const volume = chart.addSeries(
        HistogramSeries,
        { priceFormat: { type: 'volume' }, priceLineVisible: false, lastValueVisible: false },
        1,
      );
      volume.priceScale().applyOptions({ scaleMargins: { top: 0.15, bottom: 0 } });
      volumeRef.current = volume;
      const panes = chart.panes();
      if (panes.length > 1) {
        panes[0].setStretchFactor(3);
        panes[1].setStretchFactor(1);
      }
    }

    if (legendEnabled) {
      chart.subscribeCrosshairMove((param) => {
        const el = legendRef.current;
        const cfg = legendCfgRef.current;
        if (!el || !cfg) return;
        if (param.time == null || !param.point || param.point.x < 0 || param.point.y < 0) {
          el.style.opacity = '0';
          return;
        }
        const lv = seriesRef.current ? param.seriesData.get(seriesRef.current) : undefined;
        const vv = volumeRef.current ? param.seriesData.get(volumeRef.current) : undefined;
        const valNum = lv && 'value' in lv ? (lv as { value: number }).value : null;
        const volNum = vv && 'value' in vv ? (vv as { value: number }).value : null;
        const parts: string[] = [];
        if (valNum != null) {
          parts.push(`${cfg.valueLabel}: ${cfg.formatValue ? cfg.formatValue(valNum) : valNum.toLocaleString()}`);
        }
        if (volNum != null && cfg.volumeLabel) {
          parts.push(`${cfg.volumeLabel}: ${cfg.formatVolume ? cfg.formatVolume(volNum) : volNum.toLocaleString()}`);
        }
        el.textContent = parts.join('   ·   ');
        el.style.opacity = parts.length ? '1' : '0';
      });
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
  }, [height, isUp, hasVolume, area, timeVisible, legendEnabled]);

  useEffect(() => {
    const theme = themeRef.current;
    seriesRef.current?.setData(toSeries(data));
    if (theme) volumeRef.current?.setData(toVolume(data, theme));
    chartRef.current?.timeScale().fitContent();
  }, [data]);

  const chartDiv = (
    <div
      ref={containerRef}
      className={className}
      style={{ width: '100%', height }}
      role="img"
      aria-label={ariaLabel ?? `Line chart${data.length ? ` with ${data.length} points` : ''}`}
    />
  );

  if (!legend) return chartDiv;
  return (
    <div className="relative" style={{ width: '100%', height }}>
      {chartDiv}
      <div
        ref={legendRef}
        className="pointer-events-none absolute left-2 top-2 rounded-sm border border-line bg-surface/90 px-2 py-1 font-mono text-[11px] text-ink-body opacity-0 shadow-card transition-opacity"
      />
    </div>
  );
}

function toSeries(points: LinePoint[]): LineData<Time>[] {
  return points.map((p) => ({ time: p.time as Time, value: p.value }));
}

// Volume bars, tinted by bar-over-bar direction of the value series, at low
// opacity so they read as context.
function toVolume(points: LinePoint[], theme: ChartTheme): HistogramData<Time>[] {
  const out: HistogramData<Time>[] = [];
  for (let i = 0; i < points.length; i++) {
    const p = points[i];
    if (p.volume == null || !Number.isFinite(p.volume)) continue;
    const rising = i === 0 ? true : p.value >= points[i - 1].value;
    out.push({ time: p.time as Time, value: p.volume, color: rising ? theme.volUp : theme.volDown });
  }
  return out;
}

function trendUp(points: LinePoint[]): boolean {
  if (points.length < 2) return true;
  return points[points.length - 1].value >= points[0].value;
}
