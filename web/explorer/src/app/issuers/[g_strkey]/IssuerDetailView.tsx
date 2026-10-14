import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { CrossReference } from '@/components/CrossReference';
import {
  Badge,
  ButtonLink,
  Callout,
  Container,
  Fact,
  FactList,
  Mono,
  PageHeader,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { asExample } from '@/api/client';
import type { AssetDetail, IssuedAsset, Issuer } from '@/api/hooks';
import { assetHref } from '@/lib/fiat-slugs';
import {
  formatCompact,
  formatCompactUnits,
  formatPriceSmall,
  formatRelative,
  sumDecimalStrings,
  truncateMiddle,
} from '@/lib/format';
import { CURRENT_NETWORK } from '@/lib/networks';
import { isSafeHomeDomain } from '@/lib/safe-domain';

import { IssuerAssetMix } from './IssuerAssetMix';

/** Price, volume, supply and market cap per asset_id, from /v1/assets?issuer=. */
export type CoinPrices = Record<string, Partial<AssetDetail>>;

const ISSUER_CRUMBS = [
  { label: 'Home', href: '/' },
  { label: 'Issuers', href: '/issuers' },
];

export function issuerShortKey(g: string): string {
  return truncateMiddle(g, 8, 4);
}

/**
 * IssuerDetailView — the whole issuer page from one record: the on-chain
 * account entry, the organisation it declares in its stellar.toml, and
 * every classic asset it has minted. Pure on its props so the static
 * export and the runtime fallback render the same page.
 */
export function IssuerDetailView({
  gStrkey,
  detail,
  coins,
  pricesUnavailable = false,
}: {
  gStrkey: string;
  detail: Issuer;
  coins: CoinPrices;
  /** The market read failed, so blank price columns mean unknown, not unpriced. */
  pricesUnavailable?: boolean;
}) {
  // `assets` is nil server-side when the per-asset read errors or times
  // out, so absent means unknown, never "issued nothing".
  const assets = normaliseAssets(detail.assets);
  const pricing = CURRENT_NETWORK.pricing;
  const names = sep1CurrencyNames(detail.sep1_payload, gStrkey);
  // Lead with the assets that carry signal; the long tail of never-traded
  // codes stays one click away so the panel still lists every asset.
  const active = (assets ?? []).filter(
    (a) =>
      (a.observation_count ?? 0) > 0 ||
      coins[a.asset_id]?.price_usd != null ||
      coins[a.asset_id]?.market_cap_usd != null,
  );
  const dormant = (assets ?? []).filter((a) => !active.includes(a));
  const homeUrl = isSafeHomeDomain(detail.home_domain)
    ? `https://${detail.home_domain}`
    : null;
  const org = sep1Organisation(detail.sep1_payload);
  const totalObs =
    assets && assets.every((a) => a.observation_count != null)
      ? assets.reduce((sum, a) => sum + (a.observation_count ?? 0), 0)
      : null;
  // Exact decimal sum; a non-decimal or negative row voids the total
  // rather than shrinking it.
  const volumes = (assets ?? []).map((a) => coins[a.asset_id]?.volume_24h_usd);
  const totalVolume = volumes.some((v) => v?.trim().startsWith('-'))
    ? null
    : sumDecimalStrings(volumes);
  const anyVolume = totalVolume != null && !/^0(\.0*)?$/.test(totalVolume);
  const source = asExample(`/v1/issuers/${gStrkey}`);

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          ...ISSUER_CRUMBS,
          { label: detail.org_name || issuerShortKey(gStrkey) },
        ]}
        title={detail.org_name || 'Issuer'}
        meta={<Mono value={gStrkey} copy className="break-all" />}
        actions={
          <>
            {detail.org_name &&
              (detail.org_verified ? (
                <Badge
                  tone="ok"
                  dot
                  title="The organisation’s own stellar.toml lists this account as an issuer"
                >
                  Verified
                </Badge>
              ) : (
                <Badge
                  tone="neutral"
                  title="The organisation name is self-declared in this account’s stellar.toml and has not been confirmed by the organisation"
                >
                  Unverified
                </Badge>
              ))}
            <ButtonLink
              href={`/accounts/${encodeURIComponent(gStrkey)}`}
              variant="secondary"
              size="sm"
              title="Balances, signers, transactions and operations"
            >
              View as account
            </ButtonLink>
          </>
        }
      />

      {detail.scam_reason && (
        <Callout tone="bad" title="Flagged in the directory">
          {detail.scam_reason}. Assets issued by this account should not be
          trusted.
        </Callout>
      )}

      <Panel
        headingLevel={2}
        title="Issuer record"
        hint="The account entry and its SEP-1 status"
        source={source}
        bodyClassName="space-y-5"
      >
        <StatGrid cols={pricing ? 4 : 3} className="grid-cols-2">
          <StatCell>
            <Stat
              label="Assets issued"
              value={assets ? String(assets.length) : '—'}
            />
          </StatCell>
          <StatCell>
            <Stat
              label="Observations"
              value={totalObs != null ? formatCompact(totalObs) : '—'}
              sub="trades and trustline changes"
            />
          </StatCell>
          {pricing && (
            <StatCell>
              <Stat
                label="24h volume"
                value={anyVolume ? `$${formatCompactUnits(totalVolume)}` : '—'}
                sub="across issued assets"
              />
            </StatCell>
          )}
          <StatCell
            className={pricing ? undefined : 'col-span-2 sm:col-span-1'}
          >
            <Stat
              label="SEP-1 resolved"
              value={
                detail.sep1_resolved_at ? (
                  <span title={detail.sep1_resolved_at}>
                    {formatRelative(detail.sep1_resolved_at)}
                  </span>
                ) : (
                  '—'
                )
              }
              sub={
                detail.sep1_resolved_at
                  ? 'stellar.toml last read'
                  : 'no stellar.toml read'
              }
            />
          </StatCell>
        </StatGrid>

        <FactList columns={3}>
          <Fact label="Home domain" mono={false}>
            {homeUrl ? (
              <a
                href={homeUrl}
                target="_blank"
                rel="noreferrer noopener nofollow"
                className="text-ink hover:text-brand-600 font-mono hover:underline"
              >
                {detail.home_domain}
              </a>
            ) : detail.home_domain ? (
              <span
                className="text-ink-muted font-mono break-all"
                title="Not linked: this account-supplied domain is not a plain hostname"
              >
                {detail.home_domain}
              </span>
            ) : (
              <span className="text-ink-faint">—</span>
            )}
          </Fact>
          <Fact label="stellar.toml" mono={false}>
            {homeUrl ? (
              <a
                href={`${homeUrl}/.well-known/stellar.toml`}
                target="_blank"
                rel="noreferrer noopener nofollow"
                className="text-ink hover:text-brand-600 hover:underline"
              >
                View the file
              </a>
            ) : (
              <span className="text-ink-faint">—</span>
            )}
          </Fact>
          <Fact label="Created">
            {detail.creation_ledger != null ? (
              `ledger #${detail.creation_ledger}`
            ) : (
              <span className="text-ink-faint">—</span>
            )}
          </Fact>
          <Fact
            label="Account flags"
            mono={false}
            className="sm:col-span-2 lg:col-span-3"
          >
            <AuthFlags detail={detail} />
          </Fact>
        </FactList>
      </Panel>

      {(org || detail.org_name) && (
        <Panel
          headingLevel={2}
          title="Organisation"
          hint={
            detail.org_verified
              ? 'From the issuer’s stellar.toml, which lists this account back'
              : 'Self-declared in the issuer’s stellar.toml; not confirmed by the organisation'
          }
          source={source}
        >
          <FactList columns={3}>
            <Fact label="Name" mono={false}>
              {org?.name ?? detail.org_name}
              {org?.dba && org.dba !== (org.name ?? detail.org_name) && (
                <span className="text-ink-muted"> · {org.dba}</span>
              )}
            </Fact>
            {org?.url && (
              <Fact label="Website" mono={false}>
                <ExternalRef url={safeHttpsUrl(org.url)} text={org.url} />
              </Fact>
            )}
            {org?.github && (
              <Fact label="GitHub" mono={false}>
                <ExternalRef
                  url={
                    GITHUB_HANDLE_RE.test(org.github)
                      ? `https://github.com/${org.github}`
                      : null
                  }
                  text={org.github}
                />
              </Fact>
            )}
            {org?.twitter && (
              <Fact label="X" mono={false}>
                <ExternalRef
                  url={
                    xHandle(org.twitter)
                      ? `https://x.com/${xHandle(org.twitter)}`
                      : null
                  }
                  text={xHandle(org.twitter) ?? org.twitter}
                />
              </Fact>
            )}
            {org?.email && (
              <Fact label="Email" mono={false}>
                <span className="break-all">{org.email}</span>
              </Fact>
            )}
            {org?.address && (
              <Fact label="Address" mono={false}>
                {org.address}
              </Fact>
            )}
          </FactList>
          {org?.description && (
            <p className="text-ink-body mt-5 max-w-prose text-sm">
              {org.description}
            </p>
          )}
        </Panel>
      )}

      <Panel
        headingLevel={2}
        title={assets ? `Issued assets (${assets.length})` : 'Issued assets'}
        hint="Every classic asset observed minted by this account"
        source={source}
        bodyClassName="-mx-4 -mb-4"
      >
        {!assets ? (
          <p className="text-ink-muted px-4 pb-4 text-sm">
            Issued-asset list unavailable — the per-asset read did not return,
            so this is unknown rather than empty. Retry shortly.
          </p>
        ) : assets.length === 0 ? (
          <p className="text-ink-muted px-4 pb-4 text-sm">
            No issued assets observed.
          </p>
        ) : (
          <>
            {pricing && pricesUnavailable && (
              <p className="text-ink-muted px-4 pb-3 text-sm">
                Prices unavailable — the market read did not return, so the
                price, volume and market-cap columns are blank rather than zero.
                Retry shortly.
              </p>
            )}
            {pricing && !pricesUnavailable && (
              <IssuerAssetMix
                assets={assets.map((a) => ({
                  id: a.asset_id,
                  code: a.code,
                  href: assetHref(a.slug),
                  marketCapUsd: coins[a.asset_id]?.market_cap_usd,
                }))}
              />
            )}
            <IssuedAssetsTable
              assets={active.length > 0 ? active : dormant}
              coins={coins}
              names={names}
              pricing={!!pricing}
            />
            {active.length > 0 && dormant.length > 0 && (
              <details className="border-line border-t">
                <summary className="text-ink-muted hover:text-ink cursor-pointer px-4 py-3 text-sm">
                  {dormant.length} more with no trade or trustline change
                  observed
                </summary>
                <IssuedAssetsTable
                  assets={dormant}
                  coins={coins}
                  names={names}
                  pricing={!!pricing}
                />
              </details>
            )}
          </>
        )}
      </Panel>

      <CrossReference kind="account" id={gStrkey} />
    </Container>
  );
}

const AUTH_FLAGS = [
  {
    key: 'auth_required',
    label: 'Auth required',
    title: 'Holders need the issuer’s approval before they can hold the asset',
  },
  {
    key: 'auth_revocable',
    label: 'Revocable',
    title: 'The issuer can freeze any holder’s balance',
  },
  {
    key: 'auth_immutable',
    label: 'Immutable',
    title: 'The issuer can never change these flags again',
  },
  {
    key: 'auth_clawback',
    label: 'Clawback',
    title: 'The issuer can pull the asset back out of any holder’s account',
  },
] as const;

function AuthFlags({ detail }: { detail: Issuer }) {
  if (AUTH_FLAGS.every((f) => detail[f.key] == null)) {
    return (
      <Badge
        tone="neutral"
        title="Flags are read from the issuer’s account entry, which is not in the captured ledger-entry window yet"
      >
        Not read yet
      </Badge>
    );
  }
  const set = AUTH_FLAGS.filter((f) => detail[f.key]);
  const unset = AUTH_FLAGS.filter((f) => !detail[f.key]);
  return (
    <>
      <div className="flex flex-wrap items-center gap-1.5">
        {set.length === 0 && (
          <Badge
            tone="neutral"
            title="No authorization flag is set: anyone can hold the asset and the issuer cannot freeze or claw it back"
          >
            None set
          </Badge>
        )}
        {set.map((f) => (
          <Badge key={f.key} tone="warn" dot title={f.title}>
            {f.label}
          </Badge>
        ))}
        {set.length > 0 && unset.length > 0 && (
          <span className="text-ink-faint text-xs">
            Unset: {unset.map((f) => f.label.toLowerCase()).join(', ')}
          </span>
        )}
      </div>
      {detail.auth_flags_source === 'last_known_before_removal' && (
        <p className="text-ink-muted mt-2 text-xs">
          Last known
          {detail.auth_flags_as_of_ledger != null &&
            ` at ledger #${detail.auth_flags_as_of_ledger}`}{' '}
          before the account was removed. It no longer exists on the ledger, so
          these are historical values, not current policy.
        </p>
      )}
    </>
  );
}

function IssuedAssetsTable({
  assets,
  coins,
  names,
  pricing,
}: {
  assets: AssetRow[];
  coins: CoinPrices;
  names: Map<string, string>;
  pricing: boolean;
}) {
  return (
    <div className="overflow-x-auto">
      <Table>
        <THead>
          <tr>
            <Th>Asset</Th>
            {pricing && <Th align="right">Price</Th>}
            {pricing && (
              <Th align="right" className="hidden xl:table-cell">
                24h
              </Th>
            )}
            {pricing && <Th align="right">24h volume</Th>}
            {pricing && <Th align="right">Market cap</Th>}
            <Th align="right" className="hidden xl:table-cell">
              Circulating
            </Th>
            <Th align="right">Observations</Th>
            <Th align="right" className="hidden 2xl:table-cell">
              First seen
            </Th>
            <Th align="right">Last seen</Th>
          </tr>
        </THead>
        <TBody>
          {assets.map((a) => {
            const coin = coins[a.asset_id];
            const name = names.get(a.code);
            return (
              <TR key={a.asset_id}>
                <Td>
                  <div className="flex flex-wrap items-baseline gap-x-2">
                    <Link
                      href={assetHref(a.slug)}
                      className="text-ink hover:text-brand-600 font-medium"
                    >
                      {a.code}
                    </Link>
                    {name && name !== a.code && (
                      <span className="text-ink-muted max-w-48 truncate text-xs">
                        {name}
                      </span>
                    )}
                  </div>
                </Td>
                {pricing && (
                  <Td align="right">
                    <PriceCell raw={coin?.price_usd} />
                  </Td>
                )}
                {pricing && (
                  <Td align="right" className="hidden xl:table-cell">
                    <ChangeCell raw={coin?.change_24h_pct} />
                  </Td>
                )}
                {pricing && (
                  <Td align="right">
                    <UsdCell raw={coin?.volume_24h_usd} />
                  </Td>
                )}
                {pricing && (
                  <Td align="right">
                    <UsdCell raw={coin?.market_cap_usd} />
                  </Td>
                )}
                <Td align="right" className="hidden font-mono xl:table-cell">
                  {coin?.circulating_supply ? (
                    formatCompactUnits(
                      coin.circulating_supply,
                      coin.decimals ?? 7,
                    )
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
                <Td align="right" className="font-mono">
                  {a.observation_count != null ? (
                    formatCompact(a.observation_count)
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </Td>
                <Td
                  align="right"
                  className="text-ink-muted hidden font-mono text-xs 2xl:table-cell"
                >
                  <LedgerRef seq={a.first_seen_ledger} />
                </Td>
                <Td align="right" className="text-ink-muted font-mono text-xs">
                  <LedgerRef seq={a.last_seen_ledger} />
                </Td>
              </TR>
            );
          })}
        </TBody>
      </Table>
    </div>
  );
}

/** An issued-asset row with the identity fields the API may omit filled in. */
interface AssetRow {
  asset_id: string;
  code: string;
  slug: string;
  first_seen_ledger?: number;
  last_seen_ledger?: number;
  observation_count?: number;
}

function normaliseAssets(
  list: IssuedAsset[] | null | undefined,
): AssetRow[] | null {
  if (!list) return null;
  const rows = list.flatMap((a) => {
    const asset_id = a.asset_id ?? a.code;
    if (!asset_id) return [];
    return [
      {
        asset_id,
        code: a.code ?? asset_id,
        slug: a.slug ?? asset_id,
        first_seen_ledger: a.first_seen_ledger,
        last_seen_ledger: a.last_seen_ledger,
        observation_count: a.observation_count,
      },
    ];
  });
  return rows.sort(
    (x, y) =>
      (y.observation_count ?? 0) - (x.observation_count ?? 0) ||
      x.code.localeCompare(y.code),
  );
}

function LedgerRef({ seq }: { seq?: number }) {
  if (seq == null) return <span className="text-ink-faint">—</span>;
  return <>#{seq}</>;
}

function PriceCell({ raw }: { raw?: string | null }) {
  const price = raw ? formatPriceSmall(raw) : '—';
  if (price === '—') return <span className="text-ink-faint">—</span>;
  return <span className="font-mono">${price}</span>;
}

function ChangeCell({ raw }: { raw?: string | null }) {
  const n = raw ? Number(raw) : NaN;
  if (!Number.isFinite(n)) return <span className="text-ink-faint">—</span>;
  const tone = n > 0 ? 'text-up' : n < 0 ? 'text-down' : 'text-ink-muted';
  return (
    <span className={`font-mono ${tone}`}>
      {n > 0 ? '+' : ''}
      {n.toFixed(2)}%
    </span>
  );
}

function UsdCell({ raw }: { raw?: string | null }) {
  const v = formatCompactUnits(raw, 0);
  if (v === '—') return <span className="text-ink-faint">—</span>;
  return <span className="font-mono">${v}</span>;
}

/** The organisation block of a SEP-1 stellar.toml, strings only. */
export interface Sep1Organisation {
  name?: string;
  dba?: string;
  url?: string;
  github?: string;
  twitter?: string;
  email?: string;
  address?: string;
  description?: string;
}

const MAX_DESCRIPTION = 800;
const GITHUB_HANDLE_RE = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$/;
const X_HANDLE_RE = /^@?([A-Za-z0-9_]{1,15})$/;
const X_URL_RE =
  /^https?:\/\/(?:www\.)?(?:twitter|x)\.com\/@?([A-Za-z0-9_]{1,15})\/?$/i;

/** The bare handle from a SEP-1 ORG_TWITTER value, which tomls write as a handle or a profile URL. */
function xHandle(raw: string): string | null {
  const m = X_HANDLE_RE.exec(raw) ?? X_URL_RE.exec(raw);
  return m ? m[1] : null;
}

function str(v: unknown, max = 300): string | undefined {
  if (typeof v !== 'string') return undefined;
  const t = v.trim();
  if (!t) return undefined;
  if (t.length <= max) return t;
  const cut = t.lastIndexOf(' ', max - 1);
  return `${t.slice(0, cut > max / 2 ? cut : max - 1)}…`;
}

/** Reads the issuer-authored Documentation block; null when it declares nothing. */
export function sep1Organisation(payload: unknown): Sep1Organisation | null {
  if (!payload || typeof payload !== 'object') return null;
  const doc = (payload as { Documentation?: unknown }).Documentation;
  if (!doc || typeof doc !== 'object') return null;
  const d = doc as Record<string, unknown>;
  const org: Sep1Organisation = {
    name: str(d.ORG_NAME),
    dba: str(d.ORG_DBA),
    url: str(d.ORG_URL),
    github: str(d.ORG_GITHUB),
    twitter: str(d.ORG_TWITTER),
    email: str(d.ORG_OFFICIAL_EMAIL),
    address: str(d.ORG_PHYSICAL_ADDRESS),
    description: str(d.ORG_DESCRIPTION, MAX_DESCRIPTION),
  };
  return Object.values(org).some(Boolean) ? org : null;
}

/** Currency names the toml declares for this issuer, by asset code. */
export function sep1CurrencyNames(
  payload: unknown,
  issuer: string,
): Map<string, string> {
  const out = new Map<string, string>();
  if (!payload || typeof payload !== 'object') return out;
  const list = (payload as { Currencies?: unknown }).Currencies;
  if (!Array.isArray(list)) return out;
  for (const c of list) {
    if (!c || typeof c !== 'object') continue;
    const row = c as Record<string, unknown>;
    const code = str(row.Code);
    const name = str(row.Name, 120);
    const rowIssuer = str(row.Issuer);
    if (code && name && (!rowIssuer || rowIssuer === issuer)) {
      out.set(code, name);
    }
  }
  return out;
}

/**
 * An issuer-authored URL is linked only as plain https to a public
 * hostname; everything else renders as text.
 */
export function safeHttpsUrl(raw: string): string | null {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return null;
  }
  if (u.protocol !== 'https:' || u.username || u.password) return null;
  if (!isSafeHomeDomain(u.hostname)) return null;
  return u.href;
}

function ExternalRef({ url, text }: { url: string | null; text: string }) {
  if (!url) {
    return (
      <span
        className="text-ink-muted break-all"
        title="Not linked: not a plain https address"
      >
        {text}
      </span>
    );
  }
  return (
    <a
      href={url}
      target="_blank"
      rel="noreferrer noopener nofollow"
      className="text-ink hover:text-brand-600 break-all hover:underline"
    >
      {text}
    </a>
  );
}
