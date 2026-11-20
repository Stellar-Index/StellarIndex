import { formatDecimalAmount, formatReadable } from '@/lib/format';

/** "≥$1.2M" (compact) with the exact figure for hover; null when absent. */
export function usdLabel(v: string | null | undefined, lowerBound = false) {
  const shown = formatReadable(v, true);
  if (shown == null) return null;
  return {
    display: `${lowerBound ? '≥' : ''}${shown}`,
    exact: `${lowerBound ? 'At least ' : ''}$${formatDecimalAmount(v, 2)}`,
  };
}

/** One hue per lending protocol, the same on every lending chart. */
export const PROTOCOL_COLOR: Record<'blend' | 'blnt', string> = {
  blend: 'var(--color-chart-1)',
  blnt: 'var(--color-chart-3)',
};

export function protocolColor(protocol: string | null | undefined): string {
  return protocol === 'blnt'
    ? PROTOCOL_COLOR.blnt
    : protocol === 'blend' || protocol?.startsWith('blend')
      ? PROTOCOL_COLOR.blend
      : 'var(--color-chart-tail)';
}

/** Plot coordinate from a USD decimal string; never used for a shown figure. */
export function plotNumber(v: string | null | undefined): number | null {
  if (v == null || v.trim() === '') return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}
