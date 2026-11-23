'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { ArrowDownUp, BadgeCheck, ChevronDown, Search, X } from 'lucide-react';

import { Panel } from '@/components/reveal';
import { thinMarketNote } from '@/components/ThinMarketBadge';
import { asExample } from '@/api/client';
import type { components } from '@/api/types';
import {
  LIVE_PRICE_POLL_MS,
  useAsset,
  useCoins,
  useVerifiedIndex,
  coinSlug,
  isVerifiedCoin,
  isStellarAssetId,
} from '@/api/hooks';
import { CURRENT_NETWORK } from '@/lib/networks';
import {
  convertDecimalAmount,
  formatPriceSmall,
  formatRelative,
  parseAmountInput,
  positiveDecimal,
  truncateMiddle,
} from '@/lib/format';
import { cn } from '@/lib/cn';
import { iconProxySrc, isSafePublicImageUrl } from '@/lib/safe-domain';
import { useDialog } from '@/lib/useDialog';
import { fetchPriceBatchChunked, isPriceableAssetId } from '@/lib/price-batch';

import {
  THIN_MARKET_CAPTION,
  useResolvedAssetPrice,
  type PriceProvenance,
} from './LiveAssetPrice';
import {
  isDerivedProvenance,
  provenanceFromBasis,
  provenanceQualifier,
} from './priceProvenance';

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
 * price is the headline's live resolution (useResolvedAssetPrice), noted with
 * the same basis, stale and as-baked qualifiers the headline shows; every other token
 * carries its snapshot USD price from the catalogue / forex batch, which is
 * exactly the freshness the old converter used. A forex-batch leg says so:
 * the batch's `price_type` and `observed_at` ride with the rate and are
 * named under the fields, because that snapshot can be hours old and a
 * declared peg is not a market rate at all. Catalogue-priced legs
 * carry no basis and are left unannotated rather than described with a
 * guess.
 *
 * Pricing-only — the call site hides it on the lean test nets (no aggregator),
 * where every asset is $0.
 */

type PriceType = components['schemas']['Price']['price_type'];

export interface SwapToken {
  /** Stable identity: canonical asset_id ("native", "USDC-G…") or "fiat:USD". */
  key: string;
  symbol: string;
  name?: string | null;
  image?: string | null;
  /** USD per 1 unit, a wire decimal string (never a float). null while unresolved. */
  usdPrice: string | null;
  /** On-chain decimals, where known; the converted amount rounds to them. */
  decimals?: number | null;
  /**
   * How the pricing API says `usdPrice` was derived, where it told us —
   * `peg` is the operator's standing 1:1 declaration rather than an
   * observed market rate, and a converter must not present the two
   * alike. Undefined for legs priced off the asset catalogue,
   * which does not carry the basis.
   */
  basis?: PriceType | null;
  /** When `usdPrice` was OBSERVED (RFC 3339), where the API said. */
  observedAt?: string | null;
  kind: 'crypto' | 'fiat';
  /** The row is the verified asset for its ticker. */
  verified?: boolean;
  /** The API flagged the row as reusing a verified asset's ticker. */
  collision?: boolean;
  /** Issuer or contract address, the only thing that tells same-code rows apart. */
  issuer?: string | null;
  /** The price comes from a market below the substance floor. */
  thin?: boolean;
  /** The page leg's headline qualifiers (derived basis, stale, baked). */
  qualifiers?: string[];
  /** The listing price is derived (see isDerivedProvenance), not a market price. */
  derived?: boolean;
}

/** assetIssuer — the G…/C… address in a canonical asset id, if it has one. */
export function assetIssuer(assetId: string): string | null {
  const m = /(?:^|-)([GC][A-Z2-7]{55})$/.exec(assetId);
  return m ? m[1] : null;
}

/**
 * rankSwapTokens — picker order. A ticker-collision row always sorts last, and
 * with a query the verified row leads its match tier, so a look-alike cannot
 * sit above the asset whose code it copies.
 */
export function rankSwapTokens(tokens: SwapToken[], q: string): SwapToken[] {
  const tier = (t: SwapToken) => {
    if (!q) return 0;
    const sym = t.symbol.toLowerCase();
    const name = (t.name ?? '').toLowerCase();
    if (sym === q) return 0;
    if (sym.startsWith(q)) return 1;
    if (sym.includes(q)) return 2;
    if (name.startsWith(q)) return 3;
    if (name.includes(q)) return 4;
    return 5;
  };
  const trusted = (t: SwapToken) => (t.kind === 'fiat' || t.verified ? 0 : 1);
  return tokens
    .map((t, i) => ({ t, i, r: tier(t) }))
    .sort(
      (a, b) =>
        Number(a.t.collision ?? false) - Number(b.t.collision ?? false) ||
        a.r - b.r ||
        (q ? trusted(a.t) - trusted(b.t) : 0) ||
        a.i - b.i,
    )
    .map((x) => x.t);
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
  'AED',
  'ALL',
  'ARS',
  'AUD',
  'AWG',
  'BAM',
  'BBD',
  'BDT',
  'BGN',
  'BHD',
  'BIF',
  'BND',
  'BOB',
  'BRL',
  'BSD',
  'BWP',
  'BZD',
  'CAD',
  'CDF',
  'CHF',
  'CLP',
  'CNH',
  'CNY',
  'COP',
  'CRC',
  'CUP',
  'CVE',
  'CZK',
  'DJF',
  'DKK',
  'DOP',
  'DZD',
  'EGP',
  'ETB',
  'EUR',
  'FJD',
  'GBP',
  'GHS',
  'GMD',
  'GNF',
  'GTQ',
  'GYD',
  'HKD',
  'HNL',
  'HRK',
  'HTG',
  'HUF',
  'IDR',
  'ILS',
  'INR',
  'IQD',
  'ISK',
  'JMD',
  'JPY',
  'KES',
  'KHR',
  'KMF',
  'KRW',
  'KWD',
  'KYD',
  'KZT',
  'LAK',
  'LBP',
  'LKR',
  'LRD',
  'LSL',
  'LYD',
  'MAD',
  'MDL',
  'MGA',
  'MKD',
  'MOP',
  'MUR',
  'MVR',
  'MWK',
  'MXN',
  'MYR',
  'MZN',
  'NAD',
  'NGN',
  'NIO',
  'NOK',
  'NPR',
  'NZD',
  'OMR',
  'PAB',
  'PEN',
  'PGK',
  'PHP',
  'PKR',
  'PLN',
  'PYG',
  'QAR',
  'RON',
  'RSD',
  'RUB',
  'RWF',
  'SAR',
  'SCR',
  'SDG',
  'SEK',
  'SGD',
  'SOS',
  'SVC',
  'SZL',
  'THB',
  'TJS',
  'TMT',
  'TND',
  'TRY',
  'TTD',
  'TWD',
  'TZS',
  'UAH',
  'UGX',
  'UYU',
  'UZS',
  'VND',
  'XPF',
  'YER',
  'ZAR',
  'ZMW',
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

/**
 * swapBasisNote — one line naming what a leg's USD price IS, for every
 * leg the pricing API described. The converter's output is a money
 * amount, so a leg carried by the operator's 1:1 peg declaration, or by
 * an FX print taken hours ago, must say so rather than looking exactly
 * like a fresh observed quote. Legs priced off the asset
 * catalogue carry no basis and are left unannotated rather than
 * described with a guess.
 */
function swapBasisNote(tokens: SwapToken[]): string | null {
  const parts: string[] = [];
  for (const t of tokens) {
    if (t.usdPrice == null) continue;
    const bits: string[] = [];
    if (t.thin) bits.push(THIN_MARKET_CAPTION);
    if (t.basis === 'peg') {
      bits.push('declared 1:1 peg, not an observed market rate');
    }
    bits.push(...(t.qualifiers ?? []));
    if (t.observedAt != null)
      bits.push(`observed ${formatRelative(t.observedAt)}`);
    if (bits.length > 0) parts.push(`${t.symbol} price: ${bits.join(' · ')}`);
  }
  return parts.length > 0 ? parts.join(' — ') : null;
}

const USD_TOKEN: SwapToken = {
  key: 'fiat:USD',
  symbol: 'USD',
  name: 'US Dollar',
  usdPrice: '1',
  kind: 'fiat',
};

export function AssetSwap({
  symbol,
  assetId,
  image,
  priceUSD: bakedPriceUSD,
  priceProvenance = null,
  priceStale,
  priceScaleMismatch,
  decimals = null,
}: {
  symbol: string;
  assetId: string;
  image?: string | null;
  /** Build-time seed (wire decimal string); the live price replaces it. */
  priceUSD: string | null;
  priceProvenance?: PriceProvenance;
  priceStale?: boolean;
  priceScaleMismatch?: boolean;
  /** The asset's on-chain decimals; its converted amount rounds to them. */
  decimals?: number | null;
}) {
  // The headline's own resolution: polled live, blanked when withheld.
  const resolved = useResolvedAssetPrice(
    assetId,
    bakedPriceUSD,
    priceProvenance,
    priceStale,
    priceScaleMismatch,
  );
  const priceUSD = resolved.withheld ? null : positiveDecimal(resolved.price);
  // The page asset is the default "pay" leg; USD is the default "receive" leg.
  const pageToken = useMemo<SwapToken>(
    () => ({
      key: assetId,
      symbol,
      image,
      usdPrice: priceUSD,
      decimals: onChainDecimals(assetId, decimals),
      kind: 'crypto',
    }),
    [assetId, symbol, image, priceUSD, decimals],
  );

  const [fromToken, setFromToken] = useState<SwapToken>(pageToken);
  const [toToken, setToToken] = useState<SwapToken>(USD_TOKEN);
  // `amount` is always the value of the currently-edited field; the other
  // field is computed. `edited` says which one the user is driving.
  const [amount, setAmount] = useState('1');
  const [edited, setEdited] = useState<'from' | 'to'>('from');
  const [picker, setPicker] = useState<'from' | 'to' | null>(null);
  // The picker (and the fiat list it needs) is opt-in UI: don't pay for
  // /v1/price/batch on every asset-page mount just because the converter
  // is rendered. Once opened, keep fetching so a picked/closed/reopened
  // picker doesn't refetch from empty each time.
  const [pickerEverOpened, setPickerEverOpened] = useState(false);

  const fiatTokens = useFiatTokens(pickerEverOpened);

  // Stable: useDialog re-runs (and re-steals focus) whenever onClose changes.
  const closePicker = useCallback(() => setPicker(null), []);

  function openPicker(side: 'from' | 'to') {
    setPicker(side);
    setPickerEverOpened(true);
  }

  // The leg that still IS the page asset always reflects the live prop
  // price; any other leg re-fetches its own price on the same cadence
  // (a picked leg must not freeze at its pick-time snapshot, with no way
  // to tell the amount on screen was stale).
  const fromRate = useLegRate(fromToken, pageToken.key, priceUSD);
  const toRate = useLegRate(toToken, pageToken.key, priceUSD);
  const pFrom = fromRate.price;
  const pTo = toRate.price;
  const priceable = pFrom != null && pTo != null;
  // What the legs' USD prices actually are, for the legs whose price
  // came with a declared basis and an observation time.
  const fromThin = resolved.thin && fromToken.key === pageToken.key;
  const toThin = resolved.thin && toToken.key === pageToken.key;
  const thinShown = (fromThin && pFrom != null) || (toThin && pTo != null);
  // The page leg is the headline's number, so it carries the headline's
  // qualifiers: a peg, route, stale or build-time price is never bare.
  const pageQualifiers = [
    provenanceQualifier(resolved.provenance),
    resolved.stale ? 'stale' : null,
    ...(resolved.fromAssets ? [] : resolved.poll.caveats),
    resolved.bakedDerived ||
    (!resolved.live && !resolved.derived && resolved.provenance !== 'listing')
      ? 'as baked at deploy'
      : null,
  ].filter((q): q is string => q != null);
  const pageObservedAt = resolved.fromAssets
    ? resolved.detailAsOf
    : resolved.live
      ? resolved.poll.observedAt
      : null;
  const legNote = (
    token: SwapToken,
    rate: ReturnType<typeof useLegRate>,
    thin: boolean,
  ): SwapToken => {
    const isPage = token.key === pageToken.key;
    return {
      ...token,
      usdPrice: rate.price,
      basis: rate.basis,
      observedAt: isPage ? pageObservedAt : rate.observedAt,
      thin,
      qualifiers: isPage ? pageQualifiers : rate.qualifiers,
    };
  };
  const basisNote = swapBasisNote([
    legNote(fromToken, fromRate, fromThin),
    legNote(toToken, toRate, toThin),
  ]);

  const input = parseAmountInput(amount);

  // The computed counter value for the non-edited field: exact decimal
  // arithmetic, rounded once to the output leg's precision.
  const outToken = edited === 'from' ? toToken : fromToken;
  const counter =
    priceable && input != null
      ? edited === 'from'
        ? convertLeg(input, pFrom, pTo, outToken)
        : convertLeg(input, pTo, pFrom, outToken)
      : null;
  // An unpriced leg shows a dash and says why; an empty input shows nothing.
  const counterText =
    counter == null
      ? input != null
        ? '—'
        : ''
      : counter.kind === 'below'
        ? `< ${counter.unit}`
        : fmtAmount(counter.value);
  const missingReason = missingPriceNote(
    [
      { token: fromToken, rate: fromRate },
      { token: toToken, rate: toRate },
    ]
      .filter((l) => l.rate.price == null)
      .map(({ token, rate }) =>
        token.key === pageToken.key && resolved.withheld
          ? {
              symbol: token.symbol,
              withheld: true,
              detail: resolved.withheldCaption,
            }
          : { symbol: token.symbol, withheld: Boolean(rate.withheld) },
      ),
  );

  const fromValue = edited === 'from' ? amount : counterText;
  const toValue = edited === 'to' ? amount : counterText;

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
    setEdited(side);
    // The field keeps the typed separators: rewriting "1," as "1." would turn
    // a keystroke-by-keystroke "1,000" into 1.000.
    setAmount(raw.replace(/[^\d.,\s]/g, ''));
  }

  return (
    <Panel
      headingLevel={2}
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
          onPick={() => openPicker('from')}
        />
        <SwapRow
          value={toValue}
          active={edited === 'to'}
          token={toToken}
          onEdit={(v) => onEdit('to', v)}
          onPick={() => openPicker('to')}
        />

        {/* Seam swap button — the signature element. Punches through both
            rows with a ground-coloured ring so it reads as a hinge. */}
        <button
          type="button"
          aria-label="Swap the two assets"
          onClick={swap}
          className="group border-surface bg-surface-subtle text-ink-muted hover:bg-surface-muted hover:text-ink absolute top-1/2 left-1/2 z-10 -translate-x-1/2 -translate-y-1/2 rounded-full border-4 p-1.5 transition-colors"
        >
          <ArrowDownUp className="h-4 w-4 transition-transform duration-200 group-hover:rotate-180 group-active:scale-90" />
        </button>

        {/* Full-width token picker, overlaying the field stack. */}
        {picker && (
          <TokenPicker
            side={picker}
            fiat={fiatTokens}
            pageToken={pageToken}
            onClose={closePicker}
            onPick={pick}
          />
        )}
      </div>
      {missingReason != null && input != null && (
        <p className="text-ink-muted mt-3 text-xs">{missingReason}</p>
      )}
      {basisNote != null && (
        <p className="text-ink-muted mt-3 text-xs">{basisNote}</p>
      )}
      {thinShown && (
        <p className="text-warn-700 mt-1 text-xs">
          {thinMarketNote(resolved.poll.substance)}
        </p>
      )}
    </Panel>
  );
}

// Bounds an amount field: an exact BigInt conversion scales with the typed length.
const AMOUNT_MAX_LENGTH = 64;

function SwapRow({
  value,
  active,
  token,
  onEdit,
  onPick,
}: {
  value: string;
  active: boolean;
  token: SwapToken;
  onEdit: (v: string) => void;
  onPick: () => void;
}) {
  return (
    <div
      className={cn(
        'bg-surface-canvas rounded-lg border px-3.5 py-4 transition-colors',
        // Focus turns the wrapper border brand (5.33:1 on canvas, WCAG 2.4.7);
        // the input itself draws no outline.
        'focus-within:border-brand-500',
        active ? 'border-line-input' : 'border-line',
      )}
    >
      <div className="flex items-center gap-2">
        <input
          type="text"
          inputMode="decimal"
          value={value}
          onChange={(e) => onEdit(e.target.value)}
          maxLength={AMOUNT_MAX_LENGTH}
          placeholder="0"
          aria-label={`Amount in ${token.symbol}`}
          className="text-ink placeholder:text-ink-faint w-full min-w-0 bg-transparent font-mono text-2xl tabular-nums focus:outline-none"
        />
        <button
          type="button"
          onClick={onPick}
          className="border-line bg-surface text-ink hover:border-brand-500 hover:text-brand-600 inline-flex shrink-0 items-center gap-1.5 rounded-full border py-1 pr-2 pl-1 text-sm font-medium transition-colors"
        >
          <TokenIcon token={token} />
          <span className="max-w-[6rem] truncate">{token.symbol}</span>
          <ChevronDown className="text-ink-muted h-4 w-4" />
        </button>
      </div>
    </div>
  );
}

function TokenIcon({ token, size = 22 }: { token: SwapToken; size?: number }) {
  const [broken, setBroken] = useState(false);
  const dim = { width: size, height: size };
  // Host-validated, https-only — the same gate SidebarAssetIcon
  // and HomeTopAssets apply. Scheme-only validation lets a hostile
  // issuer's SEP-1 image URL point every viewer's browser at an
  // arbitrary (including private/internal) host. This was the third of
  // three <img> sites and the only one without the predicate; the guard
  // in lib/trust-surface-guards.test.ts now derives that set from source
  // so a fourth site cannot repeat it (wave-D EXR-05).
  if (token.image && isSafePublicImageUrl(token.image) && !broken) {
    return (
      // eslint-disable-next-line @next/next/no-img-element
      <img
        src={iconProxySrc(token.image)}
        alt=""
        style={dim}
        onError={() => setBroken(true)}
        className="bg-surface-subtle rounded-full object-cover"
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
  const listRef = useRef<HTMLDivElement | null>(null);
  // Escape, focus-in (the search field is the first focusable), Tab trap and
  // focus restore to the leg trigger.
  const dialogRef = useDialog<HTMLDivElement>(true, onClose);

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

  const { data: verified } = useVerifiedIndex();
  const cryptoTokens = useMemo<SwapToken[]>(() => {
    const rows = coins.data?.coins ?? [];
    return (
      rows
        .flatMap((c): SwapToken[] => {
          // A catalogue row's asset_id is its slug, which /v1/price/batch
          // rejects; price it as this network's verified issuance instead.
          // A crypto ticker with no issuance on this network (BTC, ETH) is
          // priced by its ADR-0014 cross-venue id; never an issued asset's code.
          const key =
            c.asset_id && isStellarAssetId(c.asset_id)
              ? c.asset_id
              : (verified?.issuanceBySlug?.get((c.slug ?? '').toLowerCase()) ??
                (verified != null && c.class === 'crypto' && c.code
                  ? `crypto:${c.code}`
                  : undefined));
          // The page asset is offered only at its live price (pageMatch): its
          // listing snapshot must not stand in once the headline withdraws it.
          if (!key || key === pageToken.key) return [];
          const provenance = provenanceFromBasis(c.price_basis, 'listing');
          const qualifier = provenanceQualifier(provenance);
          return [
            {
              key,
              symbol: c.code ?? coinSlug(c),
              name: c.name,
              image: c.image,
              usdPrice: positiveDecimal(c.price_usd),
              // The row's own id decides: a catalogue row's decimals are its
              // supply exponent even when its key is remapped to an issuance.
              decimals: onChainDecimals(c.asset_id ?? '', c.decimals),
              observedAt: c.price_as_of ?? null,
              qualifiers: qualifier != null ? [qualifier] : undefined,
              derived: isDerivedProvenance(provenance),
              kind: 'crypto',
              collision: c.unverified_ticker_collision ?? false,
              verified: isVerifiedCoin(c, verified),
              issuer: assetIssuer(key),
            },
          ];
        })
        // Only offer legs we can actually price.
        .filter((t) => t.usdPrice != null)
    );
  }, [coins.data, verified, pageToken.key]);

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
    // With a query, match quality leads so the thing the user typed surfaces —
    // otherwise the /v1/assets popularity fallback (which returns top coins even
    // for a non-matching query like "DKK") buries the exact fiat match.
    return rankSwapTokens(out, q);
  }, [pageMatch, cryptoTokens, fiatMatches, q]);

  return (
    // Anchor the overlay to the box that was clicked: the 'from' leg is the top
    // box (top-0); the 'to' leg is the second box, one row (66px) + the stack
    // gap (space-y-1.5 = 6px) down. The search field fills that box exactly and
    // the results list overflows below the whole widget.
    <div
      ref={dialogRef}
      role="dialog"
      aria-modal="true"
      aria-label={
        side === 'from' ? 'Choose asset to pay' : 'Choose asset to receive'
      }
      tabIndex={-1}
      className={cn(
        'border-line-strong bg-surface shadow-elevated absolute right-0 left-0 z-20 flex flex-col rounded-lg border',
        side === 'from' ? 'top-0' : 'top-[72px]',
      )}
    >
      {/* The search field is exactly a SwapRow's height (66px) so, on open, it
          fills the input box it replaced — same footprint, no jump. */}
      <div className="border-line flex h-[66px] shrink-0 items-center gap-2 border-b px-3.5">
        <Search className="text-ink-faint h-4 w-4 shrink-0" />
        <input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search assets & currencies…"
          className="text-ink placeholder:text-ink-faint w-full min-w-0 bg-transparent text-sm focus:outline-hidden"
        />
        <button
          type="button"
          aria-label="Close"
          onClick={onClose}
          className="text-ink-muted hover:text-ink shrink-0 rounded-sm p-0.5"
        >
          <X className="h-4 w-4" />
        </button>
      </div>
      <div ref={listRef} className="max-h-72 overflow-y-auto py-1">
        {coins.isLoading && list.length === 0 && (
          <p className="text-ink-muted px-3 py-4 text-sm">Searching…</p>
        )}
        {!coins.isLoading && list.length === 0 && (
          <p className="text-ink-muted px-3 py-4 text-sm">
            No matching assets.
          </p>
        )}
        {list.map((t) => (
          <button
            key={t.key}
            type="button"
            onClick={() => onPick(t)}
            className="hover:bg-surface-muted flex w-full items-center gap-2.5 px-3 py-2 text-left transition-colors"
          >
            <TokenIcon token={t} size={26} />
            <span className="min-w-0 flex-1">
              <span className="text-ink flex items-center gap-1 text-sm font-medium">
                <span className="truncate">{t.symbol}</span>
                {t.verified && (
                  <BadgeCheck
                    className="text-up h-3.5 w-3.5 shrink-0"
                    role="img"
                    aria-label="Verified currency"
                  />
                )}
                {t.collision && (
                  <span
                    className="text-warn-700 shrink-0 text-[10px] tracking-wider uppercase"
                    title="Reuses a verified asset's ticker; check the issuer"
                  >
                    unverified
                  </span>
                )}
              </span>
              <span className="text-ink-faint block truncate text-[11px]">
                {t.name ??
                  (t.kind === 'fiat' ? 'Fiat currency' : 'Stellar asset')}
                {t.issuer && (
                  <span className="font-mono" title={t.issuer}>
                    {' · '}
                    {truncateMiddle(t.issuer, 4, 4)}
                  </span>
                )}
              </span>
            </span>
            <span
              className="text-ink-muted shrink-0 font-mono text-xs tabular-nums"
              title={t.qualifiers?.join(' · ')}
            >
              {t.usdPrice != null ? `$${formatPriceSmall(t.usdPrice)}` : '—'}
              {t.basis === 'peg' && (
                <span
                  className="ml-1 text-[11px] tracking-wider uppercase"
                  title="Declared 1:1 peg — not an observed market price"
                >
                  peg
                </span>
              )}
            </span>
          </button>
        ))}
      </div>
    </div>
  );
}

// ── Data ─────────────────────────────────────────────────────────────────

/**
 * useLegRate — the live USD price for one converter leg. The page-asset
 * leg is priced off the page's live resolved price; every other leg would otherwise freeze at its pick-time snapshot from
 * the picker's `topCoins`/`searchedCoins` read, with no
 * refetch and no way to tell the number on screen had gone stale.
 * Re-fetches that leg's own price on the same 60s cadence as
 * the rest of the site's live reads, carrying `price_type`/`observed_at`
 * so a stale or declared-peg rate is visible rather than
 * assumed live.
 */
interface LegRate {
  /** USD per unit, a wire decimal string. */
  price: string | null;
  basis: PriceType | null;
  observedAt: string | null;
  /** What a picker snapshot price is (peg, route, listing); cleared by a live read. */
  qualifiers?: string[];
  /** The pricing API refused this leg (thin market, flagged issuer). */
  withheld?: boolean;
}

function useLegRate(
  token: SwapToken,
  pageKey: string,
  pagePriceUSD: string | null,
): LegRate {
  const isPageLeg = token.key === pageKey;
  const isUsd = token.key === USD_TOKEN.key;
  // The batch answers for direct markets only; a derived leg is re-read from
  // /v1/assets, as the page leg's useResolvedAssetPrice does.
  const derived = !isPageLeg && !isUsd && Boolean(token.derived);
  const detailRow = useAsset(derived ? token.key : undefined, {
    refetchInterval: LIVE_PRICE_POLL_MS,
  }).data?.data;
  const fetchable =
    !isPageLeg && !isUsd && !derived && isPriceableAssetId(token.key);
  const q = useQuery<LegRate>({
    queryKey: ['/v1/price/batch', token.key, 'swapLeg'],
    enabled: fetchable && CURRENT_NETWORK.pricing,
    queryFn: async () => {
      const { rows, failedIds, withheld } = await fetchPriceBatchChunked(
        [token.key],
        'fiat:USD',
      );
      // A failed read keeps the labelled snapshot; it is not "no price".
      if (failedIds.includes(token.key))
        throw new Error(`price batch failed for ${token.key}`);
      const row = rows.find((r) => r.asset_id === token.key);
      return {
        price: positiveDecimal(row?.price),
        basis: row?.price_type ?? null,
        observedAt: row?.observed_at ?? null,
        withheld: withheld.includes(token.key),
      };
    },
    // Seed with the picker's snapshot so the leg has a number the instant
    // it's picked, rather than blanking until the first live fetch lands —
    // but back-dated to epoch so `staleTime` treats it as stale immediately
    // and still fires the live fetch on mount instead of sitting on the
    // snapshot for a full `staleTime` window.
    initialData:
      token.usdPrice != null
        ? {
            price: token.usdPrice,
            basis: token.basis ?? null,
            observedAt: token.observedAt ?? null,
            qualifiers: token.qualifiers,
          }
        : undefined,
    initialDataUpdatedAt: 0,
    refetchInterval: 60_000,
    staleTime: 30_000,
  });

  if (isPageLeg) return { price: pagePriceUSD, basis: null, observedAt: null };
  if (isUsd) return { price: '1', basis: null, observedAt: null };
  if (derived && detailRow != null) {
    const qualifier = provenanceQualifier(
      provenanceFromBasis(detailRow.price_basis, 'listing'),
    );
    return {
      price: positiveDecimal(detailRow.price_usd),
      basis: null,
      observedAt: detailRow.price_as_of ?? null,
      qualifiers: qualifier != null ? [qualifier] : undefined,
      withheld: detailRow.price_withheld_reason != null,
    };
  }
  return q.data ?? { price: null, basis: null, observedAt: null };
}

// The forex batch → fiat SwapTokens (USD per unit is exactly what the batch
// returns: 1 fiat:EUR = X USD). USD is prepended with price 1.
function useFiatTokens(enabled: boolean): SwapToken[] {
  const fx = useQuery<SwapToken[]>({
    queryKey: ['/v1/price/batch', 'swapFiat'],
    enabled: enabled && CURRENT_NETWORK.pricing,
    queryFn: async () => {
      // Chunked and settled per chunk: an off-allow-list ticker fails only
      // its own chunk, never blanks out every fiat.
      const { rows } = await fetchPriceBatchChunked(
        FIAT_TICKERS.map((t) => `fiat:${t}`),
        'fiat:USD',
      );
      const out: SwapToken[] = [];
      for (const row of rows) {
        const ticker = row.asset_id.replace(/^fiat:/, '');
        const price = positiveDecimal(row.price);
        if (price == null) continue;
        out.push({
          key: `fiat:${ticker}`,
          symbol: ticker,
          name: fiatName(ticker),
          usdPrice: price,
          // The basis and the observation time ride with the
          // rate. Dropping them made a declared peg and an hours-old
          // FX print indistinguishable from a fresh market quote in a
          // widget whose whole output is a money amount.
          basis: row.price_type ?? null,
          observedAt: row.observed_at ?? null,
          kind: 'fiat',
        });
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
// Stellar classic precision when a crypto leg's decimals are unknown; a fiat
// leg rounds to cents but keeps four significant digits below one unit.
const DEFAULT_CRYPTO_DECIMALS = 7;

// A catalogue row (crypto:BTC, a slug) reports decimals 0, which is no
// precision at all; only a Stellar asset's decimals bound its amounts.
function onChainDecimals(
  key: string,
  decimals: number | null | undefined,
): number | null {
  return decimals != null && isStellarAssetId(key) ? decimals : null;
}

/** A converted amount, or a bound when it is below the leg's smallest unit. */
type Converted =
  { kind: 'amount'; value: string } | { kind: 'below'; unit: string };

function convertLeg(
  amount: string,
  priceIn: string | null,
  priceOut: string | null,
  out: SwapToken,
): Converted | null {
  if (priceIn == null || priceOut == null) return null;
  if (out.kind === 'fiat') {
    const value = convertDecimalAmount(amount, priceIn, priceOut, 2, 4);
    return value == null ? null : { kind: 'amount', value };
  }
  // A crypto leg never shows more places than the asset can hold, so a
  // non-zero amount below its smallest unit reads as "< unit", never "0".
  const places = out.decimals ?? DEFAULT_CRYPTO_DECIMALS;
  const value = convertDecimalAmount(amount, priceIn, priceOut, places);
  if (value == null) return null;
  const nonZero = /[1-9]/;
  if (!nonZero.test(value) && nonZero.test(amount) && nonZero.test(priceIn)) {
    const unit = places > 0 ? `0.${'0'.repeat(places - 1)}1` : '1';
    return { kind: 'below', unit };
  }
  return { kind: 'amount', value };
}

/** The converter's note naming every leg with no USD price, each with its reason. */
export function missingPriceNote(
  legs: { symbol: string; withheld: boolean; detail?: string | null }[],
): string | null {
  if (legs.length === 0) return null;
  return legs
    .map(
      (l) =>
        `${l.symbol} has no USD price${
          l.withheld
            ? `: withheld by the server${l.detail ? ` (${l.detail})` : ''}`
            : ' available'
        }`,
    )
    .join('. ');
}

/** A rounded decimal string, whole part grouped, trailing fraction zeros trimmed. */
function fmtAmount(s: string): string {
  const [whole, frac = ''] = s.split('.');
  const f = frac.replace(/0+$/, '');
  const grouped = BigInt(whole).toLocaleString('en-US');
  return f ? `${grouped}.${f}` : grouped;
}
