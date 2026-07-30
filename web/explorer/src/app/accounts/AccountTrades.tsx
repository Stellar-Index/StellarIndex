'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useQuery, keepPreviousData } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { Badge, Table, TableWrap, TBody, Td, Th, THead, TR } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { type Envelope, formatTimestamp, relativeAge } from '../explorer-shared';

type AccountTradesResp = components['schemas']['AccountTrades'];
type AccountTrade = components['schemas']['AccountTrade'];

const PAGE_SIZE = 25;

/**
 * AccountTradesPanel — the address's historic trades over
 * GET /v1/accounts/{g}/trades (taker/maker attribution), keyset-paged.
 * Sibling of AccountMovementsPanel / AccountDefiPositionsPanel (same
 * conventions). The server's always-present `note` (which venues can
 * and cannot attribute trades) renders as subtext so an empty table is
 * never read as "this address never traded anywhere".
 */
export function AccountTradesPanel({ id }: { id: string }) {
  const [cursor, setCursor] = useState('');

  const { data, isLoading, isError, error } = useQuery<AccountTradesResp>({
    queryKey: ['/v1/accounts/{id}/trades', id, cursor],
    enabled: id.length > 0,
    retry: false,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountTradesResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/trades`,
        { limit: PAGE_SIZE, ...(cursor ? { cursor } : {}) },
      );
      return env.data;
    },
    staleTime: 30_000,
  });

  const source = asExample(`/v1/accounts/${id}/trades`, { limit: PAGE_SIZE });
  const panelHint = 'historic trades where this address is the recorded taker or maker';

  if (isError) {
    return (
      <Panel title="Trades" hint={panelHint} source={source} bodyClassName="text-sm text-ink-body">
        The trades lookup failed or timed out — reload to retry
        {error instanceof Error ? `: ${error.message}` : ''}.
      </Panel>
    );
  }

  if (isLoading || !data) {
    return (
      <Panel title="Trades" hint={panelHint} source={source} bodyClassName="text-sm text-ink-muted">
        Loading…
      </Panel>
    );
  }

  const trades = data.trades ?? [];

  return (
    <Panel
      title={`Trades (${trades.length}${data.next_cursor ? '+' : ''})`}
      hint={panelHint}
      source={source}
      bodyClassName="space-y-3"
    >
      {trades.length === 0 ? (
        <p className="text-sm text-ink-muted">No attributed trades observed for this account yet.</p>
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <Th>Time</Th>
                <Th>Venue</Th>
                <Th>Pair</Th>
                <Th align="right">Base amount</Th>
                <Th align="right">Quote amount</Th>
                <Th align="right">USD volume</Th>
                <Th>Role</Th>
                <Th>Tx</Th>
              </TR>
            </THead>
            <TBody>
              {trades.map((t, i) => (
                <TradeRow key={`${t.tx_hash}-${t.op_index}-${i}`} t={t} />
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}

      <div className="flex items-center gap-2 text-xs">
        {cursor && (
          <button
            onClick={() => setCursor('')}
            className="rounded-md border border-line px-2.5 py-1 text-ink-body hover:border-brand-500"
          >
            ← Newest
          </button>
        )}
        {data.next_cursor && (
          <button
            onClick={() => setCursor(data.next_cursor ?? '')}
            className="ml-auto rounded-md border border-line px-2.5 py-1 text-ink-body hover:border-brand-500"
          >
            Load older →
          </button>
        )}
      </div>

      {/* Attribution-scope honesty note — always present on the API. */}
      {data.note && <p className="text-[11px] text-ink-faint">{data.note}</p>}
    </Panel>
  );
}

function TradeRow({ t }: { t: AccountTrade }) {
  return (
    <TR>
      <Td>
        <span className="whitespace-nowrap font-mono text-xs text-ink-muted" title={formatTimestamp(t.ts)}>
          {relativeAge(t.ts)}
        </span>
      </Td>
      <Td>
        <span className="text-xs text-ink-body">{t.source}</span>
        {t.routed_via && (
          <span
            className="ml-1 rounded-sm bg-surface-muted px-1 py-0.5 text-[9px] uppercase tracking-wider text-ink-muted"
            title={`Routed via ${t.routed_via}`}
          >
            routed
          </span>
        )}
      </Td>
      <Td>
        <span className="whitespace-nowrap text-xs">
          <AssetLink canonical={t.base_asset} /> / <AssetLink canonical={t.quote_asset} />
        </span>
      </Td>
      <Td align="right" className="font-mono text-xs tabular-nums">
        {t.base_amount}
      </Td>
      <Td align="right" className="font-mono text-xs tabular-nums">
        {t.quote_amount}
      </Td>
      <Td align="right" className="font-mono text-xs tabular-nums text-ink-muted">
        {t.usd_volume ? `$${t.usd_volume}` : '—'}
      </Td>
      <Td>
        <Badge tone={t.role === 'taker' ? 'brand' : 'neutral'}>{t.role}</Badge>
      </Td>
      <Td>
        <Link
          href={`/transactions/${t.tx_hash}/`}
          className="font-mono text-xs text-brand-600 hover:underline"
          title={t.tx_hash}
        >
          {t.tx_hash.slice(0, 8)}…{t.tx_hash.slice(-6)}
        </Link>
      </Td>
    </TR>
  );
}
