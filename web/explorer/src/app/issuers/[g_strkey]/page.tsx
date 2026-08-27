import type { Metadata } from 'next';
import { Suspense } from 'react';

import { IssuerPathView } from './IssuerPathView';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { Container, Breadcrumbs } from '@/components/ui';
import { asExample } from '@/api/client';
import { buildFetchData, failBuild, requireRows } from '@/lib/buildFetch';
import { formatCompact, formatPriceSmall, formatRelative } from '@/lib/format';
import { isSafeHomeDomain } from '@/lib/safe-domain';
import { ogImageFor } from '@/lib/seo';
import { StellarExpertLink } from '@/components/StellarExpertLink';
import { CURRENT_NETWORK, stellarChainEntityUrl } from '@/lib/networks';

/**
 * /issuers/[g_strkey] — single-issuer detail page.
 *
 * Server component fetches /v1/issuers/{g_strkey} at request
 * time. Renders the identity, auth flags, SEP-1 status, and a
 * table of every asset minted by the issuer.
 *
 * G-strkeys are 56 chars (uppercase base32). Static export with
 * output:'export' needs a non-empty generateStaticParams; we
 * pre-render the top issuers (up to 100) so deep links resolve
 * without a build-time round trip per route.
 */
type Params = Promise<{ g_strkey: string }>;

interface IssuedAsset {
  asset_id: string;
  code: string;
  slug: string;
  first_seen_ledger: number;
  last_seen_ledger: number;
  observation_count: number;
}

interface IssuerDetail {
  g_strkey: string;
  home_domain?: string;
  org_name?: string;
  org_verified?: boolean;
  scam_reason?: string;
  auth_required?: boolean;
  auth_revocable?: boolean;
  auth_immutable?: boolean;
  auth_clawback?: boolean;
  sep1_resolved_at?: string;
  creation_ledger?: number;
  assets?: IssuedAsset[];
}

export async function generateStaticParams() {
  // 'shell' is the runtime-fallback sentinel: functions/issuers/[[path]].js
  // serves its HTML for any issuer beyond the pre-rendered top-100
  // (S-022 — those used to hard-404 while search + asset pages linked
  // to them). Same pattern as accounts/contracts/ledgers/transactions.
  const fallback = [
    { g_strkey: 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN' },
  ];
  // Fail-hard (src/lib/buildFetch.ts): an unreachable or empty issuers
  // listing throws so the build fails instead of exporting only the
  // fallback route. CI-stub builds fall through to the fallback.
  const rows = requireRows(
    await buildFetchData<{ g_strkey: string }[]>('/v1/issuers?limit=100'),
    '/v1/issuers listing for /issuers/[g_strkey] static params',
  );
  const keys = rows.map((i) => i.g_strkey).filter(Boolean);
  const params =
    keys.length > 0 ? keys.map((g_strkey) => ({ g_strkey })) : fallback;
  return [{ g_strkey: 'shell' }, ...params];
}

function fetchIssuer(gStrkey: string): Promise<IssuerDetail | null> {
  // 20s budget: issuer detail is the slowest per-entity endpoint of
  // the export (~4s steady-state; >8s under the build's 9-worker
  // concurrency — measured when the first fail-hard build tripped
  // here, 2026-07-02). The old 2s timeout baked "Issuer not found".
  return buildFetchData<IssuerDetail>(
    `/v1/issuers/${encodeURIComponent(gStrkey)}`,
    { timeoutMs: 20_000 },
  );
}

interface CoinPriceRow {
  asset_id: string;
  price_usd?: string | null;
  volume_24h_usd?: string | null;
  change_24h_pct?: string | null;
  // ISS-1: served by the same fetch all along, previously dropped —
  // supply and market cap are the canonical issuer questions.
  circulating_supply?: string | null;
  market_cap_usd?: string | null;
  // Per-asset display scale for circulating_supply (classic assets are
  // 7; SEP-41 tokens vary). Served on every AssetDetail row.
  decimals?: number;
}

async function fetchIssuerCoins(
  gStrkey: string,
): Promise<Map<string, CoinPriceRow>> {
  const out = new Map<string, CoinPriceRow>();
  // /v1/assets?issuer= (rc.47 R-018 finish). Wire shape is
  // `{data: [AssetDetail]}` — fields match CoinPriceRow for the read
  // columns this page renders. An empty list is legitimate (issuer
  // with no priced assets); transport failure throws via buildFetch.
  const rows = await buildFetchData<CoinPriceRow[]>(
    `/v1/assets?issuer=${encodeURIComponent(gStrkey)}&limit=500`,
    { timeoutMs: 20_000 },
  );
  for (const c of rows ?? []) out.set(c.asset_id, c);
  return out;
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { g_strkey } = await params;
  const short = `${g_strkey.slice(0, 8)}…${g_strkey.slice(-4)}`;
  const canonical = `${CURRENT_NETWORK.explorerUrl}/issuers/${g_strkey}`;
  const title = `Issuer ${short} — Stellar`;
  const description = `Identity, auth flags, and issued assets for Stellar issuer ${short}.`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: {
      title,
      description,
      url: canonical,
      type: 'website',
      images: [ogImageFor('issuers', g_strkey)],
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      images: [ogImageFor('issuers', g_strkey)],
    },
  };
}

export default async function IssuerDetailPage({ params }: { params: Params }) {
  const { g_strkey } = await params;
  if (g_strkey === 'shell') {
    return (
      <Suspense fallback={null}>
        <IssuerPathView />
      </Suspense>
    );
  }
  // Fan out the issuer detail + per-asset price/volume calls in
  // parallel — they share zero data, so we pay 1× round trip
  // instead of 2×.
  const [detail, coinPrices] = await Promise.all([
    fetchIssuer(g_strkey),
    fetchIssuerCoins(g_strkey),
  ]);

  if (!detail) {
    // Real build: this g_strkey was promised by generateStaticParams —
    // a missing detail row means the API contradicted its own listing.
    // Fail the build rather than bake "Issuer not found" for a real
    // issuer (fail-hard contract, src/lib/buildFetch.ts). The panel
    // below renders only on CI-stub builds.
    failBuild(
      `/issuers/${g_strkey}: promised by generateStaticParams but /v1/issuers returned no row`,
    );
    return (
      <Container className="space-y-6 py-8">
        <header className="space-y-3">
          <Breadcrumbs
            items={[
              { label: 'Home', href: '/' },
              { label: 'Issuers', href: '/issuers' },
              { label: shortKey(g_strkey) },
            ]}
          />
          <h1 className="font-mono text-2xl font-semibold tracking-tight">
            {shortKey(g_strkey)}
          </h1>
        </header>
        <Panel title="Issuer not found" bodyClassName="text-sm text-ink-body">
          <p>
            No row found for that G-strkey, or the issuer hasn&apos;t issued a
            classic asset that&apos;s appeared in any trade or ChangeTrust op
            the indexer has seen.
          </p>
        </Panel>
      </Container>
    );
  }

  // `assets` is omitempty AND soft-failed to nil server-side when the
  // per-asset fan-out errors or blows its 8s deadline
  // (internal/api/v1/issuers.go, "Soft-fail on the asset list …
  // Includes deadline exceeded"). Absent therefore means "we couldn't
  // read the asset list", not "this issuer issued nothing" — every tile
  // below renders '—' rather than a fabricated 0.
  const assets = detail.assets ?? null;
  const totalObs =
    assets?.reduce((sum, a) => sum + a.observation_count, 0) ?? null;
  // Sum per-asset 24h USD volume from the parallel /v1/coins?issuer= fetch.
  // null/missing volumes drop out cleanly; the panel renders "—" when
  // every asset row had no recent USD-priced trade.
  let totalVolume24hUSD = 0;
  let anyVolume = false;
  for (const a of assets ?? []) {
    const v = Number(coinPrices.get(a.asset_id)?.volume_24h_usd ?? '');
    if (Number.isFinite(v) && v > 0) {
      totalVolume24hUSD += v;
      anyVolume = true;
    }
  }

  // FEC A1-6: BreadcrumbList JSON-LD derives from the visible Crumb[]
  // inside Breadcrumbs below — no hand-rolled LD.
  return (
    <Container className="space-y-6 py-8">
      {detail.scam_reason && (
        <div className="border-down/40 bg-down-subtle rounded-lg border-2 px-4 py-3">
          <div className="flex items-baseline gap-2">
            <span className="bg-down rounded-sm px-2 py-0.5 text-[10px] font-bold tracking-wider text-white uppercase">
              Warning
            </span>
            <span className="text-down-strong text-sm font-medium">
              This issuer is flagged as malicious or unsafe.
            </span>
          </div>
          <p className="text-down-strong mt-1.5 text-xs">
            {detail.scam_reason}. Do not trust assets issued from this account.
            Source: stellar.expert directory.
          </p>
        </div>
      )}

      <header className="space-y-3">
        <Breadcrumbs
          items={[
            { label: 'Home', href: '/' },
            { label: 'Issuers', href: '/issuers' },
            { label: detail.org_name || shortKey(g_strkey) },
          ]}
        />
        {detail.org_name ? (
          <>
            {/* CS-100: org_name is SELF-DECLARED SEP-1 metadata. Render the
                verification state next to it so an unverified name can't pass
                as an authoritative identity. Verified = the org's stellar.toml
                lists this issuer back (bidirectional). */}
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="text-2xl font-semibold tracking-tight">
                {detail.org_name}
              </h1>
              {detail.org_verified ? (
                <span
                  title="SEP-1 verified — the organisation's stellar.toml lists this issuer back (bidirectional)"
                  className="bg-up-subtle text-up-strong rounded-sm px-1.5 py-0.5 text-[10px] font-medium tracking-wider uppercase"
                >
                  ✓ Verified
                </span>
              ) : (
                <span
                  title="Unverified — this organisation name is self-declared in the issuer's SEP-1 toml and is NOT cross-confirmed by the organisation. Do not treat it as a verified identity."
                  className="bg-surface-sunk text-ink-muted rounded-sm px-1.5 py-0.5 text-[10px] font-medium tracking-wider uppercase"
                >
                  Unverified
                </span>
              )}
            </div>
            <p className="text-ink-muted font-mono text-xs break-all">
              {g_strkey}
            </p>
          </>
        ) : (
          <h1 className="font-mono text-2xl font-semibold tracking-tight break-all">
            {g_strkey}
          </h1>
        )}
        {detail.home_domain && (
          <p className="text-ink-body text-sm">
            {/* home_domain is attacker-controlled on-chain data — only
                link it when it parses as a strict hostname, else render
                as plain text (phishing guard, WA-02). */}
            {isSafeHomeDomain(detail.home_domain) ? (
              <a
                href={`https://${detail.home_domain}`}
                target="_blank"
                rel="noreferrer noopener nofollow"
                className="hover:text-brand-600 font-mono hover:underline"
              >
                {detail.home_domain}
              </a>
            ) : (
              <span
                className="text-ink-muted font-mono"
                title="Unverified issuer-supplied domain"
              >
                {detail.home_domain}
              </span>
            )}
          </p>
        )}
      </header>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Panel
          title="Activity"
          source={asExample(`/v1/issuers/${g_strkey}`)}
          panelId="activity-card"
          className="lg:col-span-2"
        >
          <dl className="grid grid-cols-2 gap-3 text-sm sm:grid-cols-5">
            <Stat label="Assets" value={assets ? String(assets.length) : '—'} />
            <Stat
              label="24h volume"
              value={anyVolume ? `$${formatCompact(totalVolume24hUSD)}` : '—'}
            />
            <Stat
              label="Total observations"
              value={totalObs != null ? formatCompact(totalObs) : '—'}
            />
            <Stat
              label="Creation ledger"
              mono
              value={
                detail.creation_ledger != null
                  ? `#${detail.creation_ledger.toLocaleString('en-US')}`
                  : '—'
              }
            />
            <Stat
              label="SEP-1 resolved"
              value={
                detail.sep1_resolved_at
                  ? formatRelative(detail.sep1_resolved_at)
                  : '—'
              }
            />
          </dl>
        </Panel>

        <Panel title="Auth flags" panelId="auth-flags-card">
          {detail.auth_required == null &&
          detail.auth_revocable == null &&
          detail.auth_immutable == null &&
          detail.auth_clawback == null ? (
            // The account-flag reader hasn't populated this issuer yet —
            // show that honestly rather than four "unknown" dots that
            // read as a broken panel (audit 2026-06-19).
            <p className="text-ink-muted text-xs">
              Not yet resolved. Auth flags are read from the issuer&apos;s
              on-chain account entry; this one is not in the captured
              ledger-entry window yet.
            </p>
          ) : (
            <ul className="space-y-1.5 text-xs">
              <FlagRow label="auth_required" v={detail.auth_required} />
              <FlagRow label="auth_revocable" v={detail.auth_revocable} />
              <FlagRow label="auth_immutable" v={detail.auth_immutable} />
              <FlagRow label="auth_clawback" v={detail.auth_clawback} />
            </ul>
          )}
        </Panel>
      </div>

      <Panel
        title="External views"
        hint="Cross-reference this issuer on other Stellar explorers"
        bodyClassName="text-sm text-ink-body"
      >
        <ul className="space-y-2">
          <li>
            <StellarExpertLink kind="account" id={g_strkey} className="hover:text-brand-600 inline-flex items-center gap-1.5 hover:underline"            >
              stellar.expert
              <span className="text-ink-faint text-[10px] tracking-wider uppercase">
                ↗
              </span>
            </StellarExpertLink>
            <span className="text-ink-faint ml-2 text-xs">
              account history, balance, signers
            </span>
          </li>
          <li>
            <a
              href={stellarChainEntityUrl('accounts', g_strkey)}
              target="_blank"
              rel="noreferrer noopener"
              className="hover:text-brand-600 inline-flex items-center gap-1.5 hover:underline"
            >
              stellarchain.io
              <span className="text-ink-faint text-[10px] tracking-wider uppercase">
                ↗
              </span>
            </a>
            <span className="text-ink-faint ml-2 text-xs">
              ledger entries, operations log
            </span>
          </li>
          {isSafeHomeDomain(detail.home_domain) && (
            <li>
              <a
                href={`https://${detail.home_domain}/.well-known/stellar.toml`}
                target="_blank"
                rel="noreferrer noopener nofollow"
                className="hover:text-brand-600 inline-flex items-center gap-1.5 hover:underline"
              >
                stellar.toml
                <span className="text-ink-faint text-[10px] tracking-wider uppercase">
                  ↗
                </span>
              </a>
              <span className="text-ink-faint ml-2 text-xs">
                SEP-1 source on {detail.home_domain}
              </span>
            </li>
          )}
        </ul>
      </Panel>

      <Panel
        title={assets ? `Issued assets (${assets.length})` : 'Issued assets'}
        hint="All classic assets we've observed minted by this G-strkey"
        source={asExample(`/v1/issuers/${g_strkey}`)}
        bodyClassName="-mx-4"
      >
        {!assets ? (
          <p className="text-ink-muted px-4 py-3 text-sm">
            Issued-asset list unavailable — the per-asset read didn&apos;t
            return, so this is unknown rather than empty. Retry shortly.
          </p>
        ) : assets.length === 0 ? (
          <p className="text-ink-muted px-4 py-3 text-sm">
            No issued assets observed.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="divide-line min-w-full divide-y text-sm">
              <thead>
                <tr className="text-ink-muted text-left text-[11px] tracking-wider uppercase">
                  <Th>Code</Th>
                  <Th align="right">Price</Th>
                  <Th align="right">24h %</Th>
                  <Th align="right">24h volume</Th>
                  <Th align="right">Market cap</Th>
                  <Th align="right">Circulating</Th>
                  <Th align="right">Observations</Th>
                  <Th align="right">First seen</Th>
                </tr>
              </thead>
              <tbody className="divide-line-subtle divide-y">
                {assets.map((a) => {
                  const coin = coinPrices.get(a.asset_id);
                  return (
                    <tr key={a.asset_id} className="hover:bg-surface-muted">
                      <Td>
                        <Link
                          href={`/assets/${a.slug}`}
                          className="hover:text-brand-600 font-medium"
                        >
                          {a.code}
                        </Link>
                        <span className="text-ink-muted ml-2 font-mono text-[11px]">
                          {a.slug}
                        </span>
                        <Link
                          href={`/markets?asset=${encodeURIComponent(a.asset_id)}`}
                          className="text-brand-600 ml-2 text-[11px] hover:underline"
                          title={`All markets for ${a.code}`}
                        >
                          markets →
                        </Link>
                      </Td>
                      <Td align="right">
                        <PriceCell raw={coin?.price_usd} />
                      </Td>
                      <Td align="right">
                        <ChangeCell raw={coin?.change_24h_pct} />
                      </Td>
                      <Td align="right">
                        <UsdVolumeCell raw={coin?.volume_24h_usd} />
                      </Td>
                      <Td align="right">
                        <UsdVolumeCell raw={coin?.market_cap_usd} />
                      </Td>
                      <Td align="right">
                        <span className="font-mono tabular-nums">
                          {/* Scale by the asset's OWN decimals — the old
                              /1e7 hardcode misstated supply for any
                              non-7-decimals SEP-41 asset. */}
                          {coin?.circulating_supply
                            ? formatCompact(
                                Number(coin.circulating_supply) /
                                  10 ** (coin.decimals ?? 7),
                              )
                            : '—'}
                        </span>
                      </Td>
                      <Td align="right">
                        <span className="font-mono tabular-nums">
                          {formatCompact(a.observation_count)}
                        </span>
                      </Td>
                      <Td align="right">
                        <span className="font-mono text-xs">
                          #{a.first_seen_ledger.toLocaleString('en-US')}
                        </span>
                      </Td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </Container>
  );
}

function shortKey(g: string): string {
  return `${g.slice(0, 8)}…${g.slice(-4)}`;
}

function Stat({
  label,
  value,
  mono,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div>
      <dt className="text-ink-muted text-[11px] tracking-wider uppercase">
        {label}
      </dt>
      <dd className={mono ? 'font-mono text-xs' : 'tabular-nums'}>{value}</dd>
    </div>
  );
}

function FlagRow({ label, v }: { label: string; v: boolean | undefined }) {
  let tone: string;
  let text: string;
  if (v === true) {
    tone = 'bg-warn-500';
    text = 'true';
  } else if (v === false) {
    tone = 'bg-line-strong';
    text = 'false';
  } else {
    tone = 'bg-line';
    text = 'unknown';
  }
  return (
    <li className="flex items-center justify-between gap-2 font-mono">
      <span className="text-ink-body">{label}</span>
      <span className="flex items-center gap-1.5">
        <span className={`inline-block h-2 w-2 rounded-full ${tone}`} />
        <span className="text-ink-body">{text}</span>
      </span>
    </li>
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
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
      scope="col"
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
      className={`px-4 py-3 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}

function PriceCell({ raw }: { raw?: string | null }) {
  if (!raw) return <span className="text-ink-faint">—</span>;
  const n = Number(raw);
  if (!Number.isFinite(n)) return <span className="text-ink-faint">—</span>;
  // FEC audit F-A4-02: the bare toFixed(6) ladder rendered sub-1e-6 prices
  // (the scam-token class this cell exists to show) as "$0.000000" — LDEX
  // was live-wrong on this page. formatPriceSmall is the COR-14 canonical
  // with the subunit-safe plain-decimal tail.
  return (
    <span className="text-ink-body font-mono tabular-nums">
      ${formatPriceSmall(n)}
    </span>
  );
}

function ChangeCell({ raw }: { raw?: string | null }) {
  if (!raw) return <span className="text-ink-faint">—</span>;
  const n = Number(raw);
  if (!Number.isFinite(n)) return <span className="text-ink-faint">—</span>;
  const tone = n > 0 ? 'text-up' : n < 0 ? 'text-down' : 'text-ink-muted';
  const sign = n > 0 ? '+' : '';
  return (
    <span className={`font-mono tabular-nums ${tone}`}>
      {sign}
      {n.toFixed(2)}%
    </span>
  );
}

function UsdVolumeCell({ raw }: { raw?: string | null }) {
  if (!raw) return <span className="text-ink-faint">—</span>;
  const n = Number(raw);
  if (!Number.isFinite(n)) return <span className="text-ink-faint">—</span>;
  return (
    <span className="text-ink-body font-mono tabular-nums">
      ${formatCompact(n)}
    </span>
  );
}
