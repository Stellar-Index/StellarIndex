'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { ArrowDownUp, ChevronDown, Search, X } from 'lucide-react';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { useCoins } from '@/api/hooks';
import { CURRENT_NETWORK } from '@/lib/networks';
import { formatSubunitPrice } from '@/lib/format';
import { cn } from '@/lib/cn';
import { isSafePublicImageUrl } from '@/lib/safe-domain';

/**
 * AssetSwap — the asset page's swap/convert widget. Two stacked amount
 * fields (pay / receive) with a swap control punching through the seam
 * between them; either field is editable and drives the other, and either
 * side's token is a full searchable picker over every priced asset and
 * fiat. Defaults to "this page's asset → USD" but is fully versatile — the
 * user can re-pick both legs (e.g. XLM→USD becomes BTC→ETH).
 *
 * Conversion is pure client-side maths through each token's USD price:
 * amount(out) = amount(in) × usdPrice(in) / usdPrice(out). The page asset's
 * price is the LIVE prop (refreshes with the parent); every other token
 * carries its snapshot USD price from the catalogue / forex batch, which is
 * exactly the freshness the old converter used.
 *
 * Pricing-only — the call site hides it on the lean test nets (no aggregator),
 * where every asset is $0.
 */

export interface SwapToken {
  /** Stable identity: canonical asset_id ("native", "USDC-G…") or "fiat:USD". */
  key: string;
  symbol: string;
  name?: string | null;
  image?: string | null;
  /** USD per 1 unit. null while unresolved. */
  usdPrice: number | null;
  kind: 'crypto' | 'fiat';
}

// Fiat legs offered in the picker, priced from the forex batch. USD is the
// implicit unit (price 1) and always present, so it is NOT listed here.
//
// These MUST stay within the API's canonical fiat allow-list
// (internal/canonical/asset_fiat.go, knownFiatCodes — ADR-0010): the price
// batch rejects the ENTIRE request with 400 on the first unrecognised code, so
// an off-list ticker here would blank out every fiat, not just itself (the
// useFiatTokens allSettled guard limits that blast radius to one chunk). This
// is the massive.com feed's universe minus USD and the handful of legacy
// pre-euro/redenominated codes (CYP/EEK/LTL/…) that never carry a live rate;
// currencies without a fresh rate are filtered out client-side, so the picker
// shows the ~108 the feed actively prices.
const FIAT_TICKERS = [
  'AED', 'ALL', 'ARS', 'AUD', 'AWG', 'BAM', 'BBD', 'BDT', 'BGN', 'BHD',
  'BIF', 'BND', 'BOB', 'BRL', 'BSD', 'BWP', 'BZD', 'CAD', 'CDF', 'CHF',
  'CLP', 'CNH', 'CNY', 'COP', 'CRC', 'CUP', 'CVE', 'CZK', 'DJF', 'DKK',
  'DOP', 'DZD', 'EGP', 'ETB', 'EUR', 'FJD', 'GBP', 'GHS', 'GMD', 'GNF',
  'GTQ', 'GYD', 'HKD', 'HNL', 'HRK', 'HTG', 'HUF', 'IDR', 'ILS', 'INR',
  'IQD', 'ISK', 'JMD', 'JPY', 'KES', 'KHR', 'KMF', 'KRW', 'KWD', 'KYD',
  'KZT', 'LAK', 'LBP', 'LKR', 'LRD', 'LSL', 'LYD', 'MAD', 'MDL', 'MGA',
  'MKD', 'MOP', 'MUR', 'MVR', 'MWK', 'MXN', 'MYR', 'MZN', 'NAD', 'NGN',
  'NIO', 'NOK', 'NPR', 'NZD', 'OMR', 'PAB', 'PEN', 'PGK', 'PHP', 'PKR',
  'PLN', 'PYG', 'QAR', 'RON', 'RSD', 'RUB', 'RWF', 'SAR', 'SCR', 'SDG',
  'SEK', 'SGD', 'SOS', 'SVC', 'SZL', 'THB', 'TJS', 'TMT', 'TND', 'TRY',
  'TTD', 'TWD', 'TZS', 'UAH', 'UGX', 'UYU', 'UZS', 'VND', 'XPF', 'YER',
  'ZAR', 'ZMW',
];

// Currency display names come from the browser's Intl.DisplayNames so the full
// list doesn't need a hand-maintained name map.
const currencyDisplay =
  typeof Intl !== 'undefined' && 'DisplayNames' in Intl
    ? new Intl.DisplayNames(['en'], { type: 'currency' })
    : null;

function fiatName(ticker: string): string {
  try {
    return currencyDisplay?.of(ticker) ?? ticker;
  } catch {
    return ticker;
  }
}

const USD_TOKEN: SwapToken = {
  key: 'fiat:USD',
  symbol: 'USD',
  name: 'US Dollar',
  usdPrice: 1,
  kind: 'fiat',
};

export function AssetSwap({
  symbol,
  assetId,
  image,
  priceUSD,
}: {
  symbol: string;
  assetId: string;
  image?: string | null;
  priceUSD: number | null;
}) {
  // The page asset is the default "pay" leg; USD is the default "receive"
  // leg. The page asset's price is kept live off the prop.
  const pageToken = useMemo<SwapToken>(
    () => ({
      key: assetId,
      symbol,
      image,
      usdPrice: priceUSD,
      kind: 'crypto',
    }),
    [assetId, symbol, image, priceUSD],
  );

  const [fromToken, setFromToken] = useState<SwapToken>(pageToken);
  const [toToken, setToToken] = useState<SwapToken>(USD_TOKEN);
  // `amount` is always the value of the currently-edited field; the other
  // field is computed. `edited` says which one the user is driving.
  const [amount, setAmount] = useState('1');
  const [edited, setEdited] = useState<'from' | 'to'>('from');
  const [picker, setPicker] = useState<'from' | 'to' | null>(null);

  const fiatTokens = useFiatTokens();

  // The leg that still IS the page asset always reflects the live prop price;
  // any other leg uses its captured price. Deriving this at render keeps the
  // live price flowing without an effect that syncs state (which triggers
  // cascading renders — react-hooks/set-state-in-effect).
  const livePrice = (t: SwapToken): number | null =>
    t.key === pageToken.key ? priceUSD : t.usdPrice;
  const pFrom = livePrice(fromToken);
  const pTo = livePrice(toToken);
  const priceable = pFrom != null && pFrom > 0 && pTo != null && pTo > 0;

  const numeric = Number(amount.replace(/,/g, ''));
  const validInput = Number.isFinite(numeric) && numeric >= 0;

  // The computed counter value for the non-edited field.
  const counter =
    priceable && validInput
      ? edited === 'from'
        ? (numeric * (pFrom as number)) / (pTo as number)
        : (numeric * (pTo as number)) / (pFrom as number)
      : null;

  const fromValue = edited === 'from' ? amount : counter != null ? fmtAmount(counter) : '';
  const toValue = edited === 'to' ? amount : counter != null ? fmtAmount(counter) : '';

  function swap() {
    setFromToken(toToken);
    setToToken(fromToken);
    // Keep the value the user is holding onto in place by flipping which side
    // is authoritative — the visible numbers swap with the tokens.
    setEdited((e) => (e === 'from' ? 'to' : 'from'));
  }

  function pick(token: SwapToken) {
    const side = picker;
    setPicker(null);
    if (!side) return;
    // Choosing the token already on the OTHER side = a swap intent.
    const other = side === 'from' ? toToken : fromToken;
    if (token.key === other.key) {
      swap();
      return;
    }
    if (side === 'from') setFromToken(token);
    else setToToken(token);
  }

  function onEdit(side: 'from' | 'to', raw: string) {
    // Permit digits + a single dot; drop grouping/letters.
    const cleaned = raw.replace(/[^\d.]/g, '').replace(/(\..*)\./g, '$1');
    setEdited(side);
    setAmount(cleaned);
  }

  return (
    <Panel
      title="Converter"
      source={asExample('/v1/price', { asset: symbol, quote: 'fiat:USD' })}
      bodyClassName=""
    >
      {/* The two stacked fields are the swap button's positioning context, so
          top-1/2 lands exactly on the seam between them. The picker overlays
          just this field stack. */}
      <div className="relative space-y-1.5">
        <SwapRow
          value={fromValue}
          active={edited === 'from'}
          token={fromToken}
          onEdit={(v) => onEdit('from', v)}
          onFocus={() => reseed('from')}
          onPick={() => setPicker('from')}
        />
        <SwapRow
          value={toValue}
          active={edited === 'to'}
          token={toToken}
          onEdit={(v) => onEdit('to', v)}
          onFocus={() => reseed('to')}
          onPick={() => setPicker('to')}
        />

        {/* Seam swap button — the signature element. Punches through both
            rows with a ground-coloured ring so it reads as a hinge. */}
        <button
          type="button"
          aria-label="Swap the two assets"
          onClick={swap}
          className="group absolute left-1/2 top-1/2 z-10 -translate-x-1/2 -translate-y-1/2 rounded-full border-4 border-surface bg-surface-canvas p-2 text-ink-muted shadow-card transition-all hover:bg-brand-50 hover:text-brand-600 focus:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500"
        >
          <ArrowDownUp className="h-4 w-4 transition-transform duration-200 group-hover:rotate-180 group-active:scale-90" />
        </button>

        {/* Full-width token picker, overlaying the field stack. */}
        {picker && (
          <TokenPicker
            side={picker}
            fiat={fiatTokens}
            pageToken={pageToken}
            onClose={() => setPicker(null)}
            onPick={pick}
          />
        )}
      </div>
    </Panel>
  );

  // Reseed the edited amount when the user clicks into the non-edited field,
  // so typing continues from the value they see rather than a stale string.
  function reseed(side: 'from' | 'to') {
    if (side === edited) return;
    const shown = side === 'from' ? fromValue : toValue;
    setEdited(side);
    setAmount((shown || '').replace(/,/g, ''));
  }
}

function SwapRow({
  value,
  active,
  token,
  onEdit,
  onFocus,
  onPick,
}: {
  value: string;
  active: boolean;
  token: SwapToken;
  onEdit: (v: string) => void;
  onFocus: () => void;
  onPick: () => void;
}) {
  return (
    <div
      className={cn(
        'rounded-lg border bg-surface-canvas px-3.5 py-4 transition-colors',
        // The active field is signalled by a lighter border, not a focus ring
        // on the input itself. That border delta alone is 1.22:1 — invisible —
        // so focus ALSO raises a ring on the wrapper (brand-500 on canvas =
        // 5.33:1), which is what WCAG 2.4.7 / 2.4.11 actually require.
        'focus-within:ring-2 focus-within:ring-brand-500/60',
        active ? 'border-line-strong' : 'border-line',
      )}
    >
      <div className="flex items-center gap-2">
        <input
          type="text"
          inputMode="decimal"
          value={value}
          onChange={(e) => onEdit(e.target.value)}
          onFocus={onFocus}
          placeholder="0"
          aria-label={`Amount in ${token.symbol}`}
          className="w-full min-w-0 bg-transparent font-mono text-2xl tabular-nums text-ink placeholder:text-ink-faint focus:outline-none"
        />
        <button
          type="button"
          onClick={onPick}
          className="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-line bg-surface py-1 pl-1 pr-2 text-sm font-medium text-ink shadow-card transition-colors hover:border-brand-500 hover:text-brand-600"
        >
          <TokenIcon token={token} />
          <span className="max-w-[6rem] truncate">{token.symbol}</span>
          <ChevronDown className="h-4 w-4 text-ink-muted" />
        </button>
      </div>
    </div>
  );
}

function TokenIcon({ token, size = 22 }: { token: SwapToken; size?: number }) {
  const [broken, setBroken] = useState(false);
  const dim = { width: size, height: size };
  // SEC-10: host-validated, https-only — the same gate SidebarAssetIcon
  // and HomeTopAssets apply. Scheme-only validation lets a hostile
  // issuer's SEP-1 image URL point every viewer's browser at an
  // arbitrary (including private/internal) host. This was the third of
  // three <img> sites and the only one without the predicate; the guard
  // in lib/trust-surface-guards.test.ts now derives that set from source
  // so a fourth site cannot repeat it (wave-D EXR-05, issue #335).
  if (token.image && isSafePublicImageUrl(token.image) && !broken) {
    return (
      // eslint-disable-next-line @next/next/no-img-element
      <img
        src={token.image}
        alt=""
        style={dim}
        onError={() => setBroken(true)}
        className="rounded-full bg-surface-subtle object-cover"
      />
    );
  }
  return (
    <span
      style={dim}
      className={cn(
        'inline-flex items-center justify-center rounded-full text-[10px] font-semibold uppercase',
        token.kind === 'fiat'
          ? 'bg-brand-50 text-brand-600'
          : 'bg-surface-subtle text-ink-muted',
      )}
    >
      {token.symbol.slice(0, 2)}
    </span>
  );
}

// ── Token picker ─────────────────────────────────────────────────────────
// Full-width overlay over the swap card: a search box plus a scrollable list
// of tokens (icon · symbol + name · USD price). Crypto results come from the
// live /v1/assets search; fiat legs are filtered locally.
function TokenPicker({
  side,
  fiat,
  pageToken,
  onClose,
  onPick,
}: {
  side: 'from' | 'to';
  fiat: SwapToken[];
  pageToken: SwapToken;
  onClose: () => void;
  onPick: (t: SwapToken) => void;
}) {
  const [query, setQuery] = useState('');
  const debounced = useDebounced(query.trim(), 200);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  // Cap the scroll area to what fits above the viewport bottom, leaving a 10px
  // gap, so a long list never overflows the page. Set imperatively (no state)
  // to avoid re-renders and the set-state-in-effect rule; recomputed on
  // resize/scroll since the trigger's viewport position moves with both.
  useEffect(() => {
    const el = listRef.current;
    if (!el) return;
    const fit = () => {
      const top = el.getBoundingClientRect().top;
      el.style.maxHeight = `${Math.max(120, window.innerHeight - top - 10)}px`;
    };
    fit();
    window.addEventListener('resize', fit);
    window.addEventListener('scroll', fit, true);
    return () => {
      window.removeEventListener('resize', fit);
      window.removeEventListener('scroll', fit, true);
    };
  }, []);

  // Close on Escape.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  // Mirror the global search modal's proven pattern: a warm top-N list (no q,
  // no order_by — the pre-warmed default tuple; an order_by or a cold search
  // tuple 503s the asset-catalogue cache) covers the empty state, and a search
  // query fires only at ≥2 chars. Anything else risks the /v1/assets warming
  // 503 that leaves the picker blank.
  // limit=100 / limit=25 are the tuples the /assets page + search modal keep
  // warm; novel limits (80, 24) are cold and hit the catalogue cache's warming
  // 503. Reuse the warm ones so the picker piggybacks on an already-hot cache.
  const isSearching = debounced.length >= 2;
  const topCoins = useCoins(100, undefined, undefined, undefined, undefined, {
    staleTime: 300_000,
  });
  const searchedCoins = useCoins(
    25,
    undefined,
    undefined,
    isSearching ? debounced : undefined,
    undefined,
    { enabled: isSearching },
  );
  const coins = isSearching ? searchedCoins : topCoins;

  const cryptoTokens = useMemo<SwapToken[]>(() => {
    const rows = coins.data?.coins ?? [];
    return rows
      .map((c): SwapToken => {
        const price = c.price_usd != null ? Number(c.price_usd) : null;
        return {
          key: c.asset_id ?? c.slug,
          symbol: c.code ?? c.slug,
          name: c.issuer ? undefined : undefined,
          image: c.image,
          usdPrice: price != null && price > 0 ? price : null,
          kind: 'crypto',
        };
      })
      // Only offer legs we can actually price.
      .filter((t) => t.usdPrice != null);
  }, [coins.data]);

  const q = debounced.toLowerCase();
  const fiatMatches = useMemo(
    () =>
      fiat.filter(
        (t) =>
          !q ||
          t.symbol.toLowerCase().includes(q) ||
          (t.name ?? '').toLowerCase().includes(q),
      ),
    [fiat, q],
  );

  // The page asset is always offered first when it matches (or on an empty
  // query), even if it's outside the top-volume search window.
  const pageMatch =
    pageToken.usdPrice != null &&
    (!q || pageToken.symbol.toLowerCase().includes(q))
      ? pageToken
      : null;

  // De-dupe: page token first, then crypto, then fiat.
  const list = useMemo(() => {
    const seen = new Set<string>();
    const out: SwapToken[] = [];
    for (const t of [pageMatch, ...cryptoTokens, ...fiatMatches]) {
      if (!t || seen.has(t.key)) continue;
      seen.add(t.key);
      out.push(t);
    }
    if (!q) return out;
    // With a query, rank by match quality so the thing the user typed surfaces
    // to the top — otherwise the /v1/assets popularity fallback (which returns
    // top coins even for a non-matching query like "DKK") buries the exact fiat
    // match at the bottom of the list. Stable within each tier.
    const rank = (t: SwapToken) => {
      const sym = t.symbol.toLowerCase();
      const name = (t.name ?? '').toLowerCase();
      if (sym === q) return 0;
      if (sym.startsWith(q)) return 1;
      if (sym.includes(q)) return 2;
      if (name.startsWith(q)) return 3;
      if (name.includes(q)) return 4;
      return 5;
    };
    return out
      .map((t, i) => ({ t, i, r: rank(t) }))
      .sort((a, b) => a.r - b.r || a.i - b.i)
      .map((x) => x.t);
  }, [pageMatch, cryptoTokens, fiatMatches, q]);

  return (
    // Anchor the overlay to the box that was clicked: the 'from' leg is the top
    // box (top-0); the 'to' leg is the second box, one row (66px) + the stack
    // gap (space-y-1.5 = 6px) down. The search field fills that box exactly and
    // the results list overflows below the whole widget.
    <div
      className={cn(
        'absolute left-0 right-0 z-20 flex flex-col rounded-lg border border-line-strong bg-surface shadow-elevated',
        side === 'from' ? 'top-0' : 'top-[72px]',
      )}
    >
      {/* The search field is exactly a SwapRow's height (66px) so, on open, it
          fills the input box it replaced — same footprint, no jump. */}
      <div className="flex h-[66px] shrink-0 items-center gap-2 border-b border-line px-3.5">
        <Search className="h-4 w-4 shrink-0 text-ink-faint" />
        <input
          ref={inputRef}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search assets & currencies…"
          className="w-full min-w-0 bg-transparent text-sm text-ink placeholder:text-ink-faint focus:outline-hidden"
        />
        <button
          type="button"
          aria-label="Close"
          onClick={onClose}
          className="shrink-0 rounded-sm p-0.5 text-ink-muted hover:text-ink"
        >
          <X className="h-4 w-4" />
        </button>
      </div>
      <div ref={listRef} className="max-h-72 overflow-y-auto py-1">
        {coins.isLoading && list.length === 0 && (
          <p className="px-3 py-4 text-sm text-ink-muted">Searching…</p>
        )}
        {!coins.isLoading && list.length === 0 && (
          <p className="px-3 py-4 text-sm text-ink-muted">No matching assets.</p>
        )}
        {list.map((t) => (
          <button
            key={t.key}
            type="button"
            onClick={() => onPick(t)}
            className="flex w-full items-center gap-2.5 px-3 py-2 text-left transition-colors hover:bg-surface-muted"
          >
            <TokenIcon token={t} size={26} />
            <span className="min-w-0 flex-1">
              <span className="block truncate text-sm font-medium text-ink">
                {t.symbol}
              </span>
              <span className="block truncate text-[11px] text-ink-faint">
                {t.name ?? (t.kind === 'fiat' ? 'Fiat currency' : 'Stellar asset')}
              </span>
            </span>
            <span className="shrink-0 font-mono text-xs tabular-nums text-ink-muted">
              {t.usdPrice != null ? fmtUsd(t.usdPrice) : '—'}
            </span>
          </button>
        ))}
      </div>
    </div>
  );
}

// ── Data ─────────────────────────────────────────────────────────────────
// The forex batch → fiat SwapTokens (USD per unit is exactly what the batch
// returns: 1 fiat:EUR = X USD). USD is prepended with price 1.
function useFiatTokens(): SwapToken[] {
  const fx = useQuery<SwapToken[]>({
    queryKey: ['/v1/price/batch', 'swapFiat'],
    enabled: CURRENT_NETWORK.pricing,
    queryFn: async () => {
      // GET /v1/price/batch caps at 100 asset_ids, so the list is split into
      // ≤100-id chunks fetched in parallel and merged. GET (not POST) keeps the
      // responses edge-cacheable. allSettled (not all): the batch 400s the
      // whole request on a single unrecognised code, so an off-allow-list
      // ticker must fail only its own chunk, never blank out every fiat.
      const CHUNK = 100;
      const chunks: string[][] = [];
      for (let i = 0; i < FIAT_TICKERS.length; i += CHUNK) {
        chunks.push(FIAT_TICKERS.slice(i, i + CHUNK));
      }
      const settled = await Promise.allSettled(
        chunks.map((chunk) => {
          const ids = chunk.map((t) => `fiat:${t}`).join(',');
          return apiGet<{
            data: Array<{ asset_id: string; price: string | null }>;
          }>(`/v1/price/batch?asset_ids=${encodeURIComponent(ids)}&quote=fiat:USD`, {});
        }),
      );
      const out: SwapToken[] = [];
      for (const res of settled) {
        if (res.status !== 'fulfilled') continue;
        for (const row of res.value.data ?? []) {
          const ticker = row.asset_id.replace(/^fiat:/, '');
          const price = row.price ? Number(row.price) : 0;
          if (!(price > 0)) continue;
          out.push({
            key: `fiat:${ticker}`,
            symbol: ticker,
            name: fiatName(ticker),
            usdPrice: price,
            kind: 'fiat',
          });
        }
      }
      return out;
    },
    refetchInterval: 5 * 60_000,
  });
  return useMemo(() => [USD_TOKEN, ...(fx.data ?? [])], [fx.data]);
}

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setV(value), ms);
    return () => clearTimeout(id);
  }, [value, ms]);
  return v;
}

// ── Formatting ─────────────────────────────────────────────────────────────
function fmtAmount(n: number): string {
  if (!Number.isFinite(n)) return '';
  if (n === 0) return '0';
  if (n >= 1_000_000) return n.toLocaleString('en-US', { maximumFractionDigits: 2 });
  if (n >= 1) return n.toLocaleString('en-US', { maximumFractionDigits: 6 });
  if (n >= 0.0001) return trimZeros(n.toFixed(6));
  return formatSubunitPrice(n);
}

const usdFmt = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  maximumFractionDigits: 2,
});

function fmtUsd(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (n > 0 && n < 0.01) return `$${formatSubunitPrice(n)}`;
  return usdFmt.format(n);
}

function trimZeros(s: string): string {
  return s.includes('.') ? s.replace(/\.?0+$/, '') : s;
}
