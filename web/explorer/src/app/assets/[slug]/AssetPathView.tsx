'use client';

import { useEffect } from 'react';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { Breadcrumbs, EmptyState, Skeleton } from '@/components/ui';
import { useLastPathSegment } from '@/lib/useLastPathSegment';
import type { Envelope } from '@/app/explorer-shared';

import { LiveAssetPrice } from './LiveAssetPrice';

interface AssetShellDetail {
  asset_id?: string;
  type?: string;
  code?: string;
  issuer?: string | null;
  home_domain?: string | null;
  decimals?: number;
  volume_24h_usd?: string | null;
  unverified_ticker_collision?: boolean;
  unverified_warning?: { note?: string } | null;
}

/**
 * AssetPathView — the runtime fallback for asset slugs outside the
 * build-time pre-render (same S1b pattern as /markets' PairPathView).
 *
 * /assets/[slug] pre-renders the top-500 listing + verified catalogue
 * at BUILD time; every other classic asset (194k of them, each with a
 * unique migration-0134 slug the API now resolves) hard-404'd on the
 * static host. functions/assets/[[path]].js serves this shell for any
 * unmatched /assets/* path; the view reads the slug from the URL and
 * hydrates from /v1/assets/{slug} — which accepts catalogue slugs,
 * canonical ids, AND classic slugs, so every spelling works here.
 */
export function AssetPathView() {
  const slug = useLastPathSegment();

  const detail = useQuery({
    queryKey: ['asset-shell', slug],
    enabled: Boolean(slug),
    queryFn: () => apiGet<Envelope<AssetShellDetail>>(`/v1/assets/${encodeURIComponent(slug!)}`),
    retry: false,
  });

  // Restamp the shell's generic baked <title> once the real asset
  // loads — the same HTML serves every long-tail asset URL. Effect,
  // not render-body mutation (react-compiler rule).
  const loadedCode = detail.data?.data?.code;
  useEffect(() => {
    if (loadedCode) {
      document.title = `${loadedCode} — Stellar asset · Stellar Index`;
    }
  }, [loadedCode]);

  if (!slug || detail.isLoading) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  const d = detail.data?.data;
  if (detail.isError || !d?.asset_id) {
    return (
      <EmptyState
        title="Asset not found"
        description={`No Stellar asset matches “${slug}” — not as a canonical id, a verified-currency slug, or a classic asset slug.`}
      />
    );
  }

  return (
    <div className="space-y-4">
      <Breadcrumbs
        items={[
          { label: 'Assets', href: '/assets' },
          { label: d.code ?? slug },
        ]}
      />
      <Panel
        title={`${d.code ?? slug} — asset`}
        hint="rendered live from the API (outside the build-time pre-render)"
        source={asExample('/v1/assets/{id}', { id: slug })}
        bodyClassName="space-y-3"
      >
        <LiveAssetPrice
          assetID={d.asset_id}
          initialPrice={null}
          initialProvenance={null}
        />
        {d.unverified_warning?.note && (
          <p className="rounded-md border border-warn-300 bg-warn-50 p-3 text-sm text-warn-800">
            {d.unverified_warning.note}
          </p>
        )}
        <dl className="grid grid-cols-1 gap-2 text-sm sm:grid-cols-2">
          <div>
            <dt className="text-ink-muted">Canonical asset id</dt>
            <dd className="break-all font-mono">{d.asset_id}</dd>
          </div>
          {d.issuer && (
            <div>
              <dt className="text-ink-muted">Issuer</dt>
              <dd className="break-all font-mono">
                <Link className="underline decoration-dotted" href={`/issuers/${d.issuer}`}>
                  {d.issuer}
                </Link>
              </dd>
            </div>
          )}
          {d.home_domain && (
            <div>
              <dt className="text-ink-muted">Home domain</dt>
              <dd className="font-mono">{d.home_domain}</dd>
            </div>
          )}
          {d.volume_24h_usd && (
            <div>
              <dt className="text-ink-muted">Volume (24h)</dt>
              <dd className="font-mono">${Number(d.volume_24h_usd).toLocaleString('en-US')}</dd>
            </div>
          )}
        </dl>
      </Panel>
    </div>
  );
}
