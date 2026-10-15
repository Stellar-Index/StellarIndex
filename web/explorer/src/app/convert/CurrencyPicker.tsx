'use client';

import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
} from 'react';
import { ChevronDown, Search } from 'lucide-react';

import { cn } from '@/lib/cn';
import { useDialog } from '@/lib/useDialog';

export type CurrencyGroup = {
  label: string;
  items: { ticker: string; name?: string }[];
};

/**
 * CurrencyPicker — the ticker badge inside a converter field, opening a
 * searchable list of every currency the converter offers.
 */
export function CurrencyPicker({
  label,
  value,
  groups,
  onPick,
}: {
  label: string;
  value: string;
  groups: CurrencyGroup[];
  onPick: (ticker: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState('');
  const [cursor, setCursor] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const listId = useId();

  const sections = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const kept = groups
      .map((g) => ({
        label: g.label,
        items: needle
          ? g.items.filter(
              (i) =>
                i.ticker.toLowerCase().includes(needle) ||
                i.name?.toLowerCase().includes(needle),
            )
          : g.items,
      }))
      .filter((g) => g.items.length > 0);
    // `start` is each section's offset into the flat list the cursor walks.
    return kept.map((g, i) => ({
      ...g,
      start: kept.slice(0, i).reduce((n, s) => n + s.items.length, 0),
    }));
  }, [groups, q]);
  const flat = useMemo(() => sections.flatMap((s) => s.items), [sections]);

  const close = useCallback(() => {
    setOpen(false);
    setQ('');
    setCursor(0);
  }, []);
  // Escape, focus move-in (the search input) and focus restore (the trigger).
  const popRef = useDialog<HTMLDivElement>(open, close);
  const pick = (ticker: string) => {
    onPick(ticker);
    close();
  };

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) close();
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open, close]);

  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setCursor((c) => Math.min(c + 1, Math.max(flat.length - 1, 0)));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setCursor((c) => Math.max(c - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      const hit = flat[cursor];
      if (hit) pick(hit.ticker);
    }
  };

  return (
    <div ref={rootRef} className="relative shrink-0">
      <button
        type="button"
        aria-label={label}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => (open ? close() : setOpen(true))}
        className="border-line bg-surface-subtle text-ink hover:border-line-strong inline-flex h-7 items-center gap-1 rounded-sm border px-1.5 font-mono text-xs tracking-wider uppercase"
      >
        {value}
        <ChevronDown className="text-ink-faint h-3 w-3" aria-hidden="true" />
      </button>
      {open && (
        <div
          ref={popRef}
          tabIndex={-1}
          className="border-line bg-surface absolute top-full right-0 z-20 mt-1 w-64 rounded-sm border"
        >
          <div className="border-line focus-within:border-brand-500 flex items-center gap-2 border-b px-2 py-1.5">
            <Search className="text-ink-faint h-3.5 w-3.5" aria-hidden="true" />
            <input
              role="combobox"
              aria-label={`Search ${label.toLowerCase()} currencies`}
              aria-controls={listId}
              aria-expanded="true"
              aria-autocomplete="list"
              value={q}
              onChange={(e) => {
                setQ(e.target.value);
                setCursor(0);
              }}
              onKeyDown={onKey}
              placeholder="Search"
              className="placeholder:text-ink-faint w-full bg-transparent text-sm outline-hidden"
            />
          </div>
          <ul
            id={listId}
            role="listbox"
            aria-label={`${label} currencies`}
            className="max-h-64 overflow-y-auto py-1"
          >
            {flat.length === 0 && (
              <li
                role="option"
                aria-selected={false}
                aria-disabled="true"
                className="text-ink-faint px-2 py-1.5 text-xs"
              >
                No match
              </li>
            )}
            {sections.map((s) => (
              <li key={s.label} role="presentation">
                <div className="text-ink-faint px-2 pt-2 pb-1 text-[11px]">
                  {s.label}
                </div>
                <ul role="group" aria-label={s.label}>
                  {s.items.map((i, j) => {
                    const at = s.start + j;
                    return (
                      <li
                        key={i.ticker}
                        role="option"
                        aria-selected={i.ticker === value}
                        onMouseEnter={() => setCursor(at)}
                        onMouseDown={(e) => e.preventDefault()}
                        onClick={() => pick(i.ticker)}
                        className={cn(
                          'flex cursor-pointer items-center gap-3 px-2 py-1.5 text-sm',
                          at === cursor ? 'text-ink bg-black' : 'text-ink-body',
                          i.ticker === value && 'text-brand-600',
                        )}
                      >
                        <span className="font-mono text-xs tracking-wider uppercase">
                          {i.ticker}
                        </span>
                        {i.name && i.name !== i.ticker && (
                          <>
                            {' '}
                            <span className="text-ink-faint min-w-0 truncate text-xs">
                              {i.name}
                            </span>
                          </>
                        )}
                      </li>
                    );
                  })}
                </ul>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}
