import { cn } from '@/lib/cn';

import { Badge } from './Badge';

export type TxStatusBadgeProps = {
  /**
   * Tri-state outcome:
   *   true      → applied (success)
   *   false     → did NOT apply (FAILED) — nothing it names actually moved
   *   undefined → outcome UNAVAILABLE (honest degraded state, disclosed via a
   *               view's `coverage_note`); rendered muted as "unknown", NEVER
   *               as success.
   */
  successful?: boolean;
  /** Human result slug (e.g. tx_failed, op_bad_auth) — the preferred failure label. */
  result?: string;
  /** Numeric XDR result code — fallback failure label + hover detail (0 = success). */
  code?: number;
  className?: string;
};

/**
 * TxStatusBadge is the shared success / failure / unknown pill for transaction
 * and operation outcomes (D-PART-FAILEDTX). A failed outcome stays VISIBLE and
 * reads as its reason slug in red, never hidden and never masquerading as a
 * real interaction; an absent outcome is the MUTED "unknown" degraded state,
 * NOT success. Built on the Badge primitive (up / down / neutral tones).
 */
export function TxStatusBadge({ successful, result, code, className }: TxStatusBadgeProps) {
  // undefined ≠ false: the parent outcome was never read (a degraded response
  // disclosed by the view's coverage_note). Muted "unknown", never success.
  if (successful == null) {
    return (
      <Badge
        tone="neutral"
        className={cn('text-ink-muted', className)}
        title="transaction outcome unavailable"
      >
        unknown
      </Badge>
    );
  }
  if (successful) {
    return (
      <Badge tone="up" className={className} title="success">
        success
      </Badge>
    );
  }
  // Failed: prefer the human slug, fall back to the numeric code, then a bare
  // "failed" — and always keep the full slug + code in the title.
  const codeLabel = code != null ? `code ${code}` : undefined;
  const label = result ?? codeLabel ?? 'failed';
  const title =
    result != null && code != null
      ? `${result} (code ${code})`
      : (result ?? codeLabel ?? 'failed');
  return (
    <Badge tone="down" className={className} title={title}>
      {label}
    </Badge>
  );
}
