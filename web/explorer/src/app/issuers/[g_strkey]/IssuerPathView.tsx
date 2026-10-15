'use client';

import { useQuery } from '@tanstack/react-query';

import { apiGet, type Envelope } from '@/api/client';
import type { AssetDetail, Issuer } from '@/api/hooks';
import {
  Button,
  ButtonLink,
  Container,
  EmptyState,
  PageHeader,
  Skeleton,
} from '@/components/ui';
import { errorStatus } from '@/lib/error-status';
import { CURRENT_NETWORK } from '@/lib/networks';
import { useLastPathSegment } from '@/lib/useLastPathSegment';

import {
  IssuerDetailView,
  issuerShortKey,
  type CoinPrices,
} from './IssuerDetailView';

const G_RE = /^G[A-Z2-7]{55}$/;

const CRUMBS = [
  { label: 'Home', href: '/' },
  { label: 'Issuers', href: '/issuers' },
];

/**
 * IssuerPathView — the runtime fallback for issuers beyond the
 * pre-rendered top-100 (search and asset pages link to arbitrary
 * issuers). Served by functions/issuers/[[path]].js as a noindex shell;
 * it fetches the same two reads the static page does and renders the
 * same view.
 */
export function IssuerPathView() {
  const g = (useLastPathSegment() ?? '').toUpperCase();
  const valid = G_RE.test(g);
  const detail = useQuery<Issuer>({
    queryKey: ['/v1/issuers/{g}', g],
    enabled: valid,
    retry: false,
    staleTime: 300_000,
    queryFn: async () =>
      (await apiGet<Envelope<Issuer>>(`/v1/issuers/${encodeURIComponent(g)}`))
        .data,
  });
  const coins = useQuery<CoinPrices>({
    queryKey: ['/v1/assets', g, 'by-issuer'],
    enabled: valid && !!CURRENT_NETWORK.pricing,
    retry: false,
    staleTime: 300_000,
    queryFn: async () => {
      const env = await apiGet<Envelope<AssetDetail[]>>('/v1/assets', {
        issuer: g,
        limit: 500,
      });
      const out: CoinPrices = {};
      for (const c of Array.isArray(env.data) ? env.data : []) {
        out[c.asset_id] = c;
      }
      return out;
    },
  });

  if (!valid) {
    return (
      <Container className="space-y-6 py-8">
        <PageHeader
          breadcrumbs={[...CRUMBS, { label: 'Issuer' }]}
          title="Issuer"
        />
        <EmptyState
          headingLevel={2}
          title="Not an account key"
          description="An issuer is addressed by its account key: 56 characters starting with G."
          action={
            <ButtonLink href="/issuers" variant="secondary" size="sm">
              Browse issuers
            </ButtonLink>
          }
        />
      </Container>
    );
  }

  if (detail.isError) {
    const notFound = errorStatus(detail.error) === 404;
    return (
      <Container className="space-y-6 py-8">
        <PageHeader
          breadcrumbs={[...CRUMBS, { label: issuerShortKey(g) }]}
          title="Issuer"
          meta={<span className="font-mono break-all">{g}</span>}
        />
        {notFound ? (
          <EmptyState
            headingLevel={2}
            title="Issuer not observed"
            description="This account has not issued a classic asset seen in any trade or trustline change the index has captured. Its account page may still exist."
            action={
              <ButtonLink
                href={`/accounts/${encodeURIComponent(g)}`}
                variant="secondary"
                size="sm"
              >
                View as account
              </ButtonLink>
            }
          />
        ) : (
          <EmptyState
            headingLevel={2}
            title="Issuer unavailable"
            description="The issuer read did not return, so whether this account issues assets is unknown right now. Retry, or open its account page."
            action={
              <div className="flex gap-2">
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => detail.refetch()}
                >
                  Retry
                </Button>
                <ButtonLink
                  href={`/accounts/${encodeURIComponent(g)}`}
                  variant="ghost"
                  size="sm"
                >
                  View as account
                </ButtonLink>
              </div>
            }
          />
        )}
      </Container>
    );
  }

  if (!detail.data || (coins.isLoading && !coins.data)) {
    return (
      <Container className="space-y-6 py-8">
        <PageHeader
          breadcrumbs={[...CRUMBS, { label: issuerShortKey(g) }]}
          title="Issuer"
          meta={<span className="font-mono break-all">{g}</span>}
        />
        <Skeleton className="h-40 w-full" />
        <Skeleton className="h-64 w-full" />
      </Container>
    );
  }

  return (
    <IssuerDetailView
      gStrkey={g}
      detail={detail.data}
      coins={coins.data ?? {}}
      pricesUnavailable={coins.isError}
    />
  );
}
