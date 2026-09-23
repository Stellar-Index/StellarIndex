import type { Metadata } from 'next';
import Link from 'next/link';
import { Suspense } from 'react';

import { API_BASE_URL } from '@/api/client';
import { ogImageFor } from '@/lib/seo';
import { Breadcrumbs, Callout, Container } from '@/components/ui';
import { type GlobalAssetView } from '../../../assets/catalogue';
import { isCIStub } from '@/lib/buildFetch';
import { CURRENT_NETWORK } from '@/lib/networks';

import { ExternalAssetDetailView } from './ExternalAssetDetailView';
import { ExternalAssetPathView } from './ExternalAssetPathView';

/**
 * /external/assets/[slug] — detail page for a NON-Stellar reference
 * asset (fiat currency or reference-only coin like BTC / ETH). The
 * Stellar-side counterpart lives at /assets/[slug]; this is the
 * external half of the LC-001 Stellar/external split.
 *
 * Backed by GET /v1/external/assets/{slug} (envelope `{data:
 * GlobalAssetView}`). It's a LEAN page: it renders only the
 * cross-chain headline (name / ticker / class + USD price +
 * authority) — none of the Stellar-specific tabs (issuer / holders /
 * liquidity / supply / SEP-1 / markets), which don't apply to an
 * asset that isn't issued on Stellar.
 */

// Static export hits every page once at build time. CI's stub
// hostname doesn't resolve, so bypass the network entirely when the
// URL looks like a CI placeholder (mirrors the Stellar page).

// 8s per fetch, matching the Stellar detail page's build budget.
const BUILD_FETCH_TIMEOUT_MS = 8_000;

// `shell` backs functions/external/assets/[[path]].js for a slug added
// after this build (T291) — see ExternalAssetPathView. Every return path
// carries it: the Function needs the document on CI-stub builds too.
const SHELL = { slug: 'shell' };
const isShell = (slug: string) => slug.toLowerCase() === 'shell';

export async function generateStaticParams() {
  // Build-time fetch of the external listing so every fiat / reference
  // coin gets a pre-rendered route. A single canonical fallback keeps
  // the static export buildable when the API is unreachable (CI) —
  // Next refuses a dynamic route under output:'export' with zero
  // params. `btc` is a real reference-only catalogue slug, so the
  // fallback page renders real content rather than a stub.
  const fallback = [{ slug: 'btc' }, SHELL];
  if (isCIStub) return fallback;
  try {
    const res = await fetch(`${API_BASE_URL}/v1/external/assets?limit=500`, {
      signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS * 2),
    });
    if (!res.ok) return fallback;
    const env = (await res.json()) as { data?: { slug?: string }[] };
    const rows = env.data ?? [];
    const seen = new Set<string>();
    const out: { slug: string }[] = [];
    for (const row of rows) {
      if (row.slug && !seen.has(row.slug)) {
        seen.add(row.slug);
        out.push({ slug: row.slug });
      }
    }
    return out.length > 0 ? [...out, SHELL] : fallback;
  } catch {
    return fallback;
  }
}

type Params = Promise<{ slug: string }>;

/**
 * The three outcomes of the build-time read, kept apart on purpose.
 *
 * Pre-fix all three collapsed into `null` and the page rendered "We
 * don't track an external asset with the slug X" — a flat denial baked
 * into the static export for a REAL asset whenever the API 5xx'd or the
 * fetch timed out (generateStaticParams had already promised the slug
 * exists). Only an authoritative 4xx may produce that claim.
 */
type ExternalAssetResult =
  | { status: 'ok'; view: GlobalAssetView }
  | { status: 'not-tracked' }
  | { status: 'unavailable' };

// fetchExternalAsset reads GET /v1/external/assets/{slug} at build time.
async function fetchExternalAsset(slug: string): Promise<ExternalAssetResult> {
  if (isCIStub) return { status: 'unavailable' };
  try {
    const res = await fetch(
      `${API_BASE_URL}/v1/external/assets/${encodeURIComponent(slug)}`,
      { signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS) },
    );
    // 4xx is the API answering authoritatively that it doesn't track
    // this slug; anything else (5xx, non-JSON, no `data`) is us failing
    // to get an answer.
    if (res.status >= 400 && res.status < 500) return { status: 'not-tracked' };
    if (!res.ok) return { status: 'unavailable' };
    const env = (await res.json()) as { data?: GlobalAssetView };
    return env.data
      ? { status: 'ok', view: env.data }
      : { status: 'unavailable' };
  } catch {
    return { status: 'unavailable' };
  }
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { slug } = await params;
  // One baked document answers every unlisted slug: generic, noindex, and
  // an explicit empty `alternates` so the root layout's canonical is
  // cleared rather than inherited (F095).
  if (isShell(slug)) {
    return {
      title: 'External asset',
      description:
        'Non-Stellar reference asset detail, rendered live from the Stellar Index API.',
      robots: { index: false, follow: true },
      alternates: {},
    };
  }
  const res = await fetchExternalAsset(slug);
  const view = res.status === 'ok' ? res.view : null;
  const name = view?.name ?? view?.ticker ?? slug;
  const canonical = `${CURRENT_NETWORK.explorerUrl}/external/assets/${view?.slug ?? slug}`;
  const description = `${name} is a non-Stellar reference asset tracked by Stellar Index for pricing (it isn't issued on Stellar). Live USD price and source authority.`;
  const title = `${name} price — external asset`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: {
      title,
      description,
      url: canonical,
      type: 'website',
      images: [ogImageFor('assets', view?.slug ?? slug)],
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      images: [ogImageFor('assets', view?.slug ?? slug)],
    },
  };
}

export default async function ExternalAssetDetailPage({
  params,
}: {
  params: Params;
}) {
  const { slug } = await params;
  if (isShell(slug)) {
    return (
      <Suspense fallback={null}>
        <ExternalAssetPathView />
      </Suspense>
    );
  }
  const result = await fetchExternalAsset(slug);

  if (result.status !== 'ok') {
    return (
      <Container className="space-y-8 py-8 sm:py-10">
        <header className="space-y-3">
          <Breadcrumbs
            items={[
              { label: 'Home', href: '/' },
              { label: 'External assets', href: '/external/assets' },
              { label: slug },
            ]}
          />
          <h1 className="text-h1 text-ink font-semibold">{slug}</h1>
        </header>
        {result.status === 'not-tracked' ? (
          <Callout tone="warn" title="External asset not found">
            <p>
              We don&apos;t track an external (non-Stellar) asset with the slug{' '}
              <code className="font-mono">{slug}</code>. If this is a
              Stellar-issued asset, its detail lives on{' '}
              <Link href="/assets" className="font-medium underline">
                /assets
              </Link>
              .
            </p>
            <p className="mt-2">
              <Link
                href="/external/assets"
                className="text-brand-600 hover:text-brand-700 font-medium"
              >
                ← External assets
              </Link>
            </p>
          </Callout>
        ) : (
          <Callout tone="info" title="Asset detail unavailable">
            <p>
              We couldn&apos;t read <code className="font-mono">{slug}</code>{' '}
              when this page was built — that means unknown, not untracked. It
              refreshes on the next build.
            </p>
            <p className="mt-2">
              <Link
                href="/external/assets"
                className="text-brand-600 hover:text-brand-700 font-medium"
              >
                ← External assets
              </Link>
            </p>
          </Callout>
        )}
      </Container>
    );
  }

  return <ExternalAssetDetailView view={result.view} />;
}
