'use client';

import Link from 'next/link';
import { AlertTriangle, XCircle } from 'lucide-react';

import { useStatus } from '@/api/hooks';
import { CURRENT_NETWORK } from '@/lib/networks';

/**
 * DegradedBanner surfaces in-product degraded / down state from
 * `/v1/status.overall`. Sits between the Navbar and the page
 * content; renders nothing when overall = "ok" or unknown.
 *
 * Why in-product instead of just on /status:
 * a consumer or developer reading prices doesn't naturally
 * navigate to a separate status domain to discover the API
 * is degraded — so a stale chart looks like normal data
 * unless we tell them otherwise. QA finding F-01 in
 * docs/review-2026-05-13-live-site-qa.md.
 *
 * Data: the shared useStatus query (FEC A6-6/D2 fold — this banner,
 * the sidebar Status pill, and the /status page used to run separate
 * poll loops over the same endpoint and could disagree in one
 * viewport). Cadence is the shared STATUS_POLL_MS (30 s).
 */

// health-check: consecutive failed polls before we flag the status feed
// itself as unreachable. 2 (~2 poll intervals) rides out one transient
// blip while still catching a real outage well within a couple of minutes
// — see the `unreachable` state below.
const FAILURE_THRESHOLD = 2;

export function DegradedBanner() {
  const feed = useStatus().data;

  // The lean test nets run no aggregator and no Prometheus/Alertmanager, so
  // /v1/status reports overall="degraded" (indexer/aggregator services read
  // "unknown", last_seen 0) and incidents_status="unknown". That is a false
  // signal — the test net is serving fine — so the banner would sit permanently
  // "Degraded performance · alert status unknown". Suppress it there entirely;
  // the banner exists to warn about the mainnet serving plane. (Called after
  // useStatus so the hook order is unconditional; pricing is a build constant.)
  if (!CURRENT_NETWORK.pricing) return null;

  // health-check: distinct from `overall` — "we could not reach the status
  // feed at all", which used to be swallowed silently and look identical
  // to "everything's fine" (the one failure mode this banner exists to
  // catch: a total outage).
  const unreachable = (feed?.consecutiveFailures ?? 0) >= FAILURE_THRESHOLD;
  const overall = feed?.status?.overall ?? 'unknown';
  // Trust signal for the incident counts (W1.1). When "unknown" (alerting
  // query failed) the counts are absence-of-signal, not an all-clear — the
  // banner renders "alert status unknown" rather than "0 active alerts".
  // Fail closed: a missing trust signal is treated as "unknown", never
  // assumed "ok".
  const incidentsStatus = feed?.status?.incidents_status ?? 'unknown';
  const incs = feed?.status?.incidents;
  // Genuine zeros only — these are read/displayed solely when
  // incidentsStatus is "ok"/"degraded" (see the render below), so an
  // absent count here is a real zero, not a blind all-clear.
  const activeCount = incs?.active_count ?? 0;
  const pageCount = incs?.page_count ?? 0;
  const topAlert =
    (incs?.active ?? []).find((i) => i.severity === 'page')?.name ??
    (incs?.active ?? [])[0]?.name ??
    null;

  if (!unreachable && (overall === 'ok' || overall === 'unknown')) return null;

  const isDown = overall === 'down' || pageCount > 0;
  const tone = unreachable
    ? {
        bg: 'bg-bad-50',
        border: 'border-bad-500/30',
        fg: 'text-bad-700',
        Icon: XCircle,
        label: 'Status feed unreachable — the API may be down',
      }
    : isDown
      ? {
          bg: 'bg-bad-50',
          border: 'border-bad-500/30',
          fg: 'text-bad-700',
          Icon: XCircle,
          label: 'Major incident in progress',
        }
      : {
          bg: 'bg-warn-50',
          border: 'border-warn-500/30',
          fg: 'text-warn-700',
          Icon: AlertTriangle,
          label: 'Degraded performance',
        };
  const Icon = tone.Icon;
  return (
    <div
      role="status"
      aria-live="polite"
      className={`border-y px-4 py-2 text-sm ${tone.bg} ${tone.border} ${tone.fg}`}
    >
      <div className="mx-auto flex max-w-7xl items-center gap-3">
        <Icon className="h-4 w-4 shrink-0" />
        <span className="font-medium">{tone.label}.</span>
        {!unreachable && (
          <span className="hidden text-xs opacity-90 sm:inline">
            {incidentsStatus === 'unknown' ? (
              // The alerting query failed — do NOT claim "0 active
              // alerts" (a false all-clear). Surface the blind spot.
              'alert status unknown'
            ) : (
              <>
                {activeCount} active alert{activeCount === 1 ? '' : 's'}
                {topAlert && (
                  <>
                    {' '}
                    · top:{' '}
                    <code className="bg-surface/40 rounded-sm px-1 py-0.5 text-[11px]">
                      {topAlert}
                    </code>
                  </>
                )}
              </>
            )}
          </span>
        )}
        <span className="ml-auto text-xs">
          <Link
            href="/status"
            target="_blank"
            rel="noopener noreferrer"
            className="underline-offset-2 hover:underline"
          >
            View status →
          </Link>
        </span>
      </div>
    </div>
  );
}
