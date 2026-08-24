'use client';

import { useState } from 'react';

import { cn } from '@/lib/cn';

/**
 * RollingNumber — odometer-style live counter. Only the characters that
 * actually changed roll in: a normal tick rolls the last digit; a carry
 * rolls exactly the carried span (…09→…10 rolls two digits, …99→…100
 * rolls three, and so on). Unchanged digits hold perfectly still
 * (tabular-nums keeps their columns fixed).
 *
 * Mechanism (v2 — the v1 "key on (length, position, char)" scheme made
 * the WHOLE number roll on first paint and on every badge remount, and
 * re-keyed every digit when the length changed):
 *
 *  - Characters are keyed on (column-from-the-right, char), so a value
 *    change remounts exactly the columns whose character changed, and a
 *    length change (a carry into a new digit) keeps every surviving
 *    column's key stable.
 *  - The `.digit-roll` animation class is assigned at BIRTH only: a span
 *    mounts with it iff its column differs from the PREVIOUS committed
 *    value (tracked in a ref). The first render of a mount has no
 *    previous value, so nothing animates on page load or when the badge
 *    unmounts/remounts around a stream hiccup — the flash Ash reported.
 *  - On later renders an unchanged column computes no animation class;
 *    removing `.digit-roll` from a settled span is visually a no-op (the
 *    animation already completed at its natural resting styles), and
 *    React never replays an animation on a node it merely updates.
 *
 * Reduced motion is handled by the global prefers-reduced-motion
 * override. The full number is duplicated sr-only for assistive tech.
 */
export function RollingNumber({
  value,
  className,
}: {
  value: number;
  className?: string;
}) {
  const formatted = value.toLocaleString('en-US');
  // Previous committed value, tracked with the adjust-state-during-render
  // pattern (same as SearchModal's open-reset): when the value changes we
  // record the outgoing one, and React immediately re-renders with the
  // adjusted state before committing. First render has no previous value,
  // so nothing rolls on mount.
  const [hist, setHist] = useState<{ cur: string; prev: string | null }>({
    cur: formatted,
    prev: null,
  });
  if (hist.cur !== formatted) {
    setHist({ cur: formatted, prev: hist.cur });
  }
  const prev = hist.cur === formatted ? hist.prev : hist.cur;

  const chars = formatted.split('');
  return (
    <span className={cn('inline-flex tabular-nums', className)}>
      <span className="sr-only">{formatted}</span>
      {chars.map((ch, i) => {
        const col = chars.length - i; // column index from the right
        const prevCh = prev == null ? null : prev[prev.length - col];
        const rolls = prev != null && prevCh !== ch;
        return (
          <span
            key={`${col}:${ch}`}
            aria-hidden
            className={cn('digit-cell inline-block', rolls && 'digit-roll')}
          >
            {ch}
          </span>
        );
      })}
    </span>
  );
}
