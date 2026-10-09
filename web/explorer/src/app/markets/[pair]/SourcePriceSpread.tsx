import { Badge } from '@/components/ui';
import { changePct, compareDecimalStrings, formatPrice } from '@/lib/format';

export type SpreadRow = { source: string; last_price?: string | null };

/**
 * SourcePriceSpread — one dot per source on the min..max band of the last
 * prices already in the venue list. Last prices are each venue's own most
 * recent trade, so they are not simultaneous quotes. Needs two priced venues.
 */
export function SourcePriceSpread({
  rows,
  label = 'Last-price spread',
  noun = 'venues',
}: {
  rows: readonly SpreadRow[];
  label?: string;
  noun?: string;
}) {
  const priced = rows.filter(
    (r): r is { source: string; last_price: string } =>
      r.last_price != null && compareDecimalStrings(r.last_price, '0') === 1,
  );
  if (priced.length < 2) return null;
  const sorted = [...priced].sort(
    (a, b) => compareDecimalStrings(a.last_price, b.last_price) ?? 0,
  );
  const lo = sorted[0].last_price;
  const hi = sorted[sorted.length - 1].last_price;
  const spread = changePct(lo, hi, 2);
  const span = Number(hi) - Number(lo);

  return (
    <div className="mb-3 space-y-1.5" data-testid="source-price-spread">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <span className="text-ink-muted">{label}</span>
        {spread != null && (
          <Badge
            tone={spread >= 1 ? 'warn' : 'ok'}
            title={`(highest - lowest) / lowest price across ${noun}`}
          >
            {spread.toFixed(2)}%
          </Badge>
        )}
        <span className="text-ink-faint">
          {formatPrice(lo)} to {formatPrice(hi)}
        </span>
      </div>
      <div
        role="img"
        aria-label={`${label}, ${sorted.length} ${noun}, spread ${spread?.toFixed(2) ?? 'n/a'}%`}
        className="bg-surface-subtle relative h-3 rounded-full"
      >
        {sorted.map((r) => (
          <span
            key={r.source}
            title={`${r.source}: ${formatPrice(r.last_price)}`}
            className="bg-brand-500 absolute top-0.5 h-2 w-2 -translate-x-1/2 rounded-full"
            style={{
              left: `${span > 0 ? ((Number(r.last_price) - Number(lo)) / span) * 96 + 2 : 50}%`,
            }}
          />
        ))}
      </div>
    </div>
  );
}
