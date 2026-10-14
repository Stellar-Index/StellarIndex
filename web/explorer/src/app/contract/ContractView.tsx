'use client';

import { useState, type ReactNode } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useQuery, keepPreviousData } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
  Badge,
  Button,
  Callout,
  Container,
  EmptyState,
  Fact,
  FactList,
  Mono,
  PageHeader,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { DirectoryLabel } from '@/components/DirectoryLabel';
import { CrossReference } from '@/components/CrossReference';
import {
  ContractDailyBars,
  CounterpartyBars,
  EventMixDonut,
  ExportChips,
} from '@/components/ContractCharts';
import { apiGet, asExample } from '@/api/client';
import { useSACWrappers } from '@/api/hooks';
import { isNativeXlmSac } from '@/lib/asset-label';
import { titleCase } from '@/lib/account-format';
import { baseUnitsDecimal, formatReadable } from '@/lib/format';
import type { paths } from '@/api/types';
import {
  type Envelope,
  type ContractResp,
  HashLink,
  formatTimestamp,
  relativeAge,
} from '../explorer-shared';
import { SacAssetPanel } from './SacAssetPanel';
import { CodeTimeline, TokenFlowSummary } from './ContractInsights';

type GetJSON<P extends keyof paths> = paths[P] extends {
  get: {
    responses: { 200: { content: { 'application/json': infer B } } };
  };
}
  ? B
  : never;

const CONTRACT_RE = /^C[A-Z2-7]{55}$/;
const ACCOUNT_RE = /^G[A-Z2-7]{55}$/;
const PAGE_SIZE = 50;

function useContractWasm(id: string) {
  const query = useQuery<ContractWasmResp>({
    queryKey: ['/v1/contracts/{id}/wasm', id],
    enabled: CONTRACT_RE.test(id),
    retry: false,
    staleTime: 3_600_000, // content-addressed wasm is immutable
    queryFn: async () => {
      const env = await apiGet<Envelope<ContractWasmResp>>(
        `/v1/contracts/${encodeURIComponent(id)}/wasm`,
      );
      return env.data;
    },
  });
  const msg = query.error instanceof Error ? query.error.message : '';
  const isSac = msg.includes('/errors/contract-is-sac');
  return { ...query, isSac };
}

export function ContractView({ id: idProp }: { id?: string } = {}) {
  const params = useSearchParams();
  const id = (idProp ?? params.get('id') ?? '').trim();
  const looksValid = CONTRACT_RE.test(id);
  const [cursor, setCursor] = useState<string | undefined>(undefined);

  const { data, isLoading, isError, error, isFetching } =
    useQuery<ContractResp>({
      queryKey: ['/v1/contracts/{id}', id, cursor ?? 'tip'],
      enabled: id.length > 0 && looksValid,
      retry: false,
      placeholderData: keepPreviousData,
      queryFn: async () => {
        const env = await apiGet<Envelope<ContractResp>>(
          `/v1/contracts/${encodeURIComponent(id)}`,
          {
            limit: PAGE_SIZE,
            ...(cursor !== undefined ? { cursor } : {}),
          },
        );
        return env.data;
      },
      staleTime: 30_000,
    });

  if (id.length === 0) {
    return (
      <Shell id={null}>
        <EmptyState
          title="No contract selected"
          description={
            <>
              Add <code className="font-mono">?id=</code> with a 56-character
              Soroban contract ID to the URL, or search for one with{' '}
              <kbd className="border-line-strong rounded-sm border px-1 font-mono text-xs">
                ⌘K
              </kbd>
              .
            </>
          }
        />
      </Shell>
    );
  }

  if (!looksValid) {
    return (
      <Shell id={id}>
        <Callout tone="bad" title="Not a contract ID">
          <span className="font-mono break-all">{id}</span> is not a valid
          Soroban contract ID. Contract IDs are 56 characters and start with{' '}
          <code className="font-mono">C</code>.
        </Callout>
      </Shell>
    );
  }

  const source = asExample(`/v1/contracts/${id}`, {
    limit: PAGE_SIZE,
    ...(cursor !== undefined ? { cursor } : {}),
  });

  if (isError) {
    return (
      <Shell id={id}>
        <Callout tone="warn" title="Contract not found">
          No events for this contract in the served tier, or the lookup failed:{' '}
          {error instanceof Error ? error.message : 'unknown error'}. Check the
          ID, or retry in a minute if the API was unavailable.
        </Callout>
      </Shell>
    );
  }

  if (isLoading || !data) {
    return (
      <Shell id={id}>
        <Panel title="Overview" source={source} bodyClassName="space-y-3">
          <Skeleton className="h-16" />
          <Skeleton className="h-10 w-2/3" />
        </Panel>
      </Shell>
    );
  }

  const cid = data.contract_id || id;
  return (
    <Shell id={cid}>
      <OverviewPanel data={data} id={cid} />
      <SacIdentity id={cid} />
      <WasmPanel id={cid} />
      <CodeHistoryPanel id={cid} />
      <TransfersPanel id={cid} />
      <InteractionsPanel id={cid} />
      <EventsPanel
        events={data.events}
        nextCursor={data.next_cursor}
        cursor={cursor}
        isFetching={isFetching}
        onNewest={() => setCursor(undefined)}
        onOlder={() => {
          if (data.next_cursor != null) setCursor(data.next_cursor);
        }}
        source={source}
      />
      <CrossReference kind="contract" id={cid} />
    </Shell>
  );
}

function OverviewPanel({ data, id }: { data: ContractResp; id: string }) {
  const act = data.activity;
  const daily = act?.daily ?? [];
  return (
    <Panel
      title="Overview"
      source={asExample(`/v1/contracts/${id}`)}
      bodyClassName="space-y-4"
    >
      <FactList columns={3}>
        <Fact label="Status" mono={false}>
          {data.ttl || data.exists === false ? (
            <ContractLiveness exists={data.exists} ttl={data.ttl} />
          ) : (
            <span
              className="text-ink-faint"
              title="No instance TTL row captured for this contract"
            >
              —
            </span>
          )}
        </Fact>
        <Fact label="Protocol" mono={false}>
          {data.protocol ? (
            <Link
              href={`/protocols/${encodeURIComponent(data.protocol)}`}
              className="text-brand-600 hover:underline"
              title="Registered contract: opens the protocol page"
            >
              {titleCase(data.protocol)}
            </Link>
          ) : (
            <span
              className="text-ink-faint"
              title="Not in the protocol registry; its events are still captured"
            >
              Not registered
            </span>
          )}
        </Fact>
        {data.directory && (
          <Fact label="Directory" mono={false}>
            <DirectoryLabel info={data.directory} />
          </Fact>
        )}
        <Fact
          label="First seen"
          title={act?.first_seen ? formatTimestamp(act.first_seen) : undefined}
        >
          <When ts={act?.first_seen} />
        </Fact>
        <Fact
          label="Last active"
          title={act?.last_seen ? formatTimestamp(act.last_seen) : undefined}
        >
          <When ts={act?.last_seen} />
        </Fact>
        <Fact label="Active ledgers">
          {act?.active_ledgers_total != null
            ? act.active_ledgers_total.toLocaleString('en-US')
            : '—'}
        </Fact>
      </FactList>
      {daily.length >= 2 && (
        <div>
          <div className={PANEL_LABEL_CLASS}>
            Ledgers with activity per day · last 30 days
          </div>
          <div className="mt-2">
            <ContractDailyBars daily={daily} />
          </div>
        </div>
      )}
    </Panel>
  );
}

function When({ ts }: { ts?: string }) {
  if (!ts) return <span className="text-ink-faint">—</span>;
  return (
    <>
      {ts.slice(0, 10)}
      <span className="text-ink-muted ml-2 font-sans text-xs">
        {relativeAge(ts)}
      </span>
    </>
  );
}

export function ContractLiveness({
  exists,
  ttl,
}: Pick<ContractResp, 'exists' | 'ttl'>) {
  if (ttl) {
    const left = ttl.live_until - ttl.as_of_ledger;
    return (
      <span className="inline-flex flex-wrap items-center gap-2">
        {ttl.state === 'archived' ? (
          <Badge
            tone="bad"
            title={`Instance TTL lapsed at ledger ${ttl.live_until}; restore it to invoke the contract.`}
          >
            Archived
          </Badge>
        ) : (
          <Badge
            tone="ok"
            title={`Instance entry live until ledger ${ttl.live_until}; it archives unless its TTL is extended first.`}
          >
            Live
          </Badge>
        )}
        {left > 0 && (
          <span
            className="text-ink-muted text-xs whitespace-nowrap"
            title={`As of ledger ${ttl.as_of_ledger}`}
          >
            {left.toLocaleString('en-US')} ledgers to expiry
          </span>
        )}
      </span>
    );
  }
  if (exists === false) {
    return (
      <Badge
        tone="neutral"
        title="The lake holds no evidence this contract was deployed: no events, activity, instance entry or TTL row."
      >
        Not captured
      </Badge>
    );
  }
  return null;
}

type ContractWasmResp = NonNullable<
  GetJSON<'/contracts/{contract_id}/wasm'>['data']
>;
type WasmExport = ContractWasmResp['exports'][number];

function exportSignature(e: WasmExport): string {
  const p = e.params.length ? e.params.join(', ') : '';
  const r = e.results.length ? e.results.join(', ') : '()';
  return `(${p}) → ${r}`;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(2)} MiB`;
}

function SacIdentity({ id }: { id: string }) {
  const { isSac } = useContractWasm(id);
  return isSac ? <SacAssetPanel contractId={id} /> : null;
}

function WasmPanel({ id }: { id: string }) {
  const { data, isLoading, isError, error, isSac, refetch } =
    useContractWasm(id);
  const source = asExample(`/v1/contracts/${id}/wasm`);

  if (isLoading) {
    return (
      <Panel title="Code (WASM)" source={source}>
        <Skeleton className="h-16" />
      </Panel>
    );
  }

  if (isError || !data) {
    const msg = error instanceof Error ? error.message : '';
    let body: ReactNode;
    if (isSac) {
      body = (
        <p className="text-ink-body text-sm">
          Stellar Asset Contract: built-in host logic rather than an uploaded
          WASM module, so there is no bytecode to show.
        </p>
      );
    } else if (msg.includes('404')) {
      body = (
        <Callout tone="warn" title="Code not captured">
          This contract’s WASM is not in the captured ledger window yet: its
          deploy-time code entry predates live capture. Exports and bytecode
          appear once that ledger is backfilled.
        </Callout>
      );
    } else {
      body = (
        <Callout tone="bad" title="Code unavailable">
          Couldn’t load this contract’s WASM{msg ? `: ${msg}` : ''}.
          <Button
            variant="secondary"
            size="sm"
            className="ml-3"
            onClick={() => refetch()}
          >
            Retry
          </Button>
        </Callout>
      );
    }
    return (
      <Panel title="Code (WASM)" source={source}>
        {body}
      </Panel>
    );
  }

  return (
    <Panel
      title="Code (WASM)"
      hint={`${data.exports.length} exports · ${formatBytes(data.size_bytes)}`}
      source={source}
      bodyClassName="space-y-4"
    >
      <FactList columns={3}>
        <Fact label="WASM hash">
          <Mono value={data.wasm_hash} truncate={{ head: 12, tail: 10 }} />
        </Fact>
        <Fact label="Size">{formatBytes(data.size_bytes)}</Fact>
        <Fact label="Exports">
          {data.exports.length.toLocaleString('en-US')}
        </Fact>
      </FactList>
      <div>
        <div className={PANEL_LABEL_CLASS}>Exported functions</div>
        <div className="mt-2">
          <ExportChips exports={data.exports} />
        </div>
      </div>
      {data.exports.length > 0 && (
        <details className="group border-line rounded-lg border">
          <summary className="text-ink-body marker:text-ink-faint hover:text-brand-600 cursor-pointer px-3 py-2 text-xs font-medium select-none">
            ABI signatures
          </summary>
          <div className="border-line overflow-x-auto border-t">
            <Table>
              <THead>
                <tr>
                  <Th>Function</Th>
                  <Th>Signature</Th>
                </tr>
              </THead>
              <TBody>
                {data.exports.map((e) => (
                  <TR key={e.name}>
                    <Td className="font-mono text-xs">{e.name}</Td>
                    <Td className="text-ink-muted font-mono text-xs">
                      {exportSignature(e)}
                    </Td>
                  </TR>
                ))}
              </TBody>
            </Table>
          </div>
        </details>
      )}
      {data.wat && <CodeDisclosure label="WAT disassembly" code={data.wat} />}
      {data.decompiled && (
        <CodeDisclosure label="Decompiled pseudocode" code={data.decompiled} />
      )}
      {data.source_note && (
        <p className="text-ink-muted text-xs">{data.source_note}</p>
      )}
    </Panel>
  );
}

function CodeDisclosure({ label, code }: { label: string; code: string }) {
  return (
    <details className="group border-line rounded-lg border">
      <summary className="text-ink-body marker:text-ink-faint hover:text-brand-600 cursor-pointer px-3 py-2 text-xs font-medium select-none">
        {label}{' '}
        <span className="text-ink-faint">
          ({code.split('\n').length.toLocaleString('en-US')} lines)
        </span>
      </summary>
      <pre className="border-line bg-surface-muted text-ink-body max-h-96 overflow-auto border-t p-3 text-[11px] leading-relaxed">
        {code}
      </pre>
    </details>
  );
}

type CodeHistoryResp = NonNullable<
  GetJSON<'/contracts/{contract_id}/code-history'>['data']
>;

function CodeHistoryPanel({ id }: { id: string }) {
  const { isSac } = useContractWasm(id);
  const { data, isLoading, isError } = useQuery<CodeHistoryResp>({
    queryKey: ['/v1/contracts/{id}/code-history', id],
    enabled: CONTRACT_RE.test(id),
    retry: false,
    staleTime: 600_000,
    queryFn: async () => {
      const env = await apiGet<Envelope<CodeHistoryResp>>(
        `/v1/contracts/${encodeURIComponent(id)}/code-history`,
      );
      return env.data;
    },
  });
  if (isSac) return null;
  const source = asExample(`/v1/contracts/${id}/code-history`);
  const versions = data?.versions ?? [];

  if (isLoading) {
    return (
      <Panel title="Code history" source={source}>
        <Skeleton className="h-10" />
      </Panel>
    );
  }
  if (isError) {
    return (
      <Panel title="Code history" source={source}>
        <p className="text-ink-muted text-sm">
          The code-history lookup failed. Reload the page to retry.
        </p>
      </Panel>
    );
  }
  if (versions.length === 0) {
    return (
      <Panel title="Code history" source={source}>
        <p className="text-ink-muted text-sm">
          No code changes in the captured ledger window: the contract has not
          been upgraded since capture began.
        </p>
      </Panel>
    );
  }

  return (
    <Panel
      title={`Code history (${versions.length} version${versions.length === 1 ? '' : 's'})`}
      hint={
        versions.length > 1
          ? 'In-place upgrades over time'
          : 'The deployed executable'
      }
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="px-4">
        <CodeTimeline versions={versions} />
      </div>
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th className="w-10">#</Th>
              <Th>From ledger</Th>
              <Th>When</Th>
              <Th>WASM hash</Th>
            </tr>
          </THead>
          <TBody>
            {versions.map((v, i) => (
              <TR key={`${v.ledger}-${v.wasm_hash}`}>
                <Td className="text-ink-faint font-mono text-xs">{i + 1}</Td>
                <Td>
                  <Link
                    href={`/ledgers/${v.ledger}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                  >
                    #{v.ledger}
                  </Link>
                </Td>
                <Td
                  className="text-ink-muted text-xs"
                  title={formatTimestamp(v.close_time)}
                >
                  {relativeAge(v.close_time)}
                </Td>
                <Td>
                  <Mono
                    value={v.wasm_hash ?? ''}
                    truncate={{ head: 12, tail: 8 }}
                    className="text-xs"
                  />
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

type InteractionsResp = NonNullable<
  GetJSON<'/contracts/{contract_id}/interactions'>['data']
>;

interface TransferRow {
  ledger?: number;
  ledger_close_time?: string;
  tx_hash?: string;
  event_kind?: string;
  from?: string;
  to?: string;
  amount?: string;
}

function transferAmount(amount: string, decimals: number): string {
  return formatReadable(baseUnitsDecimal(amount, decimals)) ?? amount;
}

function Party({ id }: { id?: string }) {
  if (!id) return <span className="text-ink-faint">—</span>;
  if (ACCOUNT_RE.test(id)) {
    return <HashLink href={`/accounts/${id}/`} value={id} head={6} tail={4} />;
  }
  if (CONTRACT_RE.test(id)) {
    return <HashLink href={`/contracts/${id}/`} value={id} head={6} tail={4} />;
  }
  return (
    <span className="font-mono text-xs" title={id}>
      {id.slice(0, 6)}…{id.slice(-4)}
    </span>
  );
}

function TransfersPanel({ id }: { id: string }) {
  const { data } = useQuery<{ transfers?: TransferRow[]; decimals?: number }>({
    queryKey: ['/v1/contracts/{id}/transfers', id],
    enabled: CONTRACT_RE.test(id),
    retry: false,
    staleTime: 30_000,
    queryFn: async () =>
      (
        await apiGet<
          Envelope<{ transfers?: TransferRow[]; decimals?: number }>
        >(`/v1/contracts/${encodeURIComponent(id)}/transfers`, { limit: 25 })
      ).data,
  });
  const rows = data?.transfers ?? [];
  const decimals = data?.decimals ?? 7;
  if (rows.length === 0) return null;

  return (
    <Panel
      title={`Token flows (last ${rows.length})`}
      hint="SEP-41 transfer, mint and burn events on this contract"
      source={asExample(`/v1/contracts/${id}/transfers`, { limit: 25 })}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="px-4">
        <TokenFlowSummary
          rows={rows}
          decimals={decimals}
          formatAmount={(raw) => transferAmount(raw, decimals)}
        />
      </div>
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>When</Th>
              <Th>Kind</Th>
              <Th>From</Th>
              <Th>To</Th>
              <Th align="right">Amount</Th>
              <Th>Tx</Th>
            </tr>
          </THead>
          <TBody>
            {rows.map((t, i) => (
              <TR key={`${t.tx_hash}-${i}`}>
                <Td
                  className="text-ink-muted text-xs whitespace-nowrap"
                  title={
                    t.ledger_close_time
                      ? formatTimestamp(t.ledger_close_time)
                      : undefined
                  }
                >
                  {t.ledger_close_time ? relativeAge(t.ledger_close_time) : '—'}
                </Td>
                <Td className="font-mono text-xs">{t.event_kind ?? '—'}</Td>
                <Td>
                  <Party id={t.from} />
                </Td>
                <Td>
                  <Party id={t.to} />
                </Td>
                <Td align="right" className="font-mono tabular-nums">
                  {t.amount ? transferAmount(t.amount, decimals) : '—'}
                </Td>
                <Td>
                  {t.tx_hash ? (
                    <HashLink
                      href={`/transactions/${t.tx_hash}/`}
                      value={t.tx_hash}
                      head={8}
                      tail={6}
                    />
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

function InteractionsPanel({ id }: { id: string }) {
  const { data: sacMap } = useSACWrappers();
  const { data, isLoading, isError } = useQuery<InteractionsResp>({
    queryKey: ['/v1/contracts/{id}/interactions', id],
    enabled: CONTRACT_RE.test(id),
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<Envelope<InteractionsResp>>(
        `/v1/contracts/${encodeURIComponent(id)}/interactions`,
        { days: 90, limit: 50 },
      );
      return env.data;
    },
  });
  const source = asExample(`/v1/contracts/${id}/interactions`, {
    days: 90,
    limit: 50,
  });
  const edges = data?.interactions ?? [];

  // A SAC counterparty reads better by the asset it wraps than by its C-address.
  const sacLabel = (cid?: string): string | undefined => {
    if (!cid) return undefined;
    const wrapped = isNativeXlmSac(cid) ? 'native' : sacMap?.[cid];
    if (!wrapped) return undefined;
    return `${wrapped === 'native' ? 'XLM' : wrapped.split(/[:-]/)[0]} SAC`;
  };

  if (isLoading) {
    return (
      <Panel title="Interaction map" source={source}>
        <Skeleton className="h-16" />
      </Panel>
    );
  }
  if (isError) {
    return (
      <Panel title="Interaction map" source={source}>
        <p className="text-ink-muted text-sm">
          The interaction scan failed or timed out. Reload the page to retry.
        </p>
      </Panel>
    );
  }
  if (edges.length === 0) {
    return (
      <Panel title="Interaction map" source={source}>
        <p className="text-ink-muted text-sm">
          No cross-contract interactions in the last 90 days: this contract did
          not share a transaction with another contract in the window.
        </p>
      </Panel>
    );
  }

  return (
    <Panel
      title={`Interaction map (${edges.length})`}
      hint="Contracts sharing transactions with this one · last 90 days"
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="px-4">
        <CounterpartyBars edges={edges} nameFor={sacLabel} />
      </div>
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Contract</Th>
              <Th>Protocol</Th>
              <Th align="right">Shared txs</Th>
            </tr>
          </THead>
          <TBody>
            {edges.map((e) => {
              const cid = e.contract_id ?? '';
              const label = sacLabel(cid);
              return (
                <TR key={cid}>
                  <Td>
                    <HashLink
                      href={`/contracts/${encodeURIComponent(cid)}/`}
                      value={cid}
                      head={8}
                      tail={6}
                    />
                    {label && (
                      <span className="text-ink-muted ml-2 text-xs">
                        {label}
                      </span>
                    )}
                  </Td>
                  <Td>
                    {e.protocol ? (
                      <Link
                        href={`/protocols/${encodeURIComponent(e.protocol)}`}
                        className="text-brand-600 hover:underline"
                      >
                        {titleCase(e.protocol)}
                      </Link>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {(e.shared_txs ?? 0).toLocaleString('en-US')}
                  </Td>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

function Shell({ id, children }: { id: string | null; children: ReactNode }) {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Contracts', href: '/contracts' },
          { label: id ? `${id.slice(0, 8)}…${id.slice(-6)}` : 'Contract' },
        ]}
        title="Contract"
        meta={id ? <Mono value={id} className="break-all" /> : undefined}
      />
      {children}
    </Container>
  );
}

function TopicLink({ topic }: { topic: string }) {
  const href = ACCOUNT_RE.test(topic)
    ? `/accounts/${topic}/`
    : CONTRACT_RE.test(topic)
      ? `/contracts/${topic}/`
      : null;
  if (!href) return <span>{topic} </span>;
  return (
    <Link href={href} className="text-brand-600 hover:underline" title={topic}>
      {topic.slice(0, 4)}…{topic.slice(-4)}{' '}
    </Link>
  );
}

function EventsPanel({
  events = [],
  nextCursor,
  cursor,
  isFetching,
  onNewest,
  onOlder,
  source,
}: {
  events: ContractResp['events'];
  nextCursor?: string;
  cursor?: string;
  isFetching: boolean;
  onNewest: () => void;
  onOlder: () => void;
  source: ReturnType<typeof asExample>;
}) {
  if (events.length === 0) {
    return (
      <Panel title="Recent events" source={source}>
        <p className="text-ink-muted text-sm">
          No events observed for this contract.
        </p>
      </Panel>
    );
  }
  return (
    <Panel
      title={`Recent events (${events.length})`}
      hint={`Newest first · ${PAGE_SIZE} per page`}
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="px-4 pb-4">
        <EventMixDonut events={events} />
      </div>
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Ledger</Th>
              <Th>When</Th>
              <Th>Tx</Th>
              <Th>Type</Th>
              <Th>Event</Th>
              <Th>Detail</Th>
            </tr>
          </THead>
          <TBody>
            {events.map((ev, i) => (
              <TR key={`${ev.tx_hash}-${ev.op_index}-${ev.event_index ?? i}`}>
                <Td>
                  <Link
                    href={`/ledgers/${ev.ledger}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                  >
                    #{ev.ledger}
                  </Link>
                </Td>
                <Td
                  className="text-ink-muted text-xs whitespace-nowrap"
                  title={formatTimestamp(ev.close_time)}
                >
                  {relativeAge(ev.close_time)}
                </Td>
                <Td>
                  {ev.tx_hash ? (
                    <HashLink
                      href={`/transactions/${ev.tx_hash}/`}
                      value={ev.tx_hash}
                      head={8}
                      tail={6}
                    />
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
                <Td className="font-mono text-xs">{ev.event_type || '—'}</Td>
                <Td className="font-mono text-xs">{ev.topic_0 || '—'}</Td>
                <Td>
                  <span
                    className="text-ink-muted block max-w-md truncate font-mono text-xs"
                    title={[...(ev.topics ?? []), ev.data ?? '']
                      .filter(Boolean)
                      .join(' · ')}
                  >
                    {(ev.topics ?? []).map((t: string, ti: number) => (
                      <TopicLink key={ti} topic={t} />
                    ))}
                    {ev.data ? (
                      <span className="text-ink-faint">{ev.data}</span>
                    ) : null}
                  </span>
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </div>
      <div className="border-line flex items-center justify-between border-t px-4 py-3">
        <span className="text-ink-muted text-xs">
          {isFetching
            ? 'Loading…'
            : cursor === undefined
              ? 'Newest page'
              : 'Older page'}
        </span>
        <div className="flex gap-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={onNewest}
            disabled={cursor === undefined || isFetching}
          >
            Newest
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={onOlder}
            disabled={nextCursor == null || isFetching}
          >
            Older
          </Button>
        </div>
      </div>
    </Panel>
  );
}
