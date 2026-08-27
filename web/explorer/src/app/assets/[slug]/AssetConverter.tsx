'use client';

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { CurrencyCombobox } from '@/components/CurrencyCombobox';
import { apiGet, asExample } from '@/api/client';
import { CURRENT_NETWORK } from '@/lib/networks';
import { formatSubunitPrice } from '@/lib/format';

interface CurrencyRow {
  ticker: string;
  name: string;
  rate_usd: number;
}

// FEATURED — kept short so the dropdown isn't overwhelming. Users
// can switch to "All currencies" to see every ticker the forex
// snapshot returns.
const FEATURED = [
  'USD',
  'EUR',
  'GBP',
  'JPY',
  'CHF',
  'CAD',
  'AUD',
  'CNY',
  'INR',
  'BRL',
  'MXN',
];
// AM-26: the full verified fiat set — the "show all" branch was a
// fiction while the batch only requested the featured ten.
const ALL_FIAT = [
  ...FEATURED,
  'KRW',
  'HKD',
  'SGD',
  'SEK',
  'NOK',
  'ZAR',
  'TRY',
  'NZD',
];

/**
 * AssetConverter — the sidebar's "exchange readout": the panel leads
 * with the live equivalence `1 {asset} = $X` (asset → USD, the framing
 * a reader expects from a ticker), with the target currency selectable
 * inside the headline. Below it, a compact amount row converts any
 * quantity in either direction; the headline equivalence stays fixed on
 * "1 unit of the asset".
 *
 * Pure client-side maths off the price prop; refreshes when the parent
 * re-fetches the price.
 *
 * F-1201 migration: pre-rc.48 this read /v1/currencies (a single bulk
 * endpoint already in "1 USD = N target" shape). rc.48 removed that
 * route; we now use /v1/price/batch ("1 unit of asset = X USD") and
 * invert at the boundary. Names are missing from the batch response —
 * we don't render them in this converter, so that's fine.
 */
export function AssetConverter({
  symbol,
  priceUSD,
}: {
  symbol: string;
  priceUSD: number | null;
}) {
  // 'asset-to-fiat' (DEFAULT): type an asset amount → target value. This
  // matches the fixed headline framing ("1 XLM = $0.19"); the old default
  // was the inverse ("1 USD = N XLM"), which read backwards for a ticker.
  const [direction, setDirection] = useState<'asset-to-fiat' | 'fiat-to-asset'>(
    'asset-to-fiat',
  );
  const [amount, setAmount] = useState('1');
  const [target, setTarget] = useState('USD');
  const [showAll, setShowAll] = useState(false);

  // Pull the forex snapshot to power non-USD targets. Stale data is
  // fine — the snapshot refreshes every 5 min, and a stale FX leg on a
  // crypto-asset converter is dominated by the crypto's own volatility.
  const fx = useQuery<CurrencyRow[]>({
    queryKey: ['/v1/price/batch', 'forAssetConverter'],
    // No aggregator/FX on the lean test nets → /v1/price/batch is empty; the
    // converter is hidden there (see the AssetSidebar call site), but gate the
    // query too so it never fires.
    enabled: CURRENT_NETWORK.pricing,
    queryFn: async () => {
      const assetIds = ALL_FIAT.filter((t) => t !== 'USD')
        .map((t) => `fiat:${t}`)
        .join(',');
      const env = await apiGet<{
        data: Array<{ asset_id: string; price: string | null }>;
      }>(
        `/v1/price/batch?asset_ids=${encodeURIComponent(assetIds)}&quote=fiat:USD`,
        {},
      );
      const rows: CurrencyRow[] = [];
      for (const row of env.data ?? []) {
        const ticker = row.asset_id.replace(/^fiat:/, '');
        const priceUSD = row.price ? Number(row.price) : 0;
        if (!(priceUSD > 0)) continue;
        // /v1/price/batch returns 1 ticker = X USD; the converter
        // expects rate_usd in the "1 USD = N target" form. Invert.
        rows.push({ ticker, name: ticker, rate_usd: 1 / priceUSD });
      }
      return rows;
    },
    refetchInterval: 5 * 60_000,
  });

  const fxByTicker = useMemo(() => {
    const m: Record<string, number> = { USD: 1 };
    for (const c of fx.data ?? []) m[c.ticker] = c.rate_usd;
    return m;
  }, [fx.data]);

  // rate_usd means "1 USD = N target" so 1 asset = N × priceUSD target.
  const targetRate = fxByTicker[target] ?? null;
  const priceable = priceUSD != null && priceUSD > 0 && targetRate != null;

  // The headline equivalence: value of ONE asset unit, in the target.
  const unitValue = priceable ? (priceUSD as number) * (targetRate as number) : null;

  const numeric = Number(amount);
  const validInput = Number.isFinite(numeric) && numeric >= 0;

  let result: number | null = null;
  if (priceable && validInput) {
    result =
      direction === 'asset-to-fiat'
        ? // asset → target: × priceUSD (→ USD), × targetRate (→ target)
          numeric * (priceUSD as number) * (targetRate as number)
        : // target → asset: ÷ targetRate (→ USD), ÷ priceUSD (→ asset)
          numeric / (targetRate as number) / (priceUSD as number);
  }

  const fromUnit = direction === 'asset-to-fiat' ? symbol : target;
  const toUnit = direction === 'asset-to-fiat' ? target : symbol;
  const targetIsUSD = target === 'USD';

  // Available targets — featured first, then the long tail once "show
  // all" is toggled. Filter against the forex snapshot so we never list
  // a ticker we can't convert. Memoised so its identity is stable.
  const tickerSet = useMemo(() => {
    const s = new Set((fx.data ?? []).map((c) => c.ticker));
    s.add('USD');
    return s;
  }, [fx.data]);

  const allTickers = useMemo(() => Array.from(tickerSet).sort(), [tickerSet]);

  // Once any currency outside FEATURED is picked (e.g. typing "ZAR" in
  // the combobox), promote to showAll so the long-tail stays visible.
  // Adjust state during render; the `!showAll` guard keeps it idempotent.
  if (!showAll && tickerSet.has(target) && !FEATURED.includes(target)) {
    setShowAll(true);
  }

  return (
    <Panel
      title="Converter"
      hint={
        priceUSD != null
          ? `Live ${symbol}/USD price + forex snapshot`
          : 'Awaiting live price'
      }
      source={asExample('/v1/price', { asset: symbol, quote: 'fiat:USD' })}
      bodyClassName="space-y-3"
    >
      {/* Exchange readout — the hero. A single equivalence statement,
          "1 {asset} = {value} {target}", typeset as an equation with the
          target unit selectable in place. This is the primary reading;
          the amount row below is the working surface. */}
      <div className="border-line bg-surface-canvas rounded-md border px-3 py-2.5">
        <div className="text-ink-faint flex items-center justify-between text-[10px] tracking-wider uppercase">
          <span>Exchange rate</span>
          {!priceable && <span>awaiting price</span>}
        </div>
        <div className="mt-1.5 flex flex-wrap items-baseline gap-x-2 gap-y-1 font-mono tabular-nums">
          <span className="text-ink-body text-sm">1</span>
          <span className="bg-surface-subtle text-ink-body rounded-sm px-1.5 py-0.5 text-xs tracking-wider uppercase">
            {symbol}
          </span>
          {/* The equals sign is the one bold accent — the pivot of the
              equation, in brand ink. */}
          <span className="text-brand-500 text-lg leading-none">=</span>
          <span className="text-ink text-2xl leading-none">
            {unitValue != null
              ? `${targetIsUSD ? '$' : ''}${formatResult(unitValue)}`
              : '—'}
          </span>
          <CurrencyCombobox
            value={target}
            onChange={(v) => {
              setTarget(v);
              setShowAll(true);
            }}
            tickers={allTickers}
          />
        </div>
      </div>

      {/* Amount converter — the working row. Left field is the editable
          input, right is the computed value; Reverse swaps which unit you
          type in without touching the headline framing. */}
      <div>
        <div className="mb-1 flex items-center justify-between">
          <span className="text-ink-muted text-xs tracking-wider uppercase">
            Amount
          </span>
          <button
            type="button"
            aria-label="Reverse conversion direction"
            onClick={() =>
              setDirection((d) =>
                d === 'asset-to-fiat' ? 'fiat-to-asset' : 'asset-to-fiat',
              )
            }
            className="border-line text-ink-muted hover:border-brand-500 hover:text-brand-600 inline-flex items-center gap-1 rounded-sm border px-1.5 py-0.5 text-[10px] tracking-wider uppercase transition-colors"
          >
            ⇄ Reverse
          </button>
        </div>
        <div className="grid grid-cols-[1fr_auto_1fr] items-center gap-2">
          <div className="border-line bg-surface flex items-center gap-1.5 rounded-md border px-2 py-1.5">
            <input
              type="number"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              min="0"
              step="any"
              inputMode="decimal"
              aria-label={`Amount in ${fromUnit}`}
              className="w-full min-w-0 bg-transparent font-mono text-base tabular-nums focus:outline-hidden"
            />
            <span className="bg-surface-subtle text-ink-body shrink-0 rounded-sm px-1 py-0.5 font-mono text-[10px] tracking-wider uppercase">
              {fromUnit}
            </span>
          </div>
          <span className="text-ink-faint text-xs" aria-hidden>
            →
          </span>
          <div className="border-line-subtle bg-surface-subtle flex items-center gap-1.5 rounded-md border px-2 py-1.5">
            <span className="text-ink w-full min-w-0 truncate font-mono text-base tabular-nums">
              {result != null ? formatResult(result) : '—'}
            </span>
            <span className="bg-surface text-ink-body shrink-0 rounded-sm px-1 py-0.5 font-mono text-[10px] tracking-wider uppercase">
              {toUnit}
            </span>
          </div>
        </div>
      </div>

      {/* Secondary rates — the inverse + FX leg, kept quiet. */}
      {unitValue != null && unitValue > 0 && (
        <p className="text-ink-muted text-xs">
          1 {target} = {formatResult(1 / unitValue)} {symbol}
          {!targetIsUSD && (
            <>
              <span className="text-ink-faint mx-2">·</span>
              <span>
                FX: 1 USD = {formatResult(targetRate as number)} {target}
              </span>
            </>
          )}
        </p>
      )}
    </Panel>
  );
}

function formatResult(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (n === 0) return '0';
  if (n >= 1_000_000)
    return n.toLocaleString('en-US', { maximumFractionDigits: 2 });
  if (n >= 1) return n.toFixed(4);
  if (n >= 0.0001) return n.toFixed(6);
  return formatSubunitPrice(n);
}
