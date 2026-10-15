'use client';

import { useState } from 'react';
import Link from 'next/link';

import {
  useCoins,
  useNativeCoin,
  useVerifiedSlugs,
  coinSlug,
  type Coin,
} from '@/api/hooks';
import { assetHref } from '@/lib/fiat-slugs';
import { useLedgerFollow } from '@/lib/live/hooks';
import {
  EmptyState,
  Skeleton,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  LinkRow,
  TR,
} from '@/components/ui';
import {
  formatCompact,
  formatCompactUnits,
  formatPriceSmall,
  truncateMiddle,
} from '@/lib/format';
import { iconProxySrc, isSafePublicImageUrl } from '@/lib/safe-domain';
import { demoteFlaggedLast } from '@/lib/directory-tags';
import { ScamBadge } from '@/components/ScamBadge';
import { ThinMarketBadge } from '@/components/ThinMarketBadge';
import { CURRENT_NETWORK } from '@/lib/networks';
import { InlineBar } from '@/components/ContractCharts';
import { StellarMark } from '@/components/StellarMark';
import { RequestReveal } from '@/components/reveal/RequestReveal';
import { asExample } from '@/api/client';
import { assetLabel } from '@/lib/asset-label';

/**
 * HomeTopAssets — the top 10 assets ranked by trailing-24h trading
 * volume across every venue we ingest.
 *
 * TWO corrections vs the old grid: (1) it ranked by observation_count
 * (an all-time cumulative counter that just floats long-lived stablecoins
 * to the top) instead of activity — now `volume_24h_usd_desc`; and (2)
 * native XLM has NO /v1/assets row, so the most-traded asset on Stellar
 * was invisible and USDC ranked #1 — we now inject native (via
 * useNativeCoin) and re-rank, so XLM takes the top spot it earns on volume.
 *
 * Server-rendered list this isn't — we want this to refresh on
 * the same TanStack cadence as the rest of the home page.
 */
export function HomeTopAssets() {
  // Ranking and half the columns are USD-derived. On a net with no
  // aggregator every price/volume is null, so the grid ranked ten assets
  // by a column of nulls and rendered four "—" columns. Rank by the
  // chain-native observation count there and drop the priced columns.
  const pricing = CURRENT_NETWORK.pricing;
  const { data, isLoading, isError } = useCoins(
    10,
    undefined,
    undefined,
    undefined,
    pricing ? 'volume_24h_usd_desc' : 'observation_count_desc',
    { includeThin: pricing },
  );
  const { data: nativeCoin } = useNativeCoin();
  const listExample = asExample('/v1/assets', {
    limit: 10,
    order_by: pricing ? 'volume_24h_usd_desc' : 'observation_count_desc',
    ...(pricing ? { include_thin: 'true' } : {}),
  });
  const { data: verifiedSlugs } = useVerifiedSlugs();
  useLedgerFollow(['/v1/assets']);

  const coins = rankTopAssets(nativeCoin, data?.coins, pricing);
  const maxVolume = Math.max(
    0,
    ...coins.map((c) => Number(c.volume_24h_usd) || 0),
  );

  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <div className="space-y-1">
          <h2 className="text-h3 text-ink font-semibold">
            Top assets by activity
          </h2>
          <p className="text-ink-body text-sm">
            {pricing
              ? 'By 24h volume across every pair and venue, XLM included.'
              : 'Ranked by observed on-chain trade count, XLM included. This network runs no price aggregator, so the USD columns are omitted rather than served empty.'}
          </p>
        </div>
        <div className="flex items-center gap-3 whitespace-nowrap">
          <RequestReveal example={listExample} position="inline" />
          <Link
            href="/assets"
            className="text-brand-600 text-sm hover:underline"
          >
            See all
          </Link>
        </div>
      </div>
      {isError ? (
        <EmptyState
          title="Couldn't load top assets."
          description="The assets feed is unavailable right now."
          action={
            <Link href="/assets" className="text-brand-600 hover:underline">
              Browse all assets
            </Link>
          }
        />
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <Th>#</Th>
                <Th>Asset</Th>
                {pricing && <Th align="right">Price</Th>}
                {pricing && <Th align="right">24h %</Th>}
                {pricing && <Th align="right">24h volume</Th>}
                {!pricing && <Th align="right">Observations</Th>}
              </TR>
            </THead>
            <TBody>
              {isLoading &&
                !data &&
                Array.from({ length: 8 }).map((_, i) => (
                  <TR key={`sk-${i}`} className="hover:bg-transparent">
                    <Td colSpan={pricing ? 5 : 3}>
                      <Skeleton className="h-5 w-full" />
                    </Td>
                  </TR>
                ))}
              {coins.map((coin, idx) => (
                <Row
                  key={coin.asset_id}
                  coin={coin}
                  rank={idx + 1}
                  pricing={pricing}
                  maxVolume={maxVolume}
                  // Withhold the badge from ticker-collision look-alikes:
                  // COALESCE(slug, code) makes an impersonator's slug the
                  // verified code, so gate on the per-row API flag too.
                  verified={
                    (verifiedSlugs?.has(coinSlug(coin).toLowerCase()) ??
                      false) &&
                    !coin.unverified_ticker_collision
                  }
                />
              ))}
            </TBody>
          </Table>
        </TableWrap>
      )}
    </section>
  );
}

function Row({
  coin,
  rank,
  verified,
  pricing,
  maxVolume,
}: {
  coin: Coin;
  rank: number;
  verified: boolean;
  pricing: boolean;
  maxVolume: number;
}) {
  const price = parseDec(coin.price_usd);
  const volumeText = formatCompactUnits(coin.volume_24h_usd);
  const sub = assetSubLabel(coin);
  return (
    <LinkRow href={assetHref(coinSlug(coin))}>
      <Td className="text-ink-faint">{rank}</Td>
      <Td>
        <Link
          href={assetHref(coinSlug(coin))}
          className="group flex items-center gap-2"
        >
          <AssetIcon
            image={coin.image}
            code={coin.code}
            native={isNativeRow(coin)}
          />
          <span className="text-ink group-hover:text-brand-600 font-medium whitespace-nowrap">
            {assetLabel(coin)}
          </span>
          {verified && (
            <span
              title="Verified currency"
              aria-label="Verified currency"
              className="inline-flex items-center"
            >
              <svg
                xmlns="http://www.w3.org/2000/svg"
                viewBox="0 0 20 20"
                fill="currentColor"
                className="text-up h-3.5 w-3.5"
                aria-hidden="true"
              >
                <path
                  fillRule="evenodd"
                  d="M10 18a8 8 0 100-16 8 8 0 000 16zm3.707-9.293a1 1 0 00-1.414-1.414L9 10.586 7.707 9.293a1 1 0 00-1.414 1.414l2 2a1 1 0 001.414 0l4-4z"
                  clipRule="evenodd"
                />
              </svg>
            </span>
          )}
          {sub && (
            <span
              className="text-ink-muted hidden text-xs whitespace-nowrap sm:inline"
              title={sub.title}
            >
              {sub.text}
            </span>
          )}
          <ScamBadge tags={coin.issuer_directory_tags} />
        </Link>
      </Td>
      {pricing && (
        <Td align="right">
          {price != null ? (
            <span className="text-ink font-mono">
              ${formatPriceSmall(price)}
              {coin.thin_market && <ThinMarketBadge className="ml-1" />}
            </span>
          ) : (
            <Dash />
          )}
        </Td>
      )}
      {pricing && (
        <Td align="right">
          <ChangePct raw={coin.change_24h_pct} />
        </Td>
      )}
      {pricing && (
        <Td align="right">
          {volumeText !== '—' ? (
            <span className="text-ink-body inline-flex items-center gap-2 font-mono">
              <InlineBar
                value={Number(coin.volume_24h_usd)}
                max={maxVolume}
                label="24h volume relative to the top asset"
              />
              ${volumeText}
            </span>
          ) : (
            <Dash />
          )}
        </Td>
      )}
      {!pricing && (
        <Td align="right">
          <span className="text-ink-body font-mono">
            {/* Absent only when the asset has no catalogue row at all —
                unknown, not zero, so it draws as a dash and not as 0. */}
            {coin.observation_count != null
              ? formatCompact(coin.observation_count)
              : '—'}
          </span>
        </Td>
      )}
    </LinkRow>
  );
}

function parseDec(s: string | null | undefined): number | null {
  if (!s) return null;
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
}

// rankTopAssets injects native XLM (absent from /v1/assets) into the listed
// rows, returning the top 10. Native is deduped by asset_id
// (belt-and-suspenders — the listing never returns it).
//
// The LISTED rows keep the order the API returned: the server honours
// order_by, so re-sorting by raw volume_24h_usd would actively undo server policy — the API ranks
// on a concentration-ADJUSTED volume so wash / operational assets don't
// sit atop the directory, while volume_24h_usd in the payload is the RAW
// figure. Sorting the page by the raw column promotes exactly the assets
// the server demoted.
//
// So native is SPLICED into the server's order instead, at the first row
// whose ranking value it exceeds. Native is XLM: never wash-flagged, so
// its raw volume and its adjusted volume agree and the comparison is
// sound for this one row.
//
// Directory-flagged rows are then demoted below every unflagged one,
// AFTER the splice: native is never flagged, so a flagged row the server
// placed last must not end up above native merely by out-volume-ing it.
function rankTopAssets(
  native: Coin | null | undefined,
  listed: Coin[] | undefined,
  pricing: boolean,
): Coin[] {
  const rows = [...(listed ?? [])];
  const tagsOf = (c: Coin) => c.issuer_directory_tags;
  if (!native || rows.some((c) => c.asset_id === native.asset_id)) {
    return demoteFlaggedLast(rows, tagsOf).slice(0, 10);
  }
  // Compare on the SAME measure the request asked the API to order
  // by. On a net with no aggregator every volume is null, so comparing
  // USD volume put every row at 0 and left the order to sort stability —
  // a "top assets by activity" grid whose ranking was arbitrary.
  const rankOf = (c: Coin) =>
    pricing ? (parseDec(c.volume_24h_usd) ?? 0) : (c.observation_count ?? 0);

  const nativeRank = rankOf(native);
  const at = rows.findIndex((c) => rankOf(c) < nativeRank);
  rows.splice(at === -1 ? rows.length : at, 0, native);
  return demoteFlaggedLast(rows, tagsOf).slice(0, 10);
}

function Dash() {
  return <span className="text-ink-faint">—</span>;
}

function ChangePct({ raw }: { raw: string | null | undefined }) {
  if (raw == null) return <Dash />;
  const n = Number(raw);
  if (!Number.isFinite(n)) return <Dash />;
  const tone = n > 0 ? 'text-up' : n < 0 ? 'text-down' : 'text-ink-muted';
  const sign = n > 0 ? '+' : '';
  return (
    <span className={`font-mono tabular-nums ${tone}`}>
      {sign}
      {n.toFixed(2)}%
    </span>
  );
}

// AssetIcon renders the asset's real logo when the API surfaces one
// (coin.image — the issuer's SEP-1 stellar.toml CURRENCIES[].image,
// sanitized server-side to http(s) only), and gracefully degrades to
// the letter stand-in otherwise: a missing image, a null field,
// or a broken/blocked URL (onError) all fall back to the letter avatar.
//
// NOTE: today coin.image is populated only on the single-asset detail
// path (/v1/assets/{id}, applySep1Overlay), NOT on the /v1/assets
// listing that feeds this grid — so the fallback is what renders until
// the listing query surfaces the image. Rendering it here is
// forward-compatible: real logos appear with zero further UI change
// the moment the listing carries them. Plain <img loading="lazy">
// (not next/image) — remote SEP-1 hosts can't be enumerated into a
// next/image domain allowlist under static export.
function AssetIcon({
  image,
  code,
  native,
}: {
  image?: string | null;
  code?: string | null;
  native?: boolean;
}) {
  const [broken, setBroken] = useState(false);
  // Host-validated, https-only — scheme-only validation let a
  // hostile issuer's SEP-1 image URL point every viewer's browser at an
  // arbitrary (including private/internal) host. See lib/safe-domain.ts.
  const safe = isSafePublicImageUrl(image) && !broken;
  if (safe) {
    return (
      // eslint-disable-next-line @next/next/no-img-element -- remote SEP-1 icons; next/image needs a domain allowlist we can't enumerate under static export
      <img
        src={iconProxySrc(image!)}
        alt=""
        aria-hidden
        width={24}
        height={24}
        loading="lazy"
        onError={() => setBroken(true)}
        className="bg-surface-subtle h-6 w-6 rounded-full object-contain"
      />
    );
  }
  return (
    <span
      aria-hidden
      className="bg-surface-subtle text-ink-muted flex h-6 w-6 items-center justify-center rounded-full font-mono text-[11px] font-medium"
    >
      {native ? (
        <StellarMark className="text-ink h-3.5 w-3.5" />
      ) : (
        (code ?? 'C').slice(0, 1).toUpperCase()
      )}
    </span>
  );
}

function isNativeRow(coin: Coin): boolean {
  return coin.type === 'native' || coin.asset_id === 'native';
}

// The identity line beside the code: the SEP-1 name when the issuer
// published one, else the issuer key, else the slug, so a classic asset
// without an issuer field is still identified by its asset_id.
function assetSubLabel(coin: Coin): { text: string; title?: string } | null {
  if (isNativeRow(coin)) return { text: 'Stellar Lumens' };
  if (coin.name && coin.name !== coin.code) return { text: coin.name };
  if (coin.issuer)
    return { text: truncateMiddle(coin.issuer, 4, 4), title: coin.issuer };
  if (!coin.code) return null;
  return { text: coinSlug(coin) };
}
