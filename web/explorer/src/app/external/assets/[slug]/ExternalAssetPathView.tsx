'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { apiGetData } from '@/api/client';
import { Callout, Container, PageHeader, Skeleton } from '@/components/ui';
import { useLastPathSegment } from '@/lib/useLastPathSegment';
import { type GlobalAssetView } from '../../../assets/catalogue';

import { ExternalAssetDetailView } from './ExternalAssetDetailView';

/**
 * ExternalAssetPathView — the runtime fallback for /external/assets/[slug]
 * outside the build-time pre-render, served by
 * functions/external/assets/[[path]].js. Reads the slug from the
 * URL and renders the same detail view the build bakes.
 */
export function ExternalAssetPathView() {
  const slug = useLastPathSegment();
  const q = useQuery<GlobalAssetView>({
    queryKey: ['/v1/external/assets', slug],
    queryFn: () =>
      apiGetData<GlobalAssetView>(
        `/v1/external/assets/${encodeURIComponent(slug)}`,
      ),
    enabled: slug !== '',
    retry: false,
  });

  if (q.data) return <ExternalAssetDetailView view={q.data} />;

  // apiGet's error message leads with the HTTP status. Only an
  // authoritative 4xx may say "not tracked"; anything else is unknown.
  const notTracked = /^4\d\d\b/.test(q.error?.message ?? '');

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'External assets', href: '/external/assets' },
          { label: slug || 'Asset' },
        ]}
        title={<span className="break-all">{slug || 'Loading…'}</span>}
      />
      {!q.isError && <Skeleton className="h-32 w-full" />}
      {q.isError && (
        <Callout
          tone={notTracked ? 'warn' : 'info'}
          title={
            notTracked ? 'External asset not found' : 'Asset detail unavailable'
          }
        >
          <p>
            {notTracked ? (
              <>
                We don&apos;t track an external (non-Stellar) asset with the
                slug <code className="font-mono">{slug}</code>. Stellar-issued
                assets live on{' '}
                <Link href="/assets" className="font-medium underline">
                  /assets
                </Link>
                .
              </>
            ) : (
              <>
                We couldn&apos;t read <code className="font-mono">{slug}</code>{' '}
                from the API just now — that means unknown, not untracked.
              </>
            )}
          </p>
        </Callout>
      )}
    </Container>
  );
}
