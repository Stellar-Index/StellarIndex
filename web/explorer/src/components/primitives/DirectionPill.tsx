import { ArrowDown, ArrowRight, ArrowUp } from 'lucide-react';
import { twMerge } from 'tailwind-merge';

export type DirectionPillProps = {
  // AGT-06: this used to read "Fraction; 0.05 = +5%", which disagreed with
  // the render code below (no *100 conversion — deltaPct.toFixed(2) is used
  // directly) and with the app-wide convention (every real percentage
  // field, e.g. change_24h_pct, already arrives as a percentage-point
  // number — see the AGT-06 removal of the fraction-based formatPctChange
  // footgun in lib/format.ts). Fixed the doc to match the code, not the
  // other way around: doing the reverse would have broken every existing
  // caller (dev/primitives styleguide, MultiWindowDelta).
  /** Already a percentage-point number — 5 = +5%. Pass null for "no data". */
  deltaPct: number | null;
  /** Tightens to a smaller chip when used in dense tables. */
  compact?: boolean;
  className?: string;
};

/**
 * Direction pill — arrow + colour-coded percent change.
 *
 * Bands per design-inventory §6.3:
 *   ↗  +1-5%      light green
 *   ↗↗ +5-20%    green
 *   ↗↗↗ >+20%    bright green
 *   ↘  ↘↘ ↘↘↘    symmetric red
 *   →  <±0.5%    grey
 */
export function DirectionPill({
  deltaPct,
  compact,
  className,
}: DirectionPillProps) {
  if (deltaPct === null || !Number.isFinite(deltaPct)) {
    return (
      <span
        className={twMerge(
          'inline-flex items-center gap-1 rounded-full bg-surface-subtle px-2 py-0.5 text-xs text-ink-muted',
          className,
        )}
        aria-label="no data"
      >
        —
      </span>
    );
  }
  const abs = Math.abs(deltaPct);
  const sign = deltaPct > 0 ? 'up' : deltaPct < 0 ? 'down' : 'flat';
  const Icon = sign === 'up' ? ArrowUp : sign === 'down' ? ArrowDown : ArrowRight;
  const bg =
    abs < 0.5
      ? 'bg-surface-subtle text-ink-body'
      : sign === 'up' && abs < 5
        ? 'bg-up-subtle/40 text-up-strong'
        : sign === 'up' && abs < 20
          ? 'bg-up-subtle text-up-strong'
          : sign === 'up'
            ? 'bg-up text-surface-canvas'
            : sign === 'down' && abs < 5
              ? 'bg-down-subtle/40 text-down-strong'
              : sign === 'down' && abs < 20
                ? 'bg-down-subtle text-down-strong'
                : 'bg-down text-surface-canvas';
  return (
    <span
      className={twMerge(
        'inline-flex items-center gap-0.5 rounded-full font-medium tabular-nums',
        compact ? 'px-1.5 py-0 text-[10px]' : 'px-2 py-0.5 text-xs',
        bg,
        className,
      )}
      aria-label={`${deltaPct > 0 ? '+' : ''}${deltaPct.toFixed(2)}% ${sign}`}
    >
      <Icon className={compact ? 'h-2.5 w-2.5' : 'h-3 w-3'} aria-hidden />
      {deltaPct > 0 ? '+' : ''}
      {deltaPct.toFixed(2)}%
    </span>
  );
}
