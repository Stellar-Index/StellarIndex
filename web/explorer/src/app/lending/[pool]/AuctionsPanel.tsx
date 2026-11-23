'use client';

import { Fragment, useState } from 'react';
import Link from 'next/link';
import { useInfiniteQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { Button, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { paths } from '@/api/types';
import { formatRelative, truncateMiddle } from '@/lib/format';
import { amountText, auctionStatus } from '../lendingInsights';
import { usePoolReserves } from './usePoolReserves';

type Resp =
  paths['/lending/pools/{pool}/auctions']['get']['responses'][200]['content']['application/json'];
type Auction = NonNullable<Resp['data']>['auctions'][number];

interface Page {
  auctions: Auction[];
  truncated: boolean;
  next: string | undefined;
}

const LIMIT = 25;

export function AccountLink({ id }: { id: string }) {
  return (
    <Link
      href={`/${id.startsWith('C') ? 'contracts' : 'accounts'}/${encodeURIComponent(id)}/`}
      className="text-brand-600 hover:underline"
      title={id}
    >
      {truncateMiddle(id, 6, 6)}
    </Link>
  );
}

function TxLink({ hash }: { hash: string }) {
  return (
    <Link
      href={`/transactions/${hash}/`}
      className="text-brand-600 hover:underline"
      title={hash}
    >
      {truncateMiddle(hash, 8, 6)}
    </Link>
  );
}

function Amounts({
  map,
  decimalsOf,
}: {
  map: Record<string, string>;
  decimalsOf: (asset: string) => number | null | undefined;
}) {
  const entries = Object.entries(map);
  if (entries.length === 0) return <>—</>;
  return (
    <>
      {entries.map(([asset, raw]) => (
        <div key={asset} className="whitespace-nowrap">
          <span className="font-mono" title={`${raw} base units`}>
            {amountText(raw, decimalsOf(asset))}
          </span>{' '}
          <AssetLink canonical={asset} />
        </div>
      ))}
    </>
  );
}

function AuctionsList({ pool }: { pool: string }) {
  const reserves = usePoolReserves(pool);
  const decimals = new Map(
    (reserves.data?.reserves ?? []).map((r) => [r.asset, r.decimals]),
  );
  const [open, setOpen] = useState<string | null>(null);
  const q = useInfiniteQuery<Page, Error, { pages: Page[] }, unknown[], string>(
    {
      queryKey: ['/v1/lending/pools/{pool}/auctions', pool],
      retry: false,
      initialPageParam: '',
      queryFn: async ({ pageParam }) => {
        const env = await apiGet<Resp>(
          `/v1/lending/pools/${encodeURIComponent(pool)}/auctions`,
          { limit: LIMIT, cursor: pageParam || undefined },
        );
        return {
          truncated: env.data?.lifecycle_truncated === true,
          auctions: env.data?.auctions ?? [],
          next: env.pagination?.next ?? undefined,
        };
      },
      getNextPageParam: (last) => last.next,
      staleTime: 60_000,
    },
  );
  const rows = (q.data?.pages ?? []).flatMap((p) => p.auctions);

  return (
    <>
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading auctions…</p>
      )}
      {q.isError && (
        <p className="text-ink-muted text-sm">
          Auctions are unavailable right now.
          <Button
            variant="secondary"
            size="sm"
            className="ml-3"
            onClick={() => void q.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
      {q.data && rows.length === 0 && (
        <p className="text-ink-muted text-sm">
          No auctions recorded for this pool.
        </p>
      )}
      {(q.data?.pages ?? []).some((p) => p.truncated) && (
        <p className="text-ink-muted text-xs">
          The fill and delete history read hit its cap, so the newest auctions
          may show fewer fills, or no deletion, than really happened.
        </p>
      )}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>When</Th>
                <Th>Type</Th>
                <Th className="hidden sm:table-cell">Account</Th>
                <Th>Lot</Th>
                <Th>Bid</Th>
                <Th>Status</Th>
                <Th>
                  <span className="sr-only">Fills</span>
                </Th>
              </tr>
            </THead>
            <TBody>
              {rows.map((a) => {
                const key = `${a.tx_hash}-${a.auction_type}-${a.user}`;
                const status = auctionStatus(a);
                const fills = a.fills ?? [];
                return (
                  <Fragment key={key}>
                    <TR>
                      <Td className="whitespace-nowrap" title={a.start_time}>
                        {formatRelative(a.start_time)}
                        <div className="text-ink-muted text-xs">
                          <Link
                            href={`/ledgers/${a.start_ledger}/`}
                            className="hover:underline"
                          >
                            #{a.start_ledger}
                          </Link>
                        </div>
                      </Td>
                      <Td className="whitespace-nowrap">
                        {a.auction_type_label.replace(/_/g, ' ')}
                      </Td>
                      <Td className="hidden font-mono sm:table-cell">
                        <AccountLink id={a.user} />
                      </Td>
                      <Td className="text-xs">
                        <Amounts
                          map={a.lot}
                          decimalsOf={(x) => decimals.get(x)}
                        />
                      </Td>
                      <Td className="text-xs">
                        <Amounts
                          map={a.bid}
                          decimalsOf={(x) => decimals.get(x)}
                        />
                      </Td>
                      <Td
                        className="whitespace-nowrap"
                        title={a.deleted_time ?? undefined}
                      >
                        {status.label}
                        {status.kind === 'deleted' && status.percent > 0
                          ? ` (${status.percent}% filled)`
                          : ''}
                      </Td>
                      <Td align="right">
                        {fills.length > 0 && (
                          <Button
                            variant="secondary"
                            size="sm"
                            aria-expanded={open === key}
                            onClick={() => setOpen(open === key ? null : key)}
                          >
                            {open === key
                              ? 'Hide fills'
                              : `Fills (${fills.length})`}
                          </Button>
                        )}
                      </Td>
                    </TR>
                    {open === key && (
                      <TR>
                        <Td colSpan={7}>
                          <ul className="space-y-1 py-1 text-xs">
                            {fills.map((f) => (
                              <li key={`${f.tx_hash}-${f.filler}`}>
                                <AccountLink id={f.filler} /> filled{' '}
                                <span className="font-mono">
                                  {f.fill_percent}%
                                </span>{' '}
                                <span title={f.time}>
                                  {formatRelative(f.time)}
                                </span>{' '}
                                <span className="font-mono">
                                  <TxLink hash={f.tx_hash} />
                                </span>
                              </li>
                            ))}
                          </ul>
                        </Td>
                      </TR>
                    )}
                  </Fragment>
                );
              })}
            </TBody>
          </Table>
        </div>
      )}
      {q.hasNextPage && (
        <Button
          variant="secondary"
          size="sm"
          disabled={q.isFetchingNextPage}
          onClick={() => void q.fetchNextPage()}
        >
          {q.isFetchingNextPage ? 'Loading…' : 'Load more'}
        </Button>
      )}
    </>
  );
}

/** `total`/`lastSeen` come from the pools listing the header already read; null when unknown. */
export function AuctionsPanel({
  pool,
  total = null,
  lastSeen = null,
}: {
  pool: string;
  total?: number | null;
  lastSeen?: string | null;
}) {
  const [shown, setShown] = useState(false);
  if (total === 0)
    return (
      <Panel headingLevel={2} title="Liquidation auctions">
        <p className="text-ink-muted text-sm">
          No liquidation auction has run in this pool.
        </p>
      </Panel>
    );
  return (
    <Panel
      headingLevel={2}
      title="Liquidation auctions"
      hint="When a borrower's position is too risky, or the pool has bad debt, the pool auctions off collateral (the lot) in exchange for debt repaid (the bid). Newest first."
      source={asExample(`/v1/lending/pools/${pool}/auctions`, {})}
      bodyClassName="space-y-3"
    >
      {shown ? (
        <AuctionsList pool={pool} />
      ) : (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          {total != null && (
            <p className="text-ink-body text-sm">
              <span className="text-ink font-mono text-lg tabular-nums">
                {total.toLocaleString('en-US')}
              </span>{' '}
              auction{total === 1 ? '' : 's'}
              {lastSeen && (
                <span className="text-ink-muted">
                  {' '}
                  · last {formatRelative(lastSeen)}
                </span>
              )}
            </p>
          )}
          <Button variant="secondary" size="sm" onClick={() => setShown(true)}>
            {total != null ? 'List them' : 'Show auctions'}
          </Button>
        </div>
      )}
    </Panel>
  );
}
