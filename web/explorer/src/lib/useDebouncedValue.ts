'use client';

import { useEffect, useState } from 'react';

/**
 * useDebouncedValue — returns `value` after it has been stable for `ms`.
 * FEC audit A3-F5: the app's two real debounce state machines (AssetsTable
 * input→URL at 250ms, SearchModal input→query at 200ms) each hand-rolled
 * this — AssetsTable's copy needed an exhaustive-deps eslint-disable to
 * keep its timer stable. One hook, per-site delay as the argument.
 */
export function useDebouncedValue<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return debounced;
}
