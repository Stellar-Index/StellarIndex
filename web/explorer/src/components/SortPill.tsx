'use client';

import type { ReactNode } from 'react';

/**
 * SortPill — the small order-by toggle above data tables. Was
 * byte-identical in DexesView / PoolsTable / PairsTable (FEC audit
 * A3-F6.1) — extracted verbatim, zero behavior change. The wider
 * aria-pressed toggle-family consolidation (F6.2, onto ui/Segmented)
 * is a recorded follow-up with a pending active-style design call.
 */
export function SortPill({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={`focus-visible:ring-brand-500/60 rounded-md px-2 py-0.5 focus-visible:ring-2 focus-visible:outline-hidden ${
        active
          ? 'bg-brand-fill text-white'
          : 'bg-surface-subtle text-ink-body hover:bg-line'
      }`}
    >
      {children}
    </button>
  );
}
