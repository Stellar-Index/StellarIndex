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
    <div
      className={cn(
        'border-line flex items-center gap-1 overflow-x-auto border-b',
        className,
      )}
    >
      {items.map((t) => {
        const active = activeHref === t.href;
        return (
          <Link
            key={t.href}
            href={t.href}
            aria-current={active ? 'page' : undefined}
            className={cn(
              '-mb-px border-b-2 px-3 py-2.5 text-sm font-medium whitespace-nowrap transition-colors',
              active
                ? 'border-brand-600 text-brand-700'
                : 'text-ink-muted hover:border-line-strong hover:text-ink border-transparent',
            )}
          >
            {t.label}
            {typeof t.count === 'number' && (
              <span
                className={cn(
                  'tnum ml-1.5 rounded-full px-1.5 py-0.5 text-[11px]',
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
  );
}

/** SegmentedControl — a pill-style toggle group (for compact in-card switches). */
export function Segmented({
  options,
  value,
  onChange,
  className,
}: {
  options: { label: ReactNode; value: string }[];
  value: string;
  onChange: (v: string) => void;
  className?: string;
}) {
  return (
    <div
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
              ? 'bg-surface text-ink shadow-xs'
              : 'text-ink-muted hover:text-ink',
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
