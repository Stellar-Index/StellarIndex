import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';

import type { components } from '@/api/types';

import { RWAView } from './RWAView';

type Schemas = components['schemas'];
type View = Schemas['RWAAssetsView'];

const apiGetData = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGetData };
});

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
    premium: { status: 'published', pct: '-3.0574' },
    circulating_supply: '12336218000000',
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
            issuer_directory_tags: ['issuer', 'malicious'],
          }),
        ],
        summary: {
          ...view().summary,
          market_cap_usd: undefined,
          assets_valued: 0,
          assets_unvalued: 1,
          lower_bound: true,
        },
        by_class: [{ class: 'bond', assets: 1, assets_unvalued: 1 }],
        by_issuer: [
          { issuer: ISSUER, name: 'Etherfuse', assets: 1, assets_unvalued: 1 },
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
        assets: [asset({ valuation: { status: 'unpriced' } })],
        summary: {
          ...view().summary,
          market_cap_usd: undefined,
          assets_valued: 0,
          assets_unvalued: 1,
          lower_bound: true,
        },
        by_class: [{ class: 'bond', assets: 1, assets_unvalued: 1 }],
        by_issuer: [{ issuer: ISSUER, assets: 1, assets_unvalued: 1 }],
      }),
    );
    renderView();

    expect(await screen.findByText('Not published')).toBeInTheDocument();
    expect(screen.queryByText('$0.00')).not.toBeInTheDocument();
    expect(
      screen.getByText(/No asset in the set publishes a valuation/),
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
    expect(screen.getByText(/At least this\./)).toBeInTheDocument();
    expect(
      screen.getByText(/publish no\s+valuation and contribute nothing/),
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
                  count: 29741,
                  actor: 'operator',
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
    expect(screen.getByText('−29,741')).toBeInTheDocument();
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
    expect(screen.getByText('Not published')).toBeInTheDocument();
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
    expect(screen.getByText(/redstone/)).toBeInTheDocument();
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
        },
      }),
    );
    renderView();

    expect(await screen.findByText('Flagged')).toBeInTheDocument();
    // No figure of any kind reaches a flagged issuer's row — not this
    // platform's, and not a third party's valuation of the real
    // instrument, which would be the larger claim of the two.
    expect(screen.queryByText('$1.0740')).not.toBeInTheDocument();
    expect(screen.queryByText(/redstone/)).not.toBeInTheDocument();
    expect(screen.queryByText('-3.06%')).not.toBeInTheDocument();
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
});
