'use client';

import { HBarList } from '@/components/charts/Bars';
import { formatCompact } from '@/lib/format';
import { protocolMeta } from './registry';

// Mirrors internal/api/v1/dex_tvl_cache.go ProtocolTVLView — served on
// every /v1/protocols row that has a TVL derivation (currently the AMMs).
export interface ProtocolTvl {
  tvl_usd: string;
  pools_total: number;
  pools_priced: number;
  unpriced_pools: number;
  as_of?: string;
  basis?: string;
}

export interface ProtocolTvlRow {
  name: string;
  tvl?: ProtocolTvl | null;
}

/**
 * ProtocolTvlPanel — "where does Stellar's DeFi liquidity sit": one bar
 * per protocol with a served TVL, largest first. HONESTY: a protocol's
 * TVL is a LOWER BOUND when it has unpriced pools (unpriced legs
 * contribute $0 — dex_tvl_cache.go) — those bars carry a "≥" prefix, a
 * hatched tail, and the priced/total pool split; each row's tooltip is
 * the server's own `basis` sentence. Protocols with no TVL derivation
 * (order book, lending, bridges, oracles) are simply absent — never
 * charted as $0.
 */
export function ProtocolTvlPanel({ rows }: { rows: ProtocolTvlRow[] }) {
  const withTvl = rows
    .filter((r): r is ProtocolTvlRow & { tvl: ProtocolTvl } => r.tvl != null)
    .map((r) => ({ ...r, usd: Number(r.tvl.tvl_usd) }))
    .filter((r) => Number.isFinite(r.usd) && r.usd > 0)
    .sort((a, b) => b.usd - a.usd);

  if (withTvl.length === 0) return null;

  const anyUnpriced = withTvl.some((r) => r.tvl.unpriced_pools > 0);

  return (
    <div className="rounded-card border border-line bg-surface p-5">
      <h2 className="text-h3 font-semibold text-ink">Value locked (USD)</h2>
      <p className="mb-3 mt-1 text-xs text-ink-muted">
        Current pool reserves valued through the served USD price tiers, per
        protocol. Source: <code className="font-mono">/v1/protocols</code>{' '}
        <code className="font-mono">tvl</code>.
      </p>
      <HBarList
        ariaLabel={`Value locked per protocol: ${withTvl
          .map((r) => `${r.name} $${formatCompact(r.usd)}`)
          .join(', ')}`}
        items={withTvl.map((r) => ({
          label: protocolMeta(r.name)?.label ?? r.name,
          value: r.usd,
          display: `${r.tvl.unpriced_pools > 0 ? '≥ ' : ''}$${formatCompact(r.usd)}`,
          annotation:
            r.tvl.unpriced_pools > 0
              ? `${r.tvl.pools_priced}/${r.tvl.pools_total} pools priced`
              : `${r.tvl.pools_total} pool${r.tvl.pools_total === 1 ? '' : 's'}`,
          hatchTail: r.tvl.unpriced_pools > 0,
          title: r.tvl.basis,
        }))}
      />
      {anyUnpriced && (
        <p className="mt-3 text-[11px] leading-relaxed text-ink-muted">
          Hatched bars are <strong>lower bounds</strong>: pools whose assets
          have no served USD price contribute $0, so the true figure is at
          least what&apos;s shown. Hover a bar for that protocol&apos;s exact
          valuation basis.
        </p>
      )}
    </div>
  );
}
