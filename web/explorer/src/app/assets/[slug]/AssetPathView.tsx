'use client';

import { useEffect } from 'react';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { Breadcrumbs, EmptyState, Skeleton } from '@/components/ui';
import { useLastPathSegment } from '@/lib/useLastPathSegment';
import type { Envelope } from '@/app/explorer-shared';

import { LiveAssetPrice, type PriceProvenance } from './LiveAssetPrice';
import { formatCompact } from '@/lib/format';

interface AssetShellDetail {
  asset_id?: string;
  type?: string;
  code?: string;
  issuer?: string | null;
  home_domain?: string | null;
  decimals?: number;
  volume_24h_usd?: string | null;
  /** Fixed-precision decimal string (ADR-0003), or null when undrivable. */
  price_usd?: string | null;
  /** Absent = direct market. See the OpenAPI enum for the other values. */
  price_basis?: 'declared_peg' | 'transitive';
  unverified_ticker_collision?: boolean;
  unverified_warning?: { note?: string } | null;
}

/**
 * Parse the wire's decimal STRING into the number LiveAssetPrice takes.
 *
 * ADR-0003 keeps money on the wire as an exact-rational string, and the
 * transitive path can return ~55 significant digits — far past what a
 * double holds. That is fine HERE and only here: this value is the
 * headline display figure, which is rounded for the reader anyway. It
 * must never be fed back into arithmetic that settles anything.
 *
 * Returns null for absent/empty/unparseable/non-positive, so a bad
 * value degrades to "no price" rather than rendering NaN or $0.
 */
function priceNumber(raw: string | null | undefined): number | null {
  if (raw == null || raw === '') return null;
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? n : null;
}

/**
 * Map the wire's `price_basis` to the caption vocabulary.
 *
 * Absent basis means a DIRECT market observation, which is 'vwap1m'.
 * An unrecognised basis (a value this build predates) maps to null
 * rather than guessing: a wrong caption states a provenance the price
 * does not have, which is worse than no caption at all.
 */
function provenanceFromBasis(
  basis: AssetShellDetail['price_basis'],
): PriceProvenance {
  switch (basis) {
    case undefined:
      return 'vwap1m';
    case 'declared_peg':
      return 'declared_peg';
    case 'transitive':
      return 'transitive';
    default:
      return null;
  }
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
    queryFn: () =>
      apiGet<Envelope<AssetShellDetail>>(
        `/v1/assets/${encodeURIComponent(slug!)}`,
      ),
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
        {/* Seed from the detail response we ALREADY hold. Passing null
            here threw away a price the page had in hand: /v1/price
            answers for DIRECT markets only, so every two-hop asset —
            the exact population transitive pricing was built for —
            rendered a permanent "—" while /v1/assets was serving a
            real, substance-gated figure. Measured on CAUP7:
            /v1/price → price:null, /v1/assets → 7768.93 basis
            "transitive". usePricePoll keeps a seeded value when the
            poll yields nothing usable, so this cannot regress the
            assets that DO have a direct market. */}
        <LiveAssetPrice
          assetID={d.asset_id}
          initialPrice={priceNumber(d.price_usd)}
          initialProvenance={provenanceFromBasis(d.price_basis)}
        />
        {d.unverified_warning?.note && (
          <p className="border-warn-300 bg-warn-50 text-warn-800 rounded-md border p-3 text-sm">
            {d.unverified_warning.note}
          </p>
        )}
        <dl className="grid grid-cols-1 gap-2 text-sm sm:grid-cols-2">
          <div>
            <dt className="text-ink-muted">Canonical asset id</dt>
            <dd className="font-mono break-all">{d.asset_id}</dd>
          </div>
          {d.issuer && (
            <div>
              <dt className="text-ink-muted">Issuer</dt>
              <dd className="font-mono break-all">
                <Link
                  className="underline decoration-dotted"
                  href={`/issuers/${d.issuer}`}
                >
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
              <dd className="font-mono">
                ${formatCompact(Number(d.volume_24h_usd))}
              </dd>
            </div>
          )}
        </dl>
      </Panel>
    </div>
  );
}
