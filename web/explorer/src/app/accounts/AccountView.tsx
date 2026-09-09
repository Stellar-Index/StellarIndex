'use client';

import { useState } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useQuery, keepPreviousData } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import {
  DirectoryLabel,
  type DirectoryInfo,
} from '@/components/DirectoryLabel';
import {
  Container,
  Breadcrumbs,
  Callout,
  TxStatusBadge,
} from '@/components/ui';
import { AccountPositions } from './AccountPositions';
import { AccountMovementsPanel } from './AccountMovements';
import { AccountDefiPositionsPanel } from './AccountDefiPositions';
import { AccountActivitySummaryPanel } from './AccountActivitySummary';
import { AccountTradesPanel } from './AccountTrades';
import { AccountGraphPanel } from './AccountGraph';
import { AccountsAnalytics } from './AccountsAnalytics';
import { useIssuers } from '@/api/hooks';
import { apiGet, asExample } from '@/api/client';
import { CURRENT_NETWORK } from '@/lib/networks';
import {
  type Envelope,
  type AccountTransactionsResp,
  type AccountOperationsResp,
  type LedgerTransaction,
  type TxOperation,
  CopyHash,
  formatTimestamp,
  relativeAge,
  renderOpFieldValue,
  stroopsToXlm,
} from '../explorer-shared';
import { CrossReference } from '@/components/CrossReference';

// Stellar account IDs are 56 chars: 'G' + 55 base32 alphanumerics.
const ACCOUNT_RE = /^G[A-Z2-7]{55}$/;

const PAGE_SIZE = 50;

/**
 * Client view for /accounts?id=G…. Fetches the account's transactions
 * and operations in parallel and renders them.
 *
 * SCOPE: "all" (ADR-0038 Phase B) — both what the account sourced and
 * where it's a non-source participant (incoming payments, trustlines,
 * merges, …). The backend stamps a `scope` field; incoming coverage
 * tracks the participant-index capture + backfill.
 */
export function AccountView({ id: idProp }: { id?: string } = {}) {
  // Path route /accounts/[g] passes `id` as a prop; legacy /accounts?id= reads
  // it from the query string (kept for redirect compatibility). Prop wins.
  const params = useSearchParams();
  const id = (idProp ?? params.get('id') ?? '').trim();
  const looksValid = ACCOUNT_RE.test(id);

  // ACC-3: both activity endpoints serve next_cursor; the tables were
  // hard-capped at one page. Same keyset pattern as the contract page.
  const [txCursor, setTxCursor] = useState('');
  const [opsCursor, setOpsCursor] = useState('');
  const txQ = useQuery<AccountTransactionsResp>({
    queryKey: ['/v1/accounts/{id}/transactions', id, txCursor],
    enabled: id.length > 0 && looksValid,
    retry: false,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountTransactionsResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/transactions`,
        { limit: PAGE_SIZE, ...(txCursor ? { cursor: txCursor } : {}) },
      );
      return env.data;
    },
    staleTime: 30_000,
  });

  const opsQ = useQuery<AccountOperationsResp>({
    queryKey: ['/v1/accounts/{id}/operations', id, opsCursor],
    enabled: id.length > 0 && looksValid,
    retry: false,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountOperationsResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/operations`,
        { limit: PAGE_SIZE, ...(opsCursor ? { cursor: opsCursor } : {}) },
      );
      return env.data;
    },
    staleTime: 30_000,
  });

  const stateQ = useQuery<AccountStateResp>({
    queryKey: ['/v1/accounts/{id}', id],
    enabled: id.length > 0 && looksValid,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountStateResp>>(
        `/v1/accounts/${encodeURIComponent(id)}`,
      );
      return env.data;
    },
    staleTime: 30_000,
  });

  // Is this account a known asset issuer? We match against the top
  // issuers list — the SAME set /issuers/[g_strkey] pre-renders via
  // generateStaticParams — so an internal link is guaranteed to
  // resolve under static export (no 404 on an un-prerendered route).
  const issuersQ = useIssuers(100);
  const isKnownIssuer = (issuersQ.data ?? []).some(
    (iss) => iss.g_strkey === id,
  );

  if (!looksValid) {
    return (
      <Shell id={id}>
        <Panel title="Invalid account ID" bodyClassName="text-sm text-ink-body">
          <p>
            <span className="font-mono break-all">{id}</span> isn&apos;t a valid
            Stellar account ID. Account IDs are 56 characters, starting with{' '}
            <code className="font-mono">G</code>.
          </p>
          {/^M[A-Z2-7]{68}$/.test(id) && (
            <p className="mt-2">
              This looks like a <strong>muxed account</strong> (M-address) — a
              G-account plus an embedded routing ID, used by exchanges and
              custodians to distinguish customers behind one shared account.
              Look up the underlying G-address to see its state and activity;
              wallets and most explorers can decode the M-form.
            </p>
          )}
        </Panel>
      </Shell>
    );
  }

  return (
    <Shell id={id}>
      <Panel
        title="Account"
        source={asExample(`/v1/accounts/${id}/transactions`, {
          limit: PAGE_SIZE,
        })}
        bodyClassName="space-y-3"
      >
        <div>
          <div className="text-ink-muted text-[11px] tracking-wider uppercase">
            Account ID
          </div>
          <div className="mt-0.5">
            <CopyHash value={id} head={16} tail={16} />
          </div>
        </div>
        {stateQ.data?.directory && (
          <DirectoryLabel info={stateQ.data.directory} />
        )}
        <ul className="text-ink-body flex flex-wrap items-center gap-x-6 gap-y-1 text-xs">
          {isKnownIssuer && (
            <li>
              <Link
                href={`/issuers/${encodeURIComponent(id)}`}
                className="text-brand-600 font-medium hover:underline"
              >
                View as issuer — issued assets &amp; auth flags →
              </Link>
            </li>
          )}
        </ul>
        <CrossReference kind="account" id={id} />
        <p className="border-line bg-surface-muted text-ink-muted rounded-md border px-3 py-2 text-xs">
          Balances + trustlines + offers below reflect the lake&apos;s captured
          ledger-entry window; the activity tables show <strong>all</strong>{' '}
          history — both what the account sourced and where it&apos;s a
          participant (incoming payments, trustlines, merges). Incoming coverage
          tracks the participant-index backfill.
        </p>
      </Panel>

      <AccountActivitySummaryPanel id={id} />

      <AccountPositions id={id} />

      <AccountStatePanel
        id={id}
        state={stateQ.data}
        isLoading={stateQ.isLoading}
        isError={stateQ.isError}
      />

      <AccountMovementsPanel id={id} />

      <AccountDefiPositionsPanel id={id} />

      <AccountTradesPanel id={id} />

      <AccountGraphPanel id={id} />

      <TransactionsPanel
        id={id}
        isLoading={txQ.isLoading}
        isError={txQ.isError}
        error={txQ.error}
        data={txQ.data}
        onOlder={
          txQ.data?.next_cursor
            ? () => setTxCursor(txQ.data?.next_cursor ?? '')
            : undefined
        }
        onNewest={txCursor ? () => setTxCursor('') : undefined}
      />
      <OperationsPanel
        id={id}
        isLoading={opsQ.isLoading}
        isError={opsQ.isError}
        error={opsQ.error}
        data={opsQ.data}
        onOlder={
          opsQ.data?.next_cursor
            ? () => setOpsCursor(opsQ.data?.next_cursor ?? '')
            : undefined
        }
        onNewest={opsCursor ? () => setOpsCursor('') : undefined}
      />
    </Shell>
  );
}

// ── Accounts directory (ranked by wealth) ───────────────────────────────
// Mirrors api/v1.AccountsListView (GET /v1/accounts). Ranks accounts by the
// total USD value of their holdings on the priced networks; on the lean nets
// (no aggregator, so no USD prices) the API ranks by native XLM balance and
// sets ranked_by="native_xlm" — we read that to label the numbers correctly.
interface AccountsListResp {
  priced_assets: number;
  ranked_by?: 'usd' | 'native_xlm';
  accounts: {
    account_id: string;
    // `value` is the ranked wealth in the ranked_by unit (USD or XLM). On older
    // servers only usd_value is present; fall back to it.
    value?: string;
    usd_value?: string;
    locked?: boolean;
  }[];
}

const DIRECTORY_SIZE = 100;

const usdFmt = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  maximumFractionDigits: 0,
});

const xlmFmt = new Intl.NumberFormat('en-US', {
  maximumFractionDigits: 0,
});

/**
 * GET /v1/accounts — the directory's one query. The header and the table
 * both subscribe under this key, so react-query fetches once and the two
 * flip to the served basis in the same render.
 */
function useAccountsDirectoryQuery() {
  return useQuery<AccountsListResp>({
    queryKey: ['/v1/accounts', DIRECTORY_SIZE],
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountsListResp>>('/v1/accounts', {
        limit: DIRECTORY_SIZE,
      });
      return env.data;
    },
    staleTime: 60_000,
  });
}

/**
 * Whether the directory is ranked in native XLM. The served `ranked_by`
 * decides once the response lands; before that — and in the static
 * export, which never fetches — the network's pricing flag stands in,
 * so the lean-net copy is right from the first byte.
 */
function rankedInNative(data: AccountsListResp | undefined): boolean {
  return data ? data.ranked_by === 'native_xlm' : !CURRENT_NETWORK.pricing;
}

/**
 * The /accounts directory frame — heading + standing description.
 *
 * Split out of AccountsDirectory so the STATIC document can carry it:
 * the body reads the URL through useSearchParams and so sits inside a
 * Suspense boundary, and that boundary's fallback is all the export
 * bakes. useQuery forces no such bailout, so the header subscribes to
 * the same /v1/accounts query as the table below it. The export ships
 * the network's pricing fallback — the copy the page metadata declares
 * — and once the response lands the served `ranked_by` wins here and in
 * the Panel at once: the API ranks by native XLM whenever its price
 * catalogue degrades, mainnet included, and this sentence must never
 * claim a USD ranking above a table titled "Ranked by XLM balance".
 */
export function AccountsDirectoryHeader() {
  const q = useAccountsDirectoryQuery();
  const isNative = rankedInNative(q.data);
  return (
    <header className="space-y-2">
      <h1 className="text-2xl font-semibold tracking-tight">Accounts</h1>
      <p className="text-ink-body max-w-3xl text-sm">
        {isNative ? (
          <>
            The largest accounts on Stellar {CURRENT_NETWORK.label}, ranked by
            native XLM balance, read straight from the certified lake&apos;s
            current-state projection.
          </>
        ) : (
          <>
            The richest accounts on Stellar, ranked by the total USD value of
            their holdings — native XLM plus every trustline asset we hold a
            verified price for, summed straight from the certified lake&apos;s
            current-state projection.
          </>
        )}
      </p>
    </header>
  );
}

/** The /accounts directory body: the analytics strip + the ranked table. */
export function AccountsDirectoryBody() {
  return (
    <>
      <AccountsAnalytics />
      <AccountsDirectory />
    </>
  );
}

function AccountsDirectory() {
  const q = useAccountsDirectoryQuery();
  const isNative = rankedInNative(q.data);
  const fmtWealth = (row: { value?: string; usd_value?: string }) => {
    const n = Number(row.value ?? row.usd_value ?? '0');
    if (!Number.isFinite(n)) return '—';
    return isNative ? `${xlmFmt.format(n)} XLM` : usdFmt.format(n);
  };

  return (
    <div className="space-y-6">
      <Panel
        title={isNative ? 'Ranked by XLM balance' : 'Ranked by USD wealth'}
        source={asExample('/v1/accounts', { limit: DIRECTORY_SIZE })}
        bodyClassName="space-y-3"
      >
        {q.isLoading && <p className="text-ink-muted text-sm">Loading…</p>}
        {q.isError && (
          <p className="text-ink-muted text-sm">
            The accounts directory is unavailable right now (the current-state
            projection is still backfilling).
          </p>
        )}
        {q.data && q.data.accounts.length === 0 && (
          <p className="text-ink-muted text-sm">
            {isNative ? 'No accounts captured yet.' : 'No priced accounts yet.'}
          </p>
        )}
        {q.data && q.data.accounts.length > 0 && (
          <>
            <table className="w-full text-sm">
              <thead>
                <tr className="border-line text-ink-muted border-b text-left text-[11px] tracking-wider uppercase">
                  <th className="w-12 py-1.5 pr-4 text-right font-normal">#</th>
                  <th className="py-1.5 pr-4 font-normal">Account</th>
                  <th className="py-1.5 text-right font-normal">
                    {isNative ? 'XLM balance' : 'USD value'}
                  </th>
                </tr>
              </thead>
              <tbody>
                {q.data.accounts.map((a, i) => (
                  <tr
                    key={a.account_id}
                    className="border-line/60 hover:bg-surface-muted border-b last:border-0"
                  >
                    <td className="text-ink-muted py-1.5 pr-4 text-right font-mono tabular-nums">
                      {i + 1}
                    </td>
                    <td className="py-1.5 pr-4 font-mono">
                      <Link
                        href={`/accounts/${encodeURIComponent(a.account_id)}/`}
                        className="hover:text-brand-600 hover:underline"
                      >
                        {a.account_id.slice(0, 10)}…{a.account_id.slice(-8)}
                      </Link>
                      {a.locked && (
                        <span
                          title="Provably unspendable — master weight 0, all thresholds 0, no signers. The balance is real; no key can ever move it (e.g. the SDF burn address)."
                          className="bg-surface-muted text-ink-muted ml-2 rounded-sm px-1.5 py-0.5 text-[9px] font-medium tracking-wider uppercase"
                        >
                          Locked
                        </span>
                      )}
                    </td>
                    <td className="py-1.5 text-right font-mono tabular-nums">
                      {fmtWealth(a)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <p className="text-ink-muted text-xs">
              {isNative ? (
                <>
                  Ranked by native XLM balance over the lake&apos;s
                  current-state projection. Accounts not yet captured in the
                  ledger-entry window are excluded.
                </>
              ) : (
                <>
                  Summed across {q.data.priced_assets} priced asset
                  {q.data.priced_assets === 1 ? '' : 's'}. Wealth not yet
                  captured in the lake&apos;s ledger-entry window (Phase-C
                  backfill in progress) is excluded.
                </>
              )}
            </p>
          </>
        )}
      </Panel>
    </div>
  );
}

// ── Account state (balances / signers / trustlines / offers) ────────────
// Mirrors api/v1.AccountStateView (GET /v1/accounts/{g}).
interface AccountStateResp {
  account_id: string;
  exists: boolean;
  balance?: string;
  seq_num?: string;
  num_subentries?: number;
  flags?: number;
  home_domain?: string;
  thresholds?: { master: number; low: number; med: number; high: number };
  signers?: { key: string; weight: number }[];
  trustlines?: {
    asset: string;
    balance: string;
    limit: string;
    flags: number;
  }[];
  offers?: {
    offer_id: number;
    selling: string;
    buying: string;
    amount: string;
    price_n: number;
    price_d: number;
  }[];
  last_modified_ledger?: number;
  directory?: DirectoryInfo;
}

function AccountStatePanel({
  id,
  state,
  isLoading,
  isError,
}: {
  id: string;
  state: AccountStateResp | undefined;
  isLoading: boolean;
  isError: boolean;
}) {
  const source = asExample(`/v1/accounts/${id}`);
  if (isLoading) {
    return (
      <Panel
        title="State"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading account state…
      </Panel>
    );
  }
  if (isError) {
    // X-1: an error must never render as a confident "nothing exists"
    // claim — the state lookup can time out under load.
    return (
      <Panel
        title="State"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        The account-state lookup failed — reload to retry. Activity below is
        unaffected.
      </Panel>
    );
  }
  if (!state || !state.exists) {
    return (
      <Panel
        title="State"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        No live account state in the captured ledger window yet — the account
        wasn’t touched since entry-change capture began. Sourced activity still
        shows below.
      </Panel>
    );
  }
  return (
    <Panel title="State" source={source} bodyClassName="space-y-5">
      <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-4">
        <Stat
          label="Native balance"
          value={`${stroopsToXlm(state.balance ?? '0')} XLM`}
          mono
        />
        <Stat label="Sequence" value={state.seq_num ?? '—'} mono />
        <Stat label="Sub-entries" value={String(state.num_subentries ?? 0)} />
        <Stat label="Home domain" value={state.home_domain || '—'} />
        {state.thresholds && (
          <Stat
            label="Thresholds (L/M/H)"
            mono
            value={`${state.thresholds.low}/${state.thresholds.med}/${state.thresholds.high}`}
          />
        )}
        <Stat
          label="Master weight"
          value={String(state.thresholds?.master ?? '—')}
        />
      </dl>

      {state.signers && state.signers.length > 0 && (
        <div>
          <div className="text-ink-muted mb-1 text-[11px] tracking-wider uppercase">
            Signers
          </div>
          <ul className="space-y-1 text-xs">
            {state.signers.map((s) => (
              <li key={s.key} className="flex items-center gap-2">
                {/^G[A-Z2-7]{55}$/.test(s.key) ? (
                  <Link
                    href={`/accounts/${s.key}/`}
                    className="text-brand-600 font-mono hover:underline"
                    title={s.key}
                  >
                    {s.key.slice(0, 8)}…{s.key.slice(-6)}
                  </Link>
                ) : (
                  <span className="text-ink-body font-mono" title={s.key}>
                    {s.key.slice(0, 8)}…{s.key.slice(-6)}
                  </span>
                )}
                <span className="text-ink-faint">weight {s.weight}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {state.trustlines && state.trustlines.length > 0 && (
        <div>
          <div className="text-ink-muted mb-1 text-[11px] tracking-wider uppercase">
            Trustlines ({state.trustlines.length})
          </div>
          <div className="overflow-x-auto">
            <table className="divide-line min-w-full divide-y text-sm">
              <thead>
                <tr className="text-ink-muted text-left text-[10px] tracking-wider uppercase">
                  <th className="py-1.5 pr-4">Asset</th>
                  <th className="py-1.5 pr-4 text-right">Balance</th>
                  <th className="py-1.5 text-right">Limit</th>
                </tr>
              </thead>
              <tbody className="divide-line-subtle divide-y">
                {state.trustlines.map((t) => (
                  <tr key={t.asset}>
                    <td className="py-1.5 pr-4 text-xs">
                      <AssetLink canonical={t.asset} />
                    </td>
                    <td className="py-1.5 pr-4 text-right font-mono tabular-nums">
                      {stroopsToXlm(t.balance)}
                    </td>
                    <td className="text-ink-muted py-1.5 text-right font-mono tabular-nums">
                      {stroopsToXlm(t.limit)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {state.offers && state.offers.length > 0 && (
        <div>
          <div className="text-ink-muted mb-1 text-[11px] tracking-wider uppercase">
            Open offers ({state.offers.length})
          </div>
          <ul className="text-ink-body space-y-1 font-mono text-xs">
            {state.offers.map((o) => (
              <li key={o.offer_id}>
                #{o.offer_id}: {stroopsToXlm(o.amount)} {o.selling} → {o.buying}{' '}
                @ {o.price_n}/{o.price_d}
              </li>
            ))}
          </ul>
        </div>
      )}
    </Panel>
  );
}

function Stat({
  label,
  value,
  mono,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div>
      <dt className="text-ink-muted text-[11px] tracking-wider uppercase">
        {label}
      </dt>
      <dd
        className={
          mono ? 'mt-0.5 font-mono text-xs break-all' : 'mt-0.5 text-sm'
        }
      >
        {value}
      </dd>
    </div>
  );
}

function Shell({
  id,
  children,
}: {
  id: string | null;
  children: React.ReactNode;
}) {
  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <Breadcrumbs
          items={[
            { label: 'Home', href: '/' },
            { label: 'Accounts', href: '/accounts' },
            { label: id ? `${id.slice(0, 8)}…${id.slice(-6)}` : 'account' },
          ]}
        />
        <h1 className="text-2xl font-semibold tracking-tight">Account</h1>
      </header>
      {children}
    </Container>
  );
}

function ActivityPager({
  onOlder,
  onNewest,
}: {
  onOlder?: () => void;
  onNewest?: () => void;
}) {
  if (!onOlder && !onNewest) return null;
  return (
    <div className="flex items-center gap-2 px-4 pt-3 text-xs">
      {onNewest && (
        <button
          onClick={onNewest}
          className="border-line text-ink-body hover:border-brand-500 rounded-md border px-2.5 py-1"
        >
          ← Newest
        </button>
      )}
      {onOlder && (
        <button
          onClick={onOlder}
          className="border-line text-ink-body hover:border-brand-500 ml-auto rounded-md border px-2.5 py-1"
        >
          Load older →
        </button>
      )}
    </div>
  );
}

function TransactionsPanel({
  id,
  isLoading,
  isError,
  error,
  data,
  onOlder,
  onNewest,
}: {
  id: string;
  isLoading: boolean;
  isError: boolean;
  error: unknown;
  onOlder?: () => void;
  onNewest?: () => void;
  data: AccountTransactionsResp | undefined;
}) {
  const source = asExample(`/v1/accounts/${id}/transactions`, {
    limit: PAGE_SIZE,
  });
  if (isError) {
    return (
      <Panel
        title="Transactions"
        source={source}
        bodyClassName="text-sm text-ink-body"
      >
        No transactions for that account in the served tier, or the lookup
        failed: {error instanceof Error ? error.message : 'unknown error'}.
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel
        title="Transactions"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }
  const transactions = data.transactions ?? [];
  if (transactions.length === 0) {
    return (
      <Panel
        title="Transactions"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        No transactions observed for this account yet.
      </Panel>
    );
  }
  return (
    <Panel
      title={`Transactions — sourced + incoming (${transactions.length})`}
      source={source}
      bodyClassName="-mx-4"
    >
      <div className="overflow-x-auto">
        <table className="divide-line min-w-full divide-y text-sm">
          <thead>
            <tr className="text-ink-muted text-left text-[11px] tracking-wider uppercase">
              <Th>Hash</Th>
              <Th>Ledger</Th>
              <Th align="right">Ops</Th>
              <Th>Result</Th>
              <Th align="right">Fee</Th>
              <Th>Memo</Th>
            </tr>
          </thead>
          <tbody className="divide-line-subtle divide-y">
            {transactions.map((t: LedgerTransaction) => (
              <tr key={t.hash} className="hover:bg-surface-muted">
                <Td>
                  <Link
                    href={`/transactions/${t.hash}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                    title={t.hash}
                  >
                    {(t.hash ?? '').slice(0, 10)}…{(t.hash ?? '').slice(-6)}
                  </Link>
                  {/* ACC-2: the API serves scope:"all" (sourced +
                      participant); without a direction marker a viewer
                      can't tell who initiated. */}
                  {t.source_account && t.source_account !== id && (
                    <span
                      title={`Initiated by ${t.source_account} — this account participates`}
                      className="bg-surface-muted text-ink-muted ml-2 rounded-sm px-1.5 py-0.5 text-[9px] font-medium tracking-wider uppercase"
                    >
                      in
                    </span>
                  )}
                </Td>
                <Td>
                  <Link
                    href={`/ledgers/${t.ledger}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                  >
                    #{(t.ledger ?? 0).toLocaleString('en-US')}
                  </Link>
                </Td>
                <Td align="right">
                  <span className="text-ink-body font-mono tabular-nums">
                    {t.operation_count}
                  </span>
                </Td>
                <Td>
                  <TxStatusBadge
                    successful={t.successful}
                    result={t.result}
                    code={t.result_code}
                  />
                </Td>
                <Td align="right">
                  <span className="text-ink-muted font-mono text-xs tabular-nums">
                    {t.fee_charged != null ? stroopsToXlm(t.fee_charged) : '—'}
                  </span>
                </Td>
                <Td>
                  {t.memo_type && t.memo_type !== 'none' ? (
                    <span
                      className="text-ink-muted font-mono text-[11px]"
                      title={t.memo ?? ''}
                    >
                      {t.memo_type}
                      {t.memo ? `: ${truncate(t.memo, 18)}` : ''}
                    </span>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <ActivityPager onOlder={onOlder} onNewest={onNewest} />
    </Panel>
  );
}

function OperationsPanel({
  id,
  isLoading,
  isError,
  error,
  data,
  onOlder,
  onNewest,
}: {
  id: string;
  isLoading: boolean;
  isError: boolean;
  error: unknown;
  onOlder?: () => void;
  onNewest?: () => void;
  data: AccountOperationsResp | undefined;
}) {
  const source = asExample(`/v1/accounts/${id}/operations`, {
    limit: PAGE_SIZE,
  });
  if (isError) {
    return (
      <Panel
        title="Operations"
        source={source}
        bodyClassName="text-sm text-ink-body"
      >
        No operations for that account in the served tier, or the lookup failed:{' '}
        {error instanceof Error ? error.message : 'unknown error'}.
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel
        title="Operations"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }
  const operations = data.operations ?? [];
  if (operations.length === 0) {
    return (
      <Panel
        title="Operations"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        No operations observed for this account yet.
      </Panel>
    );
  }
  return (
    <Panel
      title={`Operations — sourced + incoming (${operations.length})`}
      source={source}
      bodyClassName="space-y-3"
    >
      {data.coverage_note && (
        // Honest-degrade banner: the parent-transaction outcome read failed,
        // so ops below without a status are of UNKNOWN outcome (possibly a
        // FAILED transaction), not applied. Failed ops ARE listed here — this
        // marker is what keeps one from masquerading as a real interaction.
        <Callout tone="warn" title="Transaction outcomes partially unavailable">
          {data.coverage_note}
        </Callout>
      )}
      {operations.map((op: TxOperation, i: number) => (
        <OperationCard
          key={`${op.tx_hash ?? ''}-${op.op_index}-${i}`}
          op={op}
        />
      ))}
      <ActivityPager onOlder={onOlder} onNewest={onNewest} />
    </Panel>
  );
}

function OperationCard({ op }: { op: TxOperation }) {
  const fields = op.fields ?? {};
  const fieldKeys = Object.keys(fields);
  return (
    <div className="border-line rounded-lg border p-3">
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <span className="bg-surface-subtle text-ink-body rounded-sm px-1.5 py-0.5 text-[10px] tracking-wider uppercase">
          #{op.op_index}
        </span>
        <span className="text-brand-700 bg-brand-50 rounded-sm px-2 py-0.5 text-[11px] font-medium">
          {op.type}
        </span>
        {/* Failed ops stay visible + clearly marked FAILED — never hidden. */}
        <TxStatusBadge
          successful={op.transaction_successful}
          result={op.transaction_result}
          code={op.result_code}
        />
        {op.tx_hash && (
          <Link
            href={`/transactions/${op.tx_hash}/`}
            className="text-brand-600 font-mono text-[11px] hover:underline"
            title={op.tx_hash}
          >
            tx {op.tx_hash.slice(0, 8)}…{op.tx_hash.slice(-6)}
          </Link>
        )}
        {op.ledger != null && (
          <Link
            href={`/ledgers/${op.ledger}/`}
            className="text-ink-muted hover:text-brand-600 font-mono text-[11px]"
          >
            #{op.ledger.toLocaleString('en-US')}
          </Link>
        )}
        {op.close_time && (
          <span
            className="text-ink-faint font-mono text-[11px]"
            title={formatTimestamp(op.close_time)}
          >
            {relativeAge(op.close_time)}
          </span>
        )}
      </div>
      {fieldKeys.length > 0 ? (
        <dl className="grid grid-cols-1 gap-x-6 gap-y-1.5 sm:grid-cols-2">
          {fieldKeys.map((k) => (
            <div key={k} className="flex items-baseline gap-2">
              <dt className="text-ink-muted shrink-0 text-[11px] tracking-wider uppercase">
                {k}
              </dt>
              <dd className="text-ink-body font-mono text-xs break-all">
                {renderOpFieldValue(k, fields[k])}
              </dd>
            </div>
          ))}
        </dl>
      ) : (
        <p className="text-ink-faint text-xs">No decoded fields.</p>
      )}
      {op.raw_xdr && (
        <details className="border-line mt-2 rounded-sm border">
          <summary className="text-ink-muted hover:text-brand-600 cursor-pointer px-2 py-1 text-[11px] font-medium">
            Raw XDR
          </summary>
          <pre className="border-line text-ink-body overflow-x-auto border-t px-2 py-2 font-mono text-[10px] leading-relaxed break-all whitespace-pre-wrap">
            {op.raw_xdr}
          </pre>
        </details>
      )}
    </div>
  );
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n)}…` : s;
}

function Th({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <th
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
      scope="col"
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <td
      className={`px-4 py-3 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}
