'use client';

import { cn } from '@/lib/cn';

/**
 * RollingNumber — odometer-style live counter. Only the characters that
 * actually changed roll in: a normal tick rolls the last digit; a carry
 * rolls exactly the carried span (…09→…10 rolls two digits, …99→…100
 * rolls three, and so on). Unchanged digits hold perfectly still
 * (tabular-nums keeps their columns fixed).
 *
 * Mechanism: each character is keyed on (length, position, character),
 * so React remounts precisely the characters whose value changed and the
 * CSS mount animation (`.digit-roll`, globals.css) plays only for them.
 * No prev-value bookkeeping needed; unrelated re-renders reuse the same
 * keys and never replay the roll. Reduced motion is handled by the
 * global prefers-reduced-motion override.
 */
export function RollingNumber({
  value,
  className,
}: {
  value: number;
  className?: string;
}) {
  const formatted = value.toLocaleString('en-US');
  const len = formatted.length;
  return (
    <span className={cn('inline-flex tabular-nums', className)}>
      <span className="sr-only">{formatted}</span>
      {formatted.split('').map((ch, i) => (
        <span
          key={`${len}:${i}:${ch}`}
          aria-hidden
          className="digit-roll inline-block"
        >
          {ch}
        </span>
      ))}
    </span>
  );
}
