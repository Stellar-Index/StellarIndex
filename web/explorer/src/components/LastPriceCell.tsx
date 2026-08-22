'use client';

import { usePriceFlash } from '@/lib/live/hooks';
import { cn } from '@/lib/cn';
import { formatPairPrice } from '@/lib/format';

/**
 * LastPriceCell — the shared last-price table cell: adaptive pair-price
 * formatting + flash-on-change.
 *
 * Extracted 2026-08-21 from four hand-copied locals (MarketsTable,
 * PairsTable, PoolsTable, DexesView) — the COR-14/AGT-05 class where the
 * formatting ladder had already forked once; the flash behaviour had
 * forked AGAIN (DexesView's copy silently lacked it). One component, one
 * behaviour.
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
  const n = Number(raw);
  if (!Number.isFinite(n)) return <span className="text-ink-faint">—</span>;
  return (
    <span
      className={cn(
        'font-mono tabular-nums text-ink-body',
        flash === 'up' && 'flash-up',
        flash === 'down' && 'flash-down',
      )}
    >
      {formatPairPrice(n)}
    </span>
  );
}
