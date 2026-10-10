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

/**
 * formatCompactUnits — formatCompact for an exact decimal or smallest-unit
 * integer string (ADR-0003's wire shape), shifted left by `decimals` and
 * rounded from the EXACT value. `Number()`-then-divide rounds twice before
 * Intl rounds a third time, and can cross a display boundary: 10^7-scaled
 * "5123049999999999660566" is 512.3T, not the 512.31T it yields. "—" for an
 * absent or non-decimal value.
 */
export function formatCompactUnits(
  raw: string | null | undefined,
  decimals = 0,
): string {
  if (raw == null) return '—';
  const m = /^(-?)(\d+)(?:\.(\d+))?$/.exec(raw.trim());
  if (!m) return '—';
  const [, sign, whole, frac = ''] = m;
  const units = BigInt(`${sign}${whole}${frac}`);
  const scale = 10n ** BigInt(frac.length + decimals);
  const intPart = units / scale;
  // From 1,000 up, compact notation rounds at the tens place or coarser,
  // where the truncated fraction cannot move the result; Intl formats a
  // BigInt exactly.
  if (intPart >= 1000n || intPart <= -1000n)
    return COMPACT_FORMATTER.format(intPart);
  // Below that it shows at most two places: round exactly to hundredths
  // (half away from zero, Intl's default) before the value becomes a float.
  const hundredths = units * 100n;
  let q = hundredths / scale;
  const r = hundredths % scale;
  if (2n * (r < 0n ? -r : r) >= scale) q += units < 0n ? -1n : 1n;
  return COMPACT_FORMATTER.format(Number(q) / 100);
}

const USD_WHOLE_FORMATTER = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  maximumFractionDigits: 0,
});

/**
 * formatUsdWhole — a decimal string as whole dollars ("$1,235"), rounded
 * half away from zero in BigInt so a figure above 2^53 keeps every digit.
 * "—" for an absent or non-decimal value.
 */
export function formatUsdWhole(raw: string | null | undefined): string {
  const n = roundWhole(raw);
  return n == null ? '—' : USD_WHOLE_FORMATTER.format(n);
}

/** formatWhole — formatUsdWhole without the currency ("1,235"). */
export function formatWhole(raw: string | null | undefined): string {
  const n = roundWhole(raw);
  return n == null ? '—' : n.toLocaleString('en-US');
}

/**
 * formatReadable — a decimal wire string at a readable precision: compact
 * from 1M ("$11.26B"), whole and grouped from 1K ("12,346"), two places
 * below that, four significant digits below 1. Null for a non-decimal.
 */
export function formatReadable(
  raw: string | null | undefined,
  usd = false,
): string | null {
  const m = raw == null ? null : /^(-?)(\d+)(?:\.(\d+))?$/.exec(raw.trim());
  if (!m) return null;
  const [, sign, whole] = m;
  const body = raw!.trim().replace(/^-/, '');
  const out =
    whole.length > 6
      ? formatCompactUnits(body)
      : whole.length > 3
        ? formatWhole(body)
        : whole === '0'
          ? formatSubunitPrice(Number(body))
          : centsPad(formatCompactUnits(body), usd);
  return `${sign && out !== '0' ? '-' : ''}${usd ? '$' : ''}${out}`;
}

// Dollars under 1K read as cents: "$69.50", never "$69.5".
function centsPad(s: string, usd: boolean): string {
  if (!usd || !/^\d+(\.\d+)?$/.test(s)) return s;
  const [w, f = ''] = s.split('.');
  return `${w}.${f.padEnd(2, '0')}`;
}

function roundWhole(raw: string | null | undefined): bigint | null {
  if (raw == null) return null;
  const m = /^(-?)(\d+)(?:\.(\d+))?$/.exec(raw.trim());
  if (!m) return null;
  const [, sign, whole, frac = ''] = m;
  let n = BigInt(whole);
  if (frac.charCodeAt(0) >= 53) n += 1n;
  return sign ? -n : n;
}

/**
 * decimalOrNull — a best-effort decimal wire string (a USD figure) as a JS
 * number for chart geometry, or null when it is absent or not a number:
 * never the 0 that `Number(x) || 0` fabricates, which draws "no price held"
 * as a real zero.
 */
export function decimalOrNull(raw: string | null | undefined): number | null {
  if (raw == null || raw.trim() === '') return null;
  const n = Number(raw);
  return Number.isFinite(n) ? n : null;
}

const SUBUNIT_MAX_DECIMALS = 20;

// toFixed on the exact decimal, rounding half away from zero in BigInt:
// a float rounds "2.00005" down (its double is just below) and drops the
// digits of a price past 2^53.
function fixedExact(d: Decimal, places: number): string {
  const neg = d.units < 0n;
  let q = neg ? -d.units : d.units;
  const drop = d.frac - places;
  if (drop > 0) {
    const div = 10n ** BigInt(drop);
    const r = q % div;
    q /= div;
    if (2n * r >= div) q += 1n;
  } else {
    q *= 10n ** BigInt(-drop);
  }
  const digits = q.toString().padStart(places + 1, '0');
  const int = digits.slice(0, digits.length - places);
  const sign = neg && q !== 0n ? '-' : '';
  return places > 0 ? `${sign}${int}.${digits.slice(-places)}` : sign + int;
}

// formatSubunitPrice for an exact decimal.
function subunitExact(d: Decimal, sig: number): string {
  if (d.units === 0n) return '0';
  const abs = (d.units < 0n ? -d.units : d.units).toString();
  const lead = Math.max(0, d.frac - abs.length);
  let out = fixedExact(d, Math.min(lead + sig, SUBUNIT_MAX_DECIMALS));
  out = out.replace(/0+$/, '').replace(/\.$/, '');
  if (out === '0' || out === '-0') {
    return `${d.units < 0n ? '-' : ''}<0.${'0'.repeat(SUBUNIT_MAX_DECIMALS - 1)}1`;
  }
  return out;
}

// Rounds `raw` exactly under the first band whose floor it meets, else as a
// sub-unit price; '—' when it is not a plain decimal.
function bandedExact(raw: string, bands: [string, number][]): string {
  const d = parseDecimal(raw);
  if (!d) return '—';
  for (const [floor, places] of bands) {
    if ((compareDecimalStrings(raw, floor) ?? -1) >= 0)
      return fixedExact(d, places);
  }
  return subunitExact(d, 4);
}

// formatSubunitPrice — a tiny positive (or bad-data negative) value as
// a PLAIN DECIMAL with `sig` significant digits and no exponent:
// 3.353e-4 renders "0.0003353", never "$3.353e-4" (scientific
// notation is not user-friendly and the plain decimal is no less
// accurate). Trailing zeros are trimmed. Decimals
// are capped at 20 places, which keeps 1e-18 honest (its first
// significant digit is place 18) while bounding the column width.
// A non-zero value that rounds away under the cap renders as a signed
// "<" bound, so dust (or a tiny bad-data negative) never reads as "0".
export function formatSubunitPrice(n: number, sig = 4): string {
  const abs = Math.abs(n);
  if (abs === 0) return '0';
  const leadingZeros = Math.max(0, -Math.floor(Math.log10(abs)) - 1);
  const decimals = Math.min(leadingZeros + sig, SUBUNIT_MAX_DECIMALS);
  let out = n.toFixed(decimals);
  if (out.includes('.')) {
    out = out.replace(/0+$/, '').replace(/\.$/, '');
  }
  if (out === '0' || out === '-0') {
    return `${n < 0 ? '-' : ''}<0.${'0'.repeat(SUBUNIT_MAX_DECIMALS - 1)}1`;
  }
  return out;
}

// formatPriceSmall — compact USD price with a plain-decimal
// significant-digits tail below 0.001 (formatSubunitPrice), so a real
// sub-cent (or sub-1e-8) price never collapses to "0.00" the way a
// fixed-max-8dp formatter does — and never renders scientific
// notation either (this branch must not use toExponential).
// This is the /assets directory price-column formatter, lifted here
// as the single source so the asset-detail sidebar and any other
// USD-price cell share ONE implementation instead of each re-deriving
// the thresholds.
export function formatPriceSmall(n: number | string): string {
  if (typeof n === 'string')
    return bandedExact(n, [
      ['100', 2],
      ['1', 4],
      ['0.001', 6],
    ]);
  if (!Number.isFinite(n)) return '—';
  if (n >= 1) return n.toFixed(n >= 100 ? 2 : 4);
  if (n >= 0.001) return n.toFixed(6);
  if (n > 0) return formatSubunitPrice(n);
  // A negative price is bad data, not a legitimate zero; surface it.
  if (n < 0) return formatSubunitPrice(n);
  return '0';
}

// formatPairPrice — quote-per-base last-price formatter for the exchange
// and pair tables. Same shape as formatPriceSmall but tuned for pair
// prices (a >=1000 band and a lower 0.0001 plain-decimal cutoff) so a
// cheap pair never renders "0.0000" — or scientific notation.
// Returns '—' for a non-finite value. Pass the wire string to round exactly.
export function formatPairPrice(n: number | string): string {
  if (typeof n === 'string')
    return bandedExact(n, [
      ['1000', 2],
      ['1', 4],
      ['0.0001', 6],
    ]);
  if (!Number.isFinite(n)) return '—';
  return n >= 1000
    ? n.toFixed(2)
    : n >= 1
      ? n.toFixed(4)
      : n >= 0.0001
        ? n.toFixed(6)
        : formatSubunitPrice(n);
}

/** formatFractionPrice — a Stellar offer's n/d price at pair-price precision. */
export function formatFractionPrice(n: number, d: number): string {
  if (!Number.isSafeInteger(n) || n < 0) return '—';
  const q = divideDecimalString(String(n), d, 12);
  return q == null ? '—' : formatPairPrice(q);
}

// formatOraclePrice — the oracle-reading price column (/oracles and the
// per-asset oracle panel). Takes the wire STRING so a value the
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

// Percentage fields (change_24h_pct, ChangeBadge's `pct`, …) already arrive
// as percentage points: render with `.toFixed(2)}%`, never a fraction→% helper.

// Relative "time ago" label for an ISO timestamp. Returns '—' for a
// missing/unparseable value and 'now' for a (near-)future one — so a
// null/empty/garbage timestamp can never render as the literal
// "NaNd ago". Canonical home: copy-pasted `formatRelative`
// helpers across table components risk dropping the finite-guard and
// rendering "NaN".
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
 * "3 months ago", "just now"). THE long-form canonical:
 * account surfaces ("last active" prose) want words and >30d
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
 * Negative → '—' (a clock-skewed lag must read as unknown, not "-5s").
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
 * durations want the extra precision). The finite guard keeps NaN from
 * rendering "NaNm".
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
 * baseUnitsDecimal — smallest-unit integer string → the exact whole-unit
 * decimal string, or null for anything that is not a plain integer.
 */
export function baseUnitsDecimal(
  raw: string | null | undefined,
  decimals: number,
): string | null {
  const t = raw?.trim() ?? '';
  if (!/^-?\d+$/.test(t) || !Number.isInteger(decimals) || decimals < 0)
    return null;
  const neg = t.startsWith('-');
  const padded = (neg ? t.slice(1) : t).padStart(decimals + 1, '0');
  const cut = padded.length - decimals;
  const out =
    decimals > 0 ? `${padded.slice(0, cut)}.${padded.slice(cut)}` : padded;
  return neg ? `-${out}` : out;
}

/** formatUnitsReadable — a smallest-unit integer string at formatReadable precision. */
export function formatUnitsReadable(
  raw: string | null | undefined,
  decimals: number,
): string {
  return formatReadable(baseUnitsDecimal(raw, decimals)) ?? '—';
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
 * sumDecimalStrings — exact sum of fixed-point decimal strings (the wire
 * shape of every money field, ADR-0003) as a decimal string. Absent
 * entries are skipped; null when nothing was summed or any entry is not a
 * decimal, so a garbage row cannot quietly shrink a published total.
 */
export function sumDecimalStrings(
  values: readonly (string | null | undefined)[],
): string | null {
  const parsed: Decimal[] = [];
  for (const v of values) {
    if (v == null || v === '') continue;
    const d = parseDecimal(v);
    if (!d) return null;
    parsed.push(d);
  }
  if (parsed.length === 0) return null;
  const scale = Math.max(...parsed.map((p) => p.frac));
  const total = parsed.reduce((acc, p) => acc + rescale(p, scale), 0n);
  const neg = total < 0n;
  const digits = (neg ? -total : total).toString().padStart(scale + 1, '0');
  const whole = digits.slice(0, digits.length - scale);
  const frac = scale > 0 ? `.${digits.slice(digits.length - scale)}` : '';
  return `${neg ? '-' : ''}${whole}${frac}`;
}

/** `value / count` over a decimal string, truncated to `places`; null for a malformed value or a non-positive count. */
export function divideDecimalString(
  value: string | null | undefined,
  count: number | null | undefined,
  places = 6,
): string | null {
  const d = value == null ? null : parseDecimal(value);
  if (!d || count == null || !Number.isSafeInteger(count) || count <= 0)
    return null;
  const scale = Math.max(d.frac, places);
  const q = rescale(d, scale) / BigInt(count);
  const neg = q < 0n;
  const digits = (neg ? -q : q).toString().padStart(scale + 1, '0');
  const whole = digits.slice(0, digits.length - scale);
  const frac = scale > 0 ? `.${digits.slice(digits.length - scale)}` : '';
  return `${neg ? '-' : ''}${whole}${frac}`;
}

/**
 * ratioPct — `part / whole × 100` over two decimal strings, divided and
 * rounded (half away from zero, to `places`) in BigInt before the result
 * becomes a number. Null for a non-decimal input or a zero whole.
 */
export function ratioPct(
  part: string | null | undefined,
  whole: string | null | undefined,
  places = 2,
): number | null {
  const a = part == null ? null : parseDecimal(part);
  const b = whole == null ? null : parseDecimal(whole);
  if (!a || !b) return null;
  const scale = Math.max(a.frac, b.frac);
  return pctOf(rescale(a, scale), rescale(b, scale), places);
}

/**
 * changePct — `(to − from) / from × 100` over two decimal strings, exact
 * as {@link ratioPct}. Null for a non-decimal input or a zero `from`.
 */
export function changePct(
  from: string | null | undefined,
  to: string | null | undefined,
  places = 2,
): number | null {
  const f = from == null ? null : parseDecimal(from);
  const t = to == null ? null : parseDecimal(to);
  if (!f || !t) return null;
  const scale = Math.max(f.frac, t.frac);
  const base = rescale(f, scale);
  return pctOf(rescale(t, scale) - base, base, places);
}

/**
 * compareDecimalStrings — exact sign of `a − b` (-1, 0, 1) over two decimal
 * strings. Null for a non-decimal input.
 */
export function compareDecimalStrings(a: string, b: string): number | null {
  const x = parseDecimal(a);
  const y = parseDecimal(b);
  if (!x || !y) return null;
  const scale = Math.max(x.frac, y.frac);
  const d = rescale(x, scale) - rescale(y, scale);
  return d < 0n ? -1 : d > 0n ? 1 : 0;
}

/** positiveDecimal — the string itself when it is a decimal above zero, else null. */
export function positiveDecimal(raw: string | null | undefined): string | null {
  return raw != null && compareDecimalStrings(raw, '0') === 1 ? raw : null;
}

/**
 * compareDecimalDesc — a descending sort comparator over optional decimal
 * strings. Absent, empty and malformed all rank as 0, which keeps the order
 * total: a malformed value that tied with everything would scramble the rest.
 */
export function compareDecimalDesc(
  a: string | null | undefined,
  b: string | null | undefined,
): number {
  const rank = (v: string | null | undefined) =>
    v && compareDecimalStrings(v, '0') != null ? v : '0';
  return compareDecimalStrings(rank(b), rank(a)) ?? 0;
}

interface Decimal {
  units: bigint;
  frac: number;
}

function parseDecimal(v: string): Decimal | null {
  const m = /^(-?)(\d+)(?:\.(\d+))?$/.exec(v.trim());
  if (!m) return null;
  const [, sign, whole, frac = ''] = m;
  return { units: BigInt(`${sign}${whole}${frac}`), frac: frac.length };
}

function rescale(d: Decimal, scale: number): bigint {
  return d.units * 10n ** BigInt(scale - d.frac);
}

function pctOf(num: bigint, den: bigint, places: number): number | null {
  if (den === 0n) return null;
  const sign = num < 0n !== den < 0n ? -1n : 1n;
  const n = (num < 0n ? -num : num) * 100n * 10n ** BigInt(places);
  const d = den < 0n ? -den : den;
  return Number(sign * ((n + d / 2n) / d)) / 10 ** places;
}

/**
 * formatDecimalAmount — a FIXED-POINT DECIMAL STRING (the wire shape of
 * every money field, ADR-0003) → a grouped display string, without the
 * value ever touching a JS number. `Number('40538494.54')` is harmless;
 * `Number()` on this column's larger siblings is not — above 2^53 the
 * integer part rounds silently and the figure shown stops being the
 * figure served. So the integer part is grouped by Intl over a **BigInt**
 * (`Intl.NumberFormat` formats BigInt exactly — the same divide-first
 * idiom as `formatBaseUnits` above), and the fraction is carried as
 * digits rather than as arithmetic.
 *
 * The fraction is TRUNCATED to `frac` places, never rounded: these are
 * published lower bounds (internal/api/v1/dex_tvl_total.go), and
 * truncation can only understate. Wire values already carry exactly
 * `frac` places today, so nothing is actually dropped.
 *
 * Returns `null` — not '—' — for an absent or non-decimal value, so each
 * caller has to decide what absence looks like on its own surface. A
 * shared placeholder here is precisely how an absent money total becomes
 * a dash that reads as zero.
 */
export function formatDecimalAmount(
  raw: string | null | undefined,
  frac = 2,
): string | null {
  if (raw == null) return null;
  const m = /^(-?)(\d+)(?:\.(\d+))?$/.exec(raw.trim());
  if (!m) return null;
  const [, sign, whole, tail = ''] = m;
  const grouped = BigInt(whole).toLocaleString('en-US');
  if (frac <= 0) return sign + grouped;
  return `${sign}${grouped}.${tail.slice(0, frac).padEnd(frac, '0')}`;
}

/**
 * multiplyDecimalStrings — exact product of two fixed-point decimal strings
 * (the ADR-0003 wire shape) as a plain decimal string, trailing zeros
 * trimmed. BigInt end to end, so a tiny product keeps every significant
 * digit instead of rounding to a fixed number of places. `null` when either
 * input is not a plain decimal.
 */
export function multiplyDecimalStrings(a: string, b: string): string | null {
  const re = /^(-?)(\d+)(?:\.(\d+))?$/;
  const ma = re.exec(a.trim());
  const mb = re.exec(b.trim());
  if (!ma || !mb) return null;
  const [, signA, wholeA, fracA = ''] = ma;
  const [, signB, wholeB, fracB = ''] = mb;
  const scale = fracA.length + fracB.length;
  const product = BigInt(wholeA + fracA) * BigInt(wholeB + fracB);
  const digits = product.toString().padStart(scale + 1, '0');
  const whole = digits.slice(0, digits.length - scale);
  const frac = digits.slice(digits.length - scale).replace(/0+$/, '');
  const out = frac ? `${whole}.${frac}` : whole;
  return product !== 0n && signA !== signB ? `-${out}` : out;
}

/**
 * Truncate a long identifier (G-strkey, C-id, tx hash) to `head…tail`.
 * Server-safe here in lib: ui/Mono.tsx is a 'use client' module, and server
 * components cannot call client-module exports (RSC turns them into
 * throwing client references). Null/empty renders '—'; head/tail
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
