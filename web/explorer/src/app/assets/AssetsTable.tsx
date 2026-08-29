'use client';

import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import { useEffect, useMemo, useState } from 'react';
import { ChevronLeft, ChevronRight, Search } from 'lucide-react';

import { useAssets, type AssetClassFilter, type Coin } from '@/api/hooks';
import { useTableSort, SortableTh, type SortColumn } from '@/lib/useTableSort';
import { formatCompact, formatPriceSmall, truncateMiddle } from '@/lib/format';
import { demoteFlaggedLast, scamFlagTags } from '@/lib/directory-tags';
import {
  Badge,
  Button,
  Callout,
  EmptyState,
  Mono,
  Segmented,
  TBody,
  TR,
  Table,
  TableWrap,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { useDebouncedValue } from '@/lib/useDebouncedValue';
import { CURRENT_NETWORK } from '@/lib/networks';

/**
 * /assets directory table — the CMC/CoinGecko-style global asset
 * listing, redesigned per the assets-redesign spec.
 *
 * Sourced from `/v1/assets?asset_class=…` (R-018 assets-unification
 * endgame). Each row:
 *
 *   - For catalogue assets (USDC the currency, GBP, BTC, …):
 *     `asset_id` is the slug; clicking lands on
 *     `/assets/{slug}` (GlobalAssetView).
 *   - For classic_assets non-catalogue (USDC-GA5Z..., AQUA-G..., …):
 *     `asset_id` is the full classic id; clicking lands on
 *     `/assets/{slug}` (handler dispatches to AssetDetail via
 *     ticker-or-canonical-id LookupBySlug).
 *
 * Columns are deliberately data-dense and right-aligned for
 * numerics. Issuer is intentionally NOT a column — issuer detail
 * is surfaced inline on the `/assets/{slug}` detail page.
 */

// MARKET_CAP_VOLUME_THRESHOLD_USD — below this 24h USD volume, the
// market-cap column shows "—" because the price feed underlying it
// is too thin for the cap to be a confident number.
const MARKET_CAP_VOLUME_THRESHOLD_USD = 1_000;

function parseAssetClass(raw: string | null): AssetClassFilter {
  switch (raw) {
    case 'fiat':
    case 'blockchain':
    case 'stablecoin':
      return raw;
    default:
      return 'all';
  }
}

export function AssetsTable({
  verifiedSlugs = [],
  endpoint = '/v1/assets',
  basePath = '/assets',
  classOptions,
}: {
  /**
   * Slugs from `/v1/assets/verified` (fetched server-side and
   * passed in). Used to decorate matching rows with a green-check
   * verified badge. Empty array is the safe default.
   */
  verifiedSlugs?: string[];
  /**
   * Listing endpoint. `/v1/assets` (Stellar-only, default) or
   * `/v1/external/assets` (fiat + reference coins). Passed through
   * to `useAssets` and surfaced in the footer hint.
   */
  endpoint?: string;
  /**
   * Base path for the filter/pagination URL updates (`router.push`)
   * AND per-row detail links. `/assets` (default) routes rows to the
   * Stellar detail page; `/external/assets` routes them to the
   * external (fiat / reference-coin) detail page (LC-001 split).
   */
  basePath?: string;
  /**
   * Class-filter chips. Omitted on /assets (operator request
   * 2026-08-24: no type filter on the main directory); the external
   * page passes its own fiat/reference set and keeps the row.
   */
  classOptions?: { value: AssetClassFilter; label: string }[];
} = {}) {
  const router = useRouter();
  const params = useSearchParams();
  const verifiedSlugSet = new Set(verifiedSlugs.map((s) => s.toLowerCase()));
  const cursor = params.get('cursor') ?? '';
  const limitParam = params.get('limit');
  const queryParam = params.get('q') ?? '';
  const assetClass = parseAssetClass(params.get('asset_class'));

  const limit = parseLimit(limitParam);

  // #328: every USD column below (price, the three change windows, market
  // cap, 24h volume, the 7d price sparkline) is aggregator-derived. On a
  // net with no aggregator they are ALL null, so the directory rendered
  // six dashes per row and paid for a 7d price series that does not exist.
  const pricing = CURRENT_NETWORK.pricing;
  const { data, isLoading, isError, error } = useAssets(
    assetClass,
    limit,
    cursor,
    queryParam || undefined,
    { sparkline7d: pricing },
    endpoint,
  );

  // Local input state, debounced into the URL so the server-side
  // ?q= filter doesn't refire on every keystroke.
  const [q, setQ] = useState(queryParam);
  // FEC audit A3-F5: debounce via the shared hook — the hand-rolled timer
  // needed an exhaustive-deps escape hatch to stay stable.
  const debouncedQ = useDebouncedValue(q.trim(), 250);
  useEffect(() => {
    if (debouncedQ === queryParam) return;
    setQuery({ q: debouncedQ, cursor: '' });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- write-back keys on the debounced input only; queryParam (the URL this effect writes) and the stable setQuery setter stay omitted so an external URL change (back/forward) isn't overwritten by stale input.
  }, [debouncedQ]);

  const assets = data?.assets ?? [];

  // Sortable columns (site-audit S36). Value accessors mirror the row's
  // rendered numbers so the visible order matches the header the user
  // clicked. Nulls sort last (useTableSort), so unpriced/newer assets don't
  // jump to the top of an ascending sort.
  const sortColumns: SortColumn<Coin, string>[] = [
    { key: 'asset', value: (c) => c.code ?? c.slug, initialDir: 'asc' },
    { key: 'class', value: (c) => c.kind ?? c.type, initialDir: 'asc' },
    ...(pricing
      ? ([
          { key: 'price', value: (c) => parseDec(c.price_usd) },
          { key: 'change_1h', value: (c) => parseDec(c.change_1h_pct) },
          { key: 'change_24h', value: (c) => parseDec(c.change_24h_pct) },
          { key: 'change_7d', value: (c) => parseDec(c.change_7d_pct) },
          { key: 'market_cap', value: (c) => parseDec(c.market_cap_usd) },
          { key: 'volume', value: (c) => parseDec(c.volume_24h_usd) },
        ] satisfies SortColumn<Coin, string>[])
      : []),
    { key: 'circulating', value: (c) => parseDec(c.circulating_supply) },
  ];
  // Default: leave the API's incoming order (market-cap-ish rank) until the
  // user clicks a header.
  const {
    sorted: sortedAssets,
    sort,
    toggle,
    ariaSort,
  } = useTableSort<Coin, string>(assets, sortColumns, null);

  // #356: a directory-flagged (malicious/unsafe/fraud/scam/hack/phishing)
  // issuer's asset ranks BELOW every unflagged one whatever the active
  // sort key. The server already orders the fetched page that way; this
  // re-applies it after the client-side column sort, which would otherwise
  // float a scam token straight back to the top of the page the moment the
  // user clicked "Volume 24h". Stable, so the chosen column still decides
  // the order within each group — and the row and its ⚠ Flagged badge stay
  // on the page: we refuse to RANK a flagged asset, we never hide it.
  const rankedAssets = useMemo(
    () => demoteFlaggedLast(sortedAssets, (c) => c.issuer_directory_tags),
    [sortedAssets],
  );

  function setQuery(
    updates: Partial<{
      cursor: string;
      limit: string;
      q: string;
      asset_class: string;
    }>,
  ) {
    const next = new URLSearchParams(params.toString());
    for (const [k, v] of Object.entries(updates)) {
      if (v === '' || v === undefined) next.delete(k);
      else next.set(k, v);
    }
    router.push(`${basePath}?${next.toString()}`);
  }

  if (isError) {
    return (
      <Callout tone="bad" title="Failed to load assets">
        {error instanceof Error ? error.message : 'unknown error'}
      </Callout>
    );
  }

  return (
    <div className="space-y-5">
      <FilterBar
        q={q}
        onQChange={setQ}
        limit={limit}
        onLimitChange={(v) => setQuery({ limit: String(v), cursor: '' })}
        assetClass={assetClass}
        classOptions={classOptions}
        onAssetClassChange={(v) =>
          setQuery({
            asset_class: v === 'all' ? '' : v,
            // Reset cursor when class changes — different phase,
            // different stream.
            cursor: '',
          })
        }
      />

      {!isLoading && assets.length === 0 ? (
        <EmptyState
          icon={<Search className="h-5 w-5" />}
          title="No assets match this filter"
          description="Try a different asset code, slug, issuer, or class."
        />
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <tr>
                <Th>#</Th>
                <SortableTh
                  label="Asset"
                  sortKey="asset"
                  sort={sort}
                  onSort={toggle}
                  ariaSort={ariaSort}
                />
                <SortableTh
                  label="Class"
                  sortKey="class"
                  sort={sort}
                  onSort={toggle}
                  ariaSort={ariaSort}
                />
                {pricing && (
                  <>
                    <SortableTh
                      label="Price"
                      sortKey="price"
                      sort={sort}
                      onSort={toggle}
                      ariaSort={ariaSort}
                      align="right"
                    />
                    <SortableTh
                      label="1h %"
                      sortKey="change_1h"
                      sort={sort}
                      onSort={toggle}
                      ariaSort={ariaSort}
                      align="right"
                    />
                    <SortableTh
                      label="24h %"
                      sortKey="change_24h"
                      sort={sort}
                      onSort={toggle}
                      ariaSort={ariaSort}
                      align="right"
                    />
                    <SortableTh
                      label="7d %"
                      sortKey="change_7d"
                      sort={sort}
                      onSort={toggle}
                      ariaSort={ariaSort}
                      align="right"
                    />
                    <SortableTh
                      label="Market cap"
                      sortKey="market_cap"
                      sort={sort}
                      onSort={toggle}
                      ariaSort={ariaSort}
                      align="right"
                    />
                    <SortableTh
                      label="Volume 24h"
                      sortKey="volume"
                      sort={sort}
                      onSort={toggle}
                      ariaSort={ariaSort}
                      align="right"
                    />
                  </>
                )}
                <SortableTh
                  label="Circulating"
                  sortKey="circulating"
                  sort={sort}
                  onSort={toggle}
                  ariaSort={ariaSort}
                  align="right"
                />
                {pricing && <Th align="right">7d chart</Th>}
              </tr>
            </THead>
            <TBody>
              {isLoading && (
                <tr>
                  <td
                    colSpan={pricing ? 11 : 4}
                    className="text-ink-muted py-12 text-center text-sm"
                  >
                    Loading…
                  </td>
                </tr>
              )}
              {!isLoading &&
                rankedAssets.map((coin, idx) => (
                  <AssetRow
                    key={coin.asset_id}
                    coin={coin}
                    rank={idx + 1}
                    // Badge "verified" ONLY for the real verified row.
                    // The listing serves COALESCE(slug, code) AS slug, so
                    // a NULL-slug impersonator emits the verified asset's
                    // CODE as its slug and would otherwise match the
                    // verified set — the API's per-row
                    // unverified_ticker_collision flag distinguishes it.
                    verified={
                      verifiedSlugSet.has(coin.slug.toLowerCase()) &&
                      !coin.unverified_ticker_collision
                    }
                    basePath={basePath}
                    pricing={pricing}
                  />
                ))}
            </TBody>
          </Table>
        </TableWrap>
      )}

      <Pagination
        cursor={cursor}
        nextCursor={data?.next_cursor ?? ''}
        // AM-18: history.back() walks off-site when a cursor URL is
        // opened directly; keyset cursors can't step backwards, so
        // "previous" honestly means "back to the top".
        onPrev={() => setQuery({ cursor: '' })}
        onNext={() =>
          data?.next_cursor && setQuery({ cursor: data.next_cursor })
        }
      />

      <p className="text-ink-muted text-xs">
        Live data from{' '}
        <code className="bg-surface-subtle rounded-sm px-1 font-mono text-[11px]">
          {endpoint}?asset_class={assetClass}
        </code>
        . Verified catalogue rows surface first, then long-tail Stellar-classic
        rows by 24h volume. Per-asset issuer + on-chain pool detail lives on{' '}
        <code className="bg-surface-subtle rounded-sm px-1 font-mono text-[11px]">
          /assets/&#123;slug&#125;
        </code>
        .
      </p>
    </div>
  );
}

function FilterBar({
  q,
  onQChange,
  limit,
  onLimitChange,
  assetClass,
  classOptions,
  onAssetClassChange,
}: {
  q: string;
  onQChange: (v: string) => void;
  limit: number;
  onLimitChange: (v: number) => void;
  assetClass: AssetClassFilter;
  classOptions?: { value: AssetClassFilter; label: string }[];
  onAssetClassChange: (v: AssetClassFilter) => void;
}) {
  return (
    <div className="space-y-3">
      {classOptions && (
        <div className="flex flex-wrap items-center gap-2 text-xs">
          <span className="text-ink-muted">Asset type:</span>
          <Segmented
            ariaLabel="Asset type"
            options={classOptions.map((opt) => ({
              label: opt.label,
              value: opt.value,
            }))}
            value={assetClass}
            onChange={(v) => onAssetClassChange(v as AssetClassFilter)}
          />
        </div>
      )}

      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="relative">
          <Search className="text-ink-faint absolute top-1/2 left-2.5 h-4 w-4 -translate-y-1/2" />
          <input
            type="search"
            aria-label="Search assets by code, slug, or name"
            value={q}
            onChange={(e) => onQChange(e.target.value)}
            placeholder="Search by code, slug, or name…"
            className="border-line bg-surface placeholder:text-ink-faint focus:border-brand-500 focus:ring-brand-500 w-72 rounded-md border py-1.5 pr-3 pl-8 text-sm focus:ring-1 focus:outline-hidden"
          />
        </div>
        <label className="text-ink-muted flex items-center gap-2 text-xs">
          <span>Per page</span>
          <select
            value={limit}
            onChange={(e) => onLimitChange(parseInt(e.target.value, 10))}
            className="border-line bg-surface focus:border-brand-500 focus:ring-brand-500 rounded-md border px-2 py-1 text-xs focus:ring-1 focus:outline-hidden"
          >
            <option value={50}>50</option>
            <option value={100}>100</option>
            <option value={200}>200</option>
            <option value={500}>500</option>
          </select>
        </label>
      </div>
    </div>
  );
}

function AssetRow({
  coin,
  rank,
  verified,
  basePath,
  pricing,
}: {
  coin: Coin;
  rank: number;
  verified: boolean;
  basePath: string;
  pricing: boolean;
}) {
  const price = parseDec(coin.price_usd);
  const marketCapRaw = parseDec(coin.market_cap_usd);
  const volume = parseDec(coin.volume_24h_usd);
  // circulating_supply is a RAW smallest-unit integer string; render it
  // in whole asset units by scaling down 10^decimals (7 for classic /
  // native, 0 for catalogue / fiat rows). market_cap / volume / price are
  // already server-pre-scaled — do NOT divide those.
  const supplyRaw = parseDec(coin.circulating_supply);
  const supply =
    supplyRaw != null ? supplyRaw / 10 ** (coin.decimals ?? 7) : null;
  // Suppress market cap when 24h volume is below the confidence
  // threshold — without enough recent trade volume the price
  // underlying the cap is too thin to publish a believable number.
  // Catalogue fiat rows are EXEMPT: their market_cap is computed
  // from a static M2 × current FX rate; trade volume is meaningless
  // for fiat-as-money-supply.
  const marketCap =
    coin.class === 'fiat'
      ? marketCapRaw
      : marketCapRaw != null &&
          volume != null &&
          volume >= MARKET_CAP_VOLUME_THRESHOLD_USD
        ? marketCapRaw
        : null;
  // The raw canonical identifier, when it says something the code above
  // does not: `JFKBANK2-GB7KFNUR…` next to code `JFKBANK2`, but nothing
  // extra for a catalogue row whose slug IS its ticker (XLM / "xlm").
  const rawAssetId =
    coin.code && coin.slug.toLowerCase() !== coin.code.toLowerCase()
      ? coin.slug
      : null;
  return (
    <TR>
      <Td>
        <span className="text-ink-faint tnum">{rank}</span>
      </Td>
      <Td>
        <Link
          href={`${basePath}/${coin.slug}`}
          className="group flex items-baseline gap-2"
        >
          <span className="text-ink group-hover:text-brand-600 font-medium">
            {/* An uncatalogued Soroban asset has NO code at all — the
                listing serves code=NULL and slug=<contract id>. Label the
                row with the middle-truncated contract id rather than an
                empty cell (#356 note: whether it should get a synthetic
                placeholder name instead is a separate product call). */}
            {coin.code || (
              <span className="font-mono text-[13px]" title={coin.slug}>
                {truncateMiddle(coin.slug, 6, 4)}
              </span>
            )}
          </span>
          {verified && (
            <span
              title="Verified currency — in the catalogue at /v1/assets/verified"
              className="inline-flex items-center"
              aria-label="Verified currency"
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
          {coin.name && (
            <span className="text-ink-muted text-[11px]">{coin.name}</span>
          )}
        </Link>
        {/* The raw identifier, middle-truncated (#356). Classic ids are
            65+ chars (JFKBANK2-GB7KFNUR…KL5BANK) and used to render in
            full, dominating the column. The TAIL is kept because issuer
            strkeys differ near the end; hover reveals the whole value and
            the copy button yields the full, un-elided id. Skipped when the
            code already IS the label (catalogue rows: XLM / "Stellar
            Lumens") or when the primary label above is the id itself. */}
        {rawAssetId && (
          <Mono
            value={rawAssetId}
            truncate={{ head: 8, tail: 6 }}
            className="text-ink-muted text-[11px]"
          />
        )}
        <ScamBadge tags={coin.issuer_directory_tags} />
      </Td>
      <Td>
        <ClassBadge cls={coin.class} />
      </Td>
      {pricing && (
        <Td align="right">
          {price != null ? (
            <span className="text-ink font-mono tabular-nums">
              ${formatPriceSmall(price)}
              {/* Declared-peg provenance (price_basis=declared_peg): the
                server filled this price from an operator-declared 1:1
                fiat peg × the current FX rate because no market price
                survived the substance gate — annotate it so a peg-based
                figure is never presented as a market observation. */}
              {coin.price_basis === 'declared_peg' && (
                <span
                  className="text-ink-muted ml-1 font-sans text-[10px] tracking-wider uppercase"
                  title="Declared 1:1 fiat peg × current FX rate — not a market-observed price"
                >
                  pegged
                </span>
              )}
            </span>
          ) : (
            <Dash />
          )}
        </Td>
      )}
      {pricing && (
        <Td align="right">
          <ChangePct raw={coin.change_1h_pct} />
        </Td>
      )}
      {pricing && (
        <Td align="right">
          <ChangePct raw={coin.change_24h_pct} />
        </Td>
      )}
      {pricing && (
        <Td align="right">
          <ChangePct raw={coin.change_7d_pct} />
        </Td>
      )}
      {pricing && (
        <Td align="right">
          {marketCap != null ? (
            <span className="text-ink-body font-mono tabular-nums">
              ${formatCompact(marketCap)}
            </span>
          ) : (
            <Dash title="Awaiting circulating supply via SEP-1 / on-chain observer" />
          )}
        </Td>
      )}
      {pricing && (
        <Td align="right">
          {volume != null ? (
            <span className="text-ink-body font-mono tabular-nums">
              ${formatCompact(volume)}
            </span>
          ) : (
            <Dash />
          )}
        </Td>
      )}
      <Td align="right">
        {supply != null ? (
          <span className="text-ink-body font-mono tabular-nums">
            {formatCompact(supply)}
          </span>
        ) : (
          <Dash title="Awaiting issuer SEP-1 fixed_number / on-chain mint observer" />
        )}
      </Td>
      {pricing && (
        <Td align="right">
          <RowSparkline points={coin.price_history_7d} />
        </Td>
      )}
    </TR>
  );
}

// ScamBadge — compact directory-flag pill in the asset row. Renders only
// when the issuer's curated directory tags include a scam-warning flag
// (malicious/unsafe/fraud/scam/hack/phishing). Third-party attribution,
// never a verification signal — but the same flag DOES withhold the row's
// price/market cap (the scam gate) and sinks the row to the bottom of the
// ranking (demoteFlaggedLast, #356). Badged, demoted, never hidden.
function ScamBadge({ tags }: { tags?: string[] | null }) {
  const flagged = scamFlagTags(tags);
  if (flagged.length === 0) return null;
  return (
    <Badge
      tone="bad"
      className="mt-1"
      title={`Flagged by the stellar-expert community directory as: ${flagged.join(
        ', ',
      )} (third-party attribution — display-only, not a StellarIndex verification signal)`}
    >
      ⚠ Flagged
    </Badge>
  );
}

function ClassBadge({ cls }: { cls?: string }) {
  if (!cls) {
    return <span className="text-ink-faint text-xs">—</span>;
  }
  const tone: 'warn' | 'ok' | 'brand' =
    cls === 'fiat' ? 'warn' : cls === 'stablecoin' ? 'ok' : 'brand';
  const label =
    cls === 'fiat' ? 'Fiat' : cls === 'stablecoin' ? 'Stablecoin' : 'Crypto';
  return <Badge tone={tone}>{label}</Badge>;
}

function RowSparkline({
  points,
}: {
  points?: { t: string; p?: string | null }[];
}) {
  const values = (points ?? [])
    .map((pt) => (pt.p ? Number(pt.p) : null))
    .filter((v): v is number => v != null && Number.isFinite(v));
  if (values.length < 2) {
    return <span className="text-ink-faint font-mono text-[10px]">—</span>;
  }
  const W = 80;
  const H = 24;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;
  const stepX = W / (values.length - 1);
  const path = values
    .map((v, i) => {
      const x = i * stepX;
      const y = H - ((v - min) / range) * H;
      return `${i === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(' ');
  const positive = values[values.length - 1] >= values[0];
  return (
    <svg
      width={W}
      height={H}
      viewBox={`0 0 ${W} ${H}`}
      className={`inline-block ${positive ? 'text-up' : 'text-down'}`}
    >
      <path
        d={path}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function Pagination({
  cursor,
  nextCursor,
  onPrev,
  onNext,
}: {
  cursor: string;
  nextCursor: string;
  onPrev: () => void;
  onNext: () => void;
}) {
  const hasPrev = cursor !== '';
  const hasNext = nextCursor !== '';
  return (
    <div className="flex items-center justify-between gap-2 px-1">
      <Button
        variant="secondary"
        size="sm"
        disabled={!hasPrev}
        onClick={onPrev}
      >
        <ChevronLeft className="h-3.5 w-3.5" />
        Back to top
      </Button>
      <span className="text-ink-faint text-xs">
        {hasPrev || hasNext ? 'Cursor-paginated' : ' '}
      </span>
      <Button
        variant="secondary"
        size="sm"
        disabled={!hasNext}
        onClick={onNext}
      >
        Next
        <ChevronRight className="h-3.5 w-3.5" />
      </Button>
    </div>
  );
}

function Dash({ title }: { title?: string }) {
  return (
    <span className="text-ink-faint" title={title ?? 'No data yet'}>
      —
    </span>
  );
}

function ChangePct({ raw }: { raw: string | null | undefined }) {
  if (raw == null)
    return <Dash title="Not enough trade history to compute this window" />;
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

function parseDec(s: string | null | undefined): number | null {
  if (!s) return null;
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
}

function parseLimit(raw: string | null): number {
  const valid = [50, 100, 200, 500];
  if (!raw) return 100;
  const n = parseInt(raw, 10);
  return valid.includes(n) ? n : 100;
}
