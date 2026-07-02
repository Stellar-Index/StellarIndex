'use client';

import { useEffect } from 'react';
import { AlertTriangle, RefreshCw } from 'lucide-react';

import { Button, Callout, Container, EmptyState } from '@/components/ui';

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
