'use client';

import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { ThroughputPanel } from '@/components/NetworkInsight';
import {
  Button,
  ButtonLink,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
  TxStatusBadge,
  LinkRow,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { formatCompact, truncateMiddle } from '@/lib/format';
import {
  type Envelope,
  type LedgersPage,
  type LedgerTransaction,
  type LedgerTransactionsResp,
  formatTimestamp,
  stroopsToXlm,
} from '../explorer-shared';

/**
 * TransactionsView — recent network transactions. Defaults to the latest
 * ledger's transactions and pages by ledger (the served tier is
 * ledger-partitioned, so a ledger-scoped scan is cheap and needs no
 * cross-ledger tx index). `?seq=N` pins a ledger.
 *
 * The page frame (Container + <h1>) lives in page.tsx so the static export
 * carries it; everything below depends on `?seq=`.
 */
export function TransactionsView() {
  const params = useSearchParams();
  const seqRaw = params.get('seq') ?? '';
  const explicitSeq = /^\d+$/.test(seqRaw.trim())
    ? Number(seqRaw.trim())
    : null;

  // The tip ledger: the default view, and the bound that disables "Newer".
  const tipQ = useQuery<LedgersPage>({
    queryKey: ['/v1/ledgers', 'tip-1'],
    queryFn: async () => {
      const env = await apiGet<Envelope<LedgersPage>>('/v1/ledgers', {
        limit: 1,
      });
      return env.data;
    },
    staleTime: 15_000,
    retry: false,
  });
  const tipSeq = tipQ.data?.ledgers?.[0]?.sequence ?? null;
  const seq = explicitSeq ?? tipSeq;

  const txQ = useQuery<LedgerTransactionsResp>({
    queryKey: ['/v1/ledgers/{seq}/transactions', seq],
    enabled: seq != null,
    queryFn: async () => {
      const env = await apiGet<Envelope<LedgerTransactionsResp>>(
        `/v1/ledgers/${seq}/transactions`,
      );
      return env.data;
    },
    staleTime: 30_000,
    retry: false,
  });

  return (
    <div className="space-y-6">
      <ThroughputPanel headingLevel={2} defaultMetric="txs" />
      <TxTable
        seq={seq}
        isLoading={txQ.isLoading || (explicitSeq == null && tipQ.isLoading)}
        isError={txQ.isError || (explicitSeq == null && tipQ.isError)}
        rows={txQ.data?.transactions}
        atTip={tipSeq != null && seq != null && seq >= tipSeq}
        retry={() => {
          if (explicitSeq == null && tipQ.isError) void tipQ.refetch();
          else void txQ.refetch();
        }}
      />
    </div>
  );
}

const COLS = 6;

function TxTable({
  seq,
  isLoading,
  isError,
  rows,
  retry,
  atTip,
}: {
  seq: number | null;
  isLoading: boolean;
  isError: boolean;
  rows: LedgerTransaction[] | undefined;
  retry: () => void;
  atTip: boolean;
}) {
  const source =
    seq != null ? asExample(`/v1/ledgers/${seq}/transactions`) : undefined;
  const closed = rows?.[0]?.close_time;
  return (
    <Panel
      headingLevel={2}
      title={
        rows ? `Transactions (${formatCompact(rows.length)})` : 'Transactions'
      }
      hint={
        seq != null
          ? `ledger #${seq}${closed ? ` · closed ${formatTimestamp(closed)}` : ''}`
          : 'one ledger at a time'
      }
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      {seq != null && (
        <div className="mx-4 mb-4 flex flex-wrap items-center gap-2">
          {atTip ? (
            <Button variant="secondary" size="sm" disabled>
              Newer
            </Button>
          ) : (
            <ButtonLink
              href={`/transactions?seq=${seq + 1}`}
              variant="secondary"
              size="sm"
            >
              Newer
            </ButtonLink>
          )}
          <ButtonLink
            href={`/transactions?seq=${seq - 1}`}
            variant="secondary"
            size="sm"
          >
            Older
          </ButtonLink>
          <Link
            href={`/ledgers/${seq}/`}
            className="text-brand-600 ml-1 text-xs hover:underline"
          >
            Open ledger
          </Link>
          <Link
            href="/ledgers"
            className="text-brand-600 ml-auto text-xs hover:underline"
          >
            Browse ledgers
          </Link>
        </div>
      )}
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Hash</Th>
              <Th className="hidden sm:table-cell">Source</Th>
              <Th align="right">Ops</Th>
              <Th>Result</Th>
              <Th align="right" className="hidden sm:table-cell">
                Fee (XLM)
              </Th>
              <Th className="hidden md:table-cell">Memo</Th>
            </tr>
          </THead>
          <TBody>
            {isError && (
              <TR className="hover:bg-transparent">
                <Td colSpan={COLS} className="text-ink-muted py-4">
                  Transactions unavailable — the ledger read did not return, so
                  this ledger&apos;s transactions are unknown rather than empty.
                  <Button
                    variant="secondary"
                    size="sm"
                    className="ml-3"
                    onClick={retry}
                  >
                    Retry
                  </Button>
                </Td>
              </TR>
            )}
            {!isError && (isLoading || rows == null) && (
              <TR className="hover:bg-transparent">
                <Td colSpan={COLS}>
                  <Skeleton className="h-4 w-64" />
                </Td>
              </TR>
            )}
            {!isError && rows && rows.length === 0 && (
              <TR className="hover:bg-transparent">
                <Td colSpan={COLS} className="text-ink-muted py-4">
                  Ledger #{seq} closed with no transactions. Try an older
                  ledger.
                </Td>
              </TR>
            )}
            {rows?.map((t) => (
              <LinkRow href={`/transactions/${t.hash}/`} key={t.hash}>
                <Td>
                  <Link
                    href={`/transactions/${t.hash}/`}
                    className="text-brand-600 font-mono text-xs whitespace-nowrap hover:underline"
                    title={t.hash}
                  >
                    {truncateMiddle(t.hash, 10, 6)}
                  </Link>
                </Td>
                <Td className="hidden sm:table-cell">
                  <Link
                    href={`/accounts/${encodeURIComponent(t.source_account)}/`}
                    className="text-ink hover:text-brand-600 font-mono text-xs whitespace-nowrap"
                    title={t.source_account}
                  >
                    {truncateMiddle(t.source_account, 6, 4)}
                  </Link>
                </Td>
                <Td align="right" className="font-mono">
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
                  className="text-ink-muted hidden font-mono text-xs sm:table-cell"
                  title={`${t.fee_charged} stroops`}
                >
                  {stroopsToXlm(t.fee_charged)}
                </Td>
                <Td className="hidden md:table-cell">
                  {t.memo_type && t.memo_type !== 'none' ? (
                    <span
                      className="text-ink-muted font-mono text-xs"
                      title={t.memo ?? ''}
                    >
                      {t.memo_type}
                      {t.memo
                        ? `: ${t.memo.length > 18 ? `${t.memo.slice(0, 18)}…` : t.memo}`
                        : ''}
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
    </Panel>
  );
}
