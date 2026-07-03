'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { Breadcrumbs, EmptyState, Skeleton } from '@/components/ui';

import { useLastPathSegment } from '@/lib/useLastPathSegment';
import { CopyHash, type Envelope } from '@/app/explorer-shared';

const G_RE = /^G[A-Z2-7]{55}$/;

interface IssuerAssetRow {
  asset_id?: string;
  code?: string;
  observation_count?: number;
}

interface IssuerDetail {
  g_strkey: string;
  home_domain?: string | null;
  org_name?: string | null;
  org_verified?: boolean;
  scam_reason?: string;
  sep1_resolved_at?: string | null;
  assets?: IssuerAssetRow[];
}

/**
 * IssuerPathView — the runtime fallback for issuers beyond the
 * pre-rendered top-100 (site audit S-022: search and asset pages link
 * to arbitrary issuers, which hard-404'd because /issuers had no CF
 * function shell, unlike accounts/contracts/ledgers/transactions).
 * Served by functions/issuers/[[path]].js; noindex, like the other
 * long-tail shells.
 */
export function IssuerPathView() {
  const g = (useLastPathSegment() ?? '').toUpperCase();
  const valid = G_RE.test(g);

  const { data, isLoading, isError } = useQuery<IssuerDetail>({
    queryKey: ['/v1/issuers/{g}', g],
    enabled: valid,
    retry: false,
    staleTime: 300_000,
    queryFn: async () =>
      (await apiGet<Envelope<IssuerDetail>>(`/v1/issuers/${encodeURIComponent(g)}`)).data,
  });

  const title = data?.org_name || (valid ? `${g.slice(0, 8)}…${g.slice(-4)}` : 'Issuer');

  return (
    <div className="mx-auto max-w-7xl space-y-6 px-6 py-8">
      <header className="space-y-1">
        <Breadcrumbs
          items={[
            { label: 'Home', href: '/' },
            { label: 'Issuers', href: '/issuers' },
            { label: valid ? `${g.slice(0, 8)}…${g.slice(-4)}` : 'Issuer' },
          ]}
        />
        <h1 className="text-2xl font-semibold tracking-tight text-ink">{title}</h1>
      </header>

      {!valid && (
        <Panel title="Invalid issuer" bodyClassName="text-sm text-ink-body">
          The path segment isn&apos;t a valid Stellar account (G…) key.{' '}
          <Link href="/issuers" className="text-brand-600 hover:underline">
            Browse issuers →
          </Link>
        </Panel>
      )}

      {valid && (
        <Panel
          title="Issuer"
          source={asExample(`/v1/issuers/${g}`)}
          bodyClassName="space-y-3"
        >
          {isLoading && <Skeleton className="h-24 w-full" />}
          {isError && (
            <EmptyState
              title="Issuer not observed"
              description="This account hasn't issued a classic asset that's appeared in any trade or ChangeTrust operation the indexer has seen."
            />
          )}
          {data && (
            <>
              <div className="flex flex-wrap items-center gap-2 text-xs">
                {data.org_verified && (
                  <span className="rounded-sm bg-up-subtle px-1.5 py-0.5 font-medium uppercase tracking-wider text-up-strong">
                    ✓ Verified
                  </span>
                )}
                {data.scam_reason && (
                  <span
                    title={data.scam_reason}
                    className="rounded-sm bg-down-subtle px-1.5 py-0.5 font-medium uppercase tracking-wider text-down"
                  >
                    {/^deprecated/i.test(data.scam_reason) ? 'DEPRECATED' : 'FLAGGED'}
                  </span>
                )}
                {data.home_domain && (
                  <span className="text-ink-muted">{data.home_domain}</span>
                )}
              </div>
              <div>
                <div className="text-[11px] uppercase tracking-wider text-ink-muted">
                  Account
                </div>
                <div className="mt-0.5 flex flex-wrap items-center gap-3">
                  <CopyHash value={data.g_strkey} head={12} tail={12} />
                  <Link
                    href={`/accounts/${data.g_strkey}`}
                    className="text-xs text-brand-600 hover:underline"
                  >
                    Account view →
                  </Link>
                </div>
              </div>
              <div>
                <div className="text-[11px] uppercase tracking-wider text-ink-muted">
                  Issued assets
                </div>
                {(data.assets ?? []).length === 0 ? (
                  <p className="mt-1 text-sm text-ink-muted">
                    No observed classic assets.
                  </p>
                ) : (
                  <ul className="mt-1 space-y-1 text-sm">
                    {(data.assets ?? []).map((a) => (
                      <li key={a.asset_id ?? a.code}>
                        <Link
                          href={`/assets/${encodeURIComponent(a.asset_id ?? a.code ?? '')}`}
                          className="font-medium text-brand-600 hover:underline"
                        >
                          {a.code ?? a.asset_id}
                        </Link>
                        {typeof a.observation_count === 'number' && (
                          <span className="ml-2 font-mono text-xs tabular-nums text-ink-muted">
                            {a.observation_count.toLocaleString()} observations
                          </span>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </>
          )}
        </Panel>
      )}
    </div>
  );
}
