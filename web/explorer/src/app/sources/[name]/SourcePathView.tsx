'use client';

import { PageHeader } from '@/components/ui';
import { SourceStatsPanel } from '@/app/dexes/[source]/SourceStatsPanel';
import { SourceTopChart } from '@/app/dexes/[source]/SourceTopChart';
import { useSources } from '@/api/hooks';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { SourceHealthPanel } from './SourceHealthPanel';

/**
 * SourcePathView — the runtime fallback for /sources/[name] outside the
 * build-time pre-render, served by functions/sources/[[path]].js.
 * The name is read from the URL; every panel below already fetches its
 * own data client-side, so the page renders live without a rebuild.
 */
export function SourcePathView() {
  const name = useLastPathSegment();
  const sources = useSources();
  // The top-pair chart selects by source, which the API refuses for data
  // vendors; wait for the registry's `selectable` flag rather than guess.
  const selectable =
    sources.data?.find((s) => s.name === name)?.selectable === true;

  return (
    <div className="space-y-6">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Sources', href: '/sources' },
          { label: name || 'Source' },
        ]}
        title={<span className="break-all">{name || 'Loading…'}</span>}
        description="Rendered live from the API (outside the build-time pre-render)."
      />
      {name && (
        <>
          <SourceHealthPanel source={name} />
          <SourceStatsPanel source={name} />
          {selectable && <SourceTopChart source={name} sourceName={name} />}
        </>
      )}
    </div>
  );
}
