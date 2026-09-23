'use client';

import { Breadcrumbs } from '@/components/ui';
import { SourceStatsPanel } from '@/app/dexes/[source]/SourceStatsPanel';
import { SourceTopChart } from '@/app/dexes/[source]/SourceTopChart';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { SourceHealthPanel } from './SourceHealthPanel';

/**
 * SourcePathView — the runtime fallback for /sources/[name] outside the
 * build-time pre-render, served by functions/sources/[[path]].js (T291).
 * The name is read from the URL; every panel below already fetches its
 * own data client-side, so the page renders live without a rebuild.
 */
export function SourcePathView() {
  const name = useLastPathSegment();

  return (
    <div className="space-y-6">
      <header className="space-y-3">
        <Breadcrumbs
          items={[
            { label: 'Home', href: '/' },
            { label: 'Sources', href: '/sources' },
            { label: name || 'Source' },
          ]}
        />
        <h1 className="text-3xl font-semibold tracking-tight break-all">
          {name || 'Loading…'}
        </h1>
        <p className="text-ink-muted text-xs">
          Rendered live from the API (outside the build-time pre-render).
        </p>
      </header>
      {name && (
        <>
          <SourceHealthPanel source={name} />
          <SourceStatsPanel source={name} />
          <SourceTopChart source={name} sourceName={name} />
        </>
      )}
    </div>
  );
}
