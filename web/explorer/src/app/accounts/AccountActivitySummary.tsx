'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Badge } from '@/components/ui';
import { HBarList, PairedBars } from '@/components/charts/Bars';
import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { type Envelope } from '../explorer-shared';

type AccountActivityResp = components['schemas']['AccountActivity'];
type DefiActionCount = components['schemas']['AccountDefiActionCount'];

const PROTOCOL_LABEL: Record<string, string> = {
  blend: 'Blend',
  blend_backstop: 'Blend backstop',
  blend_auctions: 'Blend auctions',
  phoenix_stake: 'Phoenix stake',
  defindex: 'DeFindex',
  sorocredit: 'sorocredit',
  aquarius: 'Aquarius',
};

const numFmt = new Intl.NumberFormat('en-US');

/**
 * AccountActivitySummaryPanel — the segmented "what has this address
 * been doing" breakdown over GET /v1/accounts/{g}/activity: all-time
 * per-op-type counts, attributed trades total, per-(protocol, action)
 * DeFi event counts, and bridge-transfer counts. Sibling of
 * AccountDefiPositionsPanel (same conventions).
 *
 * The endpoint is stale-while-revalidate server-side; segments the
 * server could not read are ABSENT and named in coverage_note — this
 * panel renders that note verbatim rather than showing zeros.
 */
export function AccountActivitySummaryPanel({ id }: { id: string }) {
  const { data, isLoading, isError, error } = useQuery<AccountActivityResp>({
    queryKey: ['/v1/accounts/{id}/activity', id],
    enabled: id.length > 0,
    retry: false,
    queryFn: async () => {
      const env = await apiGet<Envelope<AccountActivityResp>>(
        `/v1/accounts/${encodeURIComponent(id)}/activity`,
      );
      return env.data;
    },
    staleTime: 60_000,
  });

  const source = asExample(`/v1/accounts/${id}/activity`);
  const panelHint =
    'all-time segmented counts — operations by type, attributed trades, DeFi actions, bridge transfers';

  if (isError) {
    return (
      <Panel title="Activity" hint={panelHint} source={source} bodyClassName="text-sm text-ink-body">
        The activity breakdown is warming or failed — reload to retry
        {error instanceof Error ? `: ${error.message}` : ''}.
      </Panel>
    );
  }

  if (isLoading || !data) {
    return (
      <Panel title="Activity" hint={panelHint} source={source} bodyClassName="text-sm text-ink-muted">
        Loading…
      </Panel>
    );
  }

  const ops = data.ops_by_type ?? [];
  const defi = data.defi_actions ?? [];
  const bridge = data.bridge_transfers;
  const bridgeTotal = bridge
    ? bridge.rozo_outbound_payments +
      bridge.rozo_inbound_payments +
      bridge.cctp_outbound_burns +
      bridge.cctp_inbound_mints
    : 0;

  const byProtocol = new Map<string, DefiActionCount[]>();
  for (const a of defi) {
    const list = byProtocol.get(a.protocol) ?? [];
    list.push(a);
    byProtocol.set(a.protocol, list);
  }

  return (
    <Panel title="Activity" hint={panelHint} source={source} bodyClassName="space-y-4">
      {data.coverage_note && (
        <p className="rounded-md border border-line bg-surface-muted px-3 py-2 text-xs text-ink-muted">
          {data.coverage_note}
        </p>
      )}

      <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-4">
        <ActivityStat
          label="Operations (all time)"
          value={
            data.ops_by_type != null
              ? numFmt.format(ops.reduce((n, c) => n + c.count, 0))
              : '—'
          }
        />
        <ActivityStat
          label="Attributed trades"
          value={data.trades_total != null ? numFmt.format(data.trades_total) : '—'}
        />
        <ActivityStat
          label="DeFi actions"
          value={
            data.defi_actions != null
              ? numFmt.format(defi.reduce((n, c) => n + c.count, 0))
              : '—'
          }
        />
        <ActivityStat label="Bridge transfers" value={bridge ? numFmt.format(bridgeTotal) : '—'} />
      </dl>

      {ops.length > 0 && (
        <div>
          <div className="mb-2 text-[11px] uppercase tracking-wider text-ink-muted">
            Operations by type — all time
          </div>
          {/* The account's operational fingerprint: is this a payment
              processor (payments dominate), a market maker (manage_offer),
              or a DeFi user (invoke_host_function)? Bars share one linear
              scale, largest first. */}
          <HBarList
            ariaLabel={`Operations by type: ${ops
              .map((c) => `${c.op_type} ${numFmt.format(c.count)}`)
              .join(', ')}`}
            items={[...ops]
              .sort((x, y) => y.count - x.count)
              .map((c) => ({
                label: c.op_type,
                value: c.count,
                display: numFmt.format(c.count),
              }))}
          />
        </div>
      )}

      {byProtocol.size > 0 && (
        <div>
          <div className="mb-1 text-[11px] uppercase tracking-wider text-ink-muted">
            DeFi actions by protocol
          </div>
          <ul className="space-y-1 text-xs">
            {Array.from(byProtocol.entries()).map(([protocol, actions]) => (
              <li key={protocol} className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
                <Badge tone="brand">{PROTOCOL_LABEL[protocol] ?? protocol}</Badge>
                {actions.map((a) => (
                  <span key={`${protocol}-${a.action}`} className="whitespace-nowrap text-ink-body">
                    {a.action}{' '}
                    <span className="font-mono tabular-nums text-ink-muted">
                      {numFmt.format(a.count)}
                    </span>
                  </span>
                ))}
              </li>
            ))}
          </ul>
        </div>
      )}

      {bridge && bridgeTotal > 0 && (
        <div>
          <div className="mb-2 text-[11px] uppercase tracking-wider text-ink-muted">
            <Link href="/bridges" className="hover:text-brand-600">
              Bridge transfers
            </Link>{' '}
            — inbound vs outbound
          </div>
          {/* Paired per-bridge bars on one shared count scale: inbound
              (Rozo inbound payments / CCTP mints) vs outbound (Rozo
              outbound payments / CCTP burns). */}
          <PairedBars
            ariaLabel={`Bridge transfers: Rozo in ${bridge.rozo_inbound_payments}, out ${bridge.rozo_outbound_payments}; CCTP mints ${bridge.cctp_inbound_mints}, burns ${bridge.cctp_outbound_burns}`}
            aLabel="Inbound"
            bLabel="Outbound"
            aColor="var(--color-up)"
            bColor="var(--color-down)"
            rows={[
              {
                label: 'Rozo',
                a: bridge.rozo_inbound_payments,
                b: bridge.rozo_outbound_payments,
                aDisplay: `${numFmt.format(bridge.rozo_inbound_payments)} in`,
                bDisplay: `${numFmt.format(bridge.rozo_outbound_payments)} out`,
              },
              {
                label: 'CCTP',
                a: bridge.cctp_inbound_mints,
                b: bridge.cctp_outbound_burns,
                aDisplay: `${numFmt.format(bridge.cctp_inbound_mints)} mints`,
                bDisplay: `${numFmt.format(bridge.cctp_outbound_burns)} burns`,
              },
            ]}
          />
          <p className="mt-1 text-[11px] text-ink-faint">{bridge.note}</p>
        </div>
      )}
    </Panel>
  );
}

function ActivityStat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-[11px] uppercase tracking-wider text-ink-muted">{label}</dt>
      <dd className="mt-0.5 font-mono text-sm tabular-nums">{value}</dd>
    </div>
  );
}
