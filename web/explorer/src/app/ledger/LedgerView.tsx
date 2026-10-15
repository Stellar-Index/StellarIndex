'use client';

import type { ReactNode } from 'react';
import { useSearchParams } from 'next/navigation';
import { useQuery, type UseQueryResult } from '@tanstack/react-query';
import { ChevronLeft, ChevronRight } from 'lucide-react';

import { Panel } from '@/components/reveal';
import { CrossReference } from '@/components/CrossReference';
import { OpsPerTxBars, ResultDonut } from '@/components/ChainCharts';
import {
  Button,
  ButtonLink,
  Callout,
  Container,
  EmptyState,
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
  TxStatusBadge,
  LinkRow,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import { totalCoinsCaption } from '@/lib/xlm-supply';
import {
  type Envelope,
  type Ledger,
  type LedgerTransactionsResp,
  HashLink,
  formatTimestamp,
  stroopsToXlm,
  stroopsToXlmFixed,
  xlmReadable,
} from '../explorer-shared';

/**
 * Client view for /ledgers/[seq] (and the legacy /ledger?seq=). Fetches the
 * ledger header and its transactions in parallel.
 */
export function LedgerView({ seq: seqProp }: { seq?: string } = {}) {
  // Path route /ledgers/[seq] passes `seq` as a prop; legacy /ledger?seq= reads
  // it from the query string (kept for redirect compatibility). Prop wins.
  const params = useSearchParams();
  const seqRaw = seqProp ?? params.get('seq') ?? '';
  const seq = /^\d+$/.test(seqRaw.trim()) ? Number(seqRaw.trim()) : null;

  const ledgerQ = useQuery<Ledger>({
    queryKey: ['/v1/ledgers/{seq}', seq],
    enabled: seq != null,
    queryFn: async () => {
      const env = await apiGet<Envelope<Ledger>>(`/v1/ledgers/${seq}`);
      return env.data;
    },
    staleTime: 60_000,
    retry: false,
  });
  const txQ = useQuery<LedgerTransactionsResp>({
    queryKey: ['/v1/ledgers/{seq}/transactions', seq],
    enabled: seq != null,
    queryFn: async () => {
      const env = await apiGet<Envelope<LedgerTransactionsResp>>(
        `/v1/ledgers/${seq}/transactions`,
      );
      return env.data;
    },
    staleTime: 60_000,
    retry: false,
  });

  if (seq == null) {
    return (
      <Shell seq={null}>
        <EmptyState
          title="No ledger selected"
          description="This page needs a ?seq= query parameter: a ledger sequence number. Browse recent ledgers and open one."
          action={
            <ButtonLink href="/ledgers" variant="secondary" size="sm">
              Browse ledgers
            </ButtonLink>
          }
        />
      </Shell>
    );
  }

  if (ledgerQ.isError) {
    return (
      <Shell seq={seq}>
        <EmptyState
          title="Ledger not found"
          description={`No ledger #${seq} in the served tier, or the lookup failed: ${errorMessage(ledgerQ.error)}.`}
          action={
            <div className="flex flex-wrap justify-center gap-2">
              <Button
                variant="secondary"
                size="sm"
                onClick={() => ledgerQ.refetch()}
              >
                Retry
              </Button>
              <ButtonLink href="/ledgers" variant="ghost" size="sm">
                Browse ledgers
              </ButtonLink>
            </div>
          }
        />
      </Shell>
    );
  }

  const ledger = ledgerQ.data;
  if (!ledger) {
    return (
      <Shell seq={seq}>
        <StatGrid cols={3}>
          {[0, 1, 2].map((i) => (
            <StatCell key={i}>
              <Skeleton className="h-3 w-24" />
              <Skeleton className="mt-3 h-7 w-16" />
            </StatCell>
          ))}
        </StatGrid>
        <Panel
          headingLevel={2}
          title="Ledger header"
          source={asExample(`/v1/ledgers/${seq}`)}
        >
          <Skeleton className="h-40" />
        </Panel>
      </Shell>
    );
  }

  return (
    <Shell seq={seq} ledger={ledger}>
      <StatGrid cols={3}>
        <StatCell>
          <Stat
            label="Transactions"
            value={count(ledger.tx_count)}
            sub="tx set · incl. failed"
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Operations"
            value={count(ledger.op_count)}
            sub="tx set · incl. ops of failed txs"
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Soroban events"
            value={
              ledger.soroban_event_count != null
                ? formatCompact(ledger.soroban_event_count)
                : '—'
            }
            sub="successful txs only"
          />
        </StatCell>
      </StatGrid>

      <Panel
        headingLevel={2}
        title="Ledger header"
        hint="Network-wide parameters in force when this ledger closed"
        source={asExample(`/v1/ledgers/${seq}`)}
      >
        <FactList columns={4}>
          <Fact label="Sequence">#{ledger.sequence}</Fact>
          <Fact label="Close time">{formatTimestamp(ledger.close_time)}</Fact>
          <Fact
            label="Protocol"
            title="Stellar protocol version in force when this ledger closed"
          >
            {ledger.protocol_version ?? '—'}
          </Fact>
          <Fact
            label="Base fee"
            title="Minimum fee per operation (1 XLM = 10,000,000 stroops); txs bid above it under surge pricing"
          >
            {ledger.base_fee ?? '—'} stroops
          </Fact>
          <Fact
            label="Base reserve"
            title="Minimum balance unit: an account holds 2 base reserves plus 1 per subentry"
          >
            {ledger.base_reserve != null
              ? `${stroopsToXlm(ledger.base_reserve)} XLM`
              : '—'}
          </Fact>
          <Fact label="Total coins" title={totalCoinsCaption()}>
            {xlmReadable(ledger.total_coins)} XLM
          </Fact>
          <Fact
            label="Fee pool"
            title="All fees collected network-wide; not redistributed since inflation ended"
          >
            {xlmReadable(ledger.fee_pool)} XLM
          </Fact>
          <Fact label="Hash" className="sm:col-span-2 lg:col-span-4">
            {ledger.hash ? <Mono value={ledger.hash} /> : '—'}
          </Fact>
          <Fact label="Previous hash" className="sm:col-span-2 lg:col-span-4">
            {ledger.prev_hash ? <Mono value={ledger.prev_hash} /> : '—'}
          </Fact>
        </FactList>
      </Panel>

      <TransactionsPanel seq={seq} q={txQ} />
    </Shell>
  );
}

function count(n: number | undefined): string {
  return n != null ? n.toLocaleString('en-US') : '—';
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : 'unknown error';
}

function Shell({
  seq,
  ledger,
  children,
}: {
  seq: number | null;
  ledger?: Ledger;
  children: ReactNode;
}) {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Ledgers', href: '/ledgers' },
          { label: seq != null ? `#${seq}` : 'Ledger' },
        ]}
        title={seq != null ? `Ledger #${seq}` : 'Ledger'}
        meta={
          ledger
            ? `Closed ${formatTimestamp(ledger.close_time)} · protocol ${ledger.protocol_version ?? '—'}`
            : undefined
        }
        actions={seq != null ? <LedgerNav seq={seq} /> : undefined}
      />
      {children}
      {seq != null && <CrossReference kind="ledger" id={String(seq)} />}
    </Container>
  );
}

function LedgerNav({ seq }: { seq: number }) {
  return (
    <>
      {seq > 1 ? (
        <ButtonLink href={`/ledgers/${seq - 1}/`} variant="secondary" size="sm">
          <ChevronLeft className="size-3.5" aria-hidden />
          Previous ledger
        </ButtonLink>
      ) : (
        <Button variant="secondary" size="sm" disabled aria-disabled="true">
          <ChevronLeft className="size-3.5" aria-hidden />
          Previous ledger
        </Button>
      )}
      <ButtonLink href={`/ledgers/${seq + 1}/`} variant="secondary" size="sm">
        Next ledger
        <ChevronRight className="size-3.5" aria-hidden />
      </ButtonLink>
    </>
  );
}

const TX_CSV_COLUMNS = [
  'hash',
  'source_account',
  'operation_count',
  'successful',
  'result_code',
  'fee_charged',
  'memo_type',
  'memo',
];

function TransactionsPanel({
  seq,
  q,
}: {
  seq: number;
  q: UseQueryResult<LedgerTransactionsResp>;
}) {
  const source = asExample(`/v1/ledgers/${seq}/transactions`);
  if (q.isError) {
    return (
      <Callout tone="bad" title="Transactions didn't load">
        <span>Failed to load transactions: {errorMessage(q.error)}.</span>
        <Button
          variant="secondary"
          size="sm"
          className="ml-3"
          onClick={() => q.refetch()}
        >
          Retry
        </Button>
      </Callout>
    );
  }
  if (!q.data) {
    return (
      <Panel headingLevel={2} title="Transactions" source={source}>
        <div className="space-y-2">
          <Skeleton className="h-5 w-full" />
          <Skeleton className="h-5 w-11/12" />
          <Skeleton className="h-5 w-4/5" />
        </div>
      </Panel>
    );
  }
  const transactions = q.data.transactions ?? [];
  if (transactions.length === 0) {
    return (
      <Panel headingLevel={2} title="Transactions" source={source}>
        <p className="text-ink-muted text-sm">
          This ledger closed no transactions.
        </p>
      </Panel>
    );
  }
  const okCount = transactions.filter((t) => t.successful).length;
  return (
    <>
      <Panel
        headingLevel={2}
        title="Transaction mix"
        hint="Outcome and operations per transaction across the tx set"
        source={source}
      >
        <div className="grid gap-6 sm:grid-cols-2">
          <ResultDonut ok={okCount} failed={transactions.length - okCount} />
          <OpsPerTxBars
            opCounts={transactions.map((t) => t.operation_count ?? 0)}
          />
        </div>
      </Panel>
      <Panel
        headingLevel={2}
        title="Transactions"
        hint={`${transactions.length} in the tx set, failed included`}
        source={source}
        download={{
          name: `ledger-${seq}-transactions`,
          columns: TX_CSV_COLUMNS,
          rows: transactions.map((t) => ({
            hash: t.hash,
            source_account: t.source_account,
            operation_count: t.operation_count,
            successful: t.successful,
            result_code: t.result_code,
            fee_charged: t.fee_charged,
            memo_type: t.memo_type,
            memo: t.memo,
          })),
        }}
        bodyClassName="-mx-4 -mb-4"
      >
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Hash</Th>
                <Th>Source</Th>
                <Th align="right">Ops</Th>
                <Th>Result</Th>
                <Th align="right">Fee (XLM)</Th>
                <Th>Memo</Th>
              </tr>
            </THead>
            <TBody>
              {transactions.map((t) => (
                <LinkRow href={`/transactions/${t.hash}/`} key={t.hash}>
                  <Td>
                    <HashLink
                      href={`/transactions/${t.hash}/`}
                      value={t.hash}
                      head={10}
                      tail={6}
                    />
                  </Td>
                  <Td>
                    <HashLink
                      href={`/accounts/${encodeURIComponent(t.source_account ?? '')}/`}
                      value={t.source_account}
                      head={6}
                      tail={4}
                      className="text-ink-muted"
                    />
                  </Td>
                  <Td align="right">{t.operation_count}</Td>
                  <Td>
                    <TxStatusBadge
                      successful={t.successful}
                      result={t.result}
                      code={t.result_code}
                    />
                  </Td>
                  <Td align="right" className="text-ink-muted">
                    {t.fee_charged != null
                      ? stroopsToXlmFixed(String(t.fee_charged))
                      : '—'}
                  </Td>
                  <Td className="text-ink-muted max-w-56 truncate text-xs">
                    {t.memo ? (
                      <span title={t.memo}>
                        {t.memo_type} · {t.memo}
                      </span>
                    ) : (
                      '—'
                    )}
                  </Td>
                </LinkRow>
              ))}
            </TBody>
          </Table>
        </div>
      </Panel>
    </>
  );
}
