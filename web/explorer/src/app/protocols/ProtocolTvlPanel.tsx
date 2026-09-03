'use client';

import { HATCH_BG, HBarList } from '@/components/charts/Bars';
import { formatCompact, formatDecimalAmount, formatRelative } from '@/lib/format';
import { protocolMeta } from './registry';

// Mirrors internal/api/v1/dex_tvl_cache.go ProtocolTVLView — served on
// every /v1/protocols row that has a TVL derivation (currently the AMMs).
export interface ProtocolTvl {
  tvl_usd: string;
  pools_total: number;
  pools_priced: number;
  unpriced_pools: number;
  as_of?: string;
  as_of_ledger?: number;
  basis?: string;
}

// Mirrors internal/api/v1/dex_tvl_total.go DEXTVLTotalView — the
// `tvl_total` object on /v1/protocols. ABSENT (undefined) whenever the
// reconciliation could admit nothing; see DexTvlHeadline.
export interface DexTvlTotal {
  tvl_usd: string;
  protocols: string[];
  lower_bound: boolean;
  pools_total: number;
  pools_priced: number;
  unpriced_pools: number;
  as_of_ledger?: number;
  as_of: string;
  basis: string;
  excluded: { subject: string; reason: string }[];
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
 *
 * `total` is the served headline (`/v1/protocols` `tvl_total`), rendered
 * by DexTvlHeadline above the bars. It is OPTIONAL on the wire and
 * optional here: see that component for why absence stays absent.
 */
export function ProtocolTvlPanel({
  rows,
  total,
}: {
  rows: ProtocolTvlRow[];
  total?: DexTvlTotal | null;
}) {
  const withTvl = rows
    .filter((r): r is ProtocolTvlRow & { tvl: ProtocolTvl } => r.tvl != null)
    .map((r) => ({ ...r, usd: Number(r.tvl.tvl_usd) }))
    .filter((r) => Number.isFinite(r.usd) && r.usd > 0)
    .sort((a, b) => b.usd - a.usd);

  if (withTvl.length === 0) return null;

  const anyUnpriced = withTvl.some((r) => r.tvl.unpriced_pools > 0);

  // The headline is the exact sum of the bars below it, so it may only
  // appear when every protocol it sums is actually charted. Under the
  // category filter (and on /bridges, /yield) some summed protocol can
  // drop out of `rows`, and a total that no longer reconciles with the
  // visible rows is a bare number again.
  const charted = new Set(withTvl.map((r) => r.name));
  const headline =
    total != null &&
    total.protocols.length > 0 &&
    total.protocols.every((p) => charted.has(p))
      ? total
      : null;

  return (
    <div className="rounded-card border border-line bg-surface p-5">
      <h2 className="text-h3 font-semibold text-ink">Value locked (USD)</h2>
      <p className="mb-3 mt-1 text-xs text-ink-muted">
        Current pool reserves valued through the served USD price tiers, per
        protocol. Source: <code className="font-mono">/v1/protocols</code>{' '}
        <code className="font-mono">tvl</code>.
      </p>
      {headline && <DexTvlHeadline total={headline} />}
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

/**
 * DexTvlHeadline — the served `tvl_total`, rendered with its provenance
 * attached rather than as a bare number.
 *
 * Three states, all deliberate:
 *
 *  - **Total.** `lower_bound: false` — every summed protocol priced every
 *    pool. Plain "$40,538,494.54".
 *  - **Lower bound.** `lower_bound: true` — the same "≥" prefix and
 *    hatch mark the bars below already use, plus the priced/total pool
 *    split, so the headline degrades exactly the way its parts do.
 *  - **Absent.** `tvl_total` is `omitempty` and is OMITTED when the
 *    reconciliation could admit nothing (dex_tvl_total.go). This
 *    component is then not rendered AT ALL — no "$0.00", no "—".
 *    Both would read as "Stellar's AMMs hold nothing", which is the one
 *    reading that is definitely wrong, and a dash beside four non-zero
 *    bars is worse than silence. The caller passes `undefined` and the
 *    panel simply shows its bars.
 *
 * `basis` (one line of provenance) is rendered as prose, not a tooltip,
 * and `excluded[]` sits one click away in a `<details>` whose summary
 * carries the count — both reachable from the surface rather than
 * hidden behind a hover a touch device cannot perform.
 *
 * The figure is formatted from the DECIMAL STRING via
 * `formatDecimalAmount` (BigInt + Intl). Nothing here calls `Number()`
 * on money: `usd` on the bars above is geometry, this is the figure.
 */
function DexTvlHeadline({ total }: { total: DexTvlTotal }) {
  const figure = formatDecimalAmount(total.tvl_usd);
  // A total whose own decimal string will not parse is not renderable as
  // a number; showing the raw string would be a bare figure with no
  // grouping and no way to tell it apart from a real one.
  if (figure == null) return null;

  return (
    <div className="mb-4 rounded-card border border-line bg-surface-subtle p-4">
      <div className="text-[11px] font-medium uppercase tracking-wider text-ink-muted">
        Total value locked
      </div>
      <div className="mt-1 flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="font-mono text-3xl font-semibold tracking-tight tnum text-ink">
          {total.lower_bound && (
            <span className="text-ink-muted" aria-hidden>
              ≥{' '}
            </span>
          )}
          ${figure}
        </span>
        {total.lower_bound && (
          <span
            aria-hidden
            className="inline-block h-3 w-6 self-center rounded-xs text-ink-faint opacity-70"
            style={{ backgroundImage: HATCH_BG }}
          />
        )}
        <span className="text-xs text-ink-muted tnum">
          {total.as_of_ledger != null && total.as_of_ledger > 0 && (
            <>ledger {total.as_of_ledger.toLocaleString('en-US')} · </>
          )}
          <time dateTime={total.as_of} title={total.as_of}>
            {formatRelative(total.as_of)}
          </time>
        </span>
      </div>
      <p className="mt-2 text-xs leading-relaxed text-ink-muted">
        {total.lower_bound && (
          <>
            <strong>At least</strong> this — {total.pools_priced.toLocaleString('en-US')}{' '}
            of {total.pools_total.toLocaleString('en-US')} pools priced;
            unpriceable reserves contribute $0.{' '}
          </>
        )}
        {total.basis}
      </p>
      {total.excluded.length > 0 && (
        <details className="group mt-2 rounded-lg border border-line">
          <summary className="cursor-pointer select-none px-3 py-1.5 text-xs font-medium text-ink-body marker:text-ink-faint hover:text-brand-600">
            What this total excludes{' '}
            <span className="text-ink-faint">({total.excluded.length})</span>
          </summary>
          <dl className="space-y-1.5 border-t border-line px-3 py-2 text-[11px] leading-relaxed">
            {total.excluded.map((e) => (
              <div key={e.subject}>
                <dt className="font-mono text-ink-body">{e.subject}</dt>
                <dd className="text-ink-muted">{e.reason}</dd>
              </div>
            ))}
          </dl>
        </details>
      )}
    </div>
  );
}
