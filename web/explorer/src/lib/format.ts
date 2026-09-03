// Number / date / currency formatters — Intl-based, pinned to en-US.
// The product is monolingual English by design: every formatter here
// and every call site passes 'en-US' explicitly so separators/grouping
// stay identical for all users and match between SSG and hydration.

const PRICE_FORMATTER = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 8,
});

const COMPACT_FORMATTER = new Intl.NumberFormat('en-US', {
  notation: 'compact',
  maximumFractionDigits: 2,
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

// formatSubunitPrice — a tiny positive (or bad-data negative) value as
// a PLAIN DECIMAL with `sig` significant digits and no exponent:
// 3.353e-4 renders "0.0003353", never "$3.353e-4" (operator call,
// 2026-08-06: scientific notation is not user-friendly and the plain
// decimal is no less accurate). Trailing zeros are trimmed. Decimals
// are capped at 20 places, which keeps 1e-18 honest (its first
// significant digit is place 18) while bounding the column width.
export function formatSubunitPrice(n: number, sig = 4): string {
  const abs = Math.abs(n);
  if (abs === 0) return '0';
  const leadingZeros = Math.max(0, -Math.floor(Math.log10(abs)) - 1);
  const decimals = Math.min(leadingZeros + sig, 20);
  let out = n.toFixed(decimals);
  if (out.includes('.')) {
    out = out.replace(/0+$/, '').replace(/\.$/, '');
  }
  return out;
}

// formatPriceSmall — compact USD price with a plain-decimal
// significant-digits tail below 0.001 (formatSubunitPrice), so a real
// sub-cent (or sub-1e-8) price never collapses to "0.00" the way a
// fixed-max-8dp formatter does — and never renders scientific
// notation either (pre-2026-08-06 this branch was toExponential).
// This is the /assets directory price-column formatter, lifted here
// as the single source so the asset-detail sidebar and any other
// USD-price cell share ONE implementation instead of each re-deriving
// the thresholds.
export function formatPriceSmall(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (n >= 1) return n.toFixed(n >= 100 ? 2 : 4);
  if (n >= 0.001) return n.toFixed(6);
  if (n > 0) return formatSubunitPrice(n);
  // COR-01: a negative price is bad data, not a legitimate zero — collapsing
  // both to the bare string '0' made a negative value look like a normal,
  // healthy zero price instead of surfacing it as the anomaly it is.
  if (n < 0) return formatSubunitPrice(n);
  return '0';
}

// formatPairPrice — quote-per-base last-price formatter for the exchange
// and pair tables. Same shape as formatPriceSmall but tuned for pair
// prices (a >=1000 band and a lower 0.0001 plain-decimal cutoff) so a
// cheap pair never renders "0.0000" — or scientific notation.
// Returns '—' for a non-finite value.
export function formatPairPrice(n: number): string {
  if (!Number.isFinite(n)) return '—';
  return n >= 1000
    ? n.toFixed(2)
    : n >= 1
      ? n.toFixed(4)
      : n >= 0.0001
        ? n.toFixed(6)
        : formatSubunitPrice(n);
}

// formatOraclePrice — the oracle-reading price column (/oracles and the
// per-asset oracle panel, #336). Takes the wire STRING so a value the
// API sent but JS cannot parse renders verbatim rather than as '—': an
// oracle reading is evidence, and dropping it because our formatter
// dislikes it is the one thing this column must not do.
//
// Sits beside formatPriceSmall rather than reusing it because oracle
// decimals go to 14 (Reflector) and the interesting reading is often
// well below a cent: the plain-decimal significant-digits tail starts at
// 0.01 here rather than formatPriceSmall's 0.001, and there is no
// >=100 → 2dp band, because an oracle's 4-decimal tail on a large
// number is exactly the digit a cross-check compares.
export function formatOraclePrice(p: string): string {
  // Number('') and Number(' ') are both 0, so a blank price would render
  // as the bare "0" — an oracle quoting the asset at zero. Blank is
  // absence, not a reading; report it as it arrived.
  if (!p.trim()) return p;
  const n = Number(p);
  if (!Number.isFinite(n)) return p;
  if (n === 0) return '0';
  if (n >= 1) return n.toFixed(4);
  if (n >= 0.01) return n.toFixed(6);
  return formatSubunitPrice(n);
}

// AGT-06: formatPctChange and formatLedger were removed as dead code —
// grep confirmed zero callers outside their own tests. formatPctChange in
// particular was a footgun: it takes a FRACTION (0.0123 → "+1.23%"), but
// every real percentage field in the app (change_24h_pct, ChangeBadge's
// `pct`, …) already arrives as a percentage point and is rendered with a
// plain `.toFixed(2)}%` — reaching for this helper on one of those fields
// would have silently multiplied the displayed change by 100.

// Relative "time ago" label for an ISO timestamp. Returns '—' for a
// missing/unparseable value and 'now' for a (near-)future one — so a
// null/empty/garbage timestamp can never render as the literal
// "NaNd ago". Canonical home for what used to be ~7 copy-pasted
// `formatRelative` helpers across the table components, two of which
// had dropped the finite-guard and did render "NaN".
export function formatRelative(
  iso: string | null | undefined,
  opts?: { suffix?: boolean },
): string {
  if (!iso) return '—';
  const ms = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(ms)) return '—';
  if (ms < 0) return 'now';
  const suffix = opts?.suffix === false ? '' : ' ago';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s${suffix}`;
  if (s < 3600) return `${Math.round(s / 60)}m${suffix}`;
  if (s < 86_400) return `${Math.round(s / 3600)}h${suffix}`;
  return `${Math.round(s / 86_400)}d${suffix}`;
}

/**
 * formatRelativeLong — coarse long-form relative time ("2 hours ago",
 * "3 months ago", "just now"). THE long-form canonical (FEC audit A3-F1):
 * re-homed from lib/account-format so exactly one word-form implementation
 * exists; account surfaces ("last active" prose) want words and >30d
 * granularity the short form lacks.
 */
export function formatRelativeLong(iso: string | null | undefined): string {
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

/**
 * formatDurationShort — seconds → "45s" / "3m" / "5h" / "2d" (no suffix).
 * FEC audit A3-F1b: consolidates the formatLag twins (diagnostics/sources)
 * and the status page's formatAge/timeSince; formatAge's negative→'—'
 * guard wins (a clock-skewed lag must read as unknown, not "-5s").
 */
export function formatDurationShort(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—';
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

/**
 * formatDurationLong — milliseconds → compound "2h 15m" (incident
 * durations want the extra precision). Re-homed from the incident page;
 * finite guard added on re-home (NaN previously rendered "NaNm").
 */
export function formatDurationLong(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—';
  const min = Math.round(ms / 60_000);
  if (min < 60) return `${min}m`;
  const h = Math.floor(min / 60);
  const m = min - h * 60;
  return m === 0 ? `${h}h` : `${h}h ${m}m`;
}

// ── Base-unit (smallest-unit integer string) scaling — ADR-0003 ──────────
//
// Balance/supply strings arrive as smallest-unit integers that can exceed
// 2^53 (stroops, Soroban i128 token units). Number()-then-divide silently
// rounds them; the repo idiom is BigInt-divide FIRST (see explorer-shared's
// stroopsToXlm), only letting the already-scaled quotient touch float land.

/**
 * scaleBaseUnits — smallest-unit integer string → whole-unit JS number
 * via the BigInt-divide-first path, or null for an absent/garbage value
 * (callers render "—", never a fabricated zero or NaN). The result is a
 * float for geometry/compact display; any sub-float precision loss is in
 * invisible digits, never a mis-scaled magnitude.
 */
export function scaleBaseUnits(
  raw: string | null | undefined,
  decimals: number,
): number | null {
  if (raw == null || raw === '') return null;
  const t = raw.trim();
  if (/^-?\d+$/.test(t)) {
    const scale = 10n ** BigInt(decimals);
    const v = BigInt(t);
    return Number(v / scale) + Number(v % scale) / Number(scale);
  }
  const n = Number(t);
  return Number.isFinite(n) ? n : null;
}

/**
 * formatBaseUnits — smallest-unit integer string → grouped whole-unit
 * display string with up to `maxFrac` fractional digits (trailing zeros
 * trimmed). Exact BigInt path for integer strings so arbitrarily large
 * balances keep every displayed digit faithful to the wire; "—" for
 * absent/garbage.
 */
export function formatBaseUnits(
  raw: string | null | undefined,
  decimals: number,
  maxFrac = 4,
): string {
  if (raw == null || raw === '') return '—';
  const t = raw.trim();
  if (!/^-?\d+$/.test(t)) {
    const n = scaleBaseUnits(t, decimals);
    if (n == null) return '—';
    return n.toLocaleString('en-US', { maximumFractionDigits: maxFrac });
  }
  const neg = t.startsWith('-');
  const abs = BigInt(neg ? t.slice(1) : t);
  const scale = 10n ** BigInt(decimals);
  const wholeStr = (abs / scale).toLocaleString('en-US');
  const fracStr = (abs % scale)
    .toString()
    .padStart(decimals, '0')
    .slice(0, maxFrac)
    .replace(/0+$/, '');
  const out = fracStr ? `${wholeStr}.${fracStr}` : wholeStr;
  return neg ? `-${out}` : out;
}

/**
 * Truncate a long identifier (G-strkey, C-id, tx hash) to `head…tail`.
 * THE canonical for the whole app (FEC audit A2-06): server-safe here in
 * lib — the previous home (ui/Mono.tsx) is a 'use client' module, so
 * server components physically could not call it (RSC turns client-module
 * exports into throwing client references). Null/empty renders '—'
 * (display-site winner semantics from explorer-shared.shortHash); head/tail
 * stay parameterized because per-context lengths (16/16, 8/6, 6/4) are
 * deliberate.
 */
export function truncateMiddle(
  s: string | null | undefined,
  head = 6,
  tail = 4,
): string {
  if (!s) return '—';
  if (s.length <= head + tail + 1) return s;
  return `${s.slice(0, head)}…${s.slice(-tail)}`;
}
