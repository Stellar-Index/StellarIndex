// Presentation helpers for the in-site account dashboard (/account/*).
// Kept apart from src/lib/format.ts (price / ledger formatters) — these
// are account-specific (integers, relative dates, tier labels) and have
// no overlap with the market formatters. Numbers render with tabular
// figures via the .tnum / font-mono utilities; these helpers only
// produce the strings.

/** Compact thousands-grouped integer, e.g. 12_345 → "12,345". */
export function fmtInt(n: number | null | undefined): string {
  if (n === null || n === undefined || Number.isNaN(n)) return '—';
  return new Intl.NumberFormat('en-US').format(Math.round(n));
}

/** Absolute date, e.g. "17 Jun 2026". */
export function fmtDate(iso: string | null | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleDateString('en-US', {
    day: '2-digit',
    month: 'short',
    year: 'numeric',
  });
}

/** Date + time, e.g. "17 Jun 2026, 14:32". */
export function fmtDateTime(iso: string | null | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString('en-US', {
    day: '2-digit',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

/** Coarse relative time — delegates to the canonical long-form in
 * lib/format (FEC audit A3-F1: one word-form implementation). */
export { formatRelativeLong as fmtRelative } from '@/lib/format';

/** Capitalise the first letter — for tier / status / role labels. */
export function titleCase(s: string | null | undefined): string {
  if (!s) return '—';
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// Per-tier metadata used across Overview / Usage / Settings. The API is
// the source of truth for which tier an account is on; this is purely
// presentation (label + rate-limit ceiling copy) and mirrors
// platform.Tier.MaxRateLimitPerMin. The canonical model is
// free/partner (the platform is free; partner limits are staff-set);
// legacy tier strings are kept here folded to their canonical rung in
// case an older API still serves them.
const TIER_RATE_CEILING: Record<string, number> = {
  free: 1000,
  partner: 100000,
  // Legacy names → canonical rungs (starter≡free; pro/business/enterprise≡partner).
  starter: 1000,
  pro: 100000,
  business: 100000,
  enterprise: 100000,
};

/** Human label for an account tier. */
export function tierLabel(tier: string | null | undefined): string {
  return titleCase(tier);
}

/** The per-minute rate ceiling for a tier, or null if unknown. */
export function tierCeiling(tier: string | null | undefined): number | null {
  if (!tier) return null;
  return TIER_RATE_CEILING[tier.toLowerCase()] ?? null;
}
