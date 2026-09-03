'use client';

import Link from 'next/link';
import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';

import { apiGet, asExample } from '@/api/client';
import { useSources } from '@/api/hooks';
import { AssetLink } from '@/components/AssetLink';
import { Panel } from '@/components/reveal';
import { Mono } from '@/components/ui';
import { isRawOracleAsset, rawOracleSymbol } from '@/lib/asset-label';
import { formatOraclePrice, formatRelative } from '@/lib/format';
import { sourceToneClass } from '@/lib/pillTone';

import type { RequestExample } from '@/api/client';
import type { components } from '@/api/types';

type OracleReading = components['schemas']['OracleReading'];
type UnverifiedWarning = components['schemas']['UnverifiedWarning'];

/**
 * SHOW_SYMBOL_MATCHED_RAW_FEEDS — whether an asset's oracle panel also
 * lists the `raw:<symbol>` feeds whose SYMBOL matches its code (e.g.
 * `raw:USDT0` beside a USDT0 asset page).
 *
 * Default OFF, and the default is the safe one. A `raw:` row is, by
 * definition, an oracle symbol that maps to no canonical asset — the
 * only thing tying it to this page is that the two strings look alike.
 * Showing it here re-creates, one layer up, exactly the by-name
 * confusion the #336 issuer gate exists to remove: a reader who sees a
 * price under this asset's heading reasonably concludes it is a price
 * FOR this asset. The full unmapped inventory is on /oracles, where it
 * sits next to no asset's identity.
 *
 * Flipping this constant to `true` (or passing the prop per call site)
 * turns the group on: it renders in its own panel, under its own
 * heading, with the non-attribution stated in the body.
 */
export const SHOW_SYMBOL_MATCHED_RAW_FEEDS = false;

const STREAMS_PARAMS = { include_unmapped: 'true' } as const;

/**
 * isUnmapped — the `mapped` flag is the contract; the `raw:` wire prefix
 * is the same fact read off the asset id, kept as a belt-and-braces
 * guard so a row can never land in the attributed table by a missing
 * flag alone. Same predicate as oracles/OraclesView.tsx.
 */
function isUnmapped(r: OracleReading): boolean {
  return r.mapped === false || isRawOracleAsset(r.asset);
}

export type AssetOraclesPanelProps = {
  /** Canonical asset_id this page is about (`native`, `USDC-GA5Z…`, `C…`). */
  assetID: string;
  /** Display code for the copy ("USDC", "AQUA"). */
  symbol: string;
  /**
   * The API's ticker-collision body when this asset BORROWS a verified
   * currency's ticker without being its verified issuer
   * (`AssetDetail.unverified_warning`). Presence changes the panel from
   * "here are the oracles" to "we will not attribute another issuer's
   * oracles to this asset" — see UnattributedOracles below.
   *
   * Callers on a network with no verified-currency curation must pass
   * null: the catalogue is a MAINNET curation and stamps genuine
   * test-net assets as collisions (page.tsx gates the header banner on
   * CURRENT_NETWORK.pricing for the same reason).
   */
  tickerCollision?: UnverifiedWarning | null;
  /** See SHOW_SYMBOL_MATCHED_RAW_FEEDS. */
  showSymbolMatchedRawFeeds?: boolean;
  /**
   * Heading rank for the panel titles. Defaults to 2 — this component is
   * a top-level section of /assets/[slug], directly under the page's
   * <h1>, and Panel's own default (3) would skip a level (#486).
   */
  headingLevel?: 2 | 3 | 4;
};

/**
 * AssetOraclesPanel — "which oracles publish a price for this asset",
 * the per-token half of /oracles (#336).
 *
 * Reads `/v1/oracle/latest?asset=<asset_id>`: one row per source that
 * has observed the asset, with the publishing contract, the reading,
 * its declared decimals scale and its age.
 *
 * Two filters stand between that response and the table, and both are
 * about not calling something an oracle price when it is not one:
 *
 *  1. ORACLE CLASS. `/v1/oracle/latest` does not apply the
 *     `class=oracle` filter `/v1/oracle/streams` applies, so the live
 *     response for `?asset=native` includes a `coingecko` row — an
 *     AGGREGATOR. A panel headed "oracle feeds" that lists CoinGecko
 *     among reflector / band / redstone misstates what CoinGecko is.
 *     Rows are intersected with `/v1/sources?class=oracle`, the same
 *     registry /oracles filters on. Without that registry we cannot
 *     tell the two apart, so a registry failure renders the unavailable
 *     state rather than an unclassified list.
 *  2. MAPPED. A `raw:<symbol>` row is reference-only and never joins an
 *     attributed list (see isUnmapped, and the raw-feed group below).
 *
 * The third filter is the API's, and it is why this component takes
 * `tickerCollision`: since #336 the server only translates a classic
 * `CODE-G…` to the global `crypto:<CODE>` key when that exact
 * (code, issuer) is the verified-currency catalogue's own issuance, so
 * an impersonator correctly gets no rows. Empty-because-impersonator
 * and empty-because-nobody-publishes-it are the same `[]` on the wire
 * and must NOT be the same sentence on the page.
 */
export function AssetOraclesPanel({
  assetID,
  symbol,
  tickerCollision = null,
  showSymbolMatchedRawFeeds = SHOW_SYMBOL_MATCHED_RAW_FEEDS,
  headingLevel = 2,
}: AssetOraclesPanelProps) {
  const collided = tickerCollision != null;
  const example = asExample('/v1/oracle/latest', { asset: assetID });

  // Not merely filtered out of the render: for a ticker-collision asset
  // the request is never issued. The server gate already answers `[]`,
  // and this keeps the refusal true on the client too — if that gate
  // ever regressed, this panel would still not put another issuer's
  // prices under this asset's name.
  const latest = useQuery<OracleReading[]>({
    queryKey: ['/v1/oracle/latest', assetID],
    queryFn: async () => {
      const env = await apiGet<{ data: OracleReading[] }>('/v1/oracle/latest', {
        asset: assetID,
      });
      return env.data ?? [];
    },
    enabled: !collided,
    refetchInterval: 60_000,
  });

  // Shares the ['/v1/sources','oracle','no-stats'] query with any other
  // oracle-class consumer in the tree; the 5-minute staleTime is the
  // hook's.
  const registry = useSources('oracle');

  const streams = useQuery<OracleReading[]>({
    queryKey: ['/v1/oracle/streams', STREAMS_PARAMS],
    queryFn: async () => {
      const env = await apiGet<{ data: OracleReading[] }>(
        '/v1/oracle/streams',
        STREAMS_PARAMS,
      );
      return env.data ?? [];
    },
    enabled: showSymbolMatchedRawFeeds && !collided,
    refetchInterval: 60_000,
  });

  const oracleNames = useMemo(
    () => new Set((registry.data ?? []).map((s) => s.name)),
    [registry.data],
  );

  const rows = useMemo(
    () =>
      (latest.data ?? [])
        .filter((r) => oracleNames.has(r.source) && !isUnmapped(r))
        .sort((a, b) => a.source.localeCompare(b.source)),
    [latest.data, oracleNames],
  );

  const symbolMatchedRaw = useMemo(
    () =>
      (streams.data ?? [])
        .filter((r) => isRawOracleAsset(r.asset) && isUnmapped(r))
        .filter(
          (r) =>
            rawOracleSymbol(r.asset).toUpperCase() === symbol.toUpperCase(),
        )
        .sort((a, b) => a.source.localeCompare(b.source)),
    [streams.data, symbol],
  );

  if (tickerCollision) {
    return (
      <UnattributedOracles
        symbol={symbol}
        warning={tickerCollision}
        example={example}
        headingLevel={headingLevel}
      />
    );
  }

  // Absent (the request never answered) is not empty (it answered with
  // nothing). Only the second is ours to state as a fact about the
  // network — the frontend-honesty rule this file's siblings follow.
  if (latest.isError || registry.isError) {
    return (
      <Panel
        headingLevel={headingLevel}
        title="Oracle feeds"
        source={example}
        bodyClassName="text-sm text-ink-muted"
      >
        Oracle feeds unavailable right now — retry shortly.
      </Panel>
    );
  }

  if (latest.isLoading || registry.isLoading) {
    return (
      <Panel
        headingLevel={headingLevel}
        title="Oracle feeds"
        source={example}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }

  if (rows.length === 0) {
    return (
      <Panel
        headingLevel={headingLevel}
        title="Oracle feeds"
        hint="No oracle publishes a price for this asset"
        source={example}
        bodyClassName="space-y-2 text-sm text-ink-muted"
      >
        <p>
          None of the oracles we ingest — Reflector, Band, RedStone, Chainlink —
          publishes a reading for {symbol}. Oracles cover a short list of global
          tickers; most Stellar assets are priced only by the markets they trade
          in.
        </p>
        <p className="text-xs">{VWAP_POLICY_NOTE}</p>
      </Panel>
    );
  }

  return (
    <div className="space-y-4">
      <Panel
        headingLevel={headingLevel}
        title={`Oracle feeds (${rows.length})`}
        hint="Latest reading per oracle publishing this asset"
        source={example}
        bodyClassName="-mx-4"
      >
        <div className="overflow-x-auto">
          <table className="divide-line min-w-full divide-y text-sm">
            <thead>
              <tr className="text-ink-muted text-left text-[10px] tracking-wider uppercase">
                <Th>Oracle</Th>
                <Th>Contract</Th>
                <Th>Quote</Th>
                <Th align="right">Latest price</Th>
                <Th align="right">Decimals</Th>
                <Th align="right">Updated</Th>
              </tr>
            </thead>
            <tbody className="divide-line-subtle divide-y">
              {rows.map((r) => (
                <ReadingRow key={`${r.source}|${r.asset}|${r.quote}`} r={r} />
              ))}
            </tbody>
          </table>
        </div>
        <p className="text-ink-muted px-4 pt-3 text-xs">
          Oracle-class publishers only — aggregator and exchange prices for{' '}
          {symbol} are on the Markets tab. {VWAP_POLICY_NOTE}
        </p>
      </Panel>

      {showSymbolMatchedRawFeeds && (
        <SymbolMatchedRawFeeds
          symbol={symbol}
          rows={symbolMatchedRaw}
          headingLevel={headingLevel}
        />
      )}
    </div>
  );
}

/**
 * VWAP_POLICY_NOTE — the sentence /oracles leads with, repeated wherever
 * an oracle reading sits next to our own price so the two are never read
 * as the same number.
 */
const VWAP_POLICY_NOTE =
  'Oracle readings are reported alongside our independent VWAP and never included in it — mixing them would import their methodology.';

/**
 * UnattributedOracles — the panel an asset gets when it wears a verified
 * currency's ticker without being that currency's issuer.
 *
 * The copy is the whole point of the state. `/v1/oracle/latest` answers
 * `[]` here exactly as it does for an asset nothing publishes, and the
 * two must not read alike: one is a gap in coverage, the other is a
 * refusal to attribute. "No oracle prices this asset" would be both
 * false (an oracle does price the ticker — for someone else) and
 * flattering, since it presents an identity refusal as a coverage
 * to-do that might one day be filled in.
 *
 * Rendered as prose rather than a Callout: the page header already
 * raises a role=alert ticker-collision banner, and a second assertive
 * alert restating the same finding is noise. This explains one
 * consequence of that finding.
 */
function UnattributedOracles({
  symbol,
  warning,
  example,
  headingLevel,
}: {
  symbol: string;
  warning: UnverifiedWarning;
  example: RequestExample;
  headingLevel: 2 | 3 | 4;
}) {
  const verifiedName = warning.verified_name || symbol;
  const linkable = !!warning.verified_asset_id && !!warning.verified_slug;
  return (
    <Panel
      headingLevel={headingLevel}
      title="Oracle feeds"
      hint={`Not attributed — “${symbol}” is another issuer's verified ticker`}
      source={example}
      bodyClassName="text-ink-body space-y-3 text-sm"
    >
      <p>
        Oracles publish under a global ticker —{' '}
        <span className="font-mono text-xs">{symbol}</span> — not under a
        Stellar (code, issuer) pair. This asset uses that ticker but is not the
        verified {verifiedName}, so the readings published for{' '}
        <span className="font-mono text-xs">{symbol}</span> are a different
        asset&apos;s readings and are not shown here.
      </p>
      <p>
        <strong className="font-semibold">This is not a coverage gap.</strong>{' '}
        We are not saying no oracle prices this asset — we are declining to
        attribute another issuer&apos;s oracle prices to it. Identity on Stellar
        is (code, issuer); a shared ticker is not a shared asset.
      </p>
      <p className="text-ink-muted">
        {linkable ? (
          <>
            The verified {verifiedName}
            {warning.verified_issuer
              ? ` — issued by ${warning.verified_issuer} —`
              : ''}{' '}
            is at{' '}
            <Link
              href={`/assets/${warning.verified_slug}`}
              className="text-brand-600 hover:underline"
            >
              /assets/{warning.verified_slug}
            </Link>
            , and its oracle feeds are on that page.
          </>
        ) : (
          <>
            No verified <span className="font-mono text-xs">{symbol}</span> is
            issued on Stellar at all, so no Stellar asset may claim that
            ticker&apos;s readings.
          </>
        )}{' '}
        <span className="font-mono text-xs">/v1/oracle/latest</span> returns
        nothing for this asset for the same reason: the API only translates a
        classic asset to a global ticker when its (code, issuer) is the
        verified-currency catalogue&apos;s own issuance.
      </p>
    </Panel>
  );
}

/**
 * SymbolMatchedRawFeeds — the opt-in group (SHOW_SYMBOL_MATCHED_RAW_FEEDS).
 *
 * These rows are matched to this page by STRING EQUALITY on the symbol
 * and nothing else, which is precisely why they render under their own
 * heading, with the non-attribution in the body, and never in the table
 * above.
 */
function SymbolMatchedRawFeeds({
  symbol,
  rows,
  headingLevel,
}: {
  symbol: string;
  rows: OracleReading[];
  headingLevel: 2 | 3 | 4;
}) {
  if (rows.length === 0) return null;
  return (
    <Panel
      headingLevel={headingLevel}
      title={`Unmapped feeds matching “${symbol}” (${rows.length})`}
      hint="Symbol match only — not attributed to this asset"
      source={asExample('/v1/oracle/streams', STREAMS_PARAMS)}
      bodyClassName="-mx-4"
    >
      <p className="text-ink-muted px-4 pb-3 text-xs">
        An oracle publishes these symbols under no canonical asset, so we record
        them verbatim and compare them to nothing. They appear here only because
        the symbol string matches this asset&apos;s code — that is a
        resemblance, not an identity, and these prices are not this asset&apos;s
        prices.
      </p>
      <div className="overflow-x-auto">
        <table className="divide-line min-w-full divide-y text-sm">
          <thead>
            <tr className="text-ink-muted text-left text-[10px] tracking-wider uppercase">
              <Th>Oracle</Th>
              <Th>Raw symbol</Th>
              <Th>Quote (assumed)</Th>
              <Th align="right">Latest price</Th>
              <Th align="right">Updated</Th>
            </tr>
          </thead>
          <tbody className="divide-line-subtle divide-y">
            {rows.map((r) => (
              <tr
                key={`${r.source}|${r.asset}|${r.quote}`}
                className="hover:bg-surface-muted"
              >
                <Td>
                  <SourcePill source={r.source} />
                </Td>
                <Td>
                  <span
                    className="text-ink-body font-mono text-xs"
                    title={`${r.asset} — unmapped oracle symbol`}
                  >
                    {rawOracleSymbol(r.asset)}
                  </span>
                </Td>
                <Td>
                  {/* Unlinked and labelled "assumed": for an unmapped row
                      the quote is the decoder's DEFAULT, not an observed
                      denomination. Same treatment as OraclesView. */}
                  <span
                    className="text-ink-muted font-mono text-xs"
                    title={`${r.quote} is the decoder's DEFAULT for an unmapped symbol, not an observed denomination — this row's true quote is unknown`}
                  >
                    {r.quote}
                  </span>
                </Td>
                <Td align="right">
                  <PriceCell r={r} />
                </Td>
                <Td align="right">
                  <Updated ts={r.ts} />
                </Td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Panel>
  );
}

function ReadingRow({ r }: { r: OracleReading }) {
  return (
    <tr className="hover:bg-surface-muted">
      <Td>
        <SourcePill source={r.source} />
      </Td>
      <Td>
        {r.contract_id ? (
          <Mono value={r.contract_id} truncate={{ head: 4, tail: 4 }} />
        ) : (
          <span
            className="text-ink-muted text-xs"
            title="Off-chain oracle — no Stellar contract publishes this reading"
          >
            off-chain
          </span>
        )}
      </Td>
      <Td>
        <AssetLink canonical={r.quote} />
      </Td>
      <Td align="right">
        <PriceCell r={r} />
      </Td>
      <Td align="right">
        <span
          className="text-ink-muted font-mono text-xs tabular-nums"
          title="Source-declared scale for price_raw (ADR-0003: the raw integer is never lost)"
        >
          {r.decimals}
        </span>
      </Td>
      <Td align="right">
        <Updated ts={r.ts} />
      </Td>
    </tr>
  );
}

function SourcePill({ source }: { source: string }) {
  return (
    <Link
      href={`/sources/${source}`}
      className={`inline-block rounded-sm px-1.5 py-0.5 text-[10px] font-medium tracking-wider uppercase hover:underline ${sourceToneClass(source)}`}
    >
      {source}
    </Link>
  );
}

function PriceCell({ r }: { r: OracleReading }) {
  return (
    <span
      className="text-ink-body font-mono tabular-nums"
      title={`${r.price_raw} at ${r.decimals} decimals`}
    >
      {formatOraclePrice(r.price)}
    </span>
  );
}

function Updated({ ts }: { ts: string }) {
  return (
    <time dateTime={ts} className="text-ink-muted font-mono text-xs" title={ts}>
      {formatRelative(ts)}
    </time>
  );
}

function Th({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <th
      scope="col"
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <td
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}
