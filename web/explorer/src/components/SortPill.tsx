'use client';

import type { ReactNode } from 'react';

/**
 * SortPill — the small order-by toggle above data tables.
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
