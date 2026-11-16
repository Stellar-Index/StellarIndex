'use client';

import Link from 'next/link';
import { useInfiniteQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { Button, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { paths } from '@/api/types';
import { formatRelative, truncateMiddle } from '@/lib/format';
import { changeValues, configKindLabel } from '../poolConfig';

type Resp =
  paths['/lending/pools/{pool}/config-history']['get']['responses'][200]['content']['application/json'];
type Change = NonNullable<NonNullable<Resp['data']>['changes']>[number];

interface Page {
  changes: Change[];
  configDecimals: number | undefined;
  next: string | undefined;
}

const LIMIT = 25;

export function ConfigHistory({ pool }: { pool: string }) {
  const q = useInfiniteQuery<Page, Error, { pages: Page[] }, unknown[], string>(
    {
      queryKey: ['/v1/lending/pools/{pool}/config-history', pool],
      retry: false,
      initialPageParam: '',
      queryFn: async ({ pageParam }) => {
        const env = await apiGet<Resp>(
          `/v1/lending/pools/${encodeURIComponent(pool)}/config-history`,
          { limit: LIMIT, cursor: pageParam || undefined },
        );
        return {
          changes: env.data?.changes ?? [],
          configDecimals: env.data?.config_decimals,
          next: env.pagination?.next ?? undefined,
        };
      },
      getNextPageParam: (last) => last.next,
      staleTime: 60_000,
    },
  );

  const pages = q.data?.pages ?? [];
  const rows = pages.flatMap((p) =>
    p.changes.map((c) => ({ c, dec: p.configDecimals })),
  );

  return (
    <Panel
      headingLevel={2}
      title="Configuration history"
      hint="Reserve and pool configuration changes, newest first."
      source={asExample(`/v1/lending/pools/${pool}/config-history`, {})}
      bodyClassName="space-y-3"
    >
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading configuration history…</p>
      )}
      {q.isError && (
        <p className="text-ink-muted text-sm">
          Configuration history is unavailable right now.
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
          No configuration changes recorded for this pool.
        </p>
      )}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>When</Th>
                <Th>Change</Th>
                <Th className="hidden sm:table-cell">Asset</Th>
                <Th>Values</Th>
                <Th>Tx</Th>
              </tr>
            </THead>
            <TBody>
              {rows.map(({ c, dec }) => (
                <TR key={`${c.tx_hash}-${c.kind}-${c.asset ?? ''}`}>
                  <Td className="whitespace-nowrap" title={c.close_time}>
                    {formatRelative(c.close_time)}
                    <div className="text-ink-muted text-xs">
                      <Link
                        href={`/ledgers/${c.ledger}/`}
                        className="hover:underline"
                      >
                        #{c.ledger}
                      </Link>
                    </div>
                  </Td>
                  <Td className="whitespace-nowrap">
                    {configKindLabel(c.kind)}
                  </Td>
                  <Td className="hidden whitespace-nowrap sm:table-cell">
                    {c.asset ? <AssetLink canonical={c.asset} /> : '—'}
                  </Td>
                  <Td className="text-xs">
                    {(() => {
                      const v = changeValues(c, dec);
                      return v.length === 0
                        ? '—'
                        : v.map((x) => (
                            <div key={x.label}>
                              <span className="text-ink-muted">{x.label} </span>
                              <span className="font-mono break-all">
                                {x.value}
                              </span>
                            </div>
                          ));
                    })()}
                  </Td>
                  <Td className="font-mono whitespace-nowrap">
                    <Link
                      href={`/transactions/${c.tx_hash}/`}
                      className="text-brand-600 hover:underline"
                      title={c.tx_hash}
                    >
                      {truncateMiddle(c.tx_hash, 8, 6)}
                    </Link>
                  </Td>
                </TR>
              ))}
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
    </Panel>
  );
}
