'use client';

import { usePriceFlash } from '@/lib/live/hooks';
import { cn } from '@/lib/cn';
import { formatPairPrice } from '@/lib/format';

/**
 * LastPriceCell — the shared last-price table cell: adaptive pair-price
 * formatting + flash-on-change.
 *
 * One component so the formatting ladder and the flash cannot fork
 * between tables again.
 *
 * Flash on change (RT-2): each cell watches its own value across
 * refetches. Hook order stays stable because the hook runs before any
 * early return. Pair prices are quote-per-base and span >9 orders of
 * magnitude across the ~5K active pairs, so formatPairPrice (@/lib/format)
 * adapts digits to keep precision visible.
 */
export function LastPriceCell({ raw }: { raw?: string | null }) {
  const flash = usePriceFlash(raw ?? undefined);
  if (!raw) return <span className="text-ink-faint">—</span>;
  const price = formatPairPrice(raw);
  if (price === '—') return <span className="text-ink-faint">—</span>;
  return (
    <span
      className={cn(
        'text-ink-body font-mono tabular-nums',
        flash === 'up' && 'flash-up',
        flash === 'down' && 'flash-down',
      )}
    >
      {price}
    </span>
  );
}
