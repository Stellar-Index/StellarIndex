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
  Segmented,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import {
  DirectoryLabel,
  type DirectoryInfo,
} from '@/components/DirectoryLabel';
import { CrossReference } from '@/components/CrossReference';
import {
  ContractDailyBars,
  ExportChips,
  InlineBar,
} from '@/components/ContractCharts';
import { apiGet, asExample } from '@/api/client';
import { useAsset, useSACWrappers } from '@/api/hooks';
import { isNativeXlmSac } from '@/lib/asset-label';
import { titleCase } from '@/lib/account-format';
import {
  DIRECTORY_OPERATOR_OVERRIDE_SOURCE,
  hasDirectoryScamFlag,
} from '@/lib/directory-tags';
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
const MUXED_RE = /^M[A-Z2-7]{68}$/;
const PAGE_SIZE = 50;
const INTERACTIONS_FOLD = 10;
const TOKEN_EVENTS = new Set(['transfer', 'mint', 'burn', 'clawback']);

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

interface TransferRow {
  ledger?: number;
  ledger_close_time?: string;
  tx_hash?: string;
  event_kind?: string;
  from?: string;
  to?: string;
  amount?: string;
}

function useTransfers(id: string) {
  return useQuery<{ transfers?: TransferRow[]; decimals?: number }>({
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
}

export function ContractView({ id: idProp }: { id?: string } = {}) {
  const params = useSearchParams();
  const id = (idProp ?? params.get('id') ?? '').trim();
  const looksValid = CONTRACT_RE.test(id);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const { isSac } = useContractWasm(id);
  const sacAsset = useAsset(isSac ? id : undefined).data?.data;

  const { data, isLoading, isError, error, isFetching, refetch } =
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
    const msg = error instanceof Error ? error.message : 'unknown error';
    return (
      <Shell id={id}>
        {msg.includes('404') ? (
          <Callout tone="warn" title="Contract not found">
            Nothing in the served tier for this contract: no events, instance
            entry or TTL row. Check the ID; a contract deployed before capture
            began appears once its ledgers are backfilled.
          </Callout>
        ) : (
          <Callout tone="bad" title="Lookup failed">
            The contract lookup did not return: {msg}.
            <Button
              variant="secondary"
              size="sm"
              className="ml-3"
              onClick={() => refetch()}
            >
              Retry
            </Button>
          </Callout>
        )}
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
        <Panel title="Recent events" source={source}>
          <Skeleton className="h-40" />
        </Panel>
        <Panel
          title="Code (WASM)"
          source={asExample(`/v1/contracts/${id}/wasm`)}
        >
          <Skeleton className="h-16" />
        </Panel>
      </Shell>
    );
  }

  const cid = data.contract_id || id;
  const title = isSac
    ? [
        sacAsset?.code || (sacAsset?.type === 'native' ? 'XLM' : ''),
        'Stellar Asset Contract',
      ]
        .filter(Boolean)
        .join(' ')
    : (data.directory?.name ??
      (data.protocol ? `${titleCase(data.protocol)} contract` : 'Contract'));

  return (
    <Shell id={cid} title={title}>
      {isSac && <SacAssetPanel contractId={cid} />}
      <OverviewPanel data={data} id={cid} isSac={isSac} />
      <EventsPanel
        id={cid}
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
      <TransfersPanel id={cid} />
      <InteractionsPanel id={cid} />
      <WasmPanel id={cid} />
      <CodeHistoryPanel id={cid} />
      <CrossReference kind="contract" id={cid} />
    </Shell>
  );
}

function OverviewPanel({
  data,
  id,
  isSac,
}: {
  data: ContractResp;
  id: string;
  isSac: boolean;
}) {
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
        {(data.protocol || !isSac) && (
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
        )}
        {isSac && (
          <Fact label="Code" mono={false}>
            <span title="A Stellar Asset Contract runs built-in host logic; there is no uploaded WASM module.">
              Built-in host logic
            </span>
          </Fact>
        )}
        {data.directory && (
          <Fact label="Directory" mono={false}>
            <DirectoryFact info={data.directory} />
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
        <Fact
          label="Active ledgers"
          title="Ledgers in which this contract emitted at least one event, since first seen"
        >
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

// The full attribution line only earns its height when the label carries a
// warning; otherwise the source is named inline and the disclaimer sits in
// the tooltip so the fact row keeps its rhythm.
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
      {info.tags.map((t) => (
        <Badge key={t}>#{t}</Badge>
      ))}
      <span className="text-ink-muted text-xs">StellarExpert directory</span>
    </span>
  );
}

function When({ ts }: { ts?: string }) {
  if (!ts) return <span className="text-ink-faint">—</span>;
  return (
    <>
      {ts.slice(0, 10)}
      <span className="text-ink-muted ml-2 text-xs">{relativeAge(ts)}</span>
    </>
  );
}

function approxLedgerSpan(ledgers: number): string {
  const secs = ledgers * 5;
  if (secs < 3600) return `${Math.max(1, Math.round(secs / 60))} min`;
  if (secs < 172_800) return `${Math.round(secs / 3600)} h`;
  return `${Math.round(secs / 86_400)} d`;
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
            title={`As of ledger ${ttl.as_of_ledger}, assuming about 5 s per ledger`}
          >
            {left.toLocaleString('en-US')} ledgers to expiry · ≈{' '}
            {approxLedgerSpan(left)}
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

function RetryButton({ onClick }: { onClick: () => void }) {
  return (
    <Button variant="secondary" size="sm" className="ml-3" onClick={onClick}>
      Retry
    </Button>
  );
}

function WasmPanel({ id }: { id: string }) {
  const { data, isLoading, isError, error, isSac, refetch } =
    useContractWasm(id);
  const source = asExample(`/v1/contracts/${id}/wasm`);
  if (isSac) return null;

  if (isLoading) {
    return (
      <Panel title="Code (WASM)" source={source}>
        <Skeleton className="h-16" />
      </Panel>
    );
  }

  if (isError || !data) {
    const msg = error instanceof Error ? error.message : '';
    return (
      <Panel title="Code (WASM)" source={source}>
        {msg.includes('404') ? (
          <Callout tone="warn" title="Code not captured">
            This contract’s WASM is not in the captured ledger window yet: its
            deploy-time code entry predates live capture. Exports and bytecode
            appear once that ledger is backfilled.
          </Callout>
        ) : (
          <Callout tone="bad" title="Code unavailable">
            The WASM lookup did not return{msg ? `: ${msg}` : ''}.
            <RetryButton onClick={() => refetch()} />
          </Callout>
        )}
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
  const { data, isLoading, isError, refetch } = useQuery<CodeHistoryResp>({
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
          The code-history lookup did not return.
          <RetryButton onClick={() => refetch()} />
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
  const { data, isError, refetch } = useTransfers(id);
  const rows = data?.transfers ?? [];
  const decimals = data?.decimals ?? 7;
  const source = asExample(`/v1/contracts/${id}/transfers`, { limit: 25 });
  if (isError) {
    return (
      <Panel title="Token flows" source={source}>
        <p className="text-ink-muted text-sm">
          The transfer lookup did not return.
          <RetryButton onClick={() => refetch()} />
        </p>
      </Panel>
    );
  }
  if (rows.length === 0) return null;

  return (
    <Panel
      title={`Token flows (last ${rows.length})`}
      hint="SEP-41 transfer, mint and burn events on this contract"
      source={source}
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
              <Th>Kind</Th>
              <Th>From</Th>
              <Th>To</Th>
              <Th align="right">Amount</Th>
              <Th>When</Th>
              <Th>Tx</Th>
            </tr>
          </THead>
          <TBody>
            {rows.map((t, i) => (
              <TR key={`${t.tx_hash}-${i}`}>
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
  const [showAll, setShowAll] = useState(false);
  const { data, isLoading, isError, refetch } = useQuery<InteractionsResp>({
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
          The interaction scan did not return or timed out.
          <RetryButton onClick={() => refetch()} />
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

  const visible = showAll ? edges : edges.slice(0, INTERACTIONS_FOLD);
  const anyProtocol = edges.some((e) => !!e.protocol);
  const max = Math.max(...edges.map((e) => e.shared_txs ?? 0));

  return (
    <Panel
      title={`Interaction map (${edges.length})`}
      hint="Shared transactions · last 90 days · counts are lower bounds"
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Contract</Th>
              {anyProtocol && <Th>Protocol</Th>}
              <Th align="right">Shared txs</Th>
            </tr>
          </THead>
          <TBody>
            {visible.map((e) => {
              const cid = e.contract_id ?? '';
              const label = sacLabel(cid);
              const n = e.shared_txs ?? 0;
              return (
                <TR key={cid}>
                  <Td>
                    {label && (
                      <span className="text-ink mr-2 text-sm">{label}</span>
                    )}
                    <HashLink
                      href={`/contracts/${encodeURIComponent(cid)}/`}
                      value={cid}
                      head={8}
                      tail={6}
                    />
                  </Td>
                  {anyProtocol && (
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
                  )}
                  <Td align="right" className="whitespace-nowrap">
                    <InlineBar
                      value={n}
                      max={max}
                      label={`${n.toLocaleString('en-US')} shared transactions`}
                    />
                    <span className="ml-3 inline-block w-12 text-right font-mono tabular-nums">
                      {n.toLocaleString('en-US')}
                    </span>
                  </Td>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </div>
      {edges.length > INTERACTIONS_FOLD && (
        <div className="border-line border-t px-4 py-3">
          <Button
            variant="subtle"
            size="sm"
            onClick={() => setShowAll((v) => !v)}
          >
            {showAll
              ? `Show top ${INTERACTIONS_FOLD}`
              : `Show all ${edges.length}`}
          </Button>
        </div>
      )}
    </Panel>
  );
}

function Shell({
  id,
  title = 'Contract',
  children,
}: {
  id: string | null;
  title?: string;
  children: ReactNode;
}) {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Contracts', href: '/contracts' },
          { label: id ? `${id.slice(0, 8)}…${id.slice(-6)}` : 'Contract' },
        ]}
        title={title}
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
  if (MUXED_RE.test(topic)) {
    return (
      <span className="whitespace-nowrap" title={`${topic} (muxed account)`}>
        {topic.slice(0, 4)}…{topic.slice(-4)}
      </span>
    );
  }
  if (!href) return <span>{topic === 'native' ? 'XLM' : topic}</span>;
  return (
    <Link
      href={href}
      className="text-brand-600 whitespace-nowrap hover:underline"
      title={topic}
    >
      {topic.slice(0, 4)}…{topic.slice(-4)}
    </Link>
  );
}

type ContractEvent = NonNullable<ContractResp['events']>[number];

// SEP-41 token events carry the amount in `data` as a bare i128 or as a
// map with `amount` + `to_muxed_id`; every other event keeps its raw
// topic/data rendering.
function tokenAmount(data: string): string | null {
  if (/^-?\d+$/.test(data)) return data;
  const m = /^\{\s*amount:\s*(-?\d+)/.exec(data);
  return m ? m[1] : null;
}

function EventDetail({
  ev,
  decimals,
}: {
  ev: ContractEvent;
  decimals: number;
}) {
  const topics = ev.topics ?? [];
  const data = ev.data ?? '';
  const amount = TOKEN_EVENTS.has(ev.topic_0 ?? '') ? tokenAmount(data) : null;
  const parties = topics.filter(
    (t) => ACCOUNT_RE.test(t) || CONTRACT_RE.test(t) || MUXED_RE.test(t),
  );
  if (amount != null && parties.length > 0) {
    return (
      <span className="inline-flex items-center gap-x-1.5 font-mono text-xs whitespace-nowrap">
        {parties.map((p, i) => (
          <span key={i} className="inline-flex items-center gap-x-1.5">
            {i > 0 && <span className="text-ink-faint">→</span>}
            <TopicLink topic={p} />
          </span>
        ))}
        <span className="text-ink tabular-nums">
          {transferAmount(amount, decimals)}
        </span>
      </span>
    );
  }
  return (
    <span
      className="inline-flex max-w-md items-center gap-x-1.5 font-mono text-xs whitespace-nowrap"
      title={[...topics, data].filter(Boolean).join(' · ')}
    >
      {topics.map((t, ti) => (
        <TopicLink key={ti} topic={t} />
      ))}
      {data ? (
        <span className="text-ink-faint min-w-0 truncate">{data}</span>
      ) : null}
    </span>
  );
}

function EventsPanel({
  id,
  events = [],
  nextCursor,
  cursor,
  isFetching,
  onNewest,
  onOlder,
  source,
}: {
  id: string;
  events: ContractResp['events'];
  nextCursor?: string;
  cursor?: string;
  isFetching: boolean;
  onNewest: () => void;
  onOlder: () => void;
  source: ReturnType<typeof asExample>;
}) {
  const [kind, setKind] = useState<string | null>(null);
  const decimals = useTransfers(id).data?.decimals ?? 7;

  if (events.length === 0) {
    return (
      <Panel title="Recent events" source={source}>
        <p className="text-ink-muted text-sm">
          No events observed for this contract in the served tier.
        </p>
      </Panel>
    );
  }

  const counts = new Map<string, number>();
  for (const ev of events) {
    const k = ev.topic_0 || '—';
    counts.set(k, (counts.get(k) ?? 0) + 1);
  }
  const kinds = [...counts.entries()].sort((a, b) => b[1] - a[1]);
  const mixedTypes = new Set(events.map((e) => e.event_type)).size > 1;
  const rows = kind
    ? events.filter((e) => (e.topic_0 || '—') === kind)
    : events;
  const cols = mixedTypes ? 6 : 5;

  return (
    <Panel
      title={`Recent events (${events.length})`}
      hint={`Newest first · ${PAGE_SIZE} per page`}
      source={source}
      bodyClassName="-mx-4 -mb-4"
    >
      {kinds.length > 1 && (
        <div className="px-4 pb-4">
          <Segmented
            ariaLabel="Filter the loaded events by name"
            className="flex-wrap font-mono"
            value={kind ?? 'all'}
            onChange={(v) => setKind(v === 'all' ? null : v)}
            options={[
              { value: 'all', label: `All ${events.length}` },
              ...kinds.map(([k, n]) => ({
                value: k,
                label: (
                  <>
                    {k} <span className="text-ink-faint tabular-nums">{n}</span>
                  </>
                ),
              })),
            ]}
          />
        </div>
      )}
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th>Event</Th>
              {mixedTypes && <Th>Type</Th>}
              <Th>Detail</Th>
              <Th>When</Th>
              <Th>Tx</Th>
              <Th>Ledger</Th>
            </tr>
          </THead>
          <TBody>
            {rows.length === 0 && (
              <TR>
                <Td colSpan={cols} className="text-ink-muted">
                  No {kind} events on this page. Clear the filter or load an
                  older page.
                </Td>
              </TR>
            )}
            {rows.map((ev, i) => (
              <TR key={`${ev.tx_hash}-${ev.op_index}-${ev.event_index ?? i}`}>
                <Td className="font-mono text-xs">{ev.topic_0 || '—'}</Td>
                {mixedTypes && (
                  <Td className="text-ink-muted font-mono text-xs">
                    {ev.event_type || '—'}
                  </Td>
                )}
                <Td>
                  <EventDetail ev={ev} decimals={decimals} />
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
                <Td>
                  <Link
                    href={`/ledgers/${ev.ledger}/`}
                    className="text-brand-600 font-mono text-xs hover:underline"
                  >
                    #{ev.ledger}
                  </Link>
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
