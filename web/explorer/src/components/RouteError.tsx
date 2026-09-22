'use client';

import { useEffect } from 'react';
import { AlertTriangle, RefreshCw } from 'lucide-react';

import { Button, Callout, Container, EmptyState } from '@/components/ui';

const CLIENT_ERRORS_ENDPOINT = '/client-errors';
const MAX_FIELD_LENGTH = 500;

/**
 * Beacons a caught route error to the `client-errors` CF Pages function
 * (web/explorer/functions/client-errors.js) so it lands in server-side
 * logs instead of only the reporting user's own browser console, which
 * nobody else ever reads. Best-effort: a reporting failure must never
 * surface as a second crash on top of the one already being handled.
 */
function reportRouteError(
  error: Error & { digest?: string },
  section?: string,
) {
  try {
    const payload = JSON.stringify({
      message: (error?.message ?? '').slice(0, MAX_FIELD_LENGTH),
      digest: error?.digest,
      section,
      path: window.location.pathname,
    });
    const blob = new Blob([payload], { type: 'application/json' });
    const sent = navigator.sendBeacon?.(CLIENT_ERRORS_ENDPOINT, blob);
    if (!sent) {
      void fetch(CLIENT_ERRORS_ENDPOINT, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: payload,
        keepalive: true,
      }).catch(() => {});
    }
  } catch {
    // Reporting is best-effort — must never throw out of error handling.
  }
}

/**
 * RouteError — shared body for route-segment `error.tsx` boundaries.
 *
 * Every data-heavy segment mounts a thin `error.tsx` wrapper around
 * this component so a render throw (bad API payload, client-fetch
 * explosion, chart-library crash) degrades to a styled retry surface
 * instead of white-screening the route. `reset()` re-renders just the
 * failed segment; the shell (nav, footer, sibling routes) stays up.
 */
export function RouteError({
  error,
  reset,
  section,
}: {
  error: Error & { digest?: string };
  reset: () => void;
  section?: string;
}) {
  useEffect(() => {
    // Keep the underlying error visible to debugging / error reporting —
    // the rendered boundary intentionally shows only a short summary.
    console.error(`[route-error]${section ? ` ${section}` : ''}`, error);
    reportRouteError(error, section);
  }, [error, section]);

  return (
    <Container className="max-w-2xl py-16">
      <EmptyState
        icon={<AlertTriangle className="h-5 w-5" aria-hidden />}
        title={
          section
            ? `The ${section} page hit an error`
            : 'This page hit an error'
        }
        description="Something threw while rendering — usually a transient data problem. Retrying re-renders just this page; the rest of the site is unaffected."
        action={
          <Button variant="primary" size="sm" onClick={reset}>
            <RefreshCw className="h-3.5 w-3.5" aria-hidden />
            Try again
          </Button>
        }
      />
      {(error?.message || error?.digest) && (
        <Callout tone="bad" title="Error detail" className="mt-4">
          <p className="font-mono text-xs break-words">
            {error?.message || 'Unknown error'}
            {error?.digest ? ` — digest ${error.digest}` : ''}
          </p>
        </Callout>
      )}
    </Container>
  );
}
