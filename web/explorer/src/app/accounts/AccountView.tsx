'use client';

import { useState, type ReactNode } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useQuery, keepPreviousData } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { InlineBar } from '@/components/ContractCharts';
import { CrossReference } from '@/components/CrossReference';
import {
  DirectoryLabel,
  type DirectoryInfo,
} from '@/components/DirectoryLabel';
import {
  Badge,
  Button,
  ButtonLink,
  Callout,
  Container,
  Fact,
  FactList,
  Mono,
  PageHeader,
  Skeleton,
  Stat,
  StatCell,
  StatGrid,
  Table,
  TBody,
  Td,
  Th,
  THead,
  TR,
  TxStatusBadge,
  LinkRow,
} from '@/components/ui';
import { AccountClaimablePanel } from './AccountClaimable';
import { AccountSignerOnPanel } from './AccountSignerOn';
import { apiGet, asExample } from '@/api/client';
import { ISSUER_LOOKUP_LIMIT, useIssuers } from '@/api/hooks';
import { cn } from '@/lib/cn';
import {
  DIRECTORY_OPERATOR_OVERRIDE_SOURCE,
  hasDirectoryScamFlag,
} from '@/lib/directory-tags';
import {
  formatFractionPrice,
  formatUsdWhole,
  formatWhole,
  ratioPct,
  sumDecimalStrings,
  truncateMiddle,
} from '@/lib/format';
import { CURRENT_NETWORK } from '@/lib/networks';
import { muxedBaseAccount } from '@/lib/strkey';
import { isSafeHomeDomain } from '@/lib/safe-domain';
import {
  type Envelope,
  type AccountTransactionsResp,
  type AccountOperationsResp,
  type LedgerTransaction,
  type TxOperation,
  OpBody,
  formatTimestamp,
  relativeAge,
  stroopsToXlm,
  xlmReadable,
} from '../explorer-shared';
import { AccountPositions } from './AccountPositions';
import { AccountMovementsPanel } from './AccountMovements';
import { AccountDefiPositionsPanel } from './AccountDefiPositions';
import { AccountActivitySummaryPanel } from './AccountActivitySummary';
import { AccountTradesPanel } from './AccountTrades';
import { AccountGraphPanel } from './AccountGraph';
import { AccountSponsorshipsPanel } from './AccountSponsorships';
import { AccountsAnalytics } from './AccountsAnalytics';
import { ACCOUNT_ID_RE as ACCOUNT_RE } from '@/lib/preload/manifest';
const PAGE_SIZE = 50;

/** Cursor paging that only walks backwards through history, like the API. */
function useOlderPager() {
  const [cursor, setCursor] = useState('');
  const [page, setPage] = useState(1);
  return {
    cursor,
    page,
    older(next: string | undefined) {
      if (!next) return;
      setCursor(next);
      setPage((p) => p + 1);
    },
    newest() {
      setCursor('');
      setPage(1);
    },
  };
}

export function AccountView({ id: idProp }: { id?: string } = {}) {
  const params = useSearchParams();
  const id = (idProp ?? params.get('id') ?? '').trim();
  const looksValid = ACCOUNT_RE.test(id);
  const tx = useOlderPager();
  const ops = useOlderPager();

  const txQ = useQuery<AccountTransactionsResp>({
    queryKey: ['/v1/accounts/{id}/transactions', id, tx.cursor],
    enabled: id.length > 0 && looksValid,
    retry: false,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountTransactionsResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/transactions`,
        { limit: PAGE_SIZE, ...(tx.cursor ? { cursor: tx.cursor } : {}) },
      );
      return env.data;
    },
    staleTime: 30_000,
  });
  const opsQ = useQuery<AccountOperationsResp>({
    queryKey: ['/v1/accounts/{id}/operations', id, ops.cursor],
    enabled: id.length > 0 && looksValid,
    retry: false,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountOperationsResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/operations`,
        { limit: PAGE_SIZE, ...(ops.cursor ? { cursor: ops.cursor } : {}) },
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
  const issuersQ = useIssuers(ISSUER_LOOKUP_LIMIT);
  const isKnownIssuer = (issuersQ.data ?? []).some(
    (iss) => iss.g_strkey === id,
  );

  if (!looksValid) {
    const base = muxedBaseAccount(id);
    const isContract = /^C[A-Z2-7]{55}$/.test(id);
    return (
      <Shell id={id}>
        <Panel
          title={
            base
              ? 'Muxed account'
              : isContract
                ? 'Contract address'
                : 'Invalid account ID'
          }
          bodyClassName="space-y-3 text-sm text-ink-body"
        >
          <Mono value={id} className="break-all" />
          {base ? (
            <p>
              <span title="An M-address is a G-account plus a routing ID; state and activity live on the G-account.">
                Base account:
              </span>{' '}
              <Link
                href={`/accounts/${base}/`}
                className="text-brand-600 font-mono break-all hover:underline"
              >
                {base}
              </Link>
            </p>
          ) : isContract ? (
            <p>
              <Link
                href={`/contracts/${id}/`}
                className="text-brand-600 hover:underline"
              >
                Open contract
              </Link>
            </p>
          ) : (
            <p className="text-ink-muted">Not a G-, M- or C-address.</p>
          )}
        </Panel>
      </Shell>
    );
  }

  const state = stateQ.data;
  return (
    <Shell
      id={id}
      title={state?.directory?.name ?? 'Account'}
      meta={<Mono value={id} copy className="break-all" />}
      actions={
        isKnownIssuer ? (
          <ButtonLink
            href={`/issuers/${encodeURIComponent(id)}`}
            variant="secondary"
            size="sm"
            title="Issued assets, supply and authorization flags"
          >
            View as issuer
          </ButtonLink>
        ) : undefined
      }
    >
      <AccountStatePanel
        id={id}
        state={state}
        isLoading={stateQ.isLoading}
        isError={stateQ.isError}
        onRetry={() => stateQ.refetch()}
      />
      <AccountPositions id={id} />
      {state?.exists && <AccountEntriesPanels state={state} />}
      <AccountClaimablePanel id={id} />
      <AccountSignerOnPanel id={id} />
      <AccountActivitySummaryPanel id={id} />
      <AccountMovementsPanel id={id} />
      <AccountDefiPositionsPanel id={id} />
      <AccountTradesPanel id={id} />
      <AccountGraphPanel id={id} />
      <AccountSponsorshipsPanel id={id} />
      <TransactionsPanel
        id={id}
        isLoading={txQ.isLoading}
        isError={txQ.isError}
        isFetching={txQ.isFetching}
        error={txQ.error}
        data={txQ.data}
        page={tx.page}
        onOlder={
          txQ.data?.next_cursor
            ? () => tx.older(txQ.data?.next_cursor)
            : undefined
        }
        onNewest={tx.cursor ? tx.newest : undefined}
        onRetry={() => txQ.refetch()}
      />
      <OperationsPanel
        id={id}
        isLoading={opsQ.isLoading}
        isError={opsQ.isError}
        isFetching={opsQ.isFetching}
        error={opsQ.error}
        data={opsQ.data}
        page={ops.page}
        onOlder={
          opsQ.data?.next_cursor
            ? () => ops.older(opsQ.data?.next_cursor)
            : undefined
        }
        onNewest={ops.cursor ? ops.newest : undefined}
        onRetry={() => opsQ.refetch()}
      />
    </Shell>
  );
}

// ---------------------------------------------------------------------------
// /accounts directory
// ---------------------------------------------------------------------------

interface AccountsListResp {
  priced_assets: number;
  ranked_by?: 'usd' | 'native_xlm';
  accounts: {
    account_id: string;
    value?: string;
    usd_value?: string;
    locked?: boolean;
  }[];
}

const DIRECTORY_SIZE = 100;

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

// Directory names for the listed rows: one batch read of GET /v1/directory
// (cap 100 = DIRECTORY_SIZE). A failed read leaves the rows bare.
function useDirectoryNames(addresses: string[]) {
  return useQuery<Record<string, DirectoryInfo>>({
    queryKey: ['/v1/directory', addresses],
    enabled: addresses.length > 0,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<
        Envelope<{ entries: Record<string, DirectoryInfo> }>
      >('/v1/directory', { addresses: addresses.join(',') });
      return env.data.entries;
    },
    staleTime: 10 * 60_000,
  });
}

function rankedInNative(data: AccountsListResp | undefined): boolean {
  return data ? data.ranked_by === 'native_xlm' : !CURRENT_NETWORK.pricing;
}

export function AccountsDirectoryHeader() {
  return (
    <PageHeader
      breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Accounts' }]}
      title="Accounts"
    />
  );
}

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
    const raw = row.value ?? row.usd_value ?? '0';
    if (!isNative) return formatUsdWhole(raw);
    const xlm = formatWhole(raw);
    return xlm === '—' ? xlm : `${xlm} XLM`;
  };
  const rows = q.data?.accounts ?? [];
  const names = useDirectoryNames(rows.map((a) => a.account_id)).data;
  // Shares are of the LISTED accounts' total (the served page), not of the
  // network, and divide in BigInt so balances above 2^53 stay exact.
  const listedTotal = sumDecimalStrings(
    rows.flatMap((a) => {
      const v = a.value ?? a.usd_value;
      return v != null ? [v] : [];
    }),
  );
  const share = (row: { value?: string; usd_value?: string }) => {
    const pct = ratioPct(row.value ?? row.usd_value, listedTotal, 1);
    return pct == null ? '—' : `${pct.toFixed(1)}%`;
  };
  return (
    <Panel
      title={isNative ? 'Ranked by XLM balance' : 'Ranked by USD wealth'}
      hint={`Top ${DIRECTORY_SIZE} in the captured ledger state`}
      source={asExample('/v1/accounts', { limit: DIRECTORY_SIZE })}
      bodyClassName="-mx-4 -mb-4"
    >
      {q.isLoading && (
        <div className="space-y-2 px-4 pb-4">
          <Skeleton className="h-5 w-64" />
          <Skeleton className="h-5 w-80" />
          <Skeleton className="h-5 w-72" />
        </div>
      )}
      {q.isError && (
        <p className="text-ink-muted px-4 pb-4 text-sm">
          The accounts directory is unavailable right now (the current-state
          projection is still backfilling).
          <RetryButton onClick={() => q.refetch()} />
        </p>
      )}
      {q.data && rows.length === 0 && (
        <p className="text-ink-muted px-4 pb-4 text-sm">
          {isNative ? 'No accounts captured yet.' : 'No priced accounts yet.'}
        </p>
      )}
      {q.data && rows.length > 0 && (
        <>
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <Th className="w-12" align="right">
                    #
                  </Th>
                  <Th>Account</Th>
                  <Th align="right">
                    {isNative ? 'XLM balance' : 'USD value'}
                  </Th>
                  <Th align="right">Share of listed</Th>
                </tr>
              </THead>
              <TBody>
                {rows.map((a, i) => (
                  <LinkRow
                    href={`/accounts/${encodeURIComponent(a.account_id)}/`}
                    key={a.account_id}
                  >
                    <Td
                      align="right"
                      className="text-ink-faint font-mono tabular-nums"
                    >
                      {i + 1}
                    </Td>
                    <Td>
                      <Link
                        href={`/accounts/${encodeURIComponent(a.account_id)}/`}
                        className="text-ink hover:text-brand-600 font-mono"
                        title={a.account_id}
                      >
                        {truncateMiddle(a.account_id, 10, 8)}
                      </Link>
                      {names?.[a.account_id] && (
                        <span
                          className={cn(
                            'ml-2',
                            hasDirectoryScamFlag(names[a.account_id].tags)
                              ? 'text-bad-700 font-medium'
                              : 'text-ink-muted',
                          )}
                          title="Label from the StellarExpert public directory (community-curated; listing is not endorsement)"
                        >
                          {names[a.account_id].name}
                        </span>
                      )}
                      {names?.[a.account_id] &&
                        hasDirectoryScamFlag(names[a.account_id].tags) && (
                          <Badge className="ml-2" tone="bad">
                            flagged
                          </Badge>
                        )}
                      {a.locked && (
                        <Badge
                          className="ml-2"
                          title="Provably unspendable — master weight 0, all thresholds 0, no signers. The balance is real; no key can ever move it (e.g. the SDF burn address)."
                        >
                          locked
                        </Badge>
                      )}
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      {fmtWealth(a)}
                    </Td>
                    <Td
                      align="right"
                      className="text-ink-muted font-mono tabular-nums"
                    >
                      {share(a)}
                    </Td>
                  </LinkRow>
                ))}
              </TBody>
            </Table>
          </div>
          <p className="border-line text-ink-muted border-t px-4 py-3 text-xs">
            {isNative
              ? 'XLM balance · accounts outside the captured window are excluded'
              : `Summed across ${q.data.priced_assets} priced asset${q.data.priced_assets === 1 ? '' : 's'} · wealth outside the captured window is excluded`}
          </p>
        </>
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// Account state (the captured ledger entry)
// ---------------------------------------------------------------------------

interface AccountStateResp {
  account_id: string;
  exists: boolean;
  balance?: string;
  seq_num?: string;
  num_subentries?: number;
  num_sponsoring?: number;
  num_sponsored?: number;
  buying_liabilities?: string;
  selling_liabilities?: string;
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
  as_of_ledger?: number;
  coverage_note?: string;
  directory?: DirectoryInfo;
}

const ACCOUNT_FLAGS: [number, string][] = [
  [1, 'auth required'],
  [2, 'auth revocable'],
  [4, 'auth immutable'],
  [8, 'clawback enabled'],
];
const TRUSTLINE_FLAGS: [number, string][] = [
  [1, 'authorized'],
  [2, 'maintain liabilities'],
  [4, 'clawback'],
];

function flagLabels(flags: number | undefined, table: [number, string][]) {
  if (!flags) return [];
  return table.filter(([bit]) => (flags & bit) !== 0).map(([, l]) => l);
}

const STATE_SCOPE_TITLE =
  "Balances, trustlines and offers reflect the lake's captured ledger-entry window; the activity panels show all history, including where the account is a participant (incoming payments, trustlines, merges).";

function AccountStatePanel({
  id,
  state,
  isLoading,
  isError,
  onRetry,
}: {
  id: string;
  state: AccountStateResp | undefined;
  isLoading: boolean;
  isError: boolean;
  onRetry: () => void;
}) {
  const source = asExample(`/v1/accounts/${id}`);
  if (isLoading) {
    return (
      <Panel title="Account" source={source} bodyClassName="space-y-4">
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-5 w-72" />
      </Panel>
    );
  }
  if (isError) {
    return (
      <Panel
        title="Account"
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        The account-state lookup failed — reload to retry. Activity below is
        unaffected.
        <RetryButton onClick={onRetry} />
      </Panel>
    );
  }
  if (!state || !state.exists) {
    return (
      <Panel
        title="Account"
        source={source}
        bodyClassName="space-y-4 text-sm text-ink-muted"
      >
        <p>
          No ledger entry captured for this account — it was merged away, never
          funded, or the state projection has not reached it yet. The activity
          below is all history and is unaffected.
        </p>
        <CrossReference kind="account" id={id} />
      </Panel>
    );
  }

  const t = state.thresholds;
  const signers = state.signers ?? [];
  const trustlines = state.trustlines ?? [];
  const offers = state.offers ?? [];
  const flags = flagLabels(state.flags, ACCOUNT_FLAGS);
  const sponsorship =
    state.num_sponsored != null || state.num_sponsoring != null
      ? `${state.num_sponsored ?? 0} sponsored · ${state.num_sponsoring ?? 0} sponsoring`
      : undefined;
  const liabilities =
    state.selling_liabilities && state.selling_liabilities !== '0'
      ? `${xlmReadable(state.selling_liabilities)} XLM in open offers`
      : 'Stellar Lumens';

  return (
    <Panel title="Account" source={source} bodyClassName="space-y-5">
      <StatGrid cols={4} className="grid-cols-2">
        <StatCell>
          <Stat
            label="XLM balance"
            value={xlmReadable(state.balance ?? '0')}
            sub={liabilities}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Trustlines"
            value={trustlines.length}
            sub={`${offers.length} open offer${offers.length === 1 ? '' : 's'}`}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Signers"
            value={signers.length}
            sub={t ? `master weight ${t.master}` : undefined}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Sub-entries"
            value={state.num_subentries ?? 0}
            sub={sponsorship}
          />
        </StatCell>
      </StatGrid>

      <FactList columns={3}>
        <Fact label="Home domain" mono={false}>
          {isSafeHomeDomain(state.home_domain) ? (
            <a
              href={`https://${state.home_domain}`}
              target="_blank"
              rel="noreferrer noopener nofollow"
              className="text-ink hover:text-brand-600 hover:underline"
            >
              {state.home_domain}
            </a>
          ) : state.home_domain ? (
            <span
              className="text-ink-muted break-all"
              title="Not linked: this account-supplied domain is not a plain hostname"
            >
              {state.home_domain}
            </span>
          ) : (
            <span className="text-ink-faint">—</span>
          )}
        </Fact>
        <Fact label="Sequence">{state.seq_num ?? '—'}</Fact>
        <Fact
          label="Thresholds"
          title="Low / medium / high operation thresholds; a transaction needs signer weight at or above the threshold of its most demanding operation."
        >
          {t ? `${t.low} / ${t.med} / ${t.high}` : '—'}
        </Fact>
        <Fact label="Master weight">{t ? String(t.master) : '—'}</Fact>
        <Fact label="Flags" mono={false}>
          {flags.length > 0 ? (
            <span className="inline-flex flex-wrap gap-1.5">
              {flags.map((f) => (
                <Badge key={f}>{f}</Badge>
              ))}
            </span>
          ) : (
            <span className="text-ink-muted">none set</span>
          )}
        </Fact>
        <Fact label="Last modified">
          {state.last_modified_ledger != null ? (
            <Link
              href={`/ledgers/${state.last_modified_ledger}/`}
              className="text-brand-600 hover:underline"
            >
              #{state.last_modified_ledger}
            </Link>
          ) : (
            '—'
          )}
        </Fact>
        {state.directory && (
          <Fact label="Directory" mono={false} className="sm:col-span-2">
            <DirectoryFact info={state.directory} />
          </Fact>
        )}
      </FactList>

      <div className="text-ink-muted space-y-1 text-xs">
        <p title={STATE_SCOPE_TITLE}>
          State as of ledger{' '}
          {state.as_of_ledger != null ? `#${state.as_of_ledger}` : '—'} ·
          activity panels cover all history
        </p>
        {state.coverage_note && <p>{state.coverage_note}</p>}
      </div>
      <CrossReference kind="account" id={id} />
    </Panel>
  );
}

function DirectoryFact({ info }: { info: DirectoryInfo }) {
  if (
    hasDirectoryScamFlag(info.tags) ||
    info.source === DIRECTORY_OPERATOR_OVERRIDE_SOURCE
  ) {
    return <DirectoryLabel info={info} />;
  }
  return (
    <span
      className="inline-flex flex-wrap items-center gap-2"
      title="Label from the StellarExpert public directory (community-curated; listing is not endorsement)"
    >
      <span className="text-ink font-medium">{info.name}</span>
      {info.tags.map((tag) => (
        <Badge key={tag}>#{tag}</Badge>
      ))}
      <span className="text-ink-muted text-xs">StellarExpert directory</span>
    </span>
  );
}

/** Signers, trustlines and open offers: the account's sub-entries as captured. */
function AccountEntriesPanels({ state }: { state: AccountStateResp }) {
  const signers = state.signers ?? [];
  const trustlines = state.trustlines ?? [];
  const offers = state.offers ?? [];
  const source = asExample(`/v1/accounts/${state.account_id}`);
  const side = [signers.length > 0, offers.length > 0].filter(Boolean).length;
  return (
    <>
      {trustlines.length > 0 && (
        <Panel
          title={`Trustlines (${trustlines.length})`}
          hint="Classic assets this account can hold"
          source={source}
          bodyClassName="-mx-4 -mb-4"
        >
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <Th>Asset</Th>
                  <Th align="right">Balance</Th>
                  <Th align="right">Limit</Th>
                  <Th align="right">Limit used</Th>
                  <Th>Authorization</Th>
                </tr>
              </THead>
              <TBody>
                {trustlines.map((tl) => {
                  const auth = flagLabels(tl.flags, TRUSTLINE_FLAGS);
                  return (
                    <TR key={tl.asset}>
                      <Td>
                        <AssetLink canonical={tl.asset} />
                      </Td>
                      <Td align="right" className="font-mono tabular-nums">
                        {xlmReadable(tl.balance)}
                      </Td>
                      <Td
                        align="right"
                        className="text-ink-muted font-mono tabular-nums"
                      >
                        {xlmReadable(tl.limit)}
                      </Td>
                      <Td align="right">
                        <TrustlineUse balance={tl.balance} limit={tl.limit} />
                      </Td>
                      <Td className="text-ink-muted text-xs">
                        {auth.length > 0 ? auth.join(' · ') : 'not authorized'}
                      </Td>
                    </TR>
                  );
                })}
              </TBody>
            </Table>
          </div>
        </Panel>
      )}
      {side > 0 && (
        <div className={cn('grid gap-6', side === 2 && 'lg:grid-cols-2')}>
          {signers.length > 0 && (
            <Panel
              title={`Signers (${signers.length})`}
              hint="Besides the master key"
              source={source}
              bodyClassName="-mx-4 -mb-4"
            >
              <div className="overflow-x-auto">
                <Table>
                  <THead>
                    <tr>
                      <Th>Key</Th>
                      <Th align="right">Weight</Th>
                    </tr>
                  </THead>
                  <TBody>
                    {signers.map((s) => (
                      <LinkRow href={`/accounts/${s.key}/`} key={s.key}>
                        <Td>
                          {ACCOUNT_RE.test(s.key) ? (
                            <Link
                              href={`/accounts/${s.key}/`}
                              className="text-brand-600 font-mono hover:underline"
                              title={s.key}
                            >
                              {truncateMiddle(s.key, 8, 6)}
                            </Link>
                          ) : (
                            <span
                              className="text-ink-body font-mono"
                              title={s.key}
                            >
                              {truncateMiddle(s.key, 8, 6)}
                            </span>
                          )}
                        </Td>
                        <Td align="right" className="font-mono tabular-nums">
                          {s.weight}
                        </Td>
                      </LinkRow>
                    ))}
                  </TBody>
                </Table>
              </div>
            </Panel>
          )}
          {offers.length > 0 && (
            <Panel
              title={`Open offers (${offers.length})`}
              hint="Resting on the SDEX order book"
              source={source}
              bodyClassName="-mx-4 -mb-4"
            >
              <div className="overflow-x-auto">
                <Table>
                  <THead>
                    <tr>
                      <Th>Offer</Th>
                      <Th>Selling</Th>
                      <Th>Buying</Th>
                      <Th align="right">Amount</Th>
                      <Th align="right">Price</Th>
                    </tr>
                  </THead>
                  <TBody>
                    {offers.map((o) => (
                      <TR key={o.offer_id}>
                        <Td className="text-ink-faint font-mono tabular-nums">
                          #{o.offer_id}
                        </Td>
                        <Td>
                          <AssetLink canonical={o.selling} />
                        </Td>
                        <Td>
                          <AssetLink canonical={o.buying} />
                        </Td>
                        <Td align="right" className="font-mono tabular-nums">
                          {xlmReadable(o.amount)}
                        </Td>
                        <Td
                          align="right"
                          className="font-mono tabular-nums"
                          title={`${o.price_n}/${o.price_d}`}
                        >
                          {formatFractionPrice(o.price_n, o.price_d)}
                        </Td>
                      </TR>
                    ))}
                  </TBody>
                </Table>
              </div>
            </Panel>
          )}
        </div>
      )}
    </>
  );
}

function TrustlineUse({ balance, limit }: { balance: string; limit: string }) {
  const pct = ratioPct(balance, limit, 1);
  if (pct === null) return <span className="text-ink-faint">—</span>;
  return (
    <span className="inline-flex items-center gap-2">
      <InlineBar value={pct} max={100} label={`${pct}% of limit used`} />
      <span className="text-ink-muted w-12 font-mono text-xs tabular-nums">
        {pct}%
      </span>
    </span>
  );
}

// ---------------------------------------------------------------------------
// Shared chrome
// ---------------------------------------------------------------------------

function Shell({
  id,
  title = 'Account',
  meta,
  actions,
  children,
}: {
  id: string | null;
  title?: string;
  meta?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Accounts', href: '/accounts' },
          { label: id ? `${id.slice(0, 8)}…${id.slice(-6)}` : 'Account' },
        ]}
        title={title}
        meta={meta}
        actions={actions}
      />
      {children}
    </Container>
  );
}

function RetryButton({ onClick }: { onClick: () => void }) {
  return (
    <Button variant="secondary" size="sm" className="ml-3" onClick={onClick}>
      Retry
    </Button>
  );
}

function ActivityPager({
  page,
  isFetching,
  onOlder,
  onNewest,
}: {
  page: number;
  isFetching?: boolean;
  onOlder?: () => void;
  onNewest?: () => void;
}) {
  if (!onOlder && !onNewest) return null;
  return (
    <div className="border-line flex items-center justify-between gap-2 border-t px-4 py-3">
      <span className="text-ink-muted tnum text-xs">
        Page {page}
        {isFetching && ' · loading…'}
      </span>
      <div className="flex gap-2">
        {onNewest && (
          <Button variant="secondary" size="sm" onClick={onNewest}>
            Newest
          </Button>
        )}
        {onOlder && (
          <Button variant="secondary" size="sm" onClick={onOlder}>
            Load older
          </Button>
        )}
      </div>
    </div>
  );
}

type HistoryPanelProps<T> = {
  id: string;
  isLoading: boolean;
  isError: boolean;
  isFetching: boolean;
  error: unknown;
  data: T | undefined;
  page: number;
  onOlder?: () => void;
  onNewest?: () => void;
  onRetry: () => void;
};

const HISTORY_HINT = 'Sourced + incoming · newest first';

function SourceCell({ source, id }: { source?: string; id: string }) {
  if (!source || source === id) {
    return <span className="text-ink-muted text-xs">this account</span>;
  }
  return (
    <Link
      href={`/accounts/${source}/`}
      className="text-brand-600 font-mono text-xs hover:underline"
      title={`Initiated by ${source} — this account participates`}
    >
      {truncateMiddle(source, 5, 5)}
    </Link>
  );
}

function TransactionsPanel({
  id,
  isLoading,
  isError,
  isFetching,
  error,
  data,
  page,
  onOlder,
  onNewest,
  onRetry,
}: HistoryPanelProps<AccountTransactionsResp>) {
  const source = asExample(`/v1/accounts/${id}/transactions`, {
    limit: PAGE_SIZE,
  });
  if (isError) {
    return (
      <Panel
        title="Transactions"
        hint={HISTORY_HINT}
        source={source}
        bodyClassName="text-sm text-ink-body"
      >
        No transactions for that account in the served tier, or the lookup
        failed: {error instanceof Error ? error.message : 'unknown error'}.
        <RetryButton onClick={onRetry} />
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel title="Transactions" hint={HISTORY_HINT} source={source}>
        <Skeleton className="h-5 w-72" />
      </Panel>
    );
  }
  const transactions = data.transactions ?? [];
  const pager = (
    <ActivityPager
      page={page}
      isFetching={isFetching}
      onOlder={onOlder}
      onNewest={onNewest}
    />
  );
  if (transactions.length === 0) {
    return (
      <Panel
        title="Transactions"
        hint={HISTORY_HINT}
        source={source}
        bodyClassName="-mx-4 -mb-4"
      >
        <p className="text-ink-muted px-4 pb-4 text-sm">
          {data.next_cursor
            ? 'No visible items on this page — older history continues.'
            : 'No transactions observed for this account yet.'}
        </p>
        {pager}
      </Panel>
    );
  }
  return (
    <Panel
      title={`Transactions (${transactions.length}${data.next_cursor ? '+' : ''})`}
      hint={HISTORY_HINT}
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Hash</Th>
              <Th>Time</Th>
              <Th>Ledger</Th>
              <Th>Source</Th>
              <Th align="right">Ops</Th>
              <Th>Result</Th>
              <Th align="right">Fee</Th>
              <Th>Memo</Th>
            </tr>
          </THead>
          <TBody>
            {transactions.map((t: LedgerTransaction) => (
              <LinkRow href={`/transactions/${t.hash}/`} key={t.hash}>
                <Td className="whitespace-nowrap">
                  <Link
                    href={`/transactions/${t.hash}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                    title={t.hash}
                  >
                    {truncateMiddle(t.hash ?? '', 8, 6)}
                  </Link>
                </Td>
                <Td
                  className="text-ink-muted text-xs whitespace-nowrap"
                  title={t.close_time ? formatTimestamp(t.close_time) : ''}
                >
                  {t.close_time ? relativeAge(t.close_time) : '—'}
                </Td>
                <Td>
                  <Link
                    href={`/ledgers/${t.ledger}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                  >
                    #{t.ledger}
                  </Link>
                </Td>
                <Td className="whitespace-nowrap">
                  <SourceCell source={t.source_account} id={id} />
                </Td>
                <Td align="right" className="font-mono tabular-nums">
                  {t.operation_count}
                </Td>
                <Td>
                  <TxStatusBadge
                    successful={t.successful}
                    result={t.result}
                    code={t.result_code}
                  />
                </Td>
                <Td
                  align="right"
                  className="text-ink-muted font-mono text-xs tabular-nums"
                >
                  {t.fee_charged != null ? stroopsToXlm(t.fee_charged) : '—'}
                </Td>
                <Td className="whitespace-nowrap">
                  {t.memo_type && t.memo_type !== 'none' ? (
                    <span className="text-xs" title={t.memo ?? ''}>
                      <span className="text-ink-muted">{t.memo_type}</span>
                      {t.memo && (
                        <span className="text-ink-body ml-1.5 font-mono">
                          {truncate(t.memo, 18)}
                        </span>
                      )}
                    </span>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
              </LinkRow>
            ))}
          </TBody>
        </Table>
      </div>
      {pager}
    </Panel>
  );
}

function OperationsPanel({
  id,
  isLoading,
  isError,
  isFetching,
  error,
  data,
  page,
  onOlder,
  onNewest,
  onRetry,
}: HistoryPanelProps<AccountOperationsResp>) {
  const source = asExample(`/v1/accounts/${id}/operations`, {
    limit: PAGE_SIZE,
  });
  if (isError) {
    return (
      <Panel
        title="Operations"
        hint={HISTORY_HINT}
        source={source}
        bodyClassName="text-sm text-ink-body"
      >
        No operations for that account in the served tier, or the lookup failed:{' '}
        {error instanceof Error ? error.message : 'unknown error'}.
        <RetryButton onClick={onRetry} />
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel title="Operations" hint={HISTORY_HINT} source={source}>
        <Skeleton className="h-5 w-72" />
      </Panel>
    );
  }
  const operations = data.operations ?? [];
  const pager = (
    <ActivityPager
      page={page}
      isFetching={isFetching}
      onOlder={onOlder}
      onNewest={onNewest}
    />
  );
  if (operations.length === 0) {
    return (
      <Panel
        title="Operations"
        hint={HISTORY_HINT}
        source={source}
        bodyClassName="-mx-4 -mb-4"
      >
        <p className="text-ink-muted px-4 pb-4 text-sm">
          {data.next_cursor
            ? 'No visible items on this page — older history continues.'
            : 'No operations observed for this account yet.'}
        </p>
        {pager}
      </Panel>
    );
  }
  return (
    <Panel
      title={`Operations (${operations.length}${data.next_cursor ? '+' : ''})`}
      hint={HISTORY_HINT}
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      {data.coverage_note && (
        <div className="px-4 pb-4">
          <Callout
            tone="warn"
            title="Transaction outcomes partially unavailable"
          >
            {data.coverage_note}
          </Callout>
        </div>
      )}
      <ol className="divide-line border-line divide-y border-t">
        {operations.map((op: TxOperation, i: number) => (
          <OperationRow
            key={`${op.tx_hash ?? ''}-${op.op_index}-${i}`}
            op={op}
          />
        ))}
      </ol>
      {pager}
    </Panel>
  );
}

function OperationRow({ op }: { op: TxOperation }) {
  return (
    <li className="px-4 py-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        <span className="text-ink text-sm font-medium">{op.type}</span>
        <span className="text-ink-faint font-mono">#{op.op_index}</span>
        <TxStatusBadge
          successful={op.transaction_successful}
          result={op.transaction_result}
          code={op.result_code}
        />
        <span className="ml-auto flex flex-wrap items-center gap-x-3 gap-y-1 font-mono">
          {op.tx_hash && (
            <Link
              href={`/transactions/${op.tx_hash}/`}
              className="text-brand-600 hover:underline"
              title={op.tx_hash}
            >
              {truncateMiddle(op.tx_hash, 8, 6)}
            </Link>
          )}
          {op.ledger != null && (
            <Link
              href={`/ledgers/${op.ledger}/`}
              className="text-ink-muted hover:text-brand-600"
            >
              #{op.ledger}
            </Link>
          )}
          {op.close_time && (
            <span
              className="text-ink-faint"
              title={formatTimestamp(op.close_time)}
            >
              {relativeAge(op.close_time)}
            </span>
          )}
        </span>
      </div>
      <div className="mt-3">
        <OpBody op={op} />
      </div>
      {op.raw_xdr && (
        <details className="mt-2 text-xs">
          <summary className="text-ink-muted hover:text-brand-600 cursor-pointer">
            Raw XDR
          </summary>
          <pre className="text-ink-body mt-1 overflow-x-auto font-mono text-[11px] leading-relaxed break-all whitespace-pre-wrap">
            {op.raw_xdr}
          </pre>
        </details>
      )}
    </li>
  );
}

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n)}…` : s;
}
