'use client';

import { useMemo, useState } from 'react';
import { ArrowDown, ArrowUp, ChevronsUpDown } from 'lucide-react';

/**
 * Shared client-side table sorting for the explorer listings (`/markets`,
 * `/assets`, `/ledgers`, `/operations`, `/contracts`, `/issuers`,
 * `/oracles`). Sortable headers carry a click target, `aria-sort` and a
 * focusable `tabIndex`.
 *
 * A column is defined by a key and a value accessor. Clicking a header
 * sorts by that column; clicking the active header flips direction.
 * Numeric-vs-string ordering is inferred from the accessor's return type,
 * and null/undefined always sort last regardless of direction (a missing
 * price should not jump to the top of an ascending sort).
 */

export type SortDir = 'asc' | 'desc';

export interface SortState<K extends string> {
  key: K | null;
  dir: SortDir;
}

export type SortValue = number | string | null | undefined;

export interface SortColumn<T, K extends string> {
  key: K;
  value: (row: T) => SortValue;
  /** Default direction when this column is first clicked. */
  initialDir?: SortDir;
}

export interface UseTableSort<T, K extends string> {
  sorted: T[];
  sort: SortState<K>;
  toggle: (key: K) => void;
  /** aria-sort value for a header cell of the given key. */
  ariaSort: (key: K) => 'ascending' | 'descending' | 'none';
}

/**
 * useTableSort keeps a stable sort by an initial column and lets the user
 * reorder by any declared column. `initialKey` may be null for "leave the
 * incoming order alone until the user clicks".
 */
export function useTableSort<T, K extends string>(
  rows: T[],
  columns: SortColumn<T, K>[],
  initialKey: K | null = null,
  initialDir: SortDir = 'desc',
): UseTableSort<T, K> {
  const [sort, setSort] = useState<SortState<K>>({
    key: initialKey,
    dir: initialDir,
  });

  const colByKey = useMemo(() => {
    const m = new Map<K, SortColumn<T, K>>();
    for (const c of columns) m.set(c.key, c);
    return m;
  }, [columns]);

  const toggle = (key: K) => {
    setSort((prev) => {
      if (prev.key !== key) {
        return { key, dir: colByKey.get(key)?.initialDir ?? 'desc' };
      }
      return { key, dir: prev.dir === 'asc' ? 'desc' : 'asc' };
    });
  };

  const sorted = useMemo(() => {
    if (!sort.key) return rows;
    const col = colByKey.get(sort.key);
    if (!col) return rows;
    const dir = sort.dir === 'asc' ? 1 : -1;
    // Stable sort: decorate with the original index so equal keys keep
    // their incoming order (the API's own ranking) rather than shuffling.
    return rows
      .map((row, i) => ({ row, i }))
      .sort((a, b) => {
        const av = col.value(a.row);
        const bv = col.value(b.row);
        const aEmpty = isEmptyValue(av);
        const bEmpty = isEmptyValue(bv);
        // Emptiness always sorts last, independent of `dir` — only the
        // ordering of non-empty values flips with direction.
        if (aEmpty || bEmpty) {
          if (aEmpty === bEmpty) return a.i - b.i;
          return aEmpty ? 1 : -1;
        }
        const cmp = compareValues(av, bv);
        return cmp !== 0 ? cmp * dir : a.i - b.i;
      })
      .map((d) => d.row);
  }, [rows, sort, colByKey]);

  const ariaSort = (key: K): 'ascending' | 'descending' | 'none' => {
    if (sort.key !== key) return 'none';
    return sort.dir === 'asc' ? 'ascending' : 'descending';
  };

  return { sorted, sort, toggle, ariaSort };
}

// isEmptyValue flags a cell as blank for sorting purposes. Emptiness is
// pinned to the bottom by the caller before `dir` is applied, so it never
// flips with direction.
function isEmptyValue(v: SortValue): boolean {
  return v == null || (typeof v === 'number' && Number.isNaN(v));
}

// compareValues orders two known-non-empty cell values.
function compareValues(a: SortValue, b: SortValue): number {
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  // Pinned to en-US, same as every formatter in lib/format.ts, so a
  // non-ASCII column sorts identically between the SSG build and
  // client hydration regardless of the browser's default locale.
  return String(a).localeCompare(String(b), 'en-US');
}

/**
 * SortableTh — a table header cell that is a real sort control: a button
 * with an arrow affordance and a correct `aria-sort` on the <th> (S27).
 *
 * `align` matches the app's <Th> convention. When `sortKey` is omitted the
 * header renders as a plain, non-interactive label (for columns like "#"
 * or a chart sparkline that don't sort).
 */
export function SortableTh<K extends string>({
  label,
  sortKey,
  sort,
  onSort,
  ariaSort,
  align = 'left',
  className = '',
}: {
  label: React.ReactNode;
  sortKey?: K;
  sort?: SortState<K>;
  onSort?: (key: K) => void;
  ariaSort?: (key: K) => 'ascending' | 'descending' | 'none';
  align?: 'left' | 'right' | 'center';
  className?: string;
}) {
  const alignCls =
    align === 'right'
      ? 'text-right'
      : align === 'center'
        ? 'text-center'
        : 'text-left';
  const base = `h-10 whitespace-nowrap px-4 font-medium ${alignCls} ${className}`;

  if (!sortKey || !onSort || !sort || !ariaSort) {
    return (
      <th className={base} scope="col">
        {label}
      </th>
    );
  }

  const active = sort.key === sortKey;
  const justify =
    align === 'right'
      ? 'justify-end'
      : align === 'center'
        ? 'justify-center'
        : 'justify-start';

  return (
    <th className={base} scope="col" aria-sort={ariaSort(sortKey)}>
      <button
        type="button"
        onClick={() => onSort(sortKey)}
        className={`group inline-flex w-full items-center gap-1 [text-transform:inherit] ${justify} hover:text-ink ${
          active ? 'text-ink' : ''
        }`}
      >
        {label}
        {active ? (
          sort.dir === 'asc' ? (
            <ArrowUp className="h-3 w-3" aria-hidden="true" />
          ) : (
            <ArrowDown className="h-3 w-3" aria-hidden="true" />
          )
        ) : (
          <ChevronsUpDown
            className="h-3 w-3 opacity-0 transition-opacity duration-150 group-hover:opacity-100 group-focus-visible:opacity-100"
            aria-hidden="true"
          />
        )}
      </button>
    </th>
  );
}
