'use client';

import { EmptyState, PageHeader } from '@/components/ui';
import { SourceStatsPanel } from '@/app/dexes/[source]/SourceStatsPanel';
import { SourceTopChart } from '@/app/dexes/[source]/SourceTopChart';
import { useSources } from '@/api/hooks';
import { CURRENT_NETWORK } from '@/lib/networks';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { sourceLabel, sourceUnitsLabel } from '../sourceLabel';
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
  const row = sources.data?.find((s) => s.name === name);
  const selectable = row?.selectable === true;
  const label = name ? sourceLabel(name) : '';

  return (
    <div className="space-y-6">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Sources', href: '/sources' },
          { label: label || 'Source' },
        ]}
        title={<span className="break-all">{label || 'Loading…'}</span>}
        meta={
          label !== name ? <span className="font-mono">{name}</span> : undefined
        }
      />
      {name && sources.data && !row && (
        <EmptyState
          headingLevel={2}
          title="Not a source on this network"
          description={`${CURRENT_NETWORK.label} does not run ${name}, so it has no health or market data here.`}
        />
      )}
      {/* Panels wait for the registry: their endpoints 404 for a name it lacks. */}
      {name && row && (
        <>
          <SourceHealthPanel source={name} />
          <SourceStatsPanel
            source={name}
            unitsLabel={
              row ? sourceUnitsLabel(row.class, row.subclass) : 'markets'
            }
          />
          {selectable && <SourceTopChart source={name} sourceName={label} />}
        </>
      )}
    </div>
  );
}
