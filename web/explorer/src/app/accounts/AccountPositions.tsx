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
import { fetchPriceBatchChunked, isPriceableAssetId } from '@/lib/price-batch';
import type { components } from '@/api/types';
import { assetHref } from '@/lib/fiat-slugs';
import { CURRENT_NETWORK } from '@/lib/networks';
import {
  baseUnitsDecimal,
  compareDecimalStrings,
  formatCompactUnits,
  formatPriceSmall,
  formatReadable,
  formatRelative,
  ratioPct,
} from '@/lib/format';

// Mirror of the slice of AccountStateResp we need (kept local so this
// reads from the SAME React Query cache key the AccountView state panel
// populates — no extra round trip).
interface AccountStateResp {
  account_id: string;
  exists: boolean;
  balance?: string;
  trustlines?: { asset: string; balance: string }[];
}

type PriceType = components['schemas']['Price']['price_type'];

/**
 * What the batch actually said about one holding's price. A bare
 * `number` would throw away the declared basis and
 * the observation time — so a `peg` (the operator's standing 1:1
 * declaration) valued a portfolio under a panel captioned "valued at the
 * live VWAP", and a rate the API stamped hours ago read as current.
 */
interface PricedAt {
  /** Decimal string exactly as the API served it; display and the
   * balance multiply both run on it, never on a float. */
  priceRaw: string;
  priceType: PriceType | null;
  observedAt: string | null;
}

/**
 * Exact `baseUnits (integer, `decimals` places) × priceRaw (decimal
 * string)`, in USD cents, computed entirely in BigInt. The
 * naive `Number(amount) * Number(price)` float multiply, summed
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
  /** Holdings whose batch chunk the API rejected — unanswered, not unpriced. */
  failedCount: number;
  /**
   * Asset ids the pricing API's serving gate refused (thin market,
   * flagged issuer). The API SAW these and declined to price
   * them, which is a different fact from a holding it never observed at
   * all, and a portfolio must not report the two alike as "unpriced".
   */
  withheldIds: Set<string>;
}

interface Holding {
  asset: string;
  balance: string; // stroops
  priceUSD: string | null;
  priceType: PriceType | null;
  valueUSD: number | null;
  /** Exact cents behind valueUSD — the portfolio total sums THIS, never
   * the re-floated `valueUSD` (a float re-sum reintroduces the
   * per-cent rounding error the BigInt multiply just removed). */
  valueCents: bigint | null;
  /** True when the pricing API withheld this asset's price rather than
   * never having observed it. */
  withheld: boolean;
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

  // A pool share or unknown_asset id would 400 its whole batch request;
  // those holdings stay listed, unpriced.
  const priceableIds = assetIds.filter(isPriceableAssetId);

  const pricesQ = useQuery<PriceBatch>({
    queryKey: ['/v1/price/batch', 'positions', priceableIds.join(',')],
    // No aggregator on the lean test nets → /v1/price/batch is empty; skip the
    // portfolio valuation there (the USD tiles/columns null-degrade to "—").
    enabled: priceableIds.length > 0 && CURRENT_NETWORK.pricing,
    retry: false,
    staleTime: 30_000,
    // A `staleTime` alone only re-fetches on the visitor's next
    // interaction — an open tab's valuation goes stale and stays stale.
    // Same live-refresh interval as the converter's identical
    // `/v1/price/batch` read (ConvertLive.tsx's useConvertRate).
    refetchInterval: 60_000,
    queryFn: async () => {
      const batch = await fetchPriceBatchChunked(priceableIds, 'fiat:USD');
      const byAsset: Record<string, PricedAt> = {};
      // Instants, not strings: RFC 3339 stamps carry variable fractional
      // precision and lexicographic order gets "…00Z" vs "…00.5Z" wrong.
      let observedAt: string | null = null;
      let oldestMs = Number.POSITIVE_INFINITY;
      for (const row of batch.rows) {
        if (!row.price || compareDecimalStrings(row.price, '0') !== 1) continue;
        const at: PricedAt = {
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
      return {
        byAsset,
        stale: batch.stale,
        observedAt,
        failedCount: batch.failedIds.length,
        withheldIds: new Set(batch.withheld),
      };
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
  const withheldIds = pricesQ.data?.withheldIds ?? new Set<string>();
  const holdings: Holding[] = assetIds.map((asset) => {
    const raw =
      asset === 'native'
        ? (state.balance ?? '0')
        : ((state.trustlines ?? []).find((t) => t.asset === asset)?.balance ??
          '0');
    const priced = priceMap[asset] ?? null;
    const priceUSD = priced?.priceRaw ?? null;
    // Exact BigInt multiply on the raw stroop integer and the API's
    // decimal price string; see valueUsdCents.
    const cents = priced ? valueUsdCents(raw, 7, priced.priceRaw) : null;
    const valueUSD = cents != null ? Number(cents) / 100 : null;
    return {
      asset,
      balance: raw,
      priceUSD,
      priceType: priced?.priceType ?? null,
      valueUSD,
      valueCents: cents,
      withheld: withheldIds.has(asset),
    };
  });
  holdings.sort((a, b) => (b.valueUSD ?? -1) - (a.valueUSD ?? -1));

  const totalCents = holdings.reduce(
    (sum, h) => sum + (h.valueCents ?? 0n),
    0n,
  );
  const total = Number(totalCents) / 100;
  const sharePct = (cents: bigint | null) =>
    (ratioPct(String(cents ?? 0n), String(totalCents), 1) ?? 0).toFixed(1);
  const pricedCount = holdings.filter((h) => h.valueUSD != null).length;
  // An unpriced positive balance adds $0 to the sum, so the total is a floor
  // (AGENTS.md invariant 1): mark it "≥" and name what it leaves out.
  const unpricedCount = holdings.length - pricedCount;
  // A rejected lookup is not "no price": say so rather than let it read as
  // an illiquid holding.
  const priceFailedCount =
    pricesQ.isError && !pricesQ.data
      ? priceableIds.length
      : (pricesQ.data?.failedCount ?? 0);
  // A holding the pricing API SAW and refused (thin market,
  // flagged issuer) is a different fact from one it never observed —
  // carve it out of the generic "unpriced" bucket rather than reporting
  // both alike.
  const withheldCount = holdings.filter(
    (h) => h.valueUSD == null && h.withheld,
  ).length;
  const neverObservedCount = unpricedCount - withheldCount;
  const lowerBound = unpricedCount > 0;
  const totalExact = baseUnitsDecimal(String(totalCents), 2);
  const totalText = `${lowerBound ? '≥ ' : ''}${formatReadable(totalExact, true)}`;
  const excludedParts: string[] = [];
  if (neverObservedCount > 0)
    excludedParts.push(`${neverObservedCount} unpriced`);
  if (withheldCount > 0) excludedParts.push(`${withheldCount} withheld`);
  const excludedText = `excludes ${excludedParts.join(', ')}`;
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
        id: h.asset,
        label: shortAssetText(h.asset),
        value: h.valueUSD as number,
        // Exact cents (not the re-floated `value`) drive the donut's
        // legend %, same invariant as the table's `sharePct` below.
        decimal: h.valueCents != null ? String(h.valueCents) : undefined,
        cents: h.valueCents,
        ...(slug ? { href: assetHref(slug) } : {}),
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
      hint="Valued at served USD prices"
      source={asExample(`/v1/accounts/${id}`)}
      bodyClassName="space-y-4"
    >
      <StatGrid cols={3}>
        <StatCell>
          <Stat
            label="Portfolio value"
            value={
              total > 0 ? (
                <span title={`$${totalExact}`}>{totalText}</span>
              ) : (
                '—'
              )
            }
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
                ? `${sharePct(slices[0].cents)}% ${shareOf}`
                : undefined
            }
          />
        </StatCell>
      </StatGrid>

      {/* The valuation is only as fresh as the prices behind it, and the
          API says how fresh that is — the panel used to claim a "live
          VWAP" and show nothing at all. */}
      {pricesQ.data?.observedAt != null && (
        <p className="text-ink-muted text-xs">
          Prices observed {formatRelative(pricesQ.data.observedAt)}
          {pricesQ.data.stale && ' · flagged stale by the pricing API'}
        </p>
      )}
      {priceFailedCount > 0 && (
        <p role="alert" className="text-down-strong text-xs">
          Price lookup failed for {priceFailedCount.toLocaleString('en-US')} of{' '}
          {holdings.length.toLocaleString('en-US')} holdings; the total excludes
          them.
        </p>
      )}

      {slices.length > 1 && total > 0 && (
        <DonutChart
          data={slices}
          centerLabel={totalText}
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
                  {formatCompactUnits(h.balance, 7)}
                </Td>
                <Td align="right" className="font-mono">
                  {h.priceUSD != null ? (
                    `$${formatPriceSmall(h.priceUSD)}`
                  ) : h.withheld ? (
                    <span
                      className="text-ink-muted text-[10px] tracking-wider uppercase"
                      title="Price withheld by the pricing API's serving gate (thin market or flagged issuer) — observed, not merely unpriced"
                    >
                      withheld
                    </span>
                  ) : (
                    '—'
                  )}
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
                  {h.valueCents != null
                    ? formatReadable(
                        baseUnitsDecimal(String(h.valueCents), 2),
                        true,
                      )
                    : '—'}
                </Td>
                <Td align="right" className="text-ink-muted font-mono">
                  {h.valueUSD != null && total > 0
                    ? `${sharePct(h.valueCents)}%`
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
