'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useQuery, keepPreviousData } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import {
  Badge,
  Callout,
  Select,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';
import {
  type Envelope,
  formatTimestamp,
  relativeAge,
  stroopsToXlm,
} from '../explorer-shared';

type AccountMovementsResp = components['schemas']['AccountMovements'];
type AccountMovement = components['schemas']['AccountMovement'];

const PAGE_SIZE = 25;

const G_RE = /^G[A-Z2-7]{55}$/;

const DIRECTION_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'All directions' },
  { value: 'sent', label: 'Sent' },
  { value: 'received', label: 'Received' },
  { value: 'self', label: 'Self' },
];

// The full movement_kind vocabulary this endpoint can serve (see
// AccountMovement.movement_kind's doc comment, openapi/stellar-index.v1.yaml).
// Every value except "transfer" only appears on ClickHouse pre-P23 archive
// rows; "transfer" is the only kind the Postgres post-P23 tail ever emits.
// An unrecognized kind isn't an error server-side — it's just a filter that
// matches nothing — so this list is a UX convenience, not a hard contract.
const KIND_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'All kinds' },
  { value: 'transfer', label: 'Transfer' },
  { value: 'payment', label: 'Payment' },
  { value: 'create_account', label: 'Create account' },
  { value: 'path_payment', label: 'Path payment' },
  { value: 'clawback', label: 'Clawback' },
  { value: 'account_merge', label: 'Account merge' },
  { value: 'claimable_balance_create', label: 'Claimable balance create' },
  { value: 'claimable_balance_claim', label: 'Claimable balance claim' },
  { value: 'claimable_balance_clawback', label: 'Claimable balance clawback' },
  { value: 'liquidity_pool_deposit', label: 'LP deposit' },
  { value: 'liquidity_pool_withdraw', label: 'LP withdraw' },
];

/**
 * AccountMovementsPanel — ADR-0048 D5's unified account-activity feed:
 * "everything this account has ever done" as a classic-asset movement,
 * newest first. The server merges the ClickHouse pre-P23 archive with the
 * Postgres post-P23 "recent tail" at read time — one feed, one page, one
 * cursor — so this panel is a thin render over GET
 * /v1/accounts/{g}/movements rather than two separate queries.
 *
 * Filters (kind/direction) are sent as the endpoint's own `?kind=`/
 * `?direction=` query params and re-fetched — not a client-side filter of
 * an already-fetched page, which would silently under-fill a page.
 *
 * Honest coverage: this is an x-stability: experimental endpoint whose
 * pre-P23 half depends on an operator-run historical backfill
 * (`classic-movements-backfill`) that hasn't necessarily run on every
 * deployment yet. When the response is NOT the full ADR-0048 D5 merge,
 * the server says so via `coverage_note` — surfaced below as a visible
 * callout, never silently dropped.
 */
export function AccountMovementsPanel({ id }: { id: string }) {
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [kind, setKind] = useState('');
  const [direction, setDirection] = useState('');

  const queryParams: Record<string, string | number | undefined> = {
    limit: PAGE_SIZE,
    ...(cursor ? { cursor } : {}),
    ...(kind ? { kind } : {}),
    ...(direction ? { direction } : {}),
  };

  const { data, isLoading, isError, error, isFetching } =
    useQuery<AccountMovementsResp>({
      queryKey: ['/v1/accounts/{id}/movements', id, cursor ?? 'tip', kind, direction],
      enabled: id.length > 0,
      retry: false,
      placeholderData: keepPreviousData,
      queryFn: async () => {
        const env = await apiGet<Envelope<AccountMovementsResp>>(
          `/v1/accounts/${encodeURIComponent(id)}/movements`,
          queryParams,
        );
        return env.data;
      },
      staleTime: 30_000,
    });

  const source = asExample(`/v1/accounts/${id}/movements`, queryParams);

  // Changing a filter starts a fresh keyset walk — an old cursor from a
  // different filter combination isn't guaranteed to still be valid.
  function updateKind(v: string) {
    setCursor(undefined);
    setKind(v);
  }
  function updateDirection(v: string) {
    setCursor(undefined);
    setDirection(v);
  }

  const filters = (
    <div className="flex flex-wrap items-center gap-4 text-xs">
      <label className="flex items-center gap-2 text-ink-muted">
        <span className="uppercase tracking-wider">Kind</span>
        <Select
          value={kind}
          onChange={(e) => updateKind(e.target.value)}
          className="w-auto text-xs"
          aria-label="Filter by movement kind"
        >
          {KIND_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
      </label>
      <label className="flex items-center gap-2 text-ink-muted">
        <span className="uppercase tracking-wider">Direction</span>
        <Select
          value={direction}
          onChange={(e) => updateDirection(e.target.value)}
          className="w-auto text-xs"
          aria-label="Filter by direction"
        >
          {DIRECTION_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </Select>
      </label>
    </div>
  );

  const panelHint =
    'every classic-asset movement, sent or received — the pre-P23 archive merged with the live post-P23 tail (ADR-0048 D5)';

  if (isError) {
    return (
      <Panel title="Activity (movements)" hint={panelHint} source={source} bodyClassName="space-y-3">
        {filters}
        <p className="text-sm text-ink-body">
          The movements lookup failed — reload to retry
          {error instanceof Error ? `: ${error.message}` : ''}.
        </p>
      </Panel>
    );
  }

  if (isLoading || !data) {
    return (
      <Panel title="Activity (movements)" hint={panelHint} source={source} bodyClassName="space-y-3">
        {filters}
        <p className="text-sm text-ink-muted">Loading…</p>
      </Panel>
    );
  }

  const movements = data.movements ?? [];

  return (
    <Panel
      title={`Activity (movements) (${movements.length})`}
      hint={panelHint}
      source={source}
      bodyClassName="space-y-3"
    >
      {filters}

      {/* Honest-degrade signal (ADR-0048 D5): present ONLY when this
          response is not the full ClickHouse+Postgres merge. Rendered as
          a real callout, not a muted footnote — a viewer relying on this
          feed for "everything this account has ever done" needs to see
          the gap, not stumble on it later. */}
      {data.coverage_note && (
        <Callout tone="warn" title="Partial coverage">
          {data.coverage_note}
        </Callout>
      )}

      {movements.length === 0 ? (
        <p className="text-sm text-ink-muted">
          {kind || direction
            ? 'No movements match this filter.'
            : 'No movements observed for this account yet.'}
        </p>
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <Th>Time</Th>
                <Th>Kind</Th>
                <Th>Direction</Th>
                <Th>Asset</Th>
                <Th align="right">Amount</Th>
                <Th>Counterparty</Th>
                <Th>Tx</Th>
              </TR>
            </THead>
            <TBody>
              {movements.map((m) => (
                <MovementRow key={`${m.tx_hash}-${m.op_index}-${m.leg_index}`} m={m} />
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}

      <div className="flex items-center gap-2 pt-1 text-xs">
        {cursor && (
          <button
            type="button"
            onClick={() => setCursor(undefined)}
            className="rounded-md border border-line px-2.5 py-1 text-ink-body hover:border-brand-500"
          >
            ← Newest
          </button>
        )}
        {data.next_cursor && (
          <button
            type="button"
            onClick={() => setCursor(data.next_cursor)}
            className="ml-auto rounded-md border border-line px-2.5 py-1 text-ink-body hover:border-brand-500"
          >
            Load older →
          </button>
        )}
        {isFetching && <span className="text-ink-faint">Loading…</span>}
      </div>
    </Panel>
  );
}

function MovementRow({ m }: { m: AccountMovement }) {
  return (
    <TR>
      <Td>
        <span
          className="whitespace-nowrap text-xs text-ink-muted"
          title={formatTimestamp(m.ledger_close_time)}
        >
          {relativeAge(m.ledger_close_time)}
        </span>
        <Link
          href={`/ledgers/${m.ledger}/`}
          className="ml-2 font-mono text-[11px] text-ink-faint hover:text-brand-600"
        >
          #{m.ledger.toLocaleString()}
        </Link>
      </Td>
      <Td>
        <Badge
          tone="brand"
          title={
            m.provenance === 'classic_derived'
              ? 'Pre-P23 ClickHouse classic-movement archive'
              : 'Post-P23 Postgres recent tail'
          }
        >
          {kindLabel(m.movement_kind)}
        </Badge>
      </Td>
      <Td>
        <DirectionBadge direction={m.direction} />
      </Td>
      <Td>
        <AssetLink canonical={m.asset} />
      </Td>
      <Td align="right" className="font-mono">
        {formatMovementAmount(m.amount)}
      </Td>
      <Td>
        <CounterpartyCell counterparty={m.counterparty} />
      </Td>
      <Td>
        <Link
          href={`/transactions/${m.tx_hash}/`}
          className="font-mono text-xs text-brand-600 hover:underline"
          title={m.tx_hash}
        >
          {m.tx_hash.slice(0, 8)}…{m.tx_hash.slice(-6)}
        </Link>
      </Td>
    </TR>
  );
}

function DirectionBadge({ direction }: { direction: string }) {
  // sent/received/self get a visually distinct tone — not just a text
  // label — so a viewer scanning the column can tell flow direction at a
  // glance (green in, red/amber out, neutral for a same-account leg).
  if (direction === 'sent') return <Badge tone="down">Sent</Badge>;
  if (direction === 'received') return <Badge tone="up">Received</Badge>;
  if (direction === 'self') return <Badge tone="neutral">Self</Badge>;
  return <Badge tone="neutral">{direction || '—'}</Badge>;
}

function CounterpartyCell({ counterparty }: { counterparty?: string }) {
  if (!counterparty) {
    return (
      <span
        className="text-ink-faint"
        title="No counterparty G-account for this movement — a claimable-balance escrow or a liquidity-pool leg, neither of which is a real account"
      >
        —
      </span>
    );
  }
  if (!G_RE.test(counterparty)) {
    // Non-G-strkey counterparty (e.g. a claimable-balance id slipping
    // through as text) — show it, but don't link somewhere that 404s.
    return (
      <span className="font-mono text-xs text-ink-body" title={counterparty}>
        {counterparty.length > 12 ? `${counterparty.slice(0, 6)}…${counterparty.slice(-4)}` : counterparty}
      </span>
    );
  }
  return (
    <Link
      href={`/accounts/${counterparty}/`}
      className="font-mono text-xs text-brand-600 hover:underline"
      title={counterparty}
    >
      {counterparty.slice(0, 6)}…{counterparty.slice(-4)}
    </Link>
  );
}

// kindLabel humanizes the wire's snake_case movement_kind ("liquidity_pool_deposit")
// into "Liquidity pool deposit" for the badge.
function kindLabel(kind: string): string {
  if (!kind) return '—';
  const words = kind.split('_');
  return words.map((w, i) => (i === 0 ? capitalize(w) : w)).join(' ');
}

function capitalize(w: string): string {
  return w.length === 0 ? w : w.charAt(0).toUpperCase() + w.slice(1);
}

// formatMovementAmount — amount is a decimal string at the asset's native
// smallest-unit scale (ADR-0003); the wire does NOT carry a per-row
// decimals field. Every row on this feed is either a classic Stellar asset
// (fixed 7-decimal stroop scale — a protocol-wide invariant, same as XLM)
// or, on the rare genuine-Soroban-native-token post-P23 row, falls back to
// that same 7-decimal scale. This is the identical "no per-row decimals on
// the wire, default 7" convention ContractView's TransfersPanel uses for
// the same reason; stroopsToXlm does the actual division as an exact BigInt
// divide (never a float above 2^53) so large amounts stay faithful.
function formatMovementAmount(amount: string): string {
  return stroopsToXlm(amount);
}
