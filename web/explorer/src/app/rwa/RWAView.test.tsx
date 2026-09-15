import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';

import type { components } from '@/api/types';

import { RWAView, splitBasis, sumUsd } from './RWAView';

type Schemas = components['schemas'];
type View = Schemas['RWAAssetsView'];

const apiGetData = vi.hoisted(() => vi.fn());
// useAssets (the stablecoin arm) reads through apiGet, not apiGetData.
// Mocked here so the sector tiles are deterministic and no test reaches
// the network.
const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGetData, apiGet };
});

/** One fiat-backed token, valued, as the served catalogue returns it. */
function stablecoinPage(
  rows: { code: string; market_cap_usd?: string }[] = [
    { code: 'USDC', market_cap_usd: '354959662.86' },
  ],
) {
  return { data: rows.map((r) => ({ ...r, class: 'stablecoin' })) };
}

const ISSUER = 'GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC';

function asset(over: Partial<Schemas['RWAAsset']> = {}): Schemas['RWAAsset'] {
  return {
    asset_id: `USTRY-${ISSUER}`,
    code: 'USTRY',
    issuer: ISSUER,
    slug: 'ustry',
    name: 'US Treasury Bill',
    home_domain: 'etherfuse.com',
    issuer_directory_name: 'Etherfuse',
    issuer_directory_tags: ['issuer'],
    basis: 'sep1_anchor_declaration',
    anchor_class: 'bond',
    anchor_asset: 'US Treasury Notes',
    valuation: {
      status: 'published',
      price_usd: '1.0412',
      market_cap_usd: '1284500.00',
    },
    reference: {
      price_usd: '1.07403800',
      source: 'redstone',
      feed: 'rwa:USTRY',
      quote: 'fiat:USD',
      as_of: '2026-09-09T15:08:40Z',
    },
    // 12,336,218,000,000 smallest units at 7dp is 1,233,621.8 tokens;
    // at the 1.074038 reference that is 1,324,956.69 — a different
    // basis over the same float, never added to the market cap.
    reference_valuation: { status: 'published', value_usd: '1324956.69' },
    premium: { status: 'published', pct: '-3.0574' },
    circulating_supply: '12336218000000',
    decimals: 7,
    volume_24h_usd: '8214.55',
    first_seen_ledger: 55008233,
    observation_count: 346312,
    ...over,
  } as Schemas['RWAAsset'];
}

function view(over: Partial<View> = {}): View {
  return {
    definition: {
      requirements: ['a', 'b', 'c', 'd'],
      anchor_classes: ['bond', 'commodity', 'realestate', 'stock'],
      recognition_tags: [
        'anchor',
        'custodian',
        'defi',
        'exchange',
        'issuer',
        'sdf',
      ],
      scam_flag_tags: [
        'malicious',
        'unsafe',
        'fraud',
        'scam',
        'hack',
        'phishing',
      ],
      bound_instruments: [
        { code: 'USTRY', issuer: ISSUER, feed: 'rwa:USTRY' },
        { code: 'CETES', issuer: ISSUER, feed: 'rwa:CETES' },
      ],
      documentation_url:
        'https://stellarindex.io/docs/methodology/rwa-definition',
    },
    summary: {
      assets: 1,
      issuers: 1,
      market_cap_usd: '1284500.00',
      assets_valued: 1,
      assets_unvalued: 0,
      lower_bound: false,
      earliest_first_seen_ledger: 55008233,
      assets_with_reference: 1,
      assets_compared: 1,
      reference_valuation: {
        value_usd: '1324956.69',
        assets_valued: 1,
        assets_unvalued: 0,
        lower_bound: false,
        sources: ['redstone'],
        basis:
          'Sum of circulating supply times an independent oracle valuation of the instrument.',
      },
      both_bases: {
        assets: 1,
        market_cap_usd: '1284500.00',
        reference_value_usd: '1324956.69',
      },
      basis:
        'Sum of the published market caps of the assets meeting the four-requirement definition.',
    },
    assets: [asset()],
    by_class: [
      {
        class: 'bond',
        assets: 1,
        market_cap_usd: '1284500.00',
        assets_unvalued: 0,
        reference_value_usd: '1324956.69',
        assets_reference_unvalued: 0,
      },
    ],
    by_issuer: [
      {
        issuer: ISSUER,
        name: 'Etherfuse',
        home_domain: 'etherfuse.com',
        assets: 1,
        market_cap_usd: '1284500.00',
        assets_unvalued: 0,
        reference_value_usd: '1324956.69',
        assets_reference_unvalued: 0,
      },
    ],
    refused: [],
    ...over,
  } as View;
}

function renderView() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <RWAView />
    </QueryClientProvider>,
  );
}

describe('RWAView', () => {
  beforeEach(() => {
    apiGetData.mockReset();
    apiGet.mockReset();
    apiGet.mockResolvedValue(stablecoinPage());
  });

  it('renders an admitted asset with its issuer identity, not the code alone', async () => {
    apiGetData.mockResolvedValue(view());
    renderView();

    expect(await screen.findByText('USTRY')).toBeInTheDocument();
    // The G-address is the identity; a page that showed only a code and
    // a friendly name would be indistinguishable from an impersonator's.
    expect(screen.getAllByText(/GCRYUG…ATMYWC/).length).toBeGreaterThan(0);
    expect(screen.getAllByText('Etherfuse').length).toBeGreaterThan(0);
    // The figure appears in the headline, the row, and both breakdowns —
    // every level is the exact sum of the level below.
    expect(screen.getAllByText('$1,284,500.00').length).toBeGreaterThanOrEqual(
      3,
    );
  });

  it('renders a withheld valuation as unavailable, never as a number', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            valuation: { status: 'withheld_issuer_flagged' },
            reference_valuation: { status: 'withheld_issuer_flagged' },
            reference: undefined,
            premium: { status: 'withheld_issuer_flagged' },
            issuer_directory_tags: ['issuer', 'malicious'],
          }),
        ],
        summary: {
          ...view().summary,
          market_cap_usd: undefined,
          assets_valued: 0,
          assets_unvalued: 1,
          lower_bound: true,
          reference_valuation: {
            assets_valued: 0,
            assets_unvalued: 1,
            lower_bound: true,
            basis: 'No member currently carries a reference valuation.',
          },
          both_bases: { assets: 0 },
        },
        by_class: [
          {
            class: 'bond',
            assets: 1,
            assets_unvalued: 1,
            assets_reference_unvalued: 1,
          },
        ],
        by_issuer: [
          {
            issuer: ISSUER,
            name: 'Etherfuse',
            assets: 1,
            assets_unvalued: 1,
            assets_reference_unvalued: 1,
          },
        ],
      }),
    );
    renderView();

    // The money cells say the word, not a figure and not a bare dash: a
    // dash beside real figures reads as zero.
    expect(
      (await screen.findAllByText('Unavailable')).length,
    ).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText('$1,284,500.00')).not.toBeInTheDocument();
    expect(screen.queryByText('$0.00')).not.toBeInTheDocument();
    // The flag itself is surfaced, not silently swallowed.
    expect(screen.getByText('Flagged')).toBeInTheDocument();
  });

  it('says the total is not published rather than showing zero', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            valuation: { status: 'unpriced' },
            reference_valuation: { status: 'no_reference_feed' },
            reference: undefined,
            premium: { status: 'no_reference_feed' },
          }),
        ],
        summary: {
          ...view().summary,
          market_cap_usd: undefined,
          assets_valued: 0,
          assets_unvalued: 1,
          lower_bound: true,
          assets_with_reference: 0,
          assets_compared: 0,
          reference_valuation: {
            assets_valued: 0,
            assets_unvalued: 1,
            lower_bound: true,
            basis: 'No member currently carries a reference valuation.',
          },
          both_bases: { assets: 0 },
        },
        by_class: [
          {
            class: 'bond',
            assets: 1,
            assets_unvalued: 1,
            assets_reference_unvalued: 1,
          },
        ],
        by_issuer: [
          {
            issuer: ISSUER,
            assets: 1,
            assets_unvalued: 1,
            assets_reference_unvalued: 1,
          },
        ],
      }),
    );
    renderView();

    // Both valuation tiles say it: neither basis has anything to
    // publish, and neither may render as a zero.
    // Backing, market cap, and Combined — which may not be published
    // from the stablecoin arm alone.
    expect(await screen.findAllByText('Not published')).toHaveLength(3);
    expect(screen.queryByText('$0.00')).not.toBeInTheDocument();
    expect(
      screen.getByText(/No asset in the set publishes a market valuation/),
    ).toBeInTheDocument();
  });

  it('marks a partly-valued total as a lower bound', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset(),
          asset({
            asset_id: `TESOURO-${ISSUER}`,
            code: 'TESOURO',
            valuation: { status: 'unpriced' },
          }),
        ],
        summary: {
          ...view().summary,
          assets: 2,
          assets_valued: 1,
          assets_unvalued: 1,
          lower_bound: true,
        },
      }),
    );
    renderView();

    expect(
      (await screen.findAllByText('$1,284,500.00')).length,
    ).toBeGreaterThan(0);
    expect(screen.getByText(/Also a floor\./)).toBeInTheDocument();
    expect(
      screen.getByText(/no market valuation and contribute/),
    ).toBeInTheDocument();
  });

  it('states the definition and how many candidates each requirement refused', async () => {
    apiGetData.mockResolvedValue(
      view({
        refused: [
          { reason: 'issuer_not_independently_recognised', assets: 3861 },
          { reason: 'issuer_scam_flagged', assets: 289 },
        ],
      }),
    );
    renderView();

    expect(await screen.findByText(/Candidates refused/)).toBeInTheDocument();
    expect(screen.getByText('(4,150)')).toBeInTheDocument();
    expect(
      screen.getByText('Issuer flagged by the independent directory'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Issuer recognised by nobody but itself'),
    ).toBeInTheDocument();
  });

  it('accounts for the population the set was narrowed from, and says who can move each drop', async () => {
    apiGetData.mockResolvedValue(
      view({
        funnel: {
          balanced: true,
          basis: 'Every issuer account that could carry a SEP-1 attestation.',
          stages: [
            {
              arm: 'classic',
              stage: 'issuers_with_home_domain',
              unit: 'issuer_accounts',
              count: 44376,
              dropped: [
                {
                  reason: 'sep1_attestation_never_fetched',
                  count: 1,
                  actor: 'operator',
                },
                // Reached, and served nothing usable. Same shape of
                // number as the row above, opposite finding and a
                // different owner — the page has to say which is which.
                {
                  reason: 'domain_served_no_sep1_attestation',
                  count: 29740,
                  actor: 'issuer',
                },
              ],
            },
            {
              arm: 'classic',
              stage: 'issuers_with_sep1_attestation',
              unit: 'issuer_accounts',
              count: 14635,
            },
            {
              arm: 'classic',
              stage: 'assets_served',
              unit: 'assets',
              count: 1,
            },
          ],
        },
      }),
    );
    renderView();

    // The population, which the page used to state nowhere at all.
    expect(await screen.findByText(/Where the population went/)).toBeVisible();
    expect(screen.getByText('44,376')).toBeInTheDocument();
    expect(screen.getByText('14,635')).toBeInTheDocument();
    // The largest coverage gap, named with its owner: a fetch nobody
    // has run reads nothing like a refusal, and a bare count renders
    // the two identically.
    expect(
      screen.getByText(/stellar\.toml not fetched yet/),
    ).toBeInTheDocument();
    expect(screen.getByText(/ours to fix/)).toBeInTheDocument();
    expect(screen.getByText('−1')).toBeInTheDocument();
    // And the far larger bucket beside it, attributed to the party who
    // actually owns it. Rendering both under "ours to fix" would
    // advertise coverage nobody here can reach.
    expect(
      screen.getByText(/Domain served no stellar\.toml/),
    ).toBeInTheDocument();
    expect(screen.getByText(/the issuer’s to fix/)).toBeInTheDocument();
    expect(screen.getByText('−29,740')).toBeInTheDocument();
  });

  it('says so when the coverage accounting could not be measured', async () => {
    apiGetData.mockResolvedValue(
      view({
        funnel: {
          balanced: false,
          basis: 'Not measured.',
          stages: [
            {
              arm: 'classic',
              stage: 'assets_served',
              unit: 'assets',
              count: 1,
              dropped: [],
            },
          ],
        },
      }),
    );
    renderView();
    expect(
      await screen.findByText(/could not be measured/),
    ).toBeInTheDocument();
  });

  it('renders the empty set as a statement about evidence, not as a zero total', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [],
        summary: {
          assets: 0,
          issuers: 0,
          assets_valued: 0,
          assets_unvalued: 0,
          lower_bound: false,
          assets_with_reference: 0,
          assets_compared: 0,
          reference_valuation: {
            assets_valued: 0,
            assets_unvalued: 0,
            lower_bound: false,
            basis: 'No asset currently meets the definition.',
          },
          both_bases: { assets: 0 },
          basis: 'No asset currently meets the definition.',
        },
        by_class: [],
        by_issuer: [],
      }),
    );
    renderView();

    expect(
      await screen.findByText('No asset currently meets the definition'),
    ).toBeInTheDocument();
    // BOTH valuation tiles say it. An empty set has no market cap and
    // no reference valuation, and neither may render as a zero.
    expect(screen.getAllByText('Not published')).toHaveLength(3);
    expect(screen.queryByText('$0.00')).not.toBeInTheDocument();
  });

  it('surfaces a failed load instead of an empty set', async () => {
    apiGetData.mockRejectedValue(new Error('502 Bad Gateway'));
    renderView();

    expect(
      await screen.findByText('Failed to load real-world assets'),
    ).toBeInTheDocument();
    // An outage must not be presented as "no RWAs exist".
    expect(
      screen.queryByText(/No asset currently meets the definition/),
    ).not.toBeInTheDocument();
  });

  it('shows the discount to the instrument valuation, with its publisher and vintage', async () => {
    apiGetData.mockResolvedValue(view());
    renderView();

    // The independent valuation, at the oracle's own scale.
    expect(await screen.findByText('$1.0740')).toBeInTheDocument();
    // Who published it and when — a valuation whose age is invisible
    // invites a comparison it cannot support.
    expect(screen.getAllByText(/redstone/).length).toBeGreaterThan(0);
    // And the gap itself, signed against the instrument.
    expect(screen.getByText('-3.06%')).toBeInTheDocument();
    // Coverage of the comparison, so the blanks are not read as zeros.
    expect(
      screen.getByText(/carry an independent oracle valuation/),
    ).toBeInTheDocument();
  });

  it('renders a refused comparison as words, never as 0.00%', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            code: 'XAU',
            reference: undefined,
            premium: { status: 'reference_not_instrument_scoped' },
          }),
        ],
      }),
    );
    renderView();

    await screen.findByText('XAU');
    // Zero would read as "trades at par" — a finding, and not the one
    // the data supports.
    expect(screen.queryByText('0.00%')).not.toBeInTheDocument();
    expect(screen.queryByText('-0.00%')).not.toBeInTheDocument();
    // Both the instrument-value and the premium cell say the word.
    expect(screen.getAllByText('Unavailable').length).toBeGreaterThanOrEqual(2);
    // Both cells state the same reason: the oracle prices something
    // else, so neither a value nor a gap can be published.
    expect(
      screen.getAllByTitle(/off-chain quantity in its own unit/).length,
    ).toBeGreaterThanOrEqual(2);
  });

  it('gives a flagged issuer no independent instrument valuation either', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            issuer_directory_tags: ['issuer', 'malicious'],
            valuation: { status: 'withheld_issuer_flagged' },
            reference_valuation: { status: 'withheld_issuer_flagged' },
            reference: undefined,
            premium: { status: 'withheld_issuer_flagged' },
          }),
        ],
        summary: {
          ...view().summary,
          market_cap_usd: undefined,
          assets_valued: 0,
          assets_unvalued: 1,
          lower_bound: true,
          assets_with_reference: 0,
          assets_compared: 0,
          reference_valuation: {
            assets_valued: 0,
            assets_unvalued: 1,
            lower_bound: true,
            basis: 'No member currently carries a reference valuation.',
          },
          both_bases: { assets: 0 },
        },
        by_class: [
          {
            class: 'bond',
            assets: 1,
            assets_unvalued: 1,
            assets_reference_unvalued: 1,
          },
        ],
        by_issuer: [
          {
            issuer: ISSUER,
            name: 'Etherfuse',
            assets: 1,
            assets_unvalued: 1,
            assets_reference_unvalued: 1,
          },
        ],
      }),
    );
    renderView();

    expect(await screen.findByText('Flagged')).toBeInTheDocument();
    // No figure of any kind reaches a flagged issuer's row — not this
    // platform's, and not a third party's valuation of the real
    // instrument, which would be the larger claim of the two.
    expect(screen.queryByText('$1.0740')).not.toBeInTheDocument();
    expect(screen.queryAllByText(/redstone/)).toHaveLength(0);
    expect(screen.queryByText('-3.06%')).not.toBeInTheDocument();
    // Including the supply-multiplied form, which is the LARGER claim
    // of the two: a real instrument's value times this account's float.
    expect(screen.queryAllByText('$1,324,956.69')).toHaveLength(0);
  });

  it('never renders a real gap as 0.00%', async () => {
    // Treasury premiums are fractions of a percent by nature. At the
    // site's usual two decimals a real −0.004% discount rounds to
    // "0.00%" — which reads as "trades at par", the same wrong reading a
    // blank cell gives, arrived at from the other direction. The cell
    // widens instead.
    apiGetData.mockResolvedValue(
      view({
        assets: [asset({ premium: { status: 'published', pct: '-0.0040' } })],
      }),
    );
    renderView();

    expect(await screen.findByText('-0.004%')).toBeInTheDocument();
    expect(screen.queryByText('0.00%')).not.toBeInTheDocument();
    expect(screen.queryByText('-0.00%')).not.toBeInTheDocument();
  });

  it('widens as far as the served precision to keep a gap visible', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [asset({ premium: { status: 'published', pct: '-0.0004' } })],
      }),
    );
    renderView();

    expect(await screen.findByText('-0.0004%')).toBeInTheDocument();
    expect(screen.queryByText('0.00%')).not.toBeInTheDocument();
    expect(screen.queryByText('-0.000%')).not.toBeInTheDocument();
  });

  it('keeps the exact served gap available where the display rounds', async () => {
    // The live CETES gap is −0.0166%, which displays at two decimals.
    // The served figure is the one the API published, and it stays
    // reachable rather than being silently replaced by its rounding.
    apiGetData.mockResolvedValue(
      view({
        assets: [asset({ premium: { status: 'published', pct: '-0.0166' } })],
      }),
    );
    renderView();

    expect(await screen.findByText('-0.02%')).toBeInTheDocument();
    expect(screen.getByTitle(/-0\.0166%/)).toBeInTheDocument();
  });

  it('names the bound pairs, not a list of codes', async () => {
    apiGetData.mockResolvedValue(view());
    renderView();

    // The page states the rule as pairs, because a code is not an
    // identity and a list of codes would suggest it is.
    expect(
      await screen.findByText(
        'Which tokens are compared against an instrument.',
      ),
    ).toBeInTheDocument();
    // Each binding is shown with its issuer, not as a bare ticker.
    expect(screen.getByText(/rwa:USTRY/)).toBeInTheDocument();
    expect(screen.getByText(/rwa:CETES/)).toBeInTheDocument();
    expect(
      screen.getAllByText((_, el) =>
        (el?.textContent ?? '').includes(
          'anyone can issue a token called USTRY',
        ),
      ).length,
    ).toBeGreaterThan(0);
  });

  it('says an unbound token is unbound, not that no oracle exists', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            reference: undefined,
            premium: { status: 'reference_not_bound' },
          }),
        ],
      }),
    );
    renderView();

    await screen.findByText('USTRY');
    expect(screen.queryByText('-3.06%')).not.toBeInTheDocument();
    expect(
      screen.getAllByTitle(/not one of the pairs bound to an oracle feed/)
        .length,
    ).toBeGreaterThanOrEqual(2);
    // The reason must not read as a statement about what the oracles carry.
    expect(
      screen.queryByTitle(/no oracle is currently publishing/),
    ).not.toBeInTheDocument();
  });

  it('says an oracle outage is an outage, not an absence', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            reference: undefined,
            premium: { status: 'reference_unavailable' },
          }),
        ],
      }),
    );
    renderView();

    await screen.findByText('USTRY');
    expect(
      screen.getAllByTitle(
        /This is an outage, not a statement that no valuation exists/,
      ).length,
    ).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText('-3.06%')).not.toBeInTheDocument();
  });

  it('labels a stale instrument valuation rather than hiding it', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            reference: {
              price_usd: '1.07403800',
              source: 'redstone',
              feed: 'rwa:USTRY',
              quote: 'fiat:USD',
              as_of: '2026-08-01T00:00:00Z',
              provenance: 'oracle_instrument_nav',
              stale: true,
            },
          }),
        ],
      }),
    );
    renderView();

    expect(await screen.findByText('$1.0740')).toBeInTheDocument();
    expect(screen.getByText('stale')).toBeInTheDocument();
    // Labelled, not withheld: it is still the last value published.
    expect(screen.getByText('-3.06%')).toBeInTheDocument();
  });
  it('shows the value of the backing beside the market cap, labelled as a claim', async () => {
    apiGetData.mockResolvedValue(view());
    renderView();

    // The figure itself, on the row and in both breakdowns.
    expect(
      (await screen.findAllByText('$1,324,956.69')).length,
    ).toBeGreaterThanOrEqual(3);
    // Never as a market cap. The column heading, the caption under the
    // cell and the tile all name the basis rather than borrowing the
    // market's vocabulary.
    expect(screen.getByText('Value of the backing')).toBeInTheDocument();
    expect(
      screen.getByText(/Circulating supply × an independent oracle/),
    ).toBeInTheDocument();
    expect(screen.getAllByText('at reference price').length).toBeGreaterThan(0);
    expect(
      screen.getByTitle(/Not a market capitalisation: nobody was observed/),
    ).toBeInTheDocument();
    // And the page states the difference in kind before either number.
    expect(
      screen.getByText('Two different kinds of number.'),
    ).toBeInTheDocument();
    expect(
      screen.getAllByText((_, el) =>
        (el?.textContent ?? '').includes('They are never added together'),
      ).length,
    ).toBeGreaterThan(0);
  });

  it('values an asset nobody trades without inventing a market cap for it', async () => {
    // The case the second basis exists for. A tokenized treasury that
    // has never traded has no market price, so its market-cap cell must
    // stay empty — while an oracle prices its instrument daily and the
    // reference cell is full.
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            code: 'USDY',
            valuation: { status: 'unpriced' },
            premium: { status: 'no_market_price' },
          }),
        ],
        summary: {
          ...view().summary,
          market_cap_usd: undefined,
          assets_valued: 0,
          assets_unvalued: 1,
          lower_bound: true,
          assets_compared: 0,
          both_bases: { assets: 0 },
        },
        by_class: [
          {
            class: 'bond',
            assets: 1,
            assets_unvalued: 1,
            reference_value_usd: '1324956.69',
            assets_reference_unvalued: 0,
          },
        ],
        by_issuer: [
          {
            issuer: ISSUER,
            assets: 1,
            assets_unvalued: 1,
            reference_value_usd: '1324956.69',
            assets_reference_unvalued: 0,
          },
        ],
      }),
    );
    renderView();

    await screen.findByText('USDY');
    // The reference figure is published...
    expect(screen.getAllByText('$1,324,956.69').length).toBeGreaterThanOrEqual(
      3,
    );
    // ...and the market cap is still absent, stated as unavailable
    // rather than filled in from the figure beside it.
    expect(screen.queryByText('$1,284,500.00')).not.toBeInTheDocument();
    expect(screen.getAllByText('Not published').length).toBe(1);
    expect(screen.getAllByText('Unavailable').length).toBeGreaterThanOrEqual(2);
  });

  it('states why a reference valuation is missing rather than showing a dash', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            reference_valuation: { status: 'supply_unavailable' },
            circulating_supply: undefined,
          }),
        ],
        summary: {
          ...view().summary,
          reference_valuation: {
            assets_valued: 0,
            assets_unvalued: 1,
            lower_bound: true,
            basis: 'No member currently carries a reference valuation.',
          },
          both_bases: { assets: 0 },
        },
        by_class: [
          {
            class: 'bond',
            assets: 1,
            market_cap_usd: '1284500.00',
            assets_unvalued: 0,
            assets_reference_unvalued: 1,
          },
        ],
        by_issuer: [
          {
            issuer: ISSUER,
            assets: 1,
            market_cap_usd: '1284500.00',
            assets_unvalued: 0,
            assets_reference_unvalued: 1,
          },
        ],
      }),
    );
    renderView();

    await screen.findByText('USTRY');
    expect(screen.queryAllByText('$1,324,956.69')).toHaveLength(0);
    expect(
      screen.getAllByTitle(/no circulating-supply reading does/).length,
    ).toBeGreaterThan(0);
  });
});

/**
 * The contract arm.
 *
 * The funnel now carries two arms that narrow different populations
 * from different roots. Rendering them as one list would invite a
 * reader to subtract the last stage of one from the first stage of the
 * next, which relates nothing — so the page has to separate them, and
 * has to say what the second one is.
 */
describe('RWAView — contract arm', () => {
  beforeEach(() => {
    apiGetData.mockReset();
    apiGet.mockReset();
    apiGet.mockResolvedValue(stablecoinPage());
  });

  const CONTRACT = 'CAAQEAYEAUDAOCAJBIFQYDIOB4IBCEQTCQKRMFYYDENBWHA5DYPSBFLM';
  const FT = 'GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5';

  function contractView(): View {
    return view({
      assets: [
        asset({
          asset_id: CONTRACT,
          code: '',
          issuer: '',
          contract_id: CONTRACT,
          symbol: 'USTRY',
          slug: CONTRACT,
          name: 'Example Treasury Fund',
          basis: 'contract_oracle_rwa_feed',
        }),
      ],
      funnel: {
        balanced: true,
        basis: 'measured',
        stages: [
          {
            arm: 'contract',
            stage: 'curated_directory_entries',
            unit: 'directory_addresses',
            count: 18439,
            dropped: [
              {
                reason: 'directory_entry_names_an_account',
                count: 18000,
                actor: 'definition',
              },
            ],
          },
          {
            arm: 'contract',
            stage: 'directory_recognised_contracts',
            unit: 'contracts',
            count: 1,
          },
          {
            arm: 'contract',
            stage: 'contract_assets_served',
            unit: 'assets',
            count: 1,
          },
        ],
      },
      unreached_entities: [
        {
          address: FT,
          name: 'Franklin Templeton',
          domain: 'franklintempleton.com',
          tags: ['issuer'],
        },
      ],
    } as Partial<View>);
  }

  it('separates the contract arm and names its stages in prose', async () => {
    apiGetData.mockResolvedValue(contractView());
    renderView();

    expect(
      await screen.findByText('Tokens issued by a contract'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Addresses in the independent directory'),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Names an entity, not a token contract/),
    ).toBeInTheDocument();
    // The wire vocabulary must not reach the page: a reader should
    // never have to know the field names to read the accounting.
    expect(screen.queryByText('curated_directory_entries')).toBeNull();
    expect(screen.queryByText('directory_entry_names_an_account')).toBeNull();
    expect(screen.queryByText('directory_addresses')).toBeNull();
  });

  it('reports a recognised issuer it holds no token for, by name', async () => {
    apiGetData.mockResolvedValue(contractView());
    renderView();

    expect(
      await screen.findByText('Recognised issuers we hold no token for'),
    ).toBeInTheDocument();
    expect(screen.getByText('Franklin Templeton')).toBeInTheDocument();
    expect(screen.getByText('franklintempleton.com')).toBeInTheDocument();
  });

  it('says a contract token is not reference-valued, and why', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: [
          asset({
            asset_id: CONTRACT,
            code: '',
            issuer: '',
            contract_id: CONTRACT,
            symbol: 'USTRY',
            basis: 'contract_oracle_rwa_feed',
            reference_valuation: { status: 'reference_contract_not_bound' },
            reference: undefined,
            premium: { status: 'reference_contract_not_bound' },
          }),
        ],
        summary: {
          ...view().summary,
          assets_with_reference: 0,
          assets_compared: 0,
          reference_valuation: {
            assets_valued: 0,
            assets_unvalued: 1,
            lower_bound: true,
            basis: 'No member currently carries a reference valuation.',
          },
          both_bases: { assets: 0 },
        },
        by_class: [
          {
            class: 'bond',
            assets: 1,
            market_cap_usd: '1284500.00',
            assets_unvalued: 0,
            assets_reference_unvalued: 1,
          },
        ],
        by_issuer: [
          {
            issuer: ISSUER,
            assets: 1,
            market_cap_usd: '1284500.00',
            assets_unvalued: 0,
            assets_reference_unvalued: 1,
          },
        ],
      }),
    );
    renderView();

    // A stated refusal, not a blank. A reader must be able to tell "we
    // will not do this" from "we forgot".
    expect(
      (await screen.findAllByTitle(/nothing binds a contract address/)).length,
    ).toBeGreaterThan(0);
    expect(screen.queryAllByText('$1,324,956.69')).toHaveLength(0);
    // The market cap it DOES have is untouched by that refusal.
    expect(screen.getAllByText('$1,284,500.00').length).toBeGreaterThan(0);
  });

  it('accounts in the funnel for every asset the oracles do not price', async () => {
    apiGetData.mockResolvedValue(
      view({
        funnel: {
          balanced: true,
          basis: 'Every issuer account that could carry a SEP-1 attestation.',
          stages: [
            {
              arm: 'classic',
              stage: 'assets_served',
              unit: 'assets',
              count: 3,
            },
            {
              arm: 'valuation',
              stage: 'assets_served_all_arms',
              unit: 'assets',
              count: 3,
              dropped: [
                {
                  reason: 'reference_not_instrument_scoped',
                  count: 1,
                  actor: 'definition',
                },
                { reason: 'supply_unavailable', count: 1, actor: 'operator' },
              ],
            },
            {
              arm: 'valuation',
              stage: 'assets_reference_valued',
              unit: 'assets',
              count: 1,
            },
          ],
        },
      }),
    );
    renderView();

    // The arm is labelled as an accounting rather than a narrowing, so
    // an unpriced member is not read as an asset the rule refused.
    expect(
      await screen.findByText('Whose backing is independently priced'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Whose backing an independent oracle prices'),
    ).toBeInTheDocument();
    // And each drop says who can move it — a gap here reads differently
    // from the rule working.
    expect(
      screen.getByText('No circulating-supply reading'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('The feed prices an ounce, not a token'),
    ).toBeInTheDocument();
    expect(screen.getAllByText(/ours to fix/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/the rule working/).length).toBeGreaterThan(0);
  });

  // ─── the reported defect ────────────────────────────────────────
  //
  // The page led with market cap. On the live set that is $17.13M
  // against $992.49M of backing, because the two largest instruments —
  // a $535.90M and a $439.46M fund — are bought and held and have no
  // observed Stellar price at all: they contribute nothing to market
  // cap while carrying $975M of backing between them. An operator
  // comparing the page against a supply-times-NAV figure published
  // elsewhere read the difference as missing data. Nothing was missing.
  // The page was leading with the basis that this set, by its nature,
  // mostly does not have.
  describe('leads with the basis the set actually has', () => {
    const LIVE_BASIS =
      'Sum of circulating supply times an independent oracle’s published ' +
      'valuation of the real-world instrument each token declares it anchors ' +
      'to. THIS IS NOT A MARKET CAPITALISATION AND NOT AN OBSERVED PRICE: ' +
      'nobody was seen paying it. It is what an oracle says one unit of the ' +
      'backing is worth, multiplied by the tokens in circulation. Assets ' +
      'with no bound and current oracle feed contribute nothing and are ' +
      'counted separately, so the total is a LOWER BOUND.';

    function liveView(over: Partial<Schemas['RWASummary']> = {}) {
      return view({
        assets: [
          asset(),
          asset({
            asset_id: `BENJI-${ISSUER}`,
            code: 'BENJI',
            slug: 'benji',
            name: 'Franklin OnChain U.S. Government Money Fund',
            valuation: { status: 'unpriced' },
            premium: { status: 'no_market_price' },
            reference_valuation: {
              status: 'published',
              value_usd: '439462363.05',
            },
          }),
          asset({
            asset_id: `USDY-${ISSUER}`,
            code: 'USDY',
            slug: 'usdy',
            name: 'Ondo US Dollar Yield',
            valuation: { status: 'unpriced' },
            premium: { status: 'no_market_price' },
            reference_valuation: {
              status: 'published',
              value_usd: '535897722.98',
            },
          }),
        ],
        summary: {
          ...view().summary,
          assets: 11,
          issuers: 5,
          market_cap_usd: '17128565.34',
          assets_valued: 3,
          assets_unvalued: 8,
          lower_bound: true,
          assets_with_reference: 7,
          assets_compared: 3,
          reference_valuation: {
            value_usd: '992488360.69',
            assets_valued: 7,
            assets_unvalued: 4,
            lower_bound: true,
            sources: ['redstone'],
            basis: LIVE_BASIS,
          },
          ...over,
        },
      });
    }

    it('puts the reference total ahead of the market cap', async () => {
      apiGetData.mockResolvedValue(liveView());
      renderView();

      const backing = await screen.findByText('Value of the backing');
      const market = screen.getByText('Market cap (observed trades)');
      // Node.DOCUMENT_POSITION_FOLLOWING === 4: `market` comes after
      // `backing` in document order. Both are present at full size —
      // the market figure is second, not hidden.
      expect(
        backing.compareDocumentPosition(market) &
          Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
      expect(screen.getAllByText('$992,488,360.69').length).toBeGreaterThan(0);
      expect(screen.getAllByText('$17,128,565.34').length).toBeGreaterThan(0);
    });

    it('carries the headline basis as page text, not only as a tooltip', async () => {
      apiGetData.mockResolvedValue(liveView());
      renderView();

      // The sentence a reader must not have to hover to find. It comes
      // from the SERVED basis, so the page cannot drift from what the
      // server says the number means.
      const stated = await screen.findByText(
        /THIS IS NOT A MARKET CAPITALISATION AND NOT AN OBSERVED PRICE/,
      );
      expect(stated).toBeInTheDocument();
      // Rendered text, not a title attribute on something invisible.
      expect(stated.textContent).toMatch(/nobody was seen paying it/);
      // And the tail of the basis is reachable without leaving the page.
      expect(
        screen.getAllByText('What this figure is, in full').length,
      ).toBeGreaterThan(0);
    });

    it('says the headline is a floor and how many assets it could not value', async () => {
      apiGetData.mockResolvedValue(liveView());
      renderView();

      expect(
        await screen.findByText(/A floor, not a total\./),
      ).toBeInTheDocument();
      expect(
        screen.getByText(/4 of 11 assets in the set carry no reference/),
      ).toBeInTheDocument();
      expect(
        screen.getByText(/at least this much, not exactly this much/),
      ).toBeInTheDocument();
    });

    it('shows a backing value on a row no market ever priced', async () => {
      apiGetData.mockResolvedValue(liveView());
      renderView();

      // The two funds that produced the whole discrepancy. Each has no
      // market cap and must still publish its backing, attributed to
      // the reference basis rather than left blank.
      expect(await screen.findByText('$439,462,363.05')).toBeInTheDocument();
      expect(screen.getByText('$535,897,722.98')).toBeInTheDocument();
      expect(
        screen.getAllByText('at reference price').length,
      ).toBeGreaterThanOrEqual(2);
      // And the market column says withheld, never zero.
      expect(screen.getAllByText('Unavailable').length).toBeGreaterThan(0);
      expect(screen.queryByText('$0.00')).not.toBeInTheDocument();
    });

    it('never renders a withheld headline as zero', async () => {
      apiGetData.mockResolvedValue(
        liveView({
          reference_valuation: {
            assets_valued: 0,
            assets_unvalued: 11,
            lower_bound: false,
            basis: 'No member currently carries a reference valuation.',
          },
        }),
      );
      renderView();

      expect(
        (await screen.findAllByText('Not published')).length,
      ).toBeGreaterThan(0);
      expect(screen.queryByText('$0.00')).not.toBeInTheDocument();
      expect(
        screen.getByText(
          'No member carries an independent valuation of its instrument',
        ),
      ).toBeInTheDocument();
    });
  });
});

// splitBasis decides what a reader sees without asking. Its edge cases
// are the ones where a basis could silently stop being shown at all.
describe('splitBasis', () => {
  it('leads with the opening sentences and defers the rest', () => {
    const { lead, rest } = splitBasis('One. Two. Three. Four.');
    expect(lead).toBe('One. Two.');
    expect(rest).toBe('Three. Four.');
  });

  it('shows everything when the basis is shorter than the lead', () => {
    const { lead, rest } = splitBasis('Only one sentence.');
    expect(lead).toBe('Only one sentence.');
    expect(rest).toBe('');
  });

  it('shows text that carries no sentence break rather than dropping it', () => {
    const { lead, rest } = splitBasis('no terminator here');
    expect(lead).toBe('no terminator here');
    expect(rest).toBe('');
  });

  it('is empty for an absent basis rather than throwing', () => {
    expect(splitBasis(undefined)).toEqual({ lead: '', rest: '' });
    expect(splitBasis(null)).toEqual({ lead: '', rest: '' });
    expect(splitBasis('   ')).toEqual({ lead: '', rest: '' });
  });
});

// Money is summed in integer cents, never in floats. A page total that
// drifts in the last place is a figure nobody published.
describe('sumUsd', () => {
  it('sums served decimal strings exactly', () => {
    expect(
      sumUsd(['354959662.86', '11780447.97', '3595985.29', '3368263.79']),
    ).toEqual({ total: '373704359.91', valued: 4, unvalued: 0 });
  });

  it('does not drift the way a float sum does', () => {
    // 0.1 + 0.2 === 0.30000000000000004 in float64.
    expect(sumUsd(['0.10', '0.20']).total).toBe('0.30');
  });

  it('counts what it could not read and still totals the rest', () => {
    expect(sumUsd(['10.00', null, undefined, 'n/a', '5.50'])).toEqual({
      total: '15.50',
      valued: 2,
      unvalued: 3,
    });
  });

  it('is null, never zero, when nothing was readable', () => {
    expect(sumUsd([null, undefined])).toEqual({
      total: null,
      valued: 0,
      unvalued: 2,
    });
    expect(sumUsd([])).toEqual({ total: null, valued: 0, unvalued: 0 });
  });
});

// The operator was reading a COMBINED figure published elsewhere
// against our real-world-asset-only headline — a whole against a part.
// The page carries all three quantities so that comparison is
// like-for-like, and keeps the stablecoin arm out of the RWA total.
describe('RWAView — the wider sector', () => {
  beforeEach(() => {
    apiGetData.mockReset();
    apiGet.mockReset();
    apiGet.mockResolvedValue(
      stablecoinPage([
        { code: 'USDC', market_cap_usd: '354959662.86' },
        { code: 'PYUSD', market_cap_usd: '11780447.97' },
        { code: 'EURC', market_cap_usd: '3595985.29' },
        { code: 'yUSDC', market_cap_usd: '3368263.79' },
      ]),
    );
  });

  function sectorView() {
    return view({
      summary: {
        ...view().summary,
        assets: 11,
        market_cap_usd: '17128565.34',
        assets_valued: 3,
        assets_unvalued: 8,
        lower_bound: true,
        reference_valuation: {
          value_usd: '992488360.69',
          assets_valued: 7,
          assets_unvalued: 4,
          lower_bound: true,
          sources: ['redstone'],
          basis: 'A. B. C.',
        },
      },
    });
  }

  it('publishes the stablecoin total from the served catalogue', async () => {
    apiGetData.mockResolvedValue(sectorView());
    renderView();

    expect(await screen.findByText('Stablecoins')).toBeInTheDocument();
    expect(screen.getByText('$373,704,359.91')).toBeInTheDocument();
    expect(
      screen.getByText('4 fiat-backed tokens issued on Stellar'),
    ).toBeInTheDocument();
  });

  it('adds the two arms into a combined figure and says it is two bases', async () => {
    apiGetData.mockResolvedValue(sectorView());
    renderView();

    expect(await screen.findByText('Combined')).toBeInTheDocument();
    // 992,488,360.69 + 373,704,359.91, exactly.
    expect(screen.getByText('$1,366,192,720.60')).toBeInTheDocument();
    expect(screen.getByText(/Two bases, added\./)).toBeInTheDocument();
    expect(
      screen.getByText(/not the real-world-asset figure/),
    ).toBeInTheDocument();
  });

  it('keeps stablecoins out of the real-world-asset figure', async () => {
    apiGetData.mockResolvedValue(sectorView());
    renderView();

    // The headline is exactly what the server served for the RWA set —
    // the stablecoin arm moved it by nothing.
    expect(await screen.findByText('$992,488,360.69')).toBeInTheDocument();
    expect(screen.getByText(/Not a real-world asset\./)).toBeInTheDocument();
    expect(
      screen.getByText(/the definition above refuses the whole class/),
    ).toBeInTheDocument();
  });

  it('withholds the combined figure when the stablecoin arm fails', async () => {
    apiGetData.mockResolvedValue(sectorView());
    apiGet.mockRejectedValue(new Error('stablecoin catalogue unreachable'));
    renderView();

    // The RWA arm still publishes; the sector arm says it does not know.
    expect(await screen.findByText('$992,488,360.69')).toBeInTheDocument();
    expect(screen.getByText('Unavailable')).toBeInTheDocument();
    // And Combined is withheld rather than published from one arm — a
    // smaller claim must never wear the bigger name.
    expect(screen.getByText('Not published')).toBeInTheDocument();
    expect(screen.queryByText('$992,488,360.69extra')).not.toBeInTheDocument();
  });

  it('marks the combined figure as a floor when either arm is short', async () => {
    apiGetData.mockResolvedValue(sectorView());
    apiGet.mockResolvedValue(
      stablecoinPage([
        { code: 'USDC', market_cap_usd: '354959662.86' },
        { code: 'USDT0' }, // in the catalogue, no supply reading yet
      ]),
    );
    renderView();

    await screen.findByText('Combined');
    // Three floors: the backing, the market cap, and the sum of both.
    expect(screen.getAllByText('≥').length).toBeGreaterThanOrEqual(3);
    expect(
      screen.getByText('1 fiat-backed token issued on Stellar'),
    ).toBeInTheDocument();
  });
});
