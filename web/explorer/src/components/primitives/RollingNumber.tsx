'use client';

import { useState } from 'react';

import { cn } from '@/lib/cn';

/**
 * RollingNumber — odometer-style live counter. Only the characters that
 * actually changed roll over: a normal tick rolls the last digit; a
 * carry rolls exactly the carried span. Unchanged digits hold perfectly
 * still (tabular-nums keeps their columns fixed).
 *
 * v3 — a TRUE mechanical rollover (v2's fade-in read as a flash: the
 * old digit vanished instantly and the new one blinked in). A changed
 * column now mounts a two-digit strip [old, new] inside a clipped
 * 1-line window; the `digit-rollover` animation slides the strip up by
 * one digit, so the old character visibly exits upward while the new
 * one arrives from below — the odometer wheel. The strip's BASE
 * transform is the end position (new digit showing), so reduced-motion
 * (which collapses animation durations) and the settled state both rest
 * on the new value.
 *
 * Keying (from v2, load-bearing): columns are keyed from the RIGHT on
 * (column, char), so a value change remounts exactly the columns whose
 * character changed, a length change keeps surviving columns' keys
 * stable, and the FIRST render of a mount (no previous value) animates
 * nothing — no page-load or badge-remount flash. A brand-new column
 * (the number grew a digit) appears without animation.
 *
 * The full number is duplicated sr-only for assistive tech; the visual
 * cells are aria-hidden.
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
  // adjusted state before committing.
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
        const prevCh = prev == null ? undefined : prev[prev.length - col];
        const rolls = prev != null && prevCh != null && prevCh !== ch;
        if (!rolls) {
          return (
            <span key={`${col}:${ch}`} aria-hidden className="digit-cell inline-block">
              {ch}
            </span>
          );
        }
        return (
          <span key={`${col}:${ch}`} aria-hidden className="digit-cell digit-window">
            <span className="digit-strip">
              <span>{prevCh}</span>
              <span>{ch}</span>
            </span>
          </span>
        );
      })}
    </span>
  );
}
