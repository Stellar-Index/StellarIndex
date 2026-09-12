'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { RWAHistoryPanel } from './RWAHistoryPanel';
import { apiGetData, asExample } from '@/api/client';
import type { components } from '@/api/types';
import {
  formatCompact,
  formatDecimalAmount,
  formatOraclePrice,
  formatRelative,
} from '@/lib/format';
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
 * Why a premium or discount is not a number. Same rule as the valuation
 * column above it: the cell renders the reason, never a dash on its own
 * and never a zero — "0.00%" reads as "trades at par", which is a
 * finding, and a comparison that was never made is not that finding.
 */
const PREMIUM_REASON: Record<string, string> = {
  withheld_issuer_flagged:
    'Withheld — the issuer carries a scam-class directory flag. No valuation is published for it, including an independent one: handing an impersonator the real instrument’s value would be a larger claim than the one the flag suppressed.',
  reference_not_bound:
    'This exact (code, issuer) is not one of the pairs bound to an oracle feed. A code alone is not an identity — anyone can issue a token under an instrument’s ticker — so no valuation of that instrument is attached to it.',
  no_reference_feed:
    'This token is bound to an oracle feed, but no oracle is currently publishing that instrument.',
  reference_unavailable:
    'The oracle reading could not be fetched, so nothing is known either way. This is an outage, not a statement that no valuation exists.',
  reference_expired:
    'The bound feed’s most recent reading is older than a week, so it is no longer treated as current.',
  reference_not_instrument_scoped:
    'An oracle prices an instrument of this name, but it prices an off-chain quantity in its own unit — a troy ounce of spot metal, one fund share — not one token. Their ratio would be a unit conversion, not a premium.',
  reference_not_usd_denominated:
    'The oracle’s value for this instrument is denominated in the reserve asset it is a claim on, not in dollars, so it is a ratio rather than a price. It is not converted here.',
  no_market_price:
    'An independent valuation exists, but no Stellar market price does, so there is nothing to compare it against.',
  market_price_not_observed:
    'The price on this row is a declared peg or a derived rate rather than a market observation. Measuring a premium against it would report the issuer’s own claim as a market finding.',
  reference_not_positive:
    'The oracle published a non-positive value, which cannot be a denominator.',
};

/**
 * Why there is no reference-priced valuation.
 *
 * The shared refusals are taken VERBATIM from PREMIUM_REASON rather
 * than reworded, because the server assigns one string to both fields
 * and two paraphrases of it on one page would read as two findings.
 * Only the two reasons that belong to the valuation alone are added.
 */
const REFERENCE_VALUE_REASON: Record<string, string> = {
  ...PREMIUM_REASON,
  reference_contract_not_bound:
    'This token is issued by a contract, and nothing binds a contract address to an oracle feed. The only available join is the symbol the contract declares about itself, and pricing a token by a self-declared ticker is exactly what this page refuses to do with a code.',
  supply_unavailable:
    'An independent valuation of the instrument exists, but no circulating-supply reading does, so there is no float to value.',
  reference_not_positive:
    'The oracle published a non-positive value. Multiplying a supply by it would produce a figure the oracle never claimed.',
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
  no_issuer_bound_sep1_entry:
    'No SEP-1 entry the issuer published about itself',
  issuer_scam_flagged: 'Issuer flagged by the independent directory',
  issuer_not_independently_recognised: 'Issuer recognised by nobody but itself',
  no_real_world_instrument_basis: 'Declares no real-world instrument',
};

/**
 * The funnel vocabulary in prose. A set this small invites "is that
 * all?", and the refusal tally alone cannot answer it: it counts only
 * the candidates that reached the definition, while most of the
 * population never gets that far. These are the stages that remove it.
 */
const FUNNEL_STAGE_PROSE: Record<string, string> = {
  issuers_with_home_domain: 'Issuers publishing a domain',
  issuers_with_sep1_attestation: 'Whose stellar.toml has been fetched',
  issuers_declaring_currencies: 'Declaring at least one asset',
  sep1_currency_entries: 'Asset declarations published',
  issuer_bound_entries: 'Declarations about the issuer’s own assets',
  candidate_assets_evaluated: 'Put to the four requirements',
  assets_admitted: 'Admitted',
  assets_served: 'Served above',
  curated_directory_entries: 'Addresses in the independent directory',
  directory_contract_addresses: 'Of those, token contracts',
  directory_recognised_contracts: 'Named as issuing or custodying value',
  contract_candidates_evaluated: 'Put to the four requirements',
  contract_assets_admitted: 'Admitted',
  contract_assets_served: 'Served above',
  directory_recognised_issuing_accounts:
    'Recognised issuers we hold no token for',
  assets_served_all_arms: 'Assets served above, both kinds',
  assets_reference_valued: 'Whose backing an independent oracle prices',
};

const FUNNEL_DROP_PROSE: Record<string, string> = {
  sep1_attestation_never_fetched: 'stellar.toml not fetched yet',
  // The one above counts issuers nothing has TRIED to fetch; this one
  // counts domains that were reached and served nothing usable. They
  // read alike as bare counts and are opposite findings, so the labels
  // have to keep them apart on the page too.
  domain_served_no_sep1_attestation: 'Domain served no stellar.toml',
  sep1_payload_unreadable: 'Fetched file would not parse',
  sep1_declares_no_currencies: 'Declares no assets',
  entry_declares_no_asset_code: 'Names no asset code',
  entry_declares_no_issuer: 'Names no issuer',
  entry_declares_another_issuer: 'Declares somebody else’s asset',
  duplicate_declaration_of_the_same_asset: 'Same asset declared twice',
  over_issuer_cap: 'Beyond the per-rebuild issuer cap',
  admitted_but_never_observed_on_chain: 'Never observed on chain',
  issuer_asset_page_truncated: 'Issuers whose asset list was not read in full',
  directory_entry_names_an_account:
    'Names an entity, not a token contract we can value',
  contract_scam_flagged: 'Contract flagged by the independent directory',
  contract_named_without_issuing_tag:
    'Named, but as infrastructure rather than an issuer',
  no_real_world_instrument_basis_for_contract:
    'Declares no real-world instrument',
  duplicate_directory_entry_for_contract: 'Same contract named twice',
  over_contract_scan_cap: 'Beyond the per-rebuild contract cap',
  // The valuation arm's drops are the statuses the rows carry, so the
  // labels here are the short form of the same reasons the table cells
  // spell out in full.
  withheld_issuer_flagged: 'Issuer flagged, so no valuation of any kind',
  reference_unavailable: 'The oracle read did not answer',
  reference_contract_not_bound: 'No oracle feed bound to this contract',
  reference_not_instrument_scoped: 'The feed prices an ounce, not a token',
  reference_not_bound: 'No oracle feed bound to this (code, issuer)',
  reference_not_usd_denominated: 'The feed is quoted in a reserve asset',
  no_reference_feed: 'Bound, but no oracle is publishing it',
  reference_expired: 'The feed has been silent for over a week',
  reference_not_positive: 'The oracle published a non-positive value',
  supply_unavailable: 'No circulating-supply reading',
  ...REFUSAL_PROSE,
};

/**
 * The two arms, and why there are two. An asset qualifies under one or
 * the other and never both, and the populations they narrow do not
 * overlap — so the counts reconcile within an arm and nowhere across
 * the boundary between them.
 */
const ARM_PROSE: Record<string, { title: string; lede: string }> = {
  classic: {
    title: 'Assets with an issuer account',
    lede: 'Every issuer that could publish a SEP-1 file, narrowed to the assets served.',
  },
  contract: {
    title: 'Tokens issued by a contract',
    lede: 'A separate population. A contract token has no issuer account for a SEP-1 file to describe, so the independent directory naming the exact contract takes that requirement’s place — and it is a harder thing to forge, not an easier one.',
  },
  valuation: {
    title: 'Whose backing is independently priced',
    lede: 'Not a membership narrowing — every asset here is already in the set. It continues past the served rows to say which of them an independent oracle prices the backing of, and why each of the others is not priced. Nothing in it admits or refuses an asset.',
  },
};

/** Who can move a number, in the page's voice. */
const FUNNEL_ACTOR_PROSE: Record<string, string> = {
  operator: 'ours to fix',
  issuer: 'the issuer’s to fix',
  definition: 'the rule working',
};

const UNIT_PROSE: Record<string, string> = {
  issuer_accounts: 'issuers',
  sep1_currency_declarations: 'declarations',
  assets: 'assets',
  directory_addresses: 'addresses',
  contracts: 'contracts',
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
        {error instanceof Error
          ? error.message
          : 'The request did not complete.'}
      </Callout>
    );
  }

  const {
    summary,
    assets,
    by_class: byClass,
    by_issuer: byIssuer,
    refused,
  } = data;
  const total = usd(summary.market_cap_usd);
  const referenceTotal = usd(summary.reference_valuation?.value_usd);

  return (
    <div className="space-y-6">
      <HeadlineStats
        summary={summary}
        total={total}
        referenceTotal={referenceTotal}
      />

      {/* Everything else on this page is a snapshot. The set's whole
          claim is about real-world value on chain, and "is it growing"
          is the question a snapshot cannot answer. */}
      <RWAHistoryPanel />

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
          <Panel
            title="By instrument class"
            headingLevel={2}
            bodyClassName="-mx-4"
          >
            <div className="overflow-x-auto">
              <GroupTable
                rows={byClass.map((c) => ({
                  key: c.class,
                  label: CLASS_LABEL[c.class] ?? c.class,
                  sub:
                    c.class === 'unclassified'
                      ? 'declared by an oracle feed, not by the issuer'
                      : undefined,
                  assets: c.assets,
                  unvalued: c.assets_unvalued,
                  usd: usd(c.market_cap_usd),
                  referenceUsd: usd(c.reference_value_usd),
                  referenceUnvalued: c.assets_reference_unvalued,
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
                  label:
                    i.name || i.home_domain || truncateMiddle(i.issuer, 6, 6),
                  sub: i.name && i.home_domain ? i.home_domain : undefined,
                  assets: i.assets,
                  unvalued: i.assets_unvalued,
                  usd: usd(i.market_cap_usd),
                  referenceUsd: usd(i.reference_value_usd),
                  referenceUnvalued: i.assets_reference_unvalued,
                }))}
                firstHeading="Issuer"
              />
            </div>
          </Panel>
        </div>
      )}

      <DefinitionPanel definition={data.definition} refused={refused} />
      <CoveragePanel funnel={data.funnel} />
      <UnreachedPanel entities={data.unreached_entities} />
    </div>
  );
}

/**
 * Where the population went. The set above is small; this is the only
 * place on the page that says how small a share of what it was drawn
 * from that is, and which stage removed the rest.
 *
 * Every drop names who can move it, because "nobody has fetched that
 * issuer's file yet" and "the rule refused an impersonator" are
 * opposite findings that a bare count renders identically.
 */
function CoveragePanel({ funnel }: { funnel?: Schemas['RWAFunnel'] }) {
  if (!funnel || funnel.stages.length === 0) return null;
  // Grouped by arm, in served order. The two arms narrow different
  // populations from different roots, so running them together as one
  // list would invite subtracting the last stage of one from the first
  // stage of the next — a comparison that relates nothing.
  const arms: { arm: string; stages: Schemas['RWAFunnelStage'][] }[] = [];
  for (const s of funnel.stages) {
    const tail = arms[arms.length - 1];
    if (tail && tail.arm === s.arm) tail.stages.push(s);
    else arms.push({ arm: s.arm, stages: [s] });
  }
  return (
    <Panel title="Where the population went" headingLevel={2}>
      <p className="text-ink-muted text-xs leading-relaxed">
        Each row is a stage; the indented lines are what it turned away, and who
        can change that.
        {!funnel.balanced &&
          ' Part of this accounting could not be measured, so the stages below do not reconcile.'}
      </p>
      {arms.map(({ arm, stages }) => (
        <section key={arm} className="mt-4">
          <h3 className="text-ink-body text-xs font-medium">
            {ARM_PROSE[arm]?.title ?? arm}
          </h3>
          <p className="text-ink-faint mt-0.5 text-[11px] leading-relaxed">
            {ARM_PROSE[arm]?.lede ?? ''}
          </p>
          <ol className="border-line mt-2 space-y-2 border-t pt-3 text-xs leading-relaxed">
            {stages.map((s) => (
              <li key={`${arm}-${s.stage}`}>
                <div className="flex justify-between gap-4">
                  <span className="text-ink-body">
                    {FUNNEL_STAGE_PROSE[s.stage] ?? s.stage}
                  </span>
                  <span className="tnum text-ink-body font-medium">
                    {s.count.toLocaleString('en-US')}{' '}
                    <span className="text-ink-faint font-normal">
                      {UNIT_PROSE[s.unit] ?? s.unit}
                    </span>
                  </span>
                </div>
                {(s.dropped ?? []).length > 0 && (
                  <dl className="mt-1 ml-4 space-y-0.5 text-[11px]">
                    {(s.dropped ?? []).map((d) => (
                      <div
                        key={d.reason}
                        className="flex justify-between gap-4"
                      >
                        <dt className="text-ink-muted">
                          {FUNNEL_DROP_PROSE[d.reason] ?? d.reason}{' '}
                          <span className="text-ink-faint">
                            &mdash; {FUNNEL_ACTOR_PROSE[d.actor] ?? d.actor}
                          </span>
                        </dt>
                        <dd className="tnum text-ink-faint">
                          &minus;{d.count.toLocaleString('en-US')}
                        </dd>
                      </div>
                    ))}
                  </dl>
                )}
              </li>
            ))}
          </ol>
        </section>
      ))}
    </Panel>
  );
}

/**
 * Entities the independent directory recognises, that carry no warning
 * flag, and for which this index holds no Stellar token at all.
 *
 * They are not refused by anything. There is nothing to refuse — no
 * token of theirs was ever collected, so none was ever evaluated. The
 * panel exists so a reader can tell a real issuer this site cannot see
 * from one that does not exist, which a table of the assets we DO hold
 * can never say.
 */
function UnreachedPanel({
  entities,
}: {
  entities?: Schemas['RWAUnreachedEntity'][];
}) {
  if (!entities || entities.length === 0) return null;
  return (
    <Panel title="Recognised issuers we hold no token for" headingLevel={2}>
      <p className="text-ink-muted text-xs leading-relaxed">
        An independent directory names each of these as an issuing or custodying
        entity, and none carries a warning flag. This index holds no Stellar
        asset for any of them — so they are absent from the set above, not
        refused by it. Where such an entity issues through a contract, the
        directory naming that contract is what would bring it in.
      </p>
      <ul className="border-line mt-3 space-y-1.5 border-t pt-3 text-xs">
        {entities.map((e) => (
          <li key={e.address} className="flex justify-between gap-4">
            <span className="text-ink-body">{e.name || e.address}</span>
            <span className="text-ink-faint">
              {e.domain || truncateMiddle(e.address, 10, 6)}
            </span>
          </li>
        ))}
      </ul>
    </Panel>
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
  referenceTotal,
}: {
  summary: Schemas['RWASummary'];
  total: string | null;
  referenceTotal: string | null;
}) {
  const reference = summary.reference_valuation;
  const both = summary.both_bases;
  return (
    <div className="space-y-3">
      <StatGrid cols={5}>
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
                ? 'No asset in the set publishes a market valuation'
                : `${summary.assets_valued} of ${summary.assets} valued at traded prices`
            }
          />
        </StatCell>
        {/* Deliberately NOT labelled as a market cap, and deliberately
            beside one: the reader who needs the difference is the one
            who would otherwise read the market figure as the size of
            the sector. The label says whose claim it is; the caption
            says nobody was seen paying it. */}
        <StatCell>
          <Stat
            label="Value of backing (reference)"
            size="lg"
            value={
              referenceTotal == null ? (
                <span className="text-ink-muted">Not published</span>
              ) : (
                <>
                  {reference?.lower_bound && (
                    <span className="text-ink-muted" aria-hidden>
                      ≥{' '}
                    </span>
                  )}
                  {referenceTotal}
                </>
              )
            }
            sub={
              referenceTotal == null
                ? 'No member carries an independent valuation of its instrument'
                : `${reference?.assets_valued ?? 0} of ${summary.assets} priced by an oracle, not by a market`
            }
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Assets"
            value={summary.assets.toLocaleString('en-US')}
            sub="meeting all four requirements"
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Issuers"
            value={summary.issuers.toLocaleString('en-US')}
            sub="independently recognised"
          />
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
      <p className="text-ink-muted text-xs leading-relaxed">
        <strong>Two different kinds of number.</strong> The market cap is what
        buyers were observed paying, under the same price, liquidity and trust
        gates the asset pages apply. The value of the backing is what an
        independent oracle says the underlying instrument is worth, multiplied
        by the tokens in circulation — nobody was seen paying it, and no gate
        here can check it. A tokenized treasury is bought and held, so most of
        this set has no market price at all and the two figures cover different
        assets. They are never added together.{' '}
        {both != null && both.assets > 0 && (
          <>
            <strong>Where both exist.</strong> {both.assets} of {summary.assets}{' '}
            carry both figures: {usd(both.market_cap_usd)} at traded prices
            against {usd(both.reference_value_usd)} at the reference. That gap,
            per asset, is the premium or discount column below.{' '}
          </>
        )}
        {summary.lower_bound && (
          <>
            <strong>At least this.</strong> {summary.assets_unvalued} asset
            {summary.assets_unvalued === 1 ? '' : 's'} in the set publish no
            market valuation and contribute nothing to the market-cap
            total.{' '}
          </>
        )}
        {summary.assets_with_reference > 0 && (
          <>
            <strong>Compared against the instrument.</strong>{' '}
            {summary.assets_with_reference} of {summary.assets} carry an
            independent oracle valuation of the instrument they anchor to, and{' '}
            {summary.assets_compared} of those also have a Stellar market price
            to measure it against. The rest state which requirement stopped the
            comparison rather than showing a zero.{' '}
          </>
        )}
        {reference?.sources != null && reference.sources.length > 0 && (
          <>
            <strong>Where the reference comes from.</strong>{' '}
            {reference.sources.join(', ')} — each row names its own feed and the
            moment that feed published.{' '}
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
          <Th align="right">Value of backing</Th>
          <Th align="right">Price</Th>
          <Th align="right">Instrument value</Th>
          <Th align="right">vs instrument</Th>
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
  const volume =
    asset.volume_24h_usd == null
      ? null
      : formatCompact(Number(asset.volume_24h_usd));
  const flagged = hasDirectoryScamFlag(asset.issuer_directory_tags);
  const reason = VALUATION_REASON[asset.valuation.status];

  return (
    <TR>
      <Td>
        <Link
          href={`/assets/${asset.slug || asset.asset_id}`}
          className="hover:text-brand-600 font-medium"
        >
          {asset.code}
        </Link>
        {asset.name && (
          <div className="text-ink-muted text-xs">{asset.name}</div>
        )}
      </Td>
      <Td>
        <div className="flex items-center gap-1.5">
          <span>
            {asset.issuer_directory_name ||
              asset.home_domain ||
              truncateMiddle(asset.issuer, 6, 6)}
          </span>
          {flagged && <Badge tone="bad">Flagged</Badge>}
        </div>
        {/* The G-address is the identity; the label above is a
            third-party convenience. Showing both keeps a reader from
            confusing the two. */}
        <div className="text-ink-faint font-mono text-[11px]">
          {truncateMiddle(asset.issuer, 6, 6)}
        </div>
      </Td>
      <Td>
        <div>
          {asset.anchor_asset || CLASS_LABEL[asset.anchor_class ?? ''] || '—'}
        </div>
        <div className="text-ink-muted text-xs">
          {asset.basis === 'sep1_anchor_declaration'
            ? `declared by the issuer${asset.anchor_class ? ` as ${asset.anchor_class}` : ''}`
            : 'priced by an independent oracle feed'}
        </div>
      </Td>
      <Td align="right">{cap ?? <Withheld reason={reason} />}</Td>
      <Td align="right">
        <ReferenceValueCell asset={asset} />
      </Td>
      <Td align="right">{price ?? <Withheld reason={reason} />}</Td>
      <Td align="right">
        <ReferenceCell asset={asset} />
      </Td>
      <Td align="right">
        <PremiumCell premium={asset.premium} />
      </Td>
      <Td align="right">
        {volume == null ? (
          <span
            className="text-ink-faint"
            title="No USD-denominated trades in the window"
          >
            —
          </span>
        ) : (
          `$${volume}`
        )}
      </Td>
      <Td align="right">
        {asset.first_seen_ledger ? (
          asset.first_seen_ledger.toLocaleString('en-US')
        ) : (
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
    <span
      className="text-ink-muted text-xs font-medium"
      title={reason ?? 'Not available'}
    >
      Unavailable
    </span>
  );
}

/**
 * The circulating supply valued at the reference price — what the
 * backing behind the tokens in issue is CLAIMED to be worth.
 *
 * It sits beside the market cap on purpose. A tokenized treasury is
 * bought and held, so for most of this set the market-cap column is
 * empty and this one is not, and a reader who saw only the market
 * column would take the sector for a fraction of its size. What must
 * not follow from putting them side by side is the reader treating them
 * as the same kind of figure, so this cell says whose claim it is
 * rather than only what it says, and the reason is on the cell even
 * when there IS a number.
 */
function ReferenceValueCell({ asset }: { asset: RWAAsset }) {
  const rv = asset.reference_valuation;
  const value = usd(rv?.value_usd);
  if (value == null) {
    return (
      <Withheld
        reason={
          REFERENCE_VALUE_REASON[rv?.status ?? ''] ??
          'No independent valuation of this instrument is available.'
        }
      />
    );
  }
  return (
    <div>
      <div
        className="tnum"
        title={`Circulating supply times ${asset.reference?.source ?? 'an oracle'}’s value for the instrument. Not a market capitalisation: nobody was observed paying this, and the liquidity and price gates behind the market-cap column cannot check it.`}
      >
        {value}
      </div>
      <div className="text-ink-faint text-[11px]">at reference price</div>
    </div>
  );
}

/**
 * An independent oracle's valuation of the instrument the token declares
 * it anchors to — deliberately NOT labelled as the token's price. The
 * publisher and the vintage sit under the figure because a valuation
 * whose age is invisible invites a comparison it cannot support.
 */
function ReferenceCell({ asset }: { asset: RWAAsset }) {
  const ref = asset.reference;
  if (!ref) {
    return <Withheld reason={PREMIUM_REASON[asset.premium.status]} />;
  }
  return (
    <div>
      <div className="tnum">${formatOraclePrice(ref.price_usd)}</div>
      <div className="text-ink-faint text-[11px]">
        {ref.source} · {formatRelative(ref.as_of)}
        {ref.stale && (
          <span
            className="text-ink-muted ml-1"
            title="Older than 72 hours — the longest ordinary gap between two strikes of a real-world instrument's value. Shown, not withheld: a value struck last week is still the last one published."
          >
            stale
          </span>
        )}
      </div>
    </div>
  );
}

/**
 * The gap between what the market pays for the token and what an
 * independent party says the instrument is worth — the number a holder
 * of a tokenized treasury actually needs.
 *
 * A refused comparison renders as a word, never as "0.00%": par is a
 * finding, and a comparison that was never made is not that finding.
 */
function PremiumCell({ premium }: { premium: RWAAsset['premium'] }) {
  if (premium.status !== 'published' || premium.pct == null) {
    return <Withheld reason={PREMIUM_REASON[premium.status]} />;
  }
  const pct = Number(premium.pct);
  if (!Number.isFinite(pct)) {
    return <Withheld reason={PREMIUM_REASON[premium.status]} />;
  }
  const tone = pct > 0 ? 'text-up' : pct < 0 ? 'text-down' : 'text-ink-body';
  return (
    <span
      className={`tnum font-medium ${tone}`}
      title={`${
        pct === 0
          ? 'The market price equals the independent valuation.'
          : pct > 0
            ? 'The token trades ABOVE the independent valuation of the instrument.'
            : 'The token trades BELOW the independent valuation of the instrument.'
      } Served figure: ${premium.pct}%.`}
    >
      {pct > 0 ? '+' : ''}
      {formatPremiumPct(pct)}%
    </span>
  );
}

/**
 * A premium at enough precision that a non-zero gap never displays as
 * zero.
 *
 * Two decimals is the site's percentage convention, but the gaps this
 * column measures are fractions of a percent by nature — a tokenized
 * treasury near its instrument's value trades within basis points of it.
 * At two decimals a real −0.004% discount renders "0.00%", which reads
 * as "trades at par": the same wrong reading a blank cell would give,
 * arrived at from the other direction. The served figure carries four
 * decimals, so widening to those is enough for any non-zero value.
 *
 * An exact zero keeps two decimals. It IS par, and saying so plainly is
 * the honest rendering of that case.
 */
function formatPremiumPct(pct: number): string {
  const abs = Math.abs(pct);
  if (abs === 0) return '0.00';
  if (abs >= 0.01) return pct.toFixed(2);
  if (abs >= 0.001) return pct.toFixed(3);
  return pct.toFixed(4);
}

function GroupTable({
  rows,
  firstHeading,
}: {
  rows: {
    key: string;
    label: string;
    sub?: string;
    assets: number;
    unvalued: number;
    usd: string | null;
    referenceUsd: string | null;
    referenceUnvalued: number;
  }[];
  firstHeading: string;
}) {
  return (
    <Table>
      <THead>
        <TR>
          <Th>{firstHeading}</Th>
          <Th align="right">Assets</Th>
          <Th align="right">Market cap</Th>
          <Th align="right">Value of backing</Th>
        </TR>
      </THead>
      <TBody>
        {rows.map((r) => (
          <TR key={r.key}>
            <Td>
              <div>{r.label}</div>
              {r.sub && <div className="text-ink-muted text-xs">{r.sub}</div>}
            </Td>
            <Td align="right">{r.assets.toLocaleString('en-US')}</Td>
            <Td align="right">
              {r.usd == null ? (
                <Withheld reason="No asset in this group publishes a market valuation." />
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
              {r.unvalued > 0 && (
                <div className="text-ink-muted text-[11px]">
                  {r.unvalued} unvalued
                </div>
              )}
            </Td>
            {/* The same group on the reference basis. Split out rather
                than merged because the two bases admit different assets:
                a group can be empty on one and full on the other, and a
                single column would hide which. */}
            <Td align="right">
              {r.referenceUsd == null ? (
                <Withheld reason="No asset in this group carries an independent valuation of its instrument." />
              ) : (
                <>
                  {r.referenceUnvalued > 0 && (
                    <span className="text-ink-muted" aria-hidden>
                      ≥{' '}
                    </span>
                  )}
                  {r.referenceUsd}
                </>
              )}
              {r.referenceUnvalued > 0 && (
                <div className="text-ink-muted text-[11px]">
                  {r.referenceUnvalued} unvalued
                </div>
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
      <ol className="text-ink-body ml-4 list-decimal space-y-1.5 text-sm leading-relaxed">
        {REQUIREMENT_PROSE.map((r) => (
          <li key={r}>{r}</li>
        ))}
      </ol>
      <p className="text-ink-muted mt-3 text-xs leading-relaxed">
        An asset failing any requirement is absent from this page — not ranked
        lower, not hidden behind a filter. It keeps its own asset page, with
        whatever warnings apply there. Recognised classes:{' '}
        <span className="font-mono">
          {definition.anchor_classes.join(', ')}
        </span>
        . Fiat-anchored tokens are stablecoins and are counted elsewhere.
      </p>
      <div className="text-ink-muted mt-3 text-xs leading-relaxed">
        <p>
          <strong className="text-ink-body">
            Which tokens are compared against an instrument.
          </strong>{' '}
          Only the exact <span className="font-mono">(code, issuer)</span> pairs
          listed below. A code is not an identity: anyone can issue a token
          called USTRY, and answering one of those with the real
          instrument&rsquo;s value would publish a discount to a security it has
          nothing to do with. The list is short by construction, and a pair
          missing from it shows the reason rather than a number.
        </p>
        <p className="mt-2">
          What the comparison rests on is that one token is one unit of the
          named instrument. The evidence is the issuer&rsquo;s own domain-bound
          declaration and the independent recognition of its account — the same
          evidence that admitted the asset — and is not a separate measurement.
        </p>
        {definition.bound_instruments.length > 0 && (
          <ul className="mt-2 space-y-0.5">
            {definition.bound_instruments.map((b) => (
              <li
                key={`${b.code}-${b.issuer}`}
                className="font-mono text-[11px]"
              >
                {b.code}
                <span className="text-ink-faint">
                  -{truncateMiddle(b.issuer, 6, 6)}
                </span>{' '}
                <span aria-hidden>&rarr;</span> {b.feed}
              </li>
            ))}
          </ul>
        )}
      </div>
      {refusedTotal > 0 && (
        <details className="group border-line mt-3 rounded-lg border">
          <summary className="text-ink-body marker:text-ink-faint hover:text-brand-600 cursor-pointer px-3 py-1.5 text-xs font-medium select-none">
            Candidates refused{' '}
            <span className="text-ink-faint">
              ({refusedTotal.toLocaleString('en-US')})
            </span>
          </summary>
          <dl className="border-line space-y-1.5 border-t px-3 py-2 text-[11px] leading-relaxed">
            {refused.map((r) => (
              <div key={r.reason} className="flex justify-between gap-4">
                <dt className="text-ink-body">
                  {REFUSAL_PROSE[r.reason] ?? r.reason}
                </dt>
                <dd className="tnum text-ink-muted">
                  {r.assets.toLocaleString('en-US')}
                </dd>
              </div>
            ))}
          </dl>
        </details>
      )}
      <div className="text-ink-muted mt-3 text-xs leading-relaxed">
        <p>
          <strong className="text-ink-body">
            Two valuations, kept apart on purpose.
          </strong>{' '}
          <em>Market cap</em> is circulating supply times a price somebody was
          observed paying, and it reaches this page only after the same
          thin-market, dust-liquidity and scam-issuer gates the asset pages
          apply have each declined to withhold it. <em>Value of backing</em> is
          circulating supply times what an independent oracle says one unit of
          the underlying instrument is worth. Nobody was observed paying that,
          and none of those gates can check it — there is no market in it for
          them to measure.
        </p>
        <p className="mt-2">
          Both are published because neither alone is honest here. A tokenized
          treasury is bought and held rather than traded, so most of this set
          has never produced a market price and the market-cap column is silent
          about assets that plainly exist; equally, a figure the issuer and its
          oracle assert is not evidence that anyone would pay it. They are never
          added together, and an asset can appear in one column, both, or
          neither. Where a token carries no reference figure, the coverage panel
          below counts it under the reason.
        </p>
      </div>
      <p className="text-ink-muted mt-3 text-[11px] leading-relaxed">
        Live data from <span className="font-mono">{ENDPOINT}</span>. Market
        valuations come from the same price, supply and trust gates the asset
        pages use, so nothing here publishes a figure those pages withhold. The
        full definition, with the evidence behind each requirement, is in the{' '}
        <Link href="/methodology" className="hover:text-brand-600 underline">
          methodology
        </Link>
        .
      </p>
    </Panel>
  );
}
