import { relativeAge } from '../../explorer-shared';

/**
 * Wire shape of the protocol detail's `analytics` status object
 * (openapi /protocols/{name} — since 1.16.0): explicit health of the
 * analytics halves so a degraded build is distinguishable from real
 * zeros / real absence.
 */
export type AnalyticsStatus = {
  status?: 'ok' | 'stale' | 'unavailable';
  as_of?: string;
};

/**
 * AnalyticsStatusNote — a one-line banner under the protocol header that
 * surfaces non-ok analytics states from the API's explicit status field
 * (never inferred from field absence — absence alone is ambiguous):
 *
 *  - `unavailable`: the server's analytics build degraded; missing
 *    sections / zero-looking figures mean DEGRADATION, not zero activity.
 *  - `stale`: real data served past its freshness horizon while the
 *    server rebuilds in the background — shown with its real age.
 *  - `ok` / absent: renders nothing.
 */
export function AnalyticsStatusNote({ analytics }: { analytics?: AnalyticsStatus }) {
  if (!analytics || analytics.status === 'ok' || !analytics.status) return null;
  if (analytics.status === 'unavailable') {
    return (
      <p
        role="status"
        className="rounded-md border border-line bg-surface-subtle px-3 py-2 text-xs text-ink-muted"
      >
        Some protocol analytics are temporarily unavailable — missing sections
        and zero-looking figures here mean the analytics build degraded, not
        that activity is zero. Refreshes automatically.
      </p>
    );
  }
  return (
    <p
      role="status"
      className="rounded-md border border-line bg-surface-subtle px-3 py-2 text-xs text-ink-muted"
    >
      Analytics snapshot
      {analytics.as_of ? ` from ${relativeAge(analytics.as_of)}` : ' is stale'} — a
      background refresh is running.
    </p>
  );
}

/**
 * BespokeUnavailable — the placeholder rendered in the bespoke block's
 * slot when the block is ABSENT because the build degraded
 * (`analytics.status === 'unavailable'`). Under `ok`, absence means the
 * category genuinely has no bespoke suite and nothing is rendered.
 */
export function BespokeUnavailable() {
  return (
    <p className="rounded-md border border-line px-3 py-6 text-center text-sm text-ink-muted">
      This protocol&rsquo;s analytics suite is temporarily unavailable — the
      server-side build degraded. It refreshes automatically; reload in a
      minute.
    </p>
  );
}
