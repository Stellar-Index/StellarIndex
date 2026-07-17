// Number / date / currency formatters — Intl-aware, locale-respecting.

const PRICE_FORMATTER = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 8,
});

const COMPACT_FORMATTER = new Intl.NumberFormat('en-US', {
  notation: 'compact',
  maximumFractionDigits: 2,
});

const PCT_FORMATTER = new Intl.NumberFormat('en-US', {
  style: 'percent',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
  signDisplay: 'exceptZero',
});

export function formatPrice(value: number | string): string {
  const n = typeof value === 'string' ? parseFloat(value) : value;
  if (!Number.isFinite(n)) return '—';
  return PRICE_FORMATTER.format(n);
}

export function formatCompact(value: number | string): string {
  const n = typeof value === 'string' ? parseFloat(value) : value;
  if (!Number.isFinite(n)) return '—';
  return COMPACT_FORMATTER.format(n);
}

// formatPriceSmall — compact USD price with a toExponential tail below
// 0.001, so a real sub-cent (or sub-1e-8) price never collapses to
// "0.00" the way a fixed-max-8dp formatter does. This is the /assets
// directory price-column formatter, lifted here as the single source so
// the asset-detail sidebar and any other USD-price cell share ONE
// implementation instead of each re-deriving the thresholds.
export function formatPriceSmall(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (n >= 1) return n.toFixed(n >= 100 ? 2 : 4);
  if (n >= 0.001) return n.toFixed(6);
  if (n > 0) return n.toExponential(3);
  return '0';
}

// formatPairPrice — quote-per-base last-price formatter for the exchange
// and pair tables. Same toExponential-below-threshold shape as
// formatPriceSmall but tuned for pair prices (a >=1000 band and a lower
// 0.0001 exponential cutoff) so a cheap pair never renders "0.0000".
// Returns '—' for a non-finite value.
export function formatPairPrice(n: number): string {
  if (!Number.isFinite(n)) return '—';
  return n >= 1000
    ? n.toFixed(2)
    : n >= 1
      ? n.toFixed(4)
      : n >= 0.0001
        ? n.toFixed(6)
        : n.toExponential(3);
}

// Pass a fraction (0.0123 → "+1.23%"). Pass a percentage point if you
// already divided.
export function formatPctChange(fraction: number): string {
  if (!Number.isFinite(fraction)) return '—';
  return PCT_FORMATTER.format(fraction);
}

export function formatLedger(ledger: number): string {
  return `#${ledger.toLocaleString('en-US')}`;
}

// Relative "time ago" label for an ISO timestamp. Returns '—' for a
// missing/unparseable value and 'now' for a (near-)future one — so a
// null/empty/garbage timestamp can never render as the literal
// "NaNd ago". Canonical home for what used to be ~7 copy-pasted
// `formatRelative` helpers across the table components, two of which
// had dropped the finite-guard and did render "NaN".
export function formatRelative(iso: string | null | undefined): string {
  if (!iso) return '—';
  const ms = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(ms)) return '—';
  if (ms < 0) return 'now';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86_400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86_400)}d ago`;
}

/**
 * formatRelativeTitle — the absolute ISO string for a title attr next
 * to formatRelative's "2m ago" (AM-23: relative-only timestamps were
 * unverifiable; hover now shows the exact instant).
 */
export function formatRelativeTitle(iso: string | null | undefined): string {
  return iso ?? '';
}
