import type { Metadata } from 'next';
import { Suspense } from 'react';

import { IssuerPathView } from './IssuerPathView';
import {
  IssuerDetailView,
  issuerShortKey,
  type CoinPrices,
} from './IssuerDetailView';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { Container, PageHeader } from '@/components/ui';
import type { AssetDetail, Issuer } from '@/api/hooks';
import { buildFetchData, failBuild, requireRows } from '@/lib/buildFetch';
import { ogImageFor, shellMetadata } from '@/lib/seo';
import { CURRENT_NETWORK, USDC_ASSET_ID } from '@/lib/networks';

/**
 * /issuers/[g_strkey] — single-issuer detail page.
 *
 * Server component fetches /v1/issuers/{g_strkey} at build time.
 * Static export with output:'export' needs a non-empty
 * generateStaticParams; we pre-render the top issuers (up to 100) so
 * deep links resolve without a build-time round trip per route.
 */

type Params = Promise<{ g_strkey: string }>;

export async function generateStaticParams() {
  // 'shell' is the runtime-fallback sentinel: functions/issuers/[[path]].js
  // serves its HTML for any issuer beyond the pre-rendered top-100
  // (they would otherwise hard-404 while search + asset pages link
  // to them). Same pattern as accounts/contracts/ledgers/transactions.
  const fallback = [{ g_strkey: USDC_ASSET_ID.split('-')[1] }];
  // Fail-hard (src/lib/buildFetch.ts): an unreachable or empty issuers
  // listing throws so the build fails instead of exporting only the
  // fallback route. CI-stub builds fall through to the fallback.
  const rows = requireRows(
    await buildFetchData<{ g_strkey: string }[]>('/v1/issuers?limit=100'),
    '/v1/issuers listing for /issuers/[g_strkey] static params',
  );
  const keys = rows.map((i) => i.g_strkey).filter(Boolean);
  const params =
    keys.length > 0 ? keys.map((g_strkey) => ({ g_strkey })) : fallback;
  return [{ g_strkey: 'shell' }, ...params];
}

function fetchIssuer(gStrkey: string): Promise<Issuer | null> {
  // 20s budget: issuer detail is the slowest per-entity endpoint of
  // the export (~4s steady-state; >8s under the build's 9-worker
  // concurrency). A short timeout would bake "Issuer not found".
  return buildFetchData<Issuer>(`/v1/issuers/${encodeURIComponent(gStrkey)}`, {
    timeoutMs: 20_000,
  });
}

async function fetchIssuerCoins(gStrkey: string): Promise<CoinPrices> {
  // An empty list is legitimate (issuer with no priced assets);
  // transport failure throws via buildFetch.
  const rows = await buildFetchData<AssetDetail[]>(
    `/v1/assets?issuer=${encodeURIComponent(gStrkey)}&limit=500`,
    { timeoutMs: 20_000 },
  );
  const out: CoinPrices = {};
  for (const c of rows ?? []) out[c.asset_id] = c;
  return out;
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { g_strkey } = await params;
  // The runtime-fallback shell's HTML serves for arbitrary long-tail
  // issuers, so it carries only generic noindex metadata.
  if (g_strkey === 'shell') {
    return shellMetadata(
      'Issuer',
      'Identity, auth flags, and issued assets for a Stellar issuer.',
    );
  }
  const short = issuerShortKey(g_strkey);
  const canonical = `${CURRENT_NETWORK.explorerUrl}/issuers/${g_strkey}`;
  const title = `Issuer ${short} — Stellar`;
  const description = `Identity, auth flags, and issued assets for Stellar issuer ${short}.`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: {
      title,
      description,
      url: canonical,
      type: 'website',
      images: [ogImageFor('issuers', g_strkey)],
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      images: [ogImageFor('issuers', g_strkey)],
    },
  };
}

export default async function IssuerDetailPage({ params }: { params: Params }) {
  const { g_strkey } = await params;
  if (g_strkey === 'shell') {
    return (
      <Suspense fallback={null}>
        <IssuerPathView />
      </Suspense>
    );
  }

  // The detail and the per-asset price join share no data, so they
  // run in parallel.
  const [detail, coins] = await Promise.all([
    fetchIssuer(g_strkey),
    fetchIssuerCoins(g_strkey),
  ]);

  if (!detail) {
    // Real build: this g_strkey was promised by generateStaticParams —
    // a missing detail row means the API contradicted its own listing.
    // Fail the build rather than bake "Issuer not found" for a real
    // issuer (fail-hard contract, src/lib/buildFetch.ts). The panel
    // below renders only on CI-stub builds.
    failBuild(
      `/issuers/${g_strkey}: promised by generateStaticParams but /v1/issuers returned no row`,
    );
    return (
      <Container className="space-y-6 py-8">
        <PageHeader
          breadcrumbs={[
            { label: 'Home', href: '/' },
            { label: 'Issuers', href: '/issuers' },
            { label: issuerShortKey(g_strkey) },
          ]}
          title="Issuer"
          meta={<span className="font-mono break-all">{g_strkey}</span>}
        />
        <Panel
          headingLevel={2}
          title="Issuer not found"
          bodyClassName="text-sm text-ink-body"
        >
          <p>
            No issuer record for this account. It has not issued a classic asset
            seen in any trade or trustline change the index has captured.{' '}
            <Link href="/issuers" className="text-brand-600 hover:underline">
              Browse issuers
            </Link>
          </p>
        </Panel>
      </Container>
    );
  }

  return <IssuerDetailView gStrkey={g_strkey} detail={detail} coins={coins} />;
}
