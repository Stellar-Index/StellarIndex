'use client';

import { useEffect, useState } from 'react';

/**
 * useDebouncedValue — returns `value` after it has been stable for `ms`.
 * Per-site delay is the argument.
 */
export function useDebouncedValue<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return debounced;
}
