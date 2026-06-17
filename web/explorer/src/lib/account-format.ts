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

/** Coarse relative time, e.g. "3 days ago", "2 hours ago", "just now". */
export function fmtRelative(iso: string | null | undefined): string {
  if (!iso) return 'never';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return 'never';
  const diffMs = Date.now() - d.getTime();
  const sec = Math.round(diffMs / 1000);
  if (sec < 45) return 'just now';
  const min = Math.round(sec / 60);
  if (min < 60) return `${min} min${min === 1 ? '' : 's'} ago`;
  const hr = Math.round(min / 60);
  if (hr < 24) return `${hr} hour${hr === 1 ? '' : 's'} ago`;
  const day = Math.round(hr / 24);
  if (day < 30) return `${day} day${day === 1 ? '' : 's'} ago`;
  const mo = Math.round(day / 30);
  if (mo < 12) return `${mo} month${mo === 1 ? '' : 's'} ago`;
  const yr = Math.round(mo / 12);
  return `${yr} year${yr === 1 ? '' : 's'} ago`;
}

/** Capitalise the first letter — for tier / status / role labels. */
export function titleCase(s: string | null | undefined): string {
  if (!s) return '—';
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// Per-tier metadata used across Overview / Usage / Settings. The API is
// the source of truth for which tier an account is on; this is purely
// presentation (label + rate-limit ceiling copy) and mirrors the tier
// ceilings documented in the keys create form.
const TIER_RATE_CEILING: Record<string, number> = {
  free: 60,
  starter: 1000,
  pro: 10000,
  business: 60000,
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
