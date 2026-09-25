'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink, assetSlug, shortAssetText } from '@/components/AssetLink';
import { DonutChart } from '@/components/charts/DonutChart';
import {
  Stat,
  StatGrid,
  StatCell,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { CURRENT_NETWORK } from '@/lib/networks';
import { formatCompact, formatPriceSmall, formatRelative } from '@/lib/format';
import { scaledUnits } from '../explorer-shared';

// Mirror of the slice of AccountStateResp we need (kept local so this
// reads from the SAME React Query cache key the AccountView state panel
// populates — no extra round trip).
interface AccountStateResp {
  account_id: string;
  exists: boolean;
  balance?: string;
  trustlines?: { asset: string; balance: string }[];
}

type PriceBatchEnvelope = components['schemas']['PriceBatchEnvelope'];
type PriceType = components['schemas']['Price']['price_type'];

/**
 * What the batch actually said about one holding's price. RLT-384: this
 * used to be a bare `number`, which threw away the declared basis and
 * the observation time — so a `peg` (the operator's standing 1:1
 * declaration) valued a portfolio under a panel captioned "valued at the
 * live VWAP", and a rate the API stamped hours ago read as current.
 */
interface PricedAt {
  price: number;
  /** Decimal string exactly as the API served it — RLT-069: the float
   * `price` above is display-only; the multiply against a holding's
   * balance must run on this, never on the floated copy. */
  priceRaw: string;
  priceType: PriceType | null;
  observedAt: string | null;
}

/**
 * Exact `baseUnits (integer, `decimals` places) × priceRaw (decimal
 * string)`, in USD cents, computed entirely in BigInt. RLT-069: the
 * previous `Number(amount) * Number(price)` float multiply, summed
 * again as a float across holdings, doesn't preserve Σ parts == whole
 * for a portfolio total (AGENTS.md invariant 1 — never accumulate
 * money in float64). Returns null on unparseable input.
 */
function valueUsdCents(
  baseUnits: string,
  decimals: number,
  priceRaw: string,
): bigint | null {
  const amountMatch = /^-?\d+$/.exec(baseUnits.trim());
  const priceMatch = /^(-?)(\d+)(?:\.(\d+))?$/.exec(priceRaw.trim());
  if (!amountMatch || !priceMatch) return null;
  const [, priceSign, priceWhole, priceFrac = ''] = priceMatch;
  const amount = BigInt(amountMatch[0]);
  const priceScaled = BigInt(`${priceSign}${priceWhole}${priceFrac}`);
  const scale = BigInt(decimals + priceFrac.length);
  const numerator = amount * priceScaled * 100n;
  const denom = 10n ** scale;
  // Round half away from zero at the cent boundary.
  const half = denom / 2n;
  return numerator >= 0n
    ? (numerator + half) / denom
    : -((-numerator + half) / denom);
}

interface PriceBatch {
  byAsset: Record<string, PricedAt>;
  /** `flags.stale` — the OR over the rows the batch returned. */
  stale: boolean;
  /** Oldest `observed_at` among the rows priced here. */
  observedAt: string | null;
}

interface Holding {
  asset: string;
  amount: number; // display units (stroops → ÷1e7)
  priceUSD: number | null;
  priceType: PriceType | null;
  valueUSD: number | null;
  /** Exact cents behind valueUSD — the portfolio total sums THIS, never
   * the re-floated `valueUSD` (RLT-069: float re-sum reintroduces the
   * per-cent rounding error the BigInt multiply just removed). */
  valueCents: bigint | null;
}

const usdFmt = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  maximumFractionDigits: 2,
});

/**
 * AccountPositions — the account's portfolio: native XLM + every
 * trustline balance, valued in USD via /v1/price/batch (the same VWAP
 * the rest of the site prices with). Shows a total, a per-asset table
 * sorted by value with % allocation, and an allocation donut. Holdings
 * we can't price (illiquid trustlines) still appear, valued "—" and
 * excluded from the allocation split.
 */
export function AccountPositions({ id }: { id: string }) {
  // Shares the AccountView state-panel cache (same queryKey) — RQ
  // dedupes, so this doesn't add a request.
  const stateQ = useQuery<AccountStateResp>({
    queryKey: ['/v1/accounts/{id}', id],
    enabled: id.length > 0,
    retry: false,
    queryFn: async () =>
      (
        await apiGet<{ data: AccountStateResp }>(
          `/v1/accounts/${encodeURIComponent(id)}`,
        )
      ).data,
    staleTime: 30_000,
  });

  // Asset_ids held: native (when there's a balance) + each trustline.
  const state = stateQ.data;
  const assetIds: string[] = [];
  if (state?.exists) {
    if (state.balance && Number(state.balance) > 0) assetIds.push('native');
    for (const t of state.trustlines ?? []) {
      if (Number(t.balance) > 0) assetIds.push(t.asset);
    }
  }

  const pricesQ = useQuery<PriceBatch>({
    queryKey: ['/v1/price/batch', 'positions', assetIds.join(',')],
    // No aggregator on the lean test nets → /v1/price/batch is empty; skip the
    // portfolio valuation there (the USD tiles/columns null-degrade to "—").
    enabled: assetIds.length > 0 && CURRENT_NETWORK.pricing,
    retry: false,
    staleTime: 30_000,
    // RLT-387: a `staleTime` alone only re-fetches on the visitor's next
    // interaction — an open tab's valuation goes stale and stays stale.
    // Same live-refresh interval as the converter's identical
    // `/v1/price/batch` read (ConvertLive.tsx's useConvertRate).
    refetchInterval: 60_000,
    queryFn: async () => {
      const env = await apiGet<PriceBatchEnvelope>('/v1/price/batch', {
        asset_ids: assetIds.join(','),
        quote: 'fiat:USD',
      });
      const byAsset: Record<string, PricedAt> = {};
      // Instants, not strings: RFC 3339 stamps carry variable fractional
      // precision and lexicographic order gets "…00Z" vs "…00.5Z" wrong.
      let observedAt: string | null = null;
      let oldestMs = Number.POSITIVE_INFINITY;
      for (const row of env.data ?? []) {
        if (!row.price) continue;
        const p = Number(row.price);
        if (!(Number.isFinite(p) && p > 0)) continue;
        const at: PricedAt = {
          price: p,
          priceRaw: row.price,
          priceType: row.price_type ?? null,
          observedAt: row.observed_at ?? null,
        };
        byAsset[row.asset_id] = at;
        // /v1/price/batch echoes native XLM as `crypto:XLM`; alias both
        // forms so a `native` holding resolves its price.
        if (row.asset_id === 'crypto:XLM') byAsset.native = at;
        if (row.asset_id === 'native') byAsset['crypto:XLM'] = at;
        const ms = at.observedAt != null ? Date.parse(at.observedAt) : NaN;
        if (Number.isFinite(ms) && ms < oldestMs) {
          oldestMs = ms;
          observedAt = at.observedAt;
        }
      }
      return { byAsset, stale: Boolean(env.flags?.stale), observedAt };
    },
  });

  if (stateQ.isLoading) {
    return (
      <Panel title="Positions" bodyClassName="text-sm text-ink-muted">
        Loading balances…
      </Panel>
    );
  }
  if (!state?.exists) {
    // No live state captured — the existing State panel already
    // explains the ledger-entry-window caveat, so stay quiet here.
    return null;
  }

  const priceMap = pricesQ.data?.byAsset ?? {};
  const holdings: Holding[] = assetIds.map((asset) => {
    const raw =
      asset === 'native'
        ? (state.balance ?? '0')
        : ((state.trustlines ?? []).find((t) => t.asset === asset)?.balance ??
          '0');
    // Balances are stroop integers (7 decimals, ADR-0003); scale via the
    // string-split path so a >9e8-XLM holding doesn't lose low digits to
    // Number() before it feeds the USD total / allocation split.
    const amount = scaledUnits(raw, 7);
    const priced = priceMap[asset] ?? null;
    const priceUSD = priced?.price ?? null;
    // Exact BigInt multiply on the raw stroop integer and the API's
    // decimal price string — RLT-069, see valueUsdCents.
    const cents = priced ? valueUsdCents(raw, 7, priced.priceRaw) : null;
    const valueUSD = cents != null ? Number(cents) / 100 : null;
    return {
      asset,
      amount,
      priceUSD,
      priceType: priced?.priceType ?? null,
      valueUSD,
      valueCents: cents,
    };
  });
  holdings.sort((a, b) => (b.valueUSD ?? -1) - (a.valueUSD ?? -1));

  const totalCents = holdings.reduce(
    (sum, h) => sum + (h.valueCents ?? 0n),
    0n,
  );
  const total = Number(totalCents) / 100;
  const pricedCount = holdings.filter((h) => h.valueUSD != null).length;
  // An unpriced positive balance adds $0 to the sum, so the total is a floor
  // (AGENTS.md invariant 1): mark it "≥" and name what it leaves out.
  const unpricedCount = holdings.length - pricedCount;
  const lowerBound = unpricedCount > 0;
  const totalText = `${lowerBound ? '≥ ' : ''}${usdFmt.format(total)}`;
  const excludedText = `excludes ${unpricedCount} unpriced`;
  const shareOf = lowerBound ? 'of priced value' : 'of value';
  const slices = holdings
    .filter((h) => h.valueUSD != null && h.valueUSD > 0)
    .map((h) => {
      // FEC audit A5-05: the local assetSlug fork lacked the canonical's
      // colon-form/C-id/length guards and could emit 404 links — the exact
      // failure its own comment said it prevented. Canonical returns null
      // for unlinkable ids; the slice then renders unlinked.
      const slug = assetSlug(h.asset);
      return {
        label: shortAssetText(h.asset),
        value: h.valueUSD as number,
        ...(slug ? { href: `/assets/${encodeURIComponent(slug)}` } : {}),
      };
    });

  if (holdings.length === 0) {
    return (
      <Panel title="Positions" bodyClassName="text-sm text-ink-muted">
        No non-zero balances in the captured ledger-entry window.
      </Panel>
    );
  }

  return (
    <Panel
      title="Positions"
      hint="Native XLM + trustline balances, valued at the USD price the pricing API serves for each one. A price it declares as something other than an observed market rate is labelled in the Price column. Holdings it won't price are listed without a USD value."
      source={asExample(`/v1/accounts/${id}`)}
      bodyClassName="space-y-4"
    >
      <StatGrid cols={3}>
        <StatCell>
          <Stat
            label="Portfolio value"
            value={total > 0 ? totalText : '—'}
            sub={lowerBound && total > 0 ? excludedText : undefined}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Holdings"
            value={holdings.length.toLocaleString('en-US')}
            sub={`${pricedCount} priced`}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Top holding"
            value={slices[0] ? slices[0].label : '—'}
            sub={
              slices[0] && total > 0
                ? `${((slices[0].value / total) * 100).toFixed(1)}% ${shareOf}`
                : undefined
            }
          />
        </StatCell>
      </StatGrid>

      {/* The valuation is only as fresh as the prices behind it, and the
          API says how fresh that is — the panel used to claim a "live
          VWAP" and show nothing at all (RLT-384). */}
      {pricesQ.data?.observedAt != null && (
        <p className="text-ink-muted text-xs">
          Prices observed {formatRelative(pricesQ.data.observedAt)}
          {pricesQ.data.stale && ' · flagged stale by the pricing API'}
        </p>
      )}

      {slices.length > 1 && total > 0 && (
        <DonutChart
          data={slices}
          centerLabel={totalText.replace(/\.00$/, '')}
          centerSub={lowerBound ? `value · ${excludedText}` : 'value'}
          formatValue={(n) => usdFmt.format(n)}
        />
      )}

      <TableWrap>
        <Table>
          <THead>
            <TR className="hover:bg-transparent">
              <Th>Asset</Th>
              <Th align="right">Balance</Th>
              <Th align="right">Price</Th>
              <Th align="right">Value</Th>
              <Th align="right">
                {lowerBound ? 'Allocation (priced)' : 'Allocation'}
              </Th>
            </TR>
          </THead>
          <TBody>
            {holdings.map((h) => (
              <TR key={h.asset}>
                <Td>
                  <AssetLink canonical={h.asset} />
                </Td>
                <Td align="right" className="text-ink-body font-mono">
                  {formatCompact(h.amount)}
                </Td>
                <Td align="right" className="font-mono">
                  {h.priceUSD != null
                    ? `$${formatPriceSmall(h.priceUSD)}`
                    : '—'}
                  {h.priceType === 'peg' && (
                    <span
                      className="text-ink-muted ml-1.5 text-[10px] tracking-wider uppercase"
                      title="Declared 1:1 peg — not an observed market price"
                    >
                      peg
                    </span>
                  )}
                </Td>
                <Td align="right" className="font-mono">
                  {h.valueUSD != null ? usdFmt.format(h.valueUSD) : '—'}
                </Td>
                <Td align="right" className="text-ink-muted font-mono">
                  {h.valueUSD != null && total > 0
                    ? `${((h.valueUSD / total) * 100).toFixed(1)}%`
                    : '—'}
                </Td>
              </TR>
            ))}
          </TBody>
        </Table>
      </TableWrap>
    </Panel>
  );
}
