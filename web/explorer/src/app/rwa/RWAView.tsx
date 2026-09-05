'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGetData, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { formatCompact, formatDecimalAmount } from '@/lib/format';
import { hasDirectoryScamFlag } from '@/lib/directory-tags';
import { truncateMiddle } from '@/components/ui/Mono';
import {
  Badge,
  Callout,
  EmptyState,
  Skeleton,
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

type Schemas = components['schemas'];
type RWAAssetsView = Schemas['RWAAssetsView'];
type RWAAsset = Schemas['RWAAsset'];

const ENDPOINT = '/v1/rwa/assets';

/**
 * Why a valuation is not a number, in the words the surface uses. The
 * table renders the reason in place of the figure — never a dash on its
 * own, and never a zero, because a reader cannot tell a withheld
 * valuation from a worthless asset when both render as "$0.00".
 */
const VALUATION_REASON: Record<string, string> = {
  withheld_issuer_flagged:
    'Withheld — the issuer carries a scam-class directory flag, so no price or market cap is published for it anywhere on this site.',
  unpriced:
    'No served USD price — either the market produced none, or it is too thin to aggregate and the price is withheld.',
  withheld_low_liquidity:
    'Withheld — a price exists but its liquidity is below the floor at which a market cap would mean anything.',
  supply_unavailable:
    'A price exists but no circulating-supply reading does, so no market cap can be computed.',
};

/**
 * The membership requirements, phrased for a reader rather than a
 * consumer. The server serves its own machine-readable list; this is
 * the same rule in prose, kept beside the numbers instead of behind a
 * link, because the rule is what makes the numbers mean anything.
 */
const REQUIREMENT_PROSE = [
  'A classic asset with both a code and an issuer account. A code alone identifies nothing on Stellar — anyone can issue a token called USTRY.',
  'The issuer publishes a SEP-1 file at the domain its account names on chain, describing this exact asset under its own address.',
  'An independent directory recognises that issuing account, and has not flagged it.',
  'The asset is a real-world instrument — either the issuer declares the class, or an independent oracle publishes a net-asset-value feed for it.',
];

const REFUSAL_PROSE: Record<string, string> = {
  not_a_classic_asset: 'Not identified by a (code, issuer) pair',
  no_issuer_bound_sep1_entry: 'No SEP-1 entry the issuer published about itself',
  issuer_scam_flagged: 'Issuer flagged by the independent directory',
  issuer_not_independently_recognised: 'Issuer recognised by nobody but itself',
  no_real_world_instrument_basis: 'Declares no real-world instrument',
};

function useRWAAssets() {
  return useQuery<RWAAssetsView>({
    queryKey: [ENDPOINT],
    queryFn: () => apiGetData<RWAAssetsView>(ENDPOINT),
    staleTime: 60_000,
    placeholderData: (prev) => prev,
  });
}

/**
 * Money, from the served decimal string. `formatDecimalAmount` returns
 * null rather than a placeholder precisely so each surface decides what
 * absence looks like — here, absence is never a dash in a money cell.
 */
function usd(value: string | null | undefined): string | null {
  const f = formatDecimalAmount(value, 2);
  return f == null ? null : `$${f}`;
}

/**
 * RWAView — the /rwa data surface.
 *
 * Four states are kept distinct, the way the rest of the explorer keeps
 * them distinct: the fetch failed; the fetch succeeded and the set is
 * empty; a member has no publishable valuation; and the total itself is
 * unpublishable. None of them is allowed to render as a zero.
 */
export function RWAView() {
  const { data, isLoading, isError, error } = useRWAAssets();

  if (isLoading && !data) return <Skeleton className="h-96 w-full" />;
  if (isError || !data) {
    return (
      <Callout tone="bad" title="Failed to load real-world assets">
        {error instanceof Error ? error.message : 'The request did not complete.'}
      </Callout>
    );
  }

  const { summary, assets, by_class: byClass, by_issuer: byIssuer, refused } = data;
  const total = usd(summary.market_cap_usd);

  return (
    <div className="space-y-6">
      <HeadlineStats summary={summary} total={total} />

      <Panel
        title="The set"
        headingLevel={2}
        hint={`${assets.length} asset${assets.length === 1 ? '' : 's'} from ${summary.issuers} issuer${summary.issuers === 1 ? '' : 's'}`}
        source={asExample(ENDPOINT)}
        bodyClassName="-mx-4"
      >
        {assets.length === 0 ? (
          <EmptyState
            headingLevel={3}
            title="No asset currently meets the definition"
            description="An asset qualifies only when its issuer both declares the real-world anchor in its own SEP-1 file and is independently recognised. Nothing on this network clears both today — which is a statement about the evidence available, not about what issuers claim."
          />
        ) : (
          <div className="overflow-x-auto">
            <AssetTable assets={assets} />
          </div>
        )}
      </Panel>

      {byClass.length > 0 && (
        <div className="grid gap-6 lg:grid-cols-2">
          <Panel title="By instrument class" headingLevel={2} bodyClassName="-mx-4">
            <div className="overflow-x-auto">
              <GroupTable
                rows={byClass.map((c) => ({
                  key: c.class,
                  label: CLASS_LABEL[c.class] ?? c.class,
                  sub: c.class === 'unclassified' ? 'declared by an oracle feed, not by the issuer' : undefined,
                  assets: c.assets,
                  unvalued: c.assets_unvalued,
                  usd: usd(c.market_cap_usd),
                }))}
                firstHeading="Class"
              />
            </div>
          </Panel>
          <Panel title="By issuer" headingLevel={2} bodyClassName="-mx-4">
            <div className="overflow-x-auto">
              <GroupTable
                rows={byIssuer.map((i) => ({
                  key: i.issuer,
                  label: i.name || i.home_domain || truncateMiddle(i.issuer, 6, 6),
                  sub: i.name && i.home_domain ? i.home_domain : undefined,
                  assets: i.assets,
                  unvalued: i.assets_unvalued,
                  usd: usd(i.market_cap_usd),
                }))}
                firstHeading="Issuer"
              />
            </div>
          </Panel>
        </div>
      )}

      <DefinitionPanel definition={data.definition} refused={refused} />
    </div>
  );
}

const CLASS_LABEL: Record<string, string> = {
  bond: 'Bonds and treasuries',
  stock: 'Equities',
  commodity: 'Commodities',
  realestate: 'Real estate',
  unclassified: 'Unclassified',
};

/**
 * The headline strip. The market-cap tile follows the same three-state
 * rule the DEX TVL headline follows: a plain figure; a figure prefixed
 * "≥" when any member is unvalued; and, when nothing publishes a
 * valuation at all, the words "Not published" — never "$0.00", which
 * would read as a real total of zero dollars.
 */
function HeadlineStats({
  summary,
  total,
}: {
  summary: Schemas['RWASummary'];
  total: string | null;
}) {
  return (
    <div className="space-y-3">
      <StatGrid cols={4}>
        <StatCell>
          <Stat
            label="Market cap"
            size="lg"
            value={
              total == null ? (
                <span className="text-ink-muted">Not published</span>
              ) : (
                <>
                  {summary.lower_bound && (
                    <span className="text-ink-muted" aria-hidden>
                      ≥{' '}
                    </span>
                  )}
                  {total}
                </>
              )
            }
            sub={
              total == null
                ? 'No asset in the set publishes a valuation'
                : `${summary.assets_valued} of ${summary.assets} valued`
            }
          />
        </StatCell>
        <StatCell>
          <Stat label="Assets" value={summary.assets.toLocaleString('en-US')} sub="meeting all four requirements" />
        </StatCell>
        <StatCell>
          <Stat label="Issuers" value={summary.issuers.toLocaleString('en-US')} sub="independently recognised" />
        </StatCell>
        <StatCell>
          <Stat
            label="On chain since"
            value={
              summary.earliest_first_seen_ledger ? (
                `ledger ${summary.earliest_first_seen_ledger.toLocaleString('en-US')}`
              ) : (
                <span className="text-ink-muted">—</span>
              )
            }
            sub="first sighting, from an index complete since genesis"
          />
        </StatCell>
      </StatGrid>
      <p className="text-xs leading-relaxed text-ink-muted">
        {summary.lower_bound && (
          <>
            <strong>At least this.</strong> {summary.assets_unvalued} asset
            {summary.assets_unvalued === 1 ? '' : 's'} in the set publish no
            valuation and contribute nothing to the total.{' '}
          </>
        )}
        {summary.basis}
      </p>
    </div>
  );
}

function AssetTable({ assets }: { assets: RWAAsset[] }) {
  return (
    <Table>
      <THead>
        <TR>
          <Th>Asset</Th>
          <Th>Issuer</Th>
          <Th>Anchor</Th>
          <Th align="right">Market cap</Th>
          <Th align="right">Price</Th>
          <Th align="right">24h volume</Th>
          <Th align="right">First seen</Th>
        </TR>
      </THead>
      <TBody>
        {assets.map((a) => (
          <AssetRow key={a.asset_id} asset={a} />
        ))}
      </TBody>
    </Table>
  );
}

function AssetRow({ asset }: { asset: RWAAsset }) {
  const cap = usd(asset.valuation.market_cap_usd);
  const price = usd(asset.valuation.price_usd);
  const volume = asset.volume_24h_usd == null ? null : formatCompact(Number(asset.volume_24h_usd));
  const flagged = hasDirectoryScamFlag(asset.issuer_directory_tags);
  const reason = VALUATION_REASON[asset.valuation.status];

  return (
    <TR>
      <Td>
        <Link href={`/assets/${asset.slug || asset.asset_id}`} className="font-medium hover:text-brand-600">
          {asset.code}
        </Link>
        {asset.name && <div className="text-xs text-ink-muted">{asset.name}</div>}
      </Td>
      <Td>
        <div className="flex items-center gap-1.5">
          <span>{asset.issuer_directory_name || asset.home_domain || truncateMiddle(asset.issuer, 6, 6)}</span>
          {flagged && <Badge tone="bad">Flagged</Badge>}
        </div>
        {/* The G-address is the identity; the label above is a
            third-party convenience. Showing both keeps a reader from
            confusing the two. */}
        <div className="font-mono text-[11px] text-ink-faint">{truncateMiddle(asset.issuer, 6, 6)}</div>
      </Td>
      <Td>
        <div>{asset.anchor_asset || CLASS_LABEL[asset.anchor_class ?? ''] || '—'}</div>
        <div className="text-xs text-ink-muted">
          {asset.basis === 'sep1_anchor_declaration'
            ? `declared by the issuer${asset.anchor_class ? ` as ${asset.anchor_class}` : ''}`
            : 'priced by an independent oracle feed'}
        </div>
      </Td>
      <Td align="right">
        {cap ?? <Withheld reason={reason} />}
      </Td>
      <Td align="right">{price ?? <Withheld reason={reason} />}</Td>
      <Td align="right">
        {volume == null ? <span className="text-ink-faint" title="No USD-denominated trades in the window">—</span> : `$${volume}`}
      </Td>
      <Td align="right">
        {asset.first_seen_ledger ? asset.first_seen_ledger.toLocaleString('en-US') : (
          <span className="text-ink-faint">—</span>
        )}
      </Td>
    </TR>
  );
}

/**
 * A valuation the platform will not publish. It reads as a word, not a
 * dash and never a number: a dash in a money column beside real figures
 * is read as zero, and zero is the one reading that is certainly wrong.
 */
function Withheld({ reason }: { reason?: string }) {
  return (
    <span className="text-xs font-medium text-ink-muted" title={reason ?? 'Not available'}>
      Unavailable
    </span>
  );
}

function GroupTable({
  rows,
  firstHeading,
}: {
  rows: { key: string; label: string; sub?: string; assets: number; unvalued: number; usd: string | null }[];
  firstHeading: string;
}) {
  return (
    <Table>
      <THead>
        <TR>
          <Th>{firstHeading}</Th>
          <Th align="right">Assets</Th>
          <Th align="right">Market cap</Th>
        </TR>
      </THead>
      <TBody>
        {rows.map((r) => (
          <TR key={r.key}>
            <Td>
              <div>{r.label}</div>
              {r.sub && <div className="text-xs text-ink-muted">{r.sub}</div>}
            </Td>
            <Td align="right">
              {r.assets.toLocaleString('en-US')}
              {r.unvalued > 0 && (
                <div className="text-[11px] text-ink-muted">{r.unvalued} unvalued</div>
              )}
            </Td>
            <Td align="right">
              {r.usd == null ? (
                <Withheld reason="No asset in this group publishes a valuation." />
              ) : (
                <>
                  {r.unvalued > 0 && (
                    <span className="text-ink-muted" aria-hidden>
                      ≥{' '}
                    </span>
                  )}
                  {r.usd}
                </>
              )}
            </Td>
          </TR>
        ))}
      </TBody>
    </Table>
  );
}

/**
 * The rule, and what it turned away. Both belong on the page: a set
 * this small invites the question "is that all?", and the honest answer
 * is a count of the candidates each requirement refused.
 */
function DefinitionPanel({
  definition,
  refused,
}: {
  definition: Schemas['RWADefinition'];
  refused: Schemas['RWARefusal'][];
}) {
  const refusedTotal = refused.reduce((n, r) => n + r.assets, 0);
  return (
    <Panel title="What qualifies, and what does not" headingLevel={2}>
      <ol className="ml-4 list-decimal space-y-1.5 text-sm leading-relaxed text-ink-body">
        {REQUIREMENT_PROSE.map((r) => (
          <li key={r}>{r}</li>
        ))}
      </ol>
      <p className="mt-3 text-xs leading-relaxed text-ink-muted">
        An asset failing any requirement is absent from this page — not
        ranked lower, not hidden behind a filter. It keeps its own asset
        page, with whatever warnings apply there. Recognised classes:{' '}
        <span className="font-mono">{definition.anchor_classes.join(', ')}</span>.
        Fiat-anchored tokens are stablecoins and are counted elsewhere.
      </p>
      {refusedTotal > 0 && (
        <details className="group mt-3 rounded-lg border border-line">
          <summary className="cursor-pointer select-none px-3 py-1.5 text-xs font-medium text-ink-body marker:text-ink-faint hover:text-brand-600">
            Candidates refused{' '}
            <span className="text-ink-faint">({refusedTotal.toLocaleString('en-US')})</span>
          </summary>
          <dl className="space-y-1.5 border-t border-line px-3 py-2 text-[11px] leading-relaxed">
            {refused.map((r) => (
              <div key={r.reason} className="flex justify-between gap-4">
                <dt className="text-ink-body">{REFUSAL_PROSE[r.reason] ?? r.reason}</dt>
                <dd className="tnum text-ink-muted">{r.assets.toLocaleString('en-US')}</dd>
              </div>
            ))}
          </dl>
        </details>
      )}
      <p className="mt-3 text-[11px] leading-relaxed text-ink-muted">
        Live data from <span className="font-mono">{ENDPOINT}</span>. Valuations
        come from the same price, supply and trust gates the asset pages use, so
        nothing here publishes a figure those pages withhold. The full
        definition, with the evidence behind each requirement, is in the{' '}
        <Link href="/methodology" className="underline hover:text-brand-600">
          methodology
        </Link>
        .
      </p>
    </Panel>
  );
}
