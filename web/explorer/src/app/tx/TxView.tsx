'use client';

import type { ReactNode } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { CrossReference } from '@/components/CrossReference';
import { OpTypeStrip, feeUsage } from '@/components/ChainCharts';
import {
  Badge,
  Button,
  Callout,
  Container,
  CopyButton,
  EmptyState,
  Fact,
  FactList,
  Mono,
  PageHeader,
  Skeleton,
  Table,
  TBody,
  Td,
  Th,
  THead,
  TxStatusBadge,
  LinkRow,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import {
  type Envelope,
  type TxSummary,
  type TxOperation,
  type TxEvent,
  HashLink,
  OpBody,
  formatTimestamp,
  opSucceeded,
  shortHash,
  stroopsToXlm,
} from '../explorer-shared';

// A 64-char hex tx hash is the canonical shape; anything else is treated as
// a likely-bad-hash 400 hint before the request is made.
const HASH_RE = /^[0-9a-fA-F]{64}$/;

/**
 * Client view for /transactions/[hash] (and the legacy /tx?hash=). Fetches
 * /v1/tx/{hash} (summary + operations + events in one response). Handles
 * 404 (not found) and 400 (bad hash) distinctly.
 */
export function TxView({ hash: hashProp }: { hash?: string } = {}) {
  const params = useSearchParams();
  const hash = (hashProp ?? params.get('hash') ?? '').trim();
  const looksValid = HASH_RE.test(hash);

  const q = useQuery<TxSummary>({
    queryKey: ['/v1/tx/{hash}', hash],
    enabled: hash.length > 0,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<TxSummary>>(
        `/v1/tx/${encodeURIComponent(hash)}`,
      );
      return env.data;
    },
    staleTime: 5 * 60_000,
  });

  if (hash.length === 0) {
    return (
      <Shell hash={null}>
        <EmptyState
          title="No transaction selected"
          description={
            <>
              This page needs a <code className="font-mono">?hash=</code> query
              parameter: a 64-character transaction hash. Use search (
              <kbd className="border-line-strong rounded-sm border px-1 font-mono text-[11px]">
                ⌘K
              </kbd>
              ) to look one up.
            </>
          }
        />
      </Shell>
    );
  }

  if (!looksValid) {
    return (
      <Shell hash={hash}>
        <EmptyState
          title="Invalid transaction hash"
          description={
            <>
              <span className="font-mono break-all">{hash}</span> isn&apos;t a
              valid transaction hash. Stellar tx hashes are 64 hexadecimal
              characters.
            </>
          }
        />
      </Shell>
    );
  }

  if (q.isError) {
    const status = errorStatus(q.error);
    return (
      <Shell hash={hash}>
        <EmptyState
          title={
            status === 400
              ? 'Invalid transaction hash'
              : 'Transaction not found'
          }
          description={
            status === 404
              ? 'No transaction with that hash in the served tier.'
              : status === 400
                ? 'The API rejected that hash as malformed.'
                : `Lookup failed: ${q.error instanceof Error ? q.error.message : 'unknown error'}.`
          }
          action={
            status === 404 || status === 400 ? undefined : (
              <Button variant="secondary" size="sm" onClick={() => q.refetch()}>
                Retry
              </Button>
            )
          }
        />
      </Shell>
    );
  }

  const tx = q.data;
  if (!tx) {
    return (
      <Shell hash={hash}>
        <Panel
          headingLevel={2}
          title="Summary"
          source={asExample(`/v1/tx/${hash}`)}
        >
          <div className="space-y-3">
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-2/3" />
            <Skeleton className="h-4 w-1/2" />
          </div>
        </Panel>
      </Shell>
    );
  }

  const ops = tx.operations ?? [];
  const failedOp = tx.successful
    ? undefined
    : ops.find((o) => opSucceeded(o) === false);
  const usage = feeUsage(tx.fee_charged, tx.max_fee);

  return (
    <Shell hash={tx.hash ?? null} tx={tx}>
      {tx.successful === false && (
        <Callout tone="bad">
          Failed
          {failedOp
            ? ` at op #${failedOp.op_index}: ${failedOp.inner_result ?? failedOp.result ?? 'unknown'}`
            : `: ${tx.fee_bump?.inner_result ?? tx.result}`}
          . The fee was charged; no operation took effect.
        </Callout>
      )}

      <Panel
        headingLevel={2}
        title="Summary"
        source={asExample(`/v1/tx/${tx.hash}`)}
      >
        <FactList>
          <Fact label="Hash">{tx.hash ? <Mono value={tx.hash} /> : '—'}</Fact>
          <Fact label="Ledger">
            <Link
              href={`/ledgers/${tx.ledger}/`}
              className="text-ink hover:text-brand-600"
            >
              #{tx.ledger}
            </Link>
          </Fact>
          <Fact label="Close time">{formatTimestamp(tx.close_time)}</Fact>
          <Fact label="Source account">
            <HashLink
              href={`/accounts/${encodeURIComponent(tx.source_account ?? '')}/`}
              value={tx.source_account}
              head={12}
              tail={8}
              copy
            />
          </Fact>
          {tx.fee_bump && (
            <Fact
              label="Fee bump"
              mono={false}
              title="Another account paid this transaction's fee. The inner transaction is unchanged."
            >
              <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-1 text-sm">
                paid by
                <HashLink
                  href={`/accounts/${encodeURIComponent(tx.fee_bump.fee_account)}/`}
                  value={tx.fee_bump.fee_account}
                  head={12}
                  tail={8}
                />
                {tx.fee_bump.inner_hash && (
                  <>
                    · inner tx
                    <Mono
                      value={tx.fee_bump.inner_hash}
                      truncate={{ head: 8, tail: 8 }}
                    />
                  </>
                )}
              </span>
            </Fact>
          )}
          <Fact label="Fee charged">
            {tx.fee_charged != null
              ? `${stroopsToXlm(tx.fee_charged)} XLM`
              : '—'}
          </Fact>
          <Fact label={tx.fee_bump ? 'Max fee (fee payer)' : 'Max fee'}>
            {tx.max_fee != null ? `${stroopsToXlm(tx.max_fee)} XLM` : '—'}
            {usage && (
              <span className="text-ink-muted">
                {' '}
                · {(usage.pctTenths / 10).toFixed(1)}% used
              </span>
            )}
          </Fact>
          {tx.fee_bump && (
            <Fact label="Max fee (inner)">
              {`${stroopsToXlm(tx.fee_bump.inner_max_fee)} XLM`}
            </Fact>
          )}
          <Fact label="Memo" mono={false}>
            <Memo type={tx.memo_type} text={tx.memo} b64={tx.memo_base64} />
          </Fact>
          {ops.length > 0 && (
            <Fact label="Operations" mono={false}>
              <OpTypeStrip types={ops.map((o) => o.type ?? 'unknown')} />
            </Fact>
          )}
        </FactList>
        {tx.coverage_note && (
          <p className="text-ink-faint mt-3 text-xs">{tx.coverage_note}</p>
        )}
      </Panel>

      <OperationsPanel
        hash={tx.hash ?? ''}
        operations={ops}
        txFailed={tx.successful === false}
      />
      <EventsPanel
        hash={tx.hash ?? ''}
        events={tx.events ?? []}
        failed={tx.successful === false}
      />
    </Shell>
  );
}

function Shell({
  hash,
  tx,
  children,
}: {
  hash: string | null;
  tx?: TxSummary;
  children: ReactNode;
}) {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Transactions', href: '/transactions' },
          { label: hash ? shortHash(hash, 8, 6) : 'Transaction' },
        ]}
        title="Transaction"
        meta={
          tx ? (
            <>
              Closed {formatTimestamp(tx.close_time)} in ledger{' '}
              <Link
                href={`/ledgers/${tx.ledger}/`}
                className="hover:text-brand-600 font-mono"
              >
                #{tx.ledger}
              </Link>
            </>
          ) : hash ? (
            <span className="font-mono break-all">{hash}</span>
          ) : undefined
        }
        actions={
          tx ? (
            <TxStatusBadge
              successful={tx.successful}
              result={tx.result}
              code={tx.result_code}
            />
          ) : undefined
        }
      />
      {children}
      {hash && <CrossReference kind="tx" id={hash} />}
    </Container>
  );
}

function OperationsPanel({
  hash,
  operations,
  txFailed,
}: {
  hash: string;
  operations: TxOperation[];
  txFailed: boolean;
}) {
  const source = asExample(`/v1/tx/${hash}`);
  if (operations.length === 0) {
    return (
      <Panel headingLevel={2} title="Operations" source={source}>
        <p className="text-ink-muted text-sm">
          No operations on this transaction.
        </p>
      </Panel>
    );
  }
  return (
    <Panel
      headingLevel={2}
      title="Operations"
      hint={`${operations.length} in this transaction, in order`}
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      <ol className="divide-line divide-y">
        {operations.map((op) => (
          <li key={op.op_index} className="space-y-3 px-4 py-4">
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
              <span className="text-ink-faint font-mono text-xs tabular-nums">
                #{op.op_index}
              </span>
              <Link
                href={`/operation?tx=${hash}&i=${op.op_index}`}
                className="text-ink hover:text-brand-600 font-mono text-[13px] font-medium"
                title="Operation detail"
              >
                {op.type}
              </Link>
              {op.result_code != null &&
                (txFailed && opSucceeded(op) ? (
                  <Badge
                    tone="neutral"
                    title="The operation itself succeeded, but the transaction failed, so nothing it did was applied."
                  >
                    rolled back
                  </Badge>
                ) : (
                  <TxStatusBadge
                    successful={opSucceeded(op)}
                    result={op.inner_result ?? op.result}
                    code={op.result_code}
                  />
                ))}
              {op.source_account && (
                <span
                  className="text-ink-muted font-mono text-xs"
                  title={op.source_account}
                >
                  src {shortHash(op.source_account, 6, 4)}
                </span>
              )}
            </div>
            <OpBody op={op} />
          </li>
        ))}
      </ol>
    </Panel>
  );
}

function EventsPanel({
  hash,
  events,
  failed,
}: {
  hash: string;
  events: TxEvent[];
  failed: boolean;
}) {
  const source = asExample(`/v1/tx/${hash}`);
  if (events.length === 0) {
    return (
      <Panel headingLevel={2} title="Events" source={source}>
        <p className="text-ink-muted text-sm">
          {failed
            ? 'Failed transaction: any events were rolled back.'
            : 'This transaction emitted no Soroban contract events.'}
        </p>
      </Panel>
    );
  }
  return (
    <Panel
      headingLevel={2}
      title="Events"
      hint={`${events.length} Soroban contract events`}
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th align="right">Op</Th>
              <Th>Contract</Th>
              <Th>Event type</Th>
              <Th>Topic 0</Th>
            </tr>
          </THead>
          <TBody>
            {events.map((ev, i) => (
              <LinkRow
                href={`/contracts/${ev.contract_id}/`}
                key={`${ev.op_index}-${ev.event_index ?? i}`}
              >
                <Td align="right" className="text-ink-muted">
                  {ev.op_index}
                </Td>
                <Td>
                  <HashLink
                    href={`/contracts/${ev.contract_id}/`}
                    value={ev.contract_id}
                    head={8}
                    tail={6}
                  />
                </Td>
                <Td className="font-mono text-[13px]">
                  {ev.event_type || '—'}
                </Td>
                <Td className="text-ink-muted font-mono text-[13px]">
                  {ev.topic_0 || '—'}
                </Td>
              </LinkRow>
            ))}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

function Memo({
  type,
  text,
  b64,
}: {
  type?: string;
  text?: string;
  b64?: string;
}) {
  if (!type || type === 'none')
    return <span className="text-ink-faint">—</span>;
  const value = text ?? b64;
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <Badge tone="neutral">{type}</Badge>
      {value && (
        <span className="font-mono text-[13px] break-all">{value}</span>
      )}
      {text == null && b64 && (
        <span
          className="text-ink-muted text-xs"
          title="Not valid UTF-8; shown as base64"
        >
          base64
        </span>
      )}
      {value && <CopyButton value={value} />}
    </span>
  );
}

function errorStatus(err: unknown): number | null {
  if (!(err instanceof Error)) return null;
  const m = err.message.match(/^(\d{3})\b/);
  return m ? Number(m[1]) : null;
}
