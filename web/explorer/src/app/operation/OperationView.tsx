'use client';

import type { ReactNode } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
  Button,
  ButtonLink,
  Container,
  EmptyState,
  Fact,
  FactList,
  PageHeader,
  Skeleton,
  Table,
  TBody,
  Td,
  Th,
  THead,
  TR,
  TxStatusBadge,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import { titleCase } from '@/lib/account-format';
import {
  type Envelope,
  type TxSummary,
  type TxEvent,
  HashLink,
  OpBody,
  formatTimestamp,
  opSucceeded,
  shortHash,
} from '../explorer-shared';

const HASH_RE = /^[0-9a-fA-F]{64}$/;

/**
 * Client view for /operation?tx=H&i=N. Fetches the containing transaction
 * (/v1/tx/{hash}) and isolates the operation at index N plus the contract
 * events it emitted, so one operation gets a first-class detail page.
 */
export function OperationView() {
  const params = useSearchParams();
  const hash = (params.get('tx') ?? '').trim();
  const idxRaw = params.get('i') ?? '';
  const idx = Number.parseInt(idxRaw, 10);
  const looksValid = HASH_RE.test(hash) && Number.isInteger(idx) && idx >= 0;

  const q = useQuery<TxSummary>({
    queryKey: ['/v1/tx/{hash}', hash],
    enabled: looksValid,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<TxSummary>>(
        `/v1/tx/${encodeURIComponent(hash)}`,
      );
      return env.data;
    },
    staleTime: 5 * 60_000,
  });

  if (hash.length === 0 || idxRaw === '') {
    return (
      <Shell hash={null} idx={null}>
        <EmptyState
          title="No operation selected"
          description={
            <>
              This page needs a <code className="font-mono">?tx=</code>{' '}
              (64-character transaction hash) and{' '}
              <code className="font-mono">?i=</code> (operation index) query
              parameter. Open an operation from the operations feed.
            </>
          }
          action={
            <ButtonLink href="/operations" variant="secondary" size="sm">
              Operations feed
            </ButtonLink>
          }
        />
      </Shell>
    );
  }

  if (!looksValid) {
    return (
      <Shell hash={hash} idx={idx}>
        <EmptyState
          title="Invalid operation reference"
          description={
            <>
              An operation is addressed by a 64-hex transaction hash and a
              non-negative index.{' '}
              <span className="font-mono break-all">{hash}</span> /{' '}
              <span className="font-mono">{idxRaw}</span> doesn&apos;t parse.
            </>
          }
        />
      </Shell>
    );
  }

  if (q.isError) {
    const status = errorStatus(q.error);
    return (
      <Shell hash={hash} idx={idx}>
        <EmptyState
          title={status === 404 ? 'Transaction not found' : 'Lookup failed'}
          description={
            status === 404
              ? 'No transaction with that hash in the served tier.'
              : `Lookup failed: ${q.error instanceof Error ? q.error.message : 'unknown error'}.`
          }
          action={
            status === 404 ? undefined : (
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
      <Shell hash={hash} idx={idx}>
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
  const op = ops.find((o) => o.op_index === idx);
  const opEvents = (tx.events ?? []).filter((e) => e.op_index === idx);

  if (!op) {
    return (
      <Shell hash={tx.hash ?? null} idx={idx}>
        <EmptyState
          title="Operation not found"
          description={
            <>
              Transaction{' '}
              <span className="font-mono">{shortHash(tx.hash, 12, 0)}</span> has{' '}
              {ops.length} operation{ops.length === 1 ? '' : 's'}; index {idx}{' '}
              is out of range.
            </>
          }
          action={
            <ButtonLink
              href={`/transactions/${tx.hash}/`}
              variant="secondary"
              size="sm"
            >
              View the transaction
            </ButtonLink>
          }
        />
      </Shell>
    );
  }

  const source = asExample(`/v1/tx/${tx.hash}`);
  return (
    <Shell hash={tx.hash ?? null} idx={idx} type={op.type} tx={tx}>
      <Panel headingLevel={2} title="Summary" source={source}>
        <FactList>
          <Fact label="Operation">
            #{op.op_index} of {ops.length}
          </Fact>
          <Fact label="Type">{op.type}</Fact>
          <Fact label="Operation result" mono={false}>
            {op.result_code == null ? (
              <span className="text-ink-faint">—</span>
            ) : (
              <TxStatusBadge
                successful={opSucceeded(op)}
                result={op.inner_result ?? op.result}
                code={op.result_code}
              />
            )}
          </Fact>
          <Fact
            label="Transaction result"
            mono={false}
            title="A failed parent transaction means nothing this operation names actually moved."
          >
            <TxStatusBadge
              successful={op.transaction_successful ?? tx.successful}
              result={op.transaction_result ?? tx.result}
              code={tx.result_code}
            />
          </Fact>
          <Fact label="Transaction">
            <HashLink
              href={`/transactions/${tx.hash}/`}
              value={tx.hash}
              head={16}
              tail={16}
              copy
            />
          </Fact>
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
            {op.source_account ? (
              <HashLink
                href={`/accounts/${encodeURIComponent(op.source_account)}/`}
                value={op.source_account}
                head={12}
                tail={8}
                copy
              />
            ) : (
              <span className="inline-flex flex-wrap items-center gap-2">
                <span className="text-ink-muted">inherits tx source</span>
                <HashLink
                  href={`/accounts/${encodeURIComponent(tx.source_account ?? '')}/`}
                  value={tx.source_account}
                  head={12}
                  tail={8}
                  copy
                />
              </span>
            )}
          </Fact>
        </FactList>
      </Panel>

      <Panel headingLevel={2} title="Decoded body" source={source}>
        <OpBody op={op} />
      </Panel>

      <OpEventsPanel hash={tx.hash ?? ''} events={opEvents} />
    </Shell>
  );
}

function OpEventsPanel({ hash, events }: { hash: string; events: TxEvent[] }) {
  const source = asExample(`/v1/tx/${hash}`);
  if (events.length === 0) {
    return (
      <Panel headingLevel={2} title="Events" source={source}>
        <p className="text-ink-muted text-sm">
          This operation emitted no Soroban contract events.
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
              <Th>Contract</Th>
              <Th>Event type</Th>
              <Th>Topic 0</Th>
            </tr>
          </THead>
          <TBody>
            {events.map((ev, i) => (
              <TR key={`${ev.event_index ?? i}`}>
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
              </TR>
            ))}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

function Shell({
  hash,
  idx,
  type,
  tx,
  children,
}: {
  hash: string | null;
  idx: number | null;
  type?: string;
  tx?: TxSummary;
  children: ReactNode;
}) {
  const last =
    hash && idx != null ? `${shortHash(hash, 8, 0)} #${idx}` : 'Operation';
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Operations', href: '/operations' },
          ...(hash
            ? [{ label: 'Transaction', href: `/transactions/${hash}/` }]
            : []),
          { label: last },
        ]}
        title={type ? titleCase(type.replace(/_/g, ' ')) : 'Operation'}
        meta={
          type ? (
            <>
              <span className="font-mono">{type}</span>
              {idx != null && <> · operation #{idx}</>}
              {tx && <> · closed {formatTimestamp(tx.close_time)}</>}
            </>
          ) : undefined
        }
      />
      {children}
    </Container>
  );
}

function errorStatus(err: unknown): number | null {
  if (!(err instanceof Error)) return null;
  const m = err.message.match(/^(\d{3})\b/);
  return m ? Number(m[1]) : null;
}
