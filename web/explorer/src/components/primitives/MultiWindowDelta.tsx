import { twMerge } from 'tailwind-merge';
import { DirectionPill } from './DirectionPill';

export type DeltaWindow = {
  /** Display label, e.g. '1h', '24h', '7d', '30d' */
  label: string;
  /** Percent change as a decimal — 5.2 means +5.2%. Null = no data. */
  deltaPct: number | null;
};

export type MultiWindowDeltaProps = {
  windows: DeltaWindow[];
  /** Compact = inline strip, tighter spacing for dense tables. */
  compact?: boolean;
  /**
   * Wrap = let the windows flow onto multiple lines instead of one
   * non-wrapping inline row. Needed in narrow columns (e.g. the asset
   * sidebar's ~300px card) where the four canonical windows overflow
   * their container and clip the trailing pill. The interpunct
   * separators are dropped in this mode — gap spacing carries the
   * rhythm, and a `·` orphaned at a line break reads as a glitch.
   */
  wrap?: boolean;
  className?: string;
};

/**
 * Multi-window delta strip — renders the
 * `1h: +0.5% · 24h: +3.2% · 7d: −1.1% · 30d: +18.4%` pattern
 * from the data-inventory doc §6.1.
 *
 * Pass any number of windows; the canonical four are h1/h24/d7/d30
 * but the worker can emit fewer (sparse history → some are null)
 * or more (e.g. 1y once we backfill far enough).
 */
export function MultiWindowDelta({
  windows,
  compact,
  wrap,
  className,
}: MultiWindowDeltaProps) {
  return (
    <div
      className={twMerge(
        'items-center text-xs',
        wrap ? 'flex flex-wrap gap-x-2 gap-y-1.5' : 'inline-flex',
        !wrap && (compact ? 'gap-1' : 'gap-2'),
        className,
      )}
    >
      {windows.map((w, i) => (
        <span key={w.label} className="inline-flex items-center gap-1">
          <span className="text-ink-muted">{w.label}:</span>
          <DirectionPill deltaPct={w.deltaPct} compact={compact} />
          {!wrap && i < windows.length - 1 && (
            <span
              className="text-ink-faint"
              aria-hidden
            >
              ·
            </span>
          )}
        </span>
      ))}
    </div>
  );
}
