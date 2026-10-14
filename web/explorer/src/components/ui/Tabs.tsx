import Link from 'next/link';
import type { ReactNode } from 'react';

import { cn } from '@/lib/cn';

export type TabItem = { label: ReactNode; href: string; count?: number };

/**
 * TabNav — a route-based underline tab strip. Pass the current pathname (or a
 * predicate result) via `isActive`. Server-safe (links, no client state).
 */
export function TabNav({
  items,
  activeHref,
  className,
}: {
  items: TabItem[];
  activeHref?: string;
  className?: string;
}) {
  return (
    // The scroll box sits inside the bordered box: an overflow-x-auto
    // element also scrolls vertically, so a link pulled onto the border with
    // -mb-px gave the strip a 1px vertical scrollbar.
    <div className={cn('border-line border-b', className)}>
      <div className="-mb-px flex items-center gap-1 overflow-x-auto">
        {items.map((t) => {
          const active = activeHref === t.href;
          return (
            <Link
              key={t.href}
              href={t.href}
              aria-current={active ? 'page' : undefined}
              className={cn(
                'border-b-2 px-3 py-2.5 text-sm font-medium whitespace-nowrap transition-colors',
                active
                  ? 'border-brand-600 text-brand-700'
                  : 'text-ink-muted hover:border-line-strong hover:text-ink border-transparent',
              )}
            >
              {t.label}
              {typeof t.count === 'number' && (
                <span
                  className={cn(
                    'tnum ml-1.5 rounded-sm px-1.5 py-0.5 text-[11px]',
                    active
                      ? 'bg-brand-50 text-brand-700'
                      : 'bg-surface-subtle text-ink-muted',
                  )}
                >
                  {t.count}
                </span>
              )}
            </Link>
          );
        })}
      </div>
    </div>
  );
}

/**
 * SegmentedControl — THE pill-style toggle group for compact in-card
 * window/metric switches; SortPill is the one sibling.
 * Quiet `bg-surface ` active style; role="group" + a required
 * aria-label + a focus-visible ring on every segment.
 */
export function Segmented({
  options,
  value,
  onChange,
  ariaLabel,
  className,
}: {
  options: { label: ReactNode; value: string }[];
  value: string;
  onChange: (v: string) => void;
  /** Accessible name for the group (absorbed from WindowPills — required). */
  ariaLabel: string;
  className?: string;
}) {
  return (
    <div
      role="group"
      aria-label={ariaLabel}
      className={cn(
        'bg-surface-subtle inline-flex rounded-lg p-0.5',
        className,
      )}
    >
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          onClick={() => onChange(o.value)}
          aria-pressed={value === o.value}
          className={cn(
            'rounded-md px-2.5 py-1 text-[13px] font-medium transition-colors',
            value === o.value
              ? 'bg-surface text-ink'
              : 'text-ink-muted hover:text-ink',
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
