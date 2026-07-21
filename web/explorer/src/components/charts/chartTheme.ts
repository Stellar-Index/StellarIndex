'use client';

import { ColorType, type DeepPartial, type ChartOptions } from 'lightweight-charts';

// Resolve a design token (CSS custom property off :root) to its value, so the
// canvas charts theme from the SAME dark tokens as the rest of the app instead
// of hardcoding colours. Falls back to the dark value if read before mount.
function token(name: string, fallback: string): string {
  if (typeof window === 'undefined') return fallback;
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return v || fallback;
}

// Append an alpha to a #rrggbb token → #rrggbbaa (lightweight-charts renders to
// canvas, which accepts 8-digit hex). Non-hex input is returned unchanged.
function withAlpha(hex: string, a: number): string {
  if (!/^#[0-9a-fA-F]{6}$/.test(hex)) return hex;
  const aa = Math.round(Math.max(0, Math.min(1, a)) * 255)
    .toString(16)
    .padStart(2, '0');
  return `${hex}${aa}`;
}

export type ChartTheme = {
  text: string;
  grid: string;
  border: string;
  up: string;
  down: string;
  upFill: string;
  downFill: string;
  volUp: string;
  volDown: string;
  brand: string;
};

/** Read the current (dark) chart theme from the design tokens. */
export function readChartTheme(): ChartTheme {
  const up = token('--color-up', '#31c48d');
  const down = token('--color-down', '#f6465d');
  const line = token('--color-line', '#23272f');
  const lineStrong = token('--color-line-strong', '#30353f');
  const ink = token('--color-ink-muted', '#8a91a0');
  const brand = token('--color-brand-500', '#4c7dff');
  return {
    text: ink,
    grid: withAlpha(line, 0.55),
    border: lineStrong,
    up,
    down,
    upFill: withAlpha(up, 0.14),
    downFill: withAlpha(down, 0.14),
    volUp: withAlpha(up, 0.4),
    volDown: withAlpha(down, 0.4),
    brand,
  };
}

/**
 * Base createChart options shared by CandleChart + LineChart. Horizontal-only
 * grid (Dune-style — verticals add noise), token-driven axis/crosshair colours,
 * and pane separators styled to the hairline `line` token.
 */
export function baseChartOptions(
  theme: ChartTheme,
  opts: { timeVisible: boolean },
): DeepPartial<ChartOptions> {
  return {
    layout: {
      background: { type: ColorType.Solid, color: 'transparent' },
      textColor: theme.text,
      fontFamily: 'var(--font-mono)',
      fontSize: 11,
      // Hide the TradingView attribution logo. This is an official
      // lightweight-charts option and license-clean: the library is Apache-2.0
      // (attribution is preserved in the dependency/NOTICE, not required as an
      // on-canvas logo), and TradingView ships this flag to disable it.
      attributionLogo: false,
      panes: {
        separatorColor: theme.border,
        separatorHoverColor: withAlpha(theme.brand, 0.4),
        enableResize: false,
      },
    },
    grid: {
      horzLines: { color: theme.grid },
      vertLines: { visible: false },
    },
    timeScale: {
      timeVisible: opts.timeVisible,
      secondsVisible: false,
      borderColor: theme.border,
    },
    rightPriceScale: {
      borderColor: theme.border,
    },
    crosshair: {
      mode: 1,
      vertLine: { color: theme.border, labelBackgroundColor: theme.brand },
      horzLine: { color: theme.border, labelBackgroundColor: theme.brand },
    },
  };
}
