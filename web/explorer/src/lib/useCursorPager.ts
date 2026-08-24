'use client';

import { useState } from 'react';

/**
 * useCursorPager — the cursor-stack pagination state machine (FEC audit
 * A3-F3): next pushes the current cursor, prev pops, changing any
 * order/filter resets. This existed byte-identically in DexesView,
 * PoolsTable, and PairsTable; extracted in the pre-drift window so the
 * fourth table can't fork it.
 */
export function useCursorPager() {
  const [cursor, setCursor] = useState<string>('');
  const [stack, setStack] = useState<string[]>([]);

  function next(nextCursor: string | undefined) {
    if (!nextCursor) return;
    setStack((s) => [...s, cursor]);
    setCursor(nextCursor);
  }
  function prev() {
    // Pure updaters (review follow-up): the originals set cursor inside
    // the stack updater, which works but re-runs the side effect if
    // React replays the updater. Read this render's stack instead.
    setCursor(stack[stack.length - 1] ?? '');
    setStack((s) => s.slice(0, -1));
  }
  function reset() {
    setCursor('');
    setStack([]);
  }

  return {
    cursor,
    /** 0-based page depth — row offset = depth * pageLimit + i. */
    depth: stack.length,
    /** 1-based page number for "page N" footers. */
    page: stack.length + 1,
    hasPrev: stack.length > 0,
    next,
    prev,
    reset,
  };
}
