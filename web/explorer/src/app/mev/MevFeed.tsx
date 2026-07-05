'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetText } from '@/components/AssetLink';
import { apiGet, asExample } from '@/api/client';

interface MevLeg {
  source: string;
  base: string;
  quote: string;
  base_amount: string;
  quote_amount: string;
  op_index: number;
  // ordering-aware kinds (sandwich / oracle_sandwich) add:
  tx_hash?: string;
  tx_index?: number;
  account?: string;
  role?: string; // bracket | victim | before | after
  // wash_trade legs add:
  maker?: string;
  taker?: string;
  ledger?: number;
}

interface MevFillRef {
  pool: string;
  user: string;
  filler?: string;
  auction_type: number;
  ledger: number;
  tx_hash: string;
}

interface MevDetail {
  assets?: string[];
  sources?: string[];
  legs?: MevLeg[];
  notional_usd?: string;
  note?: string;
  // sandwich / wash_trade
  pair?: string;
  attacker?: string;
  variant?: string; // self_trade | round_trip
  // oracle_sandwich
  oracle_source?: string;
  asset?: string;
  // liquidation_cascade
  fill?: MevFillRef;
  prior_fills?: MevFillRef[];
  window_ledgers?: number;
}

interface MevEvent {
  event_id: string;
  detected_at: string;
  detected_at_ledger: number;
  kind: string;
  tx_hashes: string[];
  accounts: string[];
  detail: MevDetail;
  profit_usd: string | null;
}

const usdFmt = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 });

const KIND_LABELS: Record<string, string> = {
  arbitrage: 'arbitrage',
  sandwich: 'sandwich',
  oracle_sandwich: 'oracle sandwich',
  liquidation_cascade: 'liq. cascade',
  wash_trade: 'wash trade',
};

/** The distinct assets an event touches, whatever its kind's detail shape. */
function eventAssets(e: MevEvent): string[] {
  if (e.detail.assets && e.detail.assets.length > 0) return e.detail.assets;
  if (e.detail.pair) return e.detail.pair.split('|');
  if (e.detail.asset) return [e.detail.asset];
  return [];
}

function evidenceCount(e: MevEvent): string {
  if (e.kind === 'liquidation_cascade') {
    const n = (e.detail.prior_fills?.length ?? 0) + 1;
    return `${n} fills`;
  }
  const n = e.detail.legs?.length ?? 0;
  return `${n} legs`;
}

export function MevFeed() {
  const q = useQuery<MevEvent[]>({
    queryKey: ['/v1/mev'],
    queryFn: async () => {
      const env = await apiGet<{ data: MevEvent[] }>('/v1/mev', { limit: 50 });
      return env.data ?? [];
    },
    staleTime: 60_000,
  });

  const rows = q.data ?? [];

  return (
    <Panel
      title={`Detected MEV events${rows.length > 0 ? ` (${rows.length})` : ''}`}
      hint="All kinds, newest first. Positional/structural evidence — direction and profit are never inferred; each event's detail.note states exactly what is claimed."
      source={asExample('/v1/mev', { limit: 50 })}
      bodyClassName="space-y-3"
    >
      {q.isLoading && <p className="text-sm text-ink-muted">Loading…</p>}
      {q.isError && (
        <p className="text-sm text-ink-muted">
          The MEV feed is unavailable right now.
        </p>
      )}
      {!q.isLoading && !q.isError && rows.length === 0 && (
        <p className="text-sm text-ink-muted">
          No MEV events detected in the recent window yet. The detectors
          scan the trade / auction / oracle streams every few minutes.
        </p>
      )}
      {rows.length > 0 && (
        <ul className="divide-y divide-line-subtle">
          {rows.map((e) => {
            const assets = eventAssets(e);
            const isCycle = e.kind === 'arbitrage' && (e.detail.assets?.length ?? 0) > 0;
            const sources = e.detail.sources ?? [];
            const actor = e.accounts[0] ?? '';
            const tx = e.tx_hashes[0] ?? '';
            return (
              <li key={e.event_id} className="py-3 text-sm">
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                  <span className="inline-block rounded-sm bg-down-subtle px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wider text-down-strong">
                    {KIND_LABELS[e.kind] ?? e.kind}
                  </span>
                  {e.kind === 'wash_trade' && e.detail.variant && (
                    <span className="text-[11px] text-ink-muted">
                      {e.detail.variant === 'self_trade' ? 'self-cross' : 'round trip'}
                    </span>
                  )}
                  {assets.length > 0 && (
                    <span className="inline-flex flex-wrap items-center gap-1 font-mono text-xs text-ink-body">
                      {assets.map((a, i) => (
                        <span key={`${a}-${i}`} className="inline-flex items-center gap-1">
                          {i > 0 && <span className="text-ink-faint">{isCycle ? '→' : '/'}</span>}
                          <AssetText canonical={a} />
                        </span>
                      ))}
                      {isCycle && (
                        <>
                          <span className="text-ink-faint">→</span>
                          <AssetText canonical={assets[0]} />
                        </>
                      )}
                    </span>
                  )}
                  {e.kind === 'liquidation_cascade' && e.detail.fill && (
                    <span className="font-mono text-xs text-ink-body" title={e.detail.fill.pool}>
                      pool {e.detail.fill.pool.slice(0, 6)}…{e.detail.fill.pool.slice(-4)}
                    </span>
                  )}
                  {sources.length > 0 && (
                    <span className="text-[11px] text-ink-muted">
                      via{' '}
                      {sources.map((s, i) => (
                        <span key={s}>
                          {i > 0 && ', '}
                          <Link href={`/sources/${encodeURIComponent(s)}`} className="hover:text-brand-600 hover:underline">
                            {s}
                          </Link>
                        </span>
                      ))}
                    </span>
                  )}
                  {e.detail.notional_usd && (
                    <span className="font-mono text-xs text-ink-body">
                      {usdFmt.format(Number(e.detail.notional_usd))}
                    </span>
                  )}
                  <span className="ml-auto font-mono text-[11px] text-ink-muted">
                    ledger {e.detected_at_ledger.toLocaleString()}
                  </span>
                </div>
                <div className="mt-1 flex flex-wrap gap-x-4 text-[11px] text-ink-muted">
                  {actor && (
                    <Link
                      href={`/accounts/${encodeURIComponent(actor)}/`}
                      className="font-mono hover:text-brand-600 hover:underline"
                      title={actor}
                    >
                      account {actor.slice(0, 6)}…{actor.slice(-4)}
                    </Link>
                  )}
                  {tx && (
                    <Link
                      href={`/transactions/${encodeURIComponent(tx)}/`}
                      className="font-mono hover:text-brand-600 hover:underline"
                    >
                      tx {tx.slice(0, 8)}…
                    </Link>
                  )}
                  {e.tx_hashes.length > 1 && <span>{e.tx_hashes.length} txs</span>}
                  <span>{evidenceCount(e)}</span>
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </Panel>
  );
}
