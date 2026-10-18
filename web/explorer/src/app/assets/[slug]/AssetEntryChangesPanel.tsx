'use client';

import { useState } from 'react';
import Link from 'next/link';
import { keepPreviousData, useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
  Badge,
  Button,
  Callout,
  Skeleton,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { formatUnitsReadable } from '@/lib/format';
import type { paths } from '@/api/types';
import { formatTimestamp, relativeAge } from '../../explorer-shared';
import { isNativeAssetID } from './TrustlineHistoryPanel';

type AssetEntryChangesResp = NonNullable<
  paths['/assets/{asset_id}/entry-changes']['get']['responses'][200]['content']['application/json']['data']
>;
type AssetEntryChange = AssetEntryChangesResp['changes'][number];

const ROLE_LABEL: Record<string, string> = {
  holder: 'Trustline',
  claimable: 'Claimable balance',
  pool: 'Pool',
  reserve_a: 'Pool reserve',
  reserve_b: 'Pool reserve',
};

/** AssetEntryChangesPanel lists the asset's trustline, claimable-balance and pool entry changes, newest first. */
export function AssetEntryChangesPanel({ assetID }: { assetID: string }) {
  const native = isNativeAssetID(assetID);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const path = `/v1/assets/${encodeURIComponent(assetID)}/entry-changes`;
  const { data, isLoading, isError, isFetching } =
    useQuery<AssetEntryChangesResp>({
      queryKey: ['/v1/assets/{id}/entry-changes', assetID, cursor ?? 'tip'],
      enabled: !native,
      retry: false,
      staleTime: 60_000,
      placeholderData: keepPreviousData,
      queryFn: async () =>
        (
          await apiGet<{ data: AssetEntryChangesResp }>(
            path,
            cursor ? { cursor } : undefined,
          )
        ).data,
    });
  if (native) return null;

  const changes = data?.changes ?? [];
  return (
    <Panel
      headingLevel={2}
      title="Entry changes"
      hint="Trustlines, claimable balances and pools"
      source={asExample(`/v1/assets/${assetID}/entry-changes`)}
      bodyClassName="space-y-3"
    >
      {isLoading ? (
        <div aria-busy="true">
          <span className="sr-only">Loading entry changes</span>
          <Skeleton className="h-[220px] w-full" aria-hidden />
        </div>
      ) : isError || !data ? (
        <Callout tone="bad">Failed to load entry changes.</Callout>
      ) : changes.length === 0 ? (
        <p className="text-ink-muted text-sm">
          No entry changes recorded for this asset.
        </p>
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <Th>Time</Th>
                <Th>Entry</Th>
                <Th>Change</Th>
                <Th>Account</Th>
                <Th align="right">Balance after</Th>
                <Th>Tx</Th>
              </TR>
            </THead>
            <TBody>
              {changes.map((c) => (
                <EntryChangeRow
                  key={`${c.ledger}-${c.tx_hash}-${c.op_index}-${c.change_index}-${c.role}`}
                  c={c}
                />
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}

      {data && (
        <p className="text-ink-faint text-[11px]">{data.coverage_note}</p>
      )}

      {data && (cursor || data.next_cursor) && (
        <div className="border-line -mx-4 -mb-4 flex items-center justify-between gap-2 border-t px-4 py-3">
          <span className="text-ink-muted text-xs">
            {isFetching ? 'Loading…' : cursor ? 'Older history' : 'Newest first'}
          </span>
          <div className="flex gap-2">
            {cursor && (
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setCursor(undefined)}
              >
                Newest
              </Button>
            )}
            {data.next_cursor && (
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setCursor(data.next_cursor)}
              >
                Load older
              </Button>
            )}
          </div>
        </div>
      )}
    </Panel>
  );
}

function EntryChangeRow({ c }: { c: AssetEntryChange }) {
  return (
    <TR>
      <Td>
        <span
          className="text-ink-muted text-xs whitespace-nowrap"
          title={formatTimestamp(c.ledger_close_time)}
        >
          {relativeAge(c.ledger_close_time)}
        </span>
        <Link
          href={`/ledgers/${c.ledger}/`}
          className="text-ink-faint hover:text-brand-600 ml-2 font-mono text-[11px]"
        >
          #{c.ledger}
        </Link>
      </Td>
      <Td>{ROLE_LABEL[c.role] ?? c.role}</Td>
      <Td>
        <Badge tone="brand">{c.change_type}</Badge>
        {c.changed.length > 0 && (
          <span className="text-ink-muted ml-2 text-xs">
            {c.changed.join(', ')}
          </span>
        )}
      </Td>
      <Td>
        {c.account ? (
          <Link
            href={`/accounts/${c.account}/`}
            className="text-brand-600 font-mono text-xs hover:underline"
            title={c.account}
          >
            {c.account.slice(0, 6)}…{c.account.slice(-4)}
          </Link>
        ) : (
          <span className="text-ink-faint">—</span>
        )}
      </Td>
      <Td align="right" className="font-mono">
        {c.amount ? formatUnitsReadable(c.amount, 7) : '—'}
      </Td>
      <Td>
        {c.tx_hash ? (
          <Link
            href={`/transactions/${c.tx_hash}/`}
            className="text-brand-600 font-mono text-xs hover:underline"
            title={c.tx_hash}
          >
            {c.tx_hash.slice(0, 8)}…{c.tx_hash.slice(-6)}
          </Link>
        ) : (
          <span className="text-ink-faint">—</span>
        )}
      </Td>
    </TR>
  );
}
