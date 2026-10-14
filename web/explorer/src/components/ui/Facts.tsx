'use client';

import { createContext, useContext, type ReactNode } from 'react';
import { cn } from '@/lib/cn';
import { PANEL_LABEL_CLASS } from './Page';

type Layout = 'rows' | 'grid';
const LayoutContext = createContext<Layout>('rows');

/**
 * FactList — the key/value list of an entity detail page. Rows (default)
 * put the label in a fixed column with a hairline between facts, the
 * canonical shape for hashes and addresses; `columns` lays the same facts
 * out as stacked cells for short values.
 */
export function FactList({
  children,
  columns,
  className,
}: {
  children: ReactNode;
  columns?: 2 | 3 | 4;
  className?: string;
}) {
  const grid = columns
    ? {
        2: 'sm:grid-cols-2',
        3: 'sm:grid-cols-2 lg:grid-cols-3',
        4: 'sm:grid-cols-2 lg:grid-cols-4',
      }[columns]
    : null;
  return (
    <LayoutContext.Provider value={grid ? 'grid' : 'rows'}>
      <dl
        className={cn(
          grid
            ? cn('grid grid-cols-1 gap-x-6 gap-y-4', grid)
            : 'divide-line divide-y',
          className,
        )}
      >
        {children}
      </dl>
    </LayoutContext.Provider>
  );
}

/** One fact. `title` goes on the value and marks it as explained on hover. */
export function Fact({
  label,
  children,
  title,
  mono = true,
  className,
}: {
  label: ReactNode;
  children: ReactNode;
  title?: string;
  /** Values are figures and identifiers by default; pass false for prose. */
  mono?: boolean;
  className?: string;
}) {
  const layout = useContext(LayoutContext);
  return (
    <div
      className={cn(
        'min-w-0',
        layout === 'rows' &&
          'grid gap-1 py-2.5 first:pt-0 last:pb-0 sm:grid-cols-[10rem_minmax(0,1fr)] sm:gap-4',
        className,
      )}
    >
      <dt className={cn(PANEL_LABEL_CLASS, layout === 'rows' && 'sm:pt-0.5')}>
        {label}
      </dt>
      <dd
        title={title}
        className={cn(
          'text-ink min-w-0 break-words',
          layout === 'grid' && 'mt-1',
          mono ? 'font-mono text-[13px] tabular-nums' : 'text-sm',
          title && 'cursor-help',
        )}
      >
        {children}
      </dd>
    </div>
  );
}
