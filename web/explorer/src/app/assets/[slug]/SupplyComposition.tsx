import { formatBaseUnits, ratioPct } from '@/lib/format';

type Seg = {
  key: string;
  label: string;
  units: bigint;
  className: string;
  style?: React.CSSProperties;
};

function big(s: string | null | undefined): bigint | null {
  const t = s?.trim();
  return t && /^\d+$/.test(t) ? BigInt(t) : null;
}

/**
 * SupplyComposition — circulating / not-circulating / headroom-to-max as
 * one stacked bar. Segments are BigInt differences of the smallest-unit
 * wire strings; Number appears only for bar width. A declared (SEP-1) max
 * is drawn dashed-outline rather than solid because the issuer's pledge
 * is not on-chain enforced; a floor supply says so in the legend.
 */
export function SupplyComposition({
  circulating,
  total,
  max,
  decimals,
  maxDeclared,
  unlimited,
  floor,
}: {
  circulating: string | null | undefined;
  total: string | null | undefined;
  max: string | null | undefined;
  decimals: number;
  maxDeclared?: boolean;
  unlimited?: boolean;
  floor?: boolean;
}) {
  const c = big(circulating);
  const t = big(total);
  const m = unlimited ? null : big(max);
  const base = t ?? c;
  if (base == null || base <= 0n) return null;

  const segs: Seg[] = [];
  if (c != null && c > 0n) {
    segs.push({
      key: 'circ',
      label: floor ? 'Circulating (at least)' : 'Circulating',
      units: c,
      className: 'bg-brand-500',
    });
  }
  if (t != null && t > (c ?? 0n)) {
    segs.push({
      key: 'locked',
      label: 'Total, not circulating',
      units: t - (c ?? 0n),
      className: 'bg-ink-faint',
    });
  }
  if (m != null && m > base) {
    segs.push({
      key: 'headroom',
      label: maxDeclared ? 'Headroom to declared max' : 'Headroom to max',
      units: m - base,
      className: maxDeclared
        ? 'border border-dashed border-ink-muted bg-transparent'
        : 'bg-surface-subtle border border-line',
    });
  }
  const denom = segs.reduce((a, s) => a + s.units, 0n);
  if (denom <= 0n) return null;

  const pct = (u: bigint) => Number((u * 10000n) / denom) / 100;
  const aria = `Supply composition${floor ? ' (circulating is a floor)' : ''}: ${segs
    .map((s) => `${s.label} ${formatBaseUnits(s.units.toString(), decimals)}`)
    .join(', ')}`;

  return (
    <div className="space-y-2" data-testid="supply-composition">
      <div
        role="img"
        aria-label={aria}
        className="flex h-3 w-full overflow-hidden rounded-xs"
      >
        {segs.map((s) => (
          <span
            key={s.key}
            className={`${s.className} h-full`}
            style={{ width: `${Math.max(pct(s.units), 0.5)}%` }}
            title={`${s.label}: ${formatBaseUnits(s.units.toString(), decimals)}`}
          />
        ))}
      </div>
      <ul className="text-ink-muted flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
        {segs.map((s) => (
          <li key={s.key} className="flex items-center gap-1.5">
            <span
              aria-hidden
              className={`${s.className} inline-block h-2 w-3 rounded-xs`}
            />
            {s.label}{' '}
            <span className="text-ink font-mono tabular-nums">
              {floor && s.key === 'circ' ? '≥ ' : ''}
              {floor && s.key === 'locked' ? '≤ ' : ''}
              {ratioPct(s.units.toString(), denom.toString(), 1)}%
            </span>
          </li>
        ))}
        {floor && (
          <li className="text-warn-700">Circulating is a floor, not exact</li>
        )}
        {unlimited && <li>No cap: issuer asserts unbounded supply</li>}
      </ul>
    </div>
  );
}
