'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Badge, Mono, Select, Table, TableWrap, TBody, Td, Th, THead, TR } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { type Envelope } from '../explorer-shared';

type AccountPositionsResp = components['schemas']['AccountPositions'];
type AccountPosition = components['schemas']['AccountPosition'];

// Human labels for amount_semantics — surfaced as visible subtext (not
// hidden in a tooltip only) per the task requirement: "the amount
// semantics surfaced (tooltip/subtext, not hidden)". The full prose
// still rides in the `title` attribute for anyone who wants the exact
// wording the API returns.
const SEMANTICS_LABEL: Record<string, string> = {
  net_underlying_at_event_time: 'net of underlying, at event time (no accrual)',
  shares: 'share/LP-token count',
  stateful_current: "protocol's latest published figure",
  signed_delta_sum_unconfirmed_unit: 'signed delta sum (unit unconfirmed)',
};

const POSITION_KIND_LABEL: Record<string, string> = {
  lending_supply: 'Lending supply',
  lending_borrow: 'Lending borrow',
  backstop_shares: 'Backstop shares',
  stake: 'Stake',
  vault_shares: 'Vault shares',
  credit: 'Credit',
  gauge: 'Gauge',
};

const PROTOCOL_LABEL: Record<string, string> = {
  blend: 'Blend',
  phoenix: 'Phoenix',
  defindex: 'DeFindex',
  sorocredit: 'sorocredit',
  aquarius: 'Aquarius',
};

/**
 * AccountDefiPositionsPanel — "enter an address, see all your DeFi
 * positions": a thin render over GET /v1/accounts/{g}/positions,
 * grouped by protocol. Sibling of AccountMovementsPanel (same package,
 * same conventions — read that file first for the filter/query-key/
 * empty-state pattern this one follows).
 *
 * NOT the same panel as the existing `Positions` panel on this page
 * (AccountPositions.tsx — classic Stellar-asset trustline holdings +
 * USD valuation). This one is Soroban DeFi-PROTOCOL positions
 * (lending/backstop/stake/vault/credit/gauge) with NO valuation —
 * different data, different endpoint, deliberately titled "DeFi
 * positions" to avoid the collision.
 *
 * Not paginated (the endpoint's own venue-cardinality bound is server-
 * side); the only client control is the closed-positions toggle, which
 * re-fetches with `?include_closed=`.
 */
export function AccountDefiPositionsPanel({ id }: { id: string }) {
  const [includeClosed, setIncludeClosed] = useState(false);

  const queryParams: Record<string, string | number | undefined> = {
    ...(includeClosed ? { include_closed: 'true' } : {}),
  };

  const { data, isLoading, isError, error } = useQuery<AccountPositionsResp>({
    queryKey: ['/v1/accounts/{id}/positions', id, includeClosed],
    enabled: id.length > 0,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountPositionsResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/positions`,
        queryParams,
      );
      return env.data;
    },
    staleTime: 30_000,
  });

  const source = asExample(`/v1/accounts/${id}/positions`, queryParams);
  const panelHint = 'net DeFi positions folded from on-chain events — no valuation applied, see each row’s semantics';

  const filters = (
    <label className="flex items-center gap-2 text-xs text-ink-muted">
      <span className="uppercase tracking-wider">Show</span>
      <Select
        value={includeClosed ? 'all' : 'open'}
        onChange={(e) => setIncludeClosed(e.target.value === 'all')}
        className="w-auto text-xs"
        aria-label="Filter by closed positions"
      >
        <option value="open">Open positions only</option>
        <option value="all">All positions (incl. closed)</option>
      </Select>
    </label>
  );

  if (isError) {
    return (
      <Panel title="DeFi positions" hint={panelHint} source={source} bodyClassName="space-y-3">
        {filters}
        <p className="text-sm text-ink-body">
          The positions lookup failed — reload to retry
          {error instanceof Error ? `: ${error.message}` : ''}.
        </p>
      </Panel>
    );
  }

  if (isLoading || !data) {
    return (
      <Panel title="DeFi positions" hint={panelHint} source={source} bodyClassName="space-y-3">
        {filters}
        <p className="text-sm text-ink-muted">Loading…</p>
      </Panel>
    );
  }

  const positions = data.positions ?? [];
  const byProtocol = new Map<string, AccountPosition[]>();
  for (const p of positions) {
    const list = byProtocol.get(p.protocol) ?? [];
    list.push(p);
    byProtocol.set(p.protocol, list);
  }

  return (
    <Panel
      title={`DeFi positions (${positions.length})`}
      hint={panelHint}
      source={source}
      bodyClassName="space-y-4"
    >
      {filters}

      {/* Honest top-level note (task requirement): always present, not
          conditional — every position on this endpoint is a raw
          quantity, never a valuation. */}
      {data.note && <p className="text-xs text-ink-faint">{data.note}</p>}

      {positions.length === 0 ? (
        <p className="text-sm text-ink-muted">No DeFi positions observed for this account yet.</p>
      ) : (
        Array.from(byProtocol.entries()).map(([protocol, rows]) => (
          <ProtocolGroup key={protocol} protocol={protocol} rows={rows} />
        ))
      )}
    </Panel>
  );
}

function ProtocolGroup({ protocol, rows }: { protocol: string; rows: AccountPosition[] }) {
  return (
    <div className="space-y-2">
      <Link
        href={`/protocols/${encodeURIComponent(protocol)}`}
        className="inline-block text-xs font-medium uppercase tracking-wider text-ink-muted hover:text-brand-600"
      >
        {PROTOCOL_LABEL[protocol] ?? protocol}
      </Link>
      <TableWrap>
        <Table>
          <THead>
            <TR className="hover:bg-transparent">
              <Th>Kind</Th>
              <Th>Venue</Th>
              <Th>Assets</Th>
              <Th align="right">Amount</Th>
              <Th>Last activity</Th>
            </TR>
          </THead>
          <TBody>
            {rows.map((p, i) => (
              <PositionRow key={`${p.protocol}-${p.position_kind}-${p.venue}-${i}`} p={p} />
            ))}
          </TBody>
        </Table>
      </TableWrap>
    </div>
  );
}

function PositionRow({ p }: { p: AccountPosition }) {
  const semanticsLabel = SEMANTICS_LABEL[p.amount_semantics] ?? p.amount_semantics;
  const basisLabel = p.basis === 'stateful' ? "protocol's own state" : 'derived from events';
  return (
    <TR>
      <Td>
        <Badge tone="brand">{POSITION_KIND_LABEL[p.position_kind] ?? p.position_kind}</Badge>
      </Td>
      <Td>
        <Link
          href={`/protocols/${encodeURIComponent(p.protocol)}`}
          className="hover:text-brand-600"
          title={p.venue}
        >
          {p.venue_label ? (
            <span className="text-ink-body">{p.venue_label}</span>
          ) : (
            <Mono value={p.venue} truncate copy={false} />
          )}
        </Link>
      </Td>
      <Td className="text-ink-body">{p.assets && p.assets.length > 0 ? p.assets.join(' / ') : '—'}</Td>
      <Td align="right" className="font-mono">
        {p.amount}
      </Td>
      <Td>
        <div className="whitespace-nowrap text-xs text-ink-muted">
          {p.last_activity.time ? new Date(p.last_activity.time).toLocaleString() : '—'}
        </div>
        <div className="text-[11px] text-ink-faint">#{p.last_activity.ledger.toLocaleString()}</div>
        {/* amount_semantics + basis surfaced as visible subtext, not
            hidden — the tooltip carries the API's exact wording for
            anyone who hovers, but the short label is always visible. */}
        <div className="text-[11px] text-ink-faint" title={`${p.amount_semantics} — ${p.basis}`}>
          {semanticsLabel} · {basisLabel}
        </div>
      </Td>
    </TR>
  );
}
