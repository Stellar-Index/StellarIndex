'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { Container, Breadcrumbs, EmptyState, Skeleton } from '@/components/ui';

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
      (
        await apiGet<Envelope<IssuerDetail>>(
          `/v1/issuers/${encodeURIComponent(g)}`,
        )
      ).data,
  });

  const title =
    data?.org_name || (valid ? `${g.slice(0, 8)}…${g.slice(-4)}` : 'Issuer');

  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-1">
        <Breadcrumbs
          items={[
            { label: 'Home', href: '/' },
            { label: 'Issuers', href: '/issuers' },
            { label: valid ? `${g.slice(0, 8)}…${g.slice(-4)}` : 'Issuer' },
          ]}
        />
        <h1 className="text-ink text-2xl font-semibold tracking-tight">
          {title}
        </h1>
      </header>

      {!valid && (
        <Panel
          headingLevel={2}
          title="Invalid issuer"
          bodyClassName="text-sm text-ink-body"
        >
          The path segment isn&apos;t a valid Stellar account (G…) key.{' '}
          <Link href="/issuers" className="text-brand-600 hover:underline">
            Browse issuers →
          </Link>
        </Panel>
      )}

      {valid && (
        <Panel
          headingLevel={2}
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
                  <span className="bg-up-subtle text-up-strong rounded-sm px-1.5 py-0.5 font-medium tracking-wider uppercase">
                    ✓ Verified
                  </span>
                )}
                {data.scam_reason && (
                  <span
                    title={data.scam_reason}
                    className="bg-down-subtle text-down rounded-sm px-1.5 py-0.5 font-medium tracking-wider uppercase"
                  >
                    {/^deprecated/i.test(data.scam_reason)
                      ? 'DEPRECATED'
                      : 'FLAGGED'}
                  </span>
                )}
                {data.home_domain && (
                  <span className="text-ink-muted">{data.home_domain}</span>
                )}
              </div>
              <div>
                <div className="text-ink-muted text-[11px] tracking-wider uppercase">
                  Account
                </div>
                <div className="mt-0.5 flex flex-wrap items-center gap-3">
                  <CopyHash value={data.g_strkey} head={12} tail={12} />
                  <Link
                    href={`/accounts/${data.g_strkey}`}
                    className="text-brand-600 text-xs hover:underline"
                  >
                    Account view →
                  </Link>
                </div>
              </div>
              <div>
                <div className="text-ink-muted text-[11px] tracking-wider uppercase">
                  Issued assets
                </div>
                {/* `assets` is soft-failed to nil server-side on an
                    errored/deadline-exceeded per-asset read
                    (internal/api/v1/issuers.go) — absent is unknown,
                    not "this issuer has issued nothing". */}
                {!data.assets ? (
                  <p className="text-ink-muted mt-1 text-sm">
                    Issued-asset list unavailable — retry shortly.
                  </p>
                ) : data.assets.length === 0 ? (
                  <p className="text-ink-muted mt-1 text-sm">
                    No observed classic assets.
                  </p>
                ) : (
                  <ul className="mt-1 space-y-1 text-sm">
                    {(data.assets ?? []).map((a) => (
                      <li key={a.asset_id ?? a.code}>
                        <Link
                          href={`/assets/${encodeURIComponent(a.asset_id ?? a.code ?? '')}`}
                          className="text-brand-600 font-medium hover:underline"
                        >
                          {a.code ?? a.asset_id}
                        </Link>
                        {typeof a.observation_count === 'number' && (
                          <span className="text-ink-muted ml-2 font-mono text-xs tabular-nums">
                            {a.observation_count.toLocaleString('en-US')}{' '}
                            observations
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
    </Container>
  );
}
