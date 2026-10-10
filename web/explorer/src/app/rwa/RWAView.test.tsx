import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';

import type { components } from '@/api/types';

import {
  RWAView,
  rwaCsvRow,
  splitBasis,
  sumStablecoins,
  sumUsd,
} from './RWAView';

type Schemas = components['schemas'];
type View = Schemas['RWAAssetsView'];
type Asset = Schemas['RWAAsset'];
type Summary = Schemas['RWASummary'];

const apiGetData = vi.hoisted(() => vi.fn());
// useAssets (the stablecoin arm) reads through apiGet, not apiGetData.
const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGetData, apiGet };
});

function stablecoinPage(
  rows: {
    code: string;
    market_cap_usd?: string;
    listing_valuation?: { status: string; value_usd?: string };
  }[] = [{ code: 'USDC', market_cap_usd: '354959662.86' }],
) {
  return { data: rows.map((r) => ({ ...r, class: 'stablecoin' })) };
}

const ISSUER = 'GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC';
const CAP = '1284500.00';
const REF = '1324956.69';

function asset(over: Partial<Asset> = {}): Asset {
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
      market_cap_usd: CAP,
    },
    reference: {
      price_usd: '1.07403800',
      source: 'redstone',
      feed: 'rwa:USTRY',
      quote: 'fiat:USD',
      as_of: '2026-09-09T15:08:40Z',
    },
    // 1,233,621.8 tokens at the 1.074038 reference: a different basis over
    // the same float, never added to the market cap.
    reference_valuation: { status: 'published', value_usd: REF },
    premium: { status: 'published', pct: '-3.0574' },
    circulating_supply: '12336218000000',
    decimals: 7,
    volume_24h_usd: '8214.55',
    first_seen_ledger: 55008233,
    observation_count: 346312,
    ...over,
  } as Asset;
}

/** by_class + by_issuer for one asset, valued on each basis or not. */
function rollups(market: boolean, reference: boolean) {
  const cells = {
    assets: 1,
    ...(market
      ? { market_cap_usd: CAP, assets_unvalued: 0 }
      : { assets_unvalued: 1 }),
    ...(reference
      ? { reference_value_usd: REF, assets_reference_unvalued: 0 }
      : { assets_reference_unvalued: 1 }),
  };
  return {
    by_class: [{ class: 'bond', ...cells }],
    by_issuer: [
      {
        issuer: ISSUER,
        name: 'Etherfuse',
        home_domain: 'etherfuse.com',
        ...cells,
      },
    ],
  } as Pick<View, 'by_class' | 'by_issuer'>;
}

const NO_REFERENCE = {
  assets_valued: 0,
  assets_unvalued: 1,
  lower_bound: true,
  basis: 'No member currently carries a reference valuation.',
};

function view(over: Partial<View> = {}): View {
  return {
    definition: {
      requirements: ['a', 'b', 'c', 'd'],
      anchor_classes: ['bond', 'commodity', 'realestate', 'stock'],
      recognition_tags: 'anchor custodian defi exchange issuer sdf'.split(' '),
      scam_flag_tags: 'malicious unsafe fraud scam hack phishing'.split(' '),
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
      market_cap_usd: CAP,
      assets_valued: 1,
      assets_unvalued: 0,
      lower_bound: false,
      earliest_first_seen_ledger: 55008233,
      assets_with_reference: 1,
      assets_compared: 1,
      reference_valuation: {
        value_usd: REF,
        assets_valued: 1,
        assets_unvalued: 0,
        lower_bound: false,
        sources: ['redstone'],
        basis:
          'Sum of circulating supply times an independent oracle valuation of the instrument.',
      },
      both_bases: { assets: 1, market_cap_usd: CAP, reference_value_usd: REF },
      basis:
        'Sum of the published market caps of the assets meeting the four-requirement definition.',
    },
    assets: [asset()],
    ...rollups(true, true),
    refused: [],
    ...over,
  } as View;
}

/** One asset that lacks the market basis, the reference basis, or both. */
function partlyValued(
  a: Partial<Asset>,
  { market = false, reference = false } = {},
): View {
  return view({
    assets: [asset(a)],
    summary: {
      ...view().summary,
      ...(market
        ? {}
        : {
            market_cap_usd: undefined,
            assets_valued: 0,
            assets_unvalued: 1,
            lower_bound: true,
            assets_compared: 0,
          }),
      ...(reference
        ? {}
        : {
            assets_with_reference: 0,
            assets_compared: 0,
            reference_valuation: NO_REFERENCE,
          }),
      both_bases: { assets: 0 },
    },
    ...rollups(market, reference),
  });
}

const LIVE_REFERENCE = {
  value_usd: '992488360.69',
  assets_valued: 7,
  assets_unvalued: 4,
  lower_bound: true,
  sources: ['redstone'],
  basis:
    'Supply times an oracle’s valuation of the instrument. THIS IS NOT A ' +
    'MARKET CAPITALISATION AND NOT AN OBSERVED PRICE: nobody was seen ' +
    'paying it. Assets with no feed count separately: a LOWER BOUND.',
};

/** The live set's shape: the backing dwarfs the observed market cap. */
function wideSummary(
  reference: Partial<Summary['reference_valuation']> = {},
): Summary {
  return {
    ...view().summary,
    assets: 11,
    issuers: 5,
    market_cap_usd: '17128565.34',
    assets_valued: 3,
    assets_unvalued: 8,
    lower_bound: true,
    assets_with_reference: 7,
    assets_compared: 3,
    reference_valuation: { ...LIVE_REFERENCE, ...reference },
  };
}

type Stage = NonNullable<View['funnel']>['stages'][number];
function stage(
  arm: string,
  name: string,
  unit: string,
  count: number,
  dropped?: Stage['dropped'],
): Stage {
  return { arm, stage: name, unit, count, dropped } as Stage;
}

// History and premium answer from their own endpoints; a rejection keeps
// those panels out of the way without feeding them the wrong payload.
function show(v: View | Error) {
  apiGetData.mockImplementation((path: string) =>
    v instanceof Error ||
    path === '/v1/rwa/history' ||
    path === '/v1/rwa/premium'
      ? Promise.reject(v instanceof Error ? v : new Error('unavailable'))
      : Promise.resolve(v),
  );
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <RWAView />
    </QueryClientProvider>,
  );
}

const present = (...texts: (string | RegExp)[]) => {
  for (const t of texts) expect(screen.getByText(t)).toBeInTheDocument();
};
const one = (over: Partial<Asset>) => view({ assets: [asset(over)] });
type Drop = NonNullable<Stage['dropped']>[number];
const drop = (reason: Drop['reason'], count: number, actor: Drop['actor']) =>
  ({ reason, count, actor }) as Drop;
const atLeast = (t: string | RegExp, n: number) =>
  expect(screen.getAllByText(t).length).toBeGreaterThanOrEqual(n);
const absent = (...texts: (string | RegExp)[]) => {
  for (const t of texts) expect(screen.queryAllByText(t)).toHaveLength(0);
};

beforeEach(() => {
  apiGetData.mockReset();
  apiGet.mockReset();
  apiGet.mockResolvedValue(stablecoinPage());
});

describe('RWAView', () => {
  it('rounds 24h volume above 2^53 from the exact decimal', async () => {
    show(one({ volume_24h_usd: '1000000004999999999' }));
    await screen.findByText('$1,000,000T');
  });

  it('gives a flagged issuer no figure of any kind, only the word', async () => {
    show(
      partlyValued({
        valuation: { status: 'withheld_issuer_flagged' },
        reference_valuation: { status: 'withheld_issuer_flagged' },
        reference: undefined,
        premium: { status: 'withheld_issuer_flagged' },
        issuer_directory_tags: ['issuer', 'malicious'],
      }),
    );

    await screen.findByText('Flagged');
    // A bare dash beside real figures reads as zero.
    atLeast('Unavailable', 2);
    // Neither this platform's figure nor a third party's instrument valuation.
    absent('$1.28M', '$0.00', '$1.0740', /redstone/, '-3.06%', '$1.32M');
  });

  it('says the total is not published rather than showing zero', async () => {
    show(
      partlyValued({
        valuation: { status: 'unpriced' },
        reference_valuation: { status: 'no_reference_feed' },
        reference: undefined,
        premium: { status: 'no_reference_feed' },
      }),
    );

    // Backing, market cap, and Combined (never published from one arm).
    expect(await screen.findAllByText('Not published')).toHaveLength(3);
    absent('$0.00');
    present(/No asset in the set publishes a market valuation/);
  });

  it('marks a partly-valued total as a lower bound', async () => {
    show(
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

    await screen.findAllByText('$1.28M');
    present(/Also a floor\./, /no market valuation and contribute/);
  });

  // The contract vocabulary is a superset of the classic one: only the
  // difference is printed, and an absent list prints no clause at all.
  it.each([
    [
      'names only the contract-only class',
      ['bond', 'commodity', 'fund', 'realestate', 'stock'],
      true,
    ],
    [
      'omits the clause when no contract vocabulary is served',
      undefined,
      false,
    ],
  ])('%s', async (_, contractClasses, clause) => {
    show(
      view({
        definition: {
          ...view().definition,
          contract_anchor_classes: contractClasses,
        },
      }),
    );

    await screen.findByText('bond, commodity, realestate, stock');
    expect(screen.queryByText('fund') !== null).toBe(clause);
    expect(screen.queryByText(/statement from a primary source/) !== null).toBe(
      clause,
    );
  });

  it('states how many candidates each requirement refused', async () => {
    show(
      view({
        refused: [
          { reason: 'issuer_not_independently_recognised', assets: 3861 },
          { reason: 'issuer_scam_flagged', assets: 289 },
        ],
      }),
    );

    await screen.findByText(/Candidates refused/);
    present(
      '(4,150)',
      'Issuer flagged by the independent directory',
      'Issuer recognised by nobody but itself',
    );
  });

  it('accounts for the population the set was narrowed from, and says who can move each drop', async () => {
    show(
      view({
        funnel: {
          balanced: true,
          basis: 'Every issuer account that could carry a SEP-1 attestation.',
          stages: [
            // Same shape of number, opposite finding and a different owner.
            stage(
              'classic',
              'issuers_with_home_domain',
              'issuer_accounts',
              44376,
              [
                drop('sep1_attestation_never_fetched', 1, 'operator'),
                drop('domain_served_no_sep1_attestation', 29740, 'issuer'),
              ],
            ),
            stage(
              'classic',
              'issuers_with_sep1_attestation',
              'issuer_accounts',
              14635,
            ),
            stage('classic', 'assets_served', 'assets', 1),
          ],
        },
      }),
    );

    expect(await screen.findByText(/Where the population went/)).toBeVisible();
    present(
      '44,376',
      '14,635',
      /stellar\.toml not fetched yet/,
      /ours to fix/,
      '−1',
      /Domain served no stellar\.toml/,
      /the issuer’s to fix/,
      '−29,740',
    );
  });

  it('says so when the coverage accounting could not be measured', async () => {
    show(
      view({
        funnel: {
          balanced: false,
          basis: 'Not measured.',
          stages: [stage('classic', 'assets_served', 'assets', 1, [])],
        },
      }),
    );
    await screen.findByText(/could not be measured/);
  });

  it('renders the empty set as a statement about evidence, not as a zero total', async () => {
    const basis = 'No asset currently meets the definition.';
    show(
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
            ...NO_REFERENCE,
            assets_unvalued: 0,
            lower_bound: false,
            basis,
          },
          both_bases: { assets: 0 },
          basis,
        },
        by_class: [],
        by_issuer: [],
      }),
    );

    await screen.findByText('No asset currently meets the definition');
    expect(screen.getAllByText('Not published')).toHaveLength(3);
    absent('$0.00');
  });

  it('surfaces a failed load instead of an empty set', async () => {
    show(new Error('502 Bad Gateway'));

    await screen.findByText('Failed to load real-world assets');
    absent(/No asset currently meets the definition/);
  });

  it('shows issuer identity, the instrument valuation, the signed gap, and the backing labelled as a claim', async () => {
    show(view());

    await screen.findByText('$1.0740');
    // A code plus a friendly name is indistinguishable from an impersonator's.
    atLeast(/GCRYUG…ATMYWC/, 1);
    atLeast('Etherfuse', 1);
    // Headline, row and both breakdowns: each level sums the one below.
    atLeast('$1.28M', 3);
    atLeast(/redstone/, 1);
    present('-3.06%', /carry an independent oracle valuation/);
    // The backing on the row and in both breakdowns, never named a market cap.
    atLeast('$1.32M', 3);
    present(
      'Value of the backing',
      /Circulating supply × an independent oracle/,
    );
    atLeast('at reference price', 1);
    expect(
      screen.getByTitle(/Not a market capitalisation: nobody was observed/),
    ).toBeInTheDocument();
    present('Two different kinds of number.');
    const anyText = (s: string) =>
      screen.getAllByText((_, el) => (el?.textContent ?? '').includes(s))
        .length;
    expect(anyText('They are never added together')).toBeGreaterThan(0);
    // Bound pairs carry their issuer: a code is not an identity.
    present(
      'Which tokens are compared against an instrument.',
      /rwa:USTRY/,
      /rwa:CETES/,
    );
    expect(anyText('anyone can issue a token called USTRY')).toBeGreaterThan(0);
  });

  // Treasury premiums are fractions of a percent; "0.00%" would read as par.
  // The exact served gap stays in the title where the display rounds.
  it.each([
    ['-0.0040', '-0.004%', ['0.00%', '-0.00%']],
    ['-0.0004', '-0.0004%', ['0.00%', '-0.000%']],
    ['-0.0166', '-0.02%', ['0.00%']],
  ])('renders a %s gap as %s, never par', async (pct, shown, hidden) => {
    show(one({ premium: { status: 'published', pct } }));
    await screen.findByText(shown);
    absent(...hidden);
    screen.getAllByTitle(new RegExp(`${pct.replace('.', '\\.')}%`));
  });

  // Both the instrument-value and the premium cell state the reason in words.
  it.each([
    ['reference_not_instrument_scoped', /off-chain quantity in its own unit/],
    ['reference_not_bound', /not one of the pairs bound to an oracle feed/],
    [
      'reference_unavailable',
      /This is an outage, not a statement that no valuation exists/,
    ],
  ])(
    'renders a %s comparison as a stated reason, never 0.00%',
    async (status, reason) => {
      show(
        one({ reference: undefined, premium: { status } as Asset['premium'] }),
      );

      await screen.findByText('USTRY');
      absent('-3.06%', '0.00%', '-0.00%');
      atLeast('Unavailable', 2);
      expect(screen.getAllByTitle(reason).length).toBeGreaterThanOrEqual(2);
      if (status === 'reference_not_bound') {
        expect(
          screen.queryByTitle(/no oracle is currently publishing/),
        ).not.toBeInTheDocument();
      }
    },
  );

  it('labels a stale instrument valuation rather than hiding it', async () => {
    show(
      one({
        reference: {
          ...asset().reference!,
          as_of: '2026-08-01T00:00:00Z',
          provenance: 'oracle_instrument_nav',
          stale: true,
        },
      }),
    );

    await screen.findByText('$1.0740');
    present('stale', '-3.06%');
  });

  it('values an asset nobody trades without inventing a market cap for it', async () => {
    show(
      partlyValued(
        {
          code: 'USDY',
          valuation: { status: 'unpriced' },
          premium: { status: 'no_market_price' },
        },
        { reference: true },
      ),
    );

    await screen.findByText('USDY');
    atLeast('$1.32M', 3);
    absent('$1.28M');
    expect(screen.getAllByText('Not published')).toHaveLength(1);
    atLeast('Unavailable', 2);
  });

  it('states why a reference valuation is missing rather than showing a dash', async () => {
    show(
      partlyValued(
        {
          reference_valuation: { status: 'supply_unavailable' },
          circulating_supply: undefined,
        },
        { market: true },
      ),
    );

    await screen.findByText('USTRY');
    absent('$1.32M');
    screen.getAllByTitle(/no circulating-supply reading does/);
  });

  it('describes a third-party-curated basis as curation, not an oracle feed', async () => {
    show(one({ basis: 'third_party_curated', anchor_class: undefined }));

    await screen.findByText('USTRY');
    present(/listed by a named third-party curator/);
    absent('priced by an independent oracle feed');
  });
});

// The funnel's arms narrow different populations; the page separates them
// so nobody subtracts one arm's last stage from the next one's first.
describe('RWAView — contract arm', () => {
  const CONTRACT = 'CAAQEAYEAUDAOCAJBIFQYDIOB4IBCEQTCQKRMFYYDENBWHA5DYPSBFLM';
  const contractAsset: Partial<Asset> = {
    asset_id: CONTRACT,
    code: '',
    issuer: '',
    contract_id: CONTRACT,
    symbol: 'USTRY',
    basis: 'contract_oracle_rwa_feed',
  };

  it('separates the contract arm in prose and names the recognised issuers it holds no token for', async () => {
    show(
      view({
        assets: [
          asset({
            ...contractAsset,
            slug: CONTRACT,
            name: 'Example Treasury Fund',
          }),
        ],
        funnel: {
          balanced: true,
          basis: 'measured',
          stages: [
            stage(
              'contract',
              'curated_directory_entries',
              'directory_addresses',
              18439,
              [drop('directory_entry_names_an_account', 18000, 'definition')],
            ),
            stage('contract', 'directory_recognised_contracts', 'contracts', 1),
            stage('contract', 'contract_assets_served', 'assets', 1),
          ],
        },
        unreached_entities: [
          {
            address: 'GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5',
            name: 'Franklin Templeton',
            domain: 'franklintempleton.com',
            tags: ['issuer'],
          },
        ],
      } as Partial<View>),
    );

    await screen.findByText('Tokens issued by a contract');
    present(
      'Addresses in the independent directory',
      /Names an entity, not a token contract/,
    );
    // Wire vocabulary never reaches the page.
    absent(
      'curated_directory_entries',
      'directory_entry_names_an_account',
      'directory_addresses',
    );
    present(
      'Recognised issuers we hold no token for',
      'Franklin Templeton',
      'franklintempleton.com',
    );
  });

  it('says a contract token is not reference-valued, and why', async () => {
    show(
      partlyValued(
        {
          ...contractAsset,
          reference_valuation: { status: 'reference_contract_not_bound' },
          reference: undefined,
          premium: { status: 'reference_contract_not_bound' },
        },
        { market: true },
      ),
    );

    // A stated refusal: "we will not" must read differently from "we forgot".
    await screen.findAllByTitle(/nothing binds a contract address/);
    absent('$1.32M');
    atLeast('$1.28M', 1);
  });

  it('accounts in the funnel for every asset the oracles do not price', async () => {
    show(
      view({
        funnel: {
          balanced: true,
          basis: 'Every issuer account that could carry a SEP-1 attestation.',
          stages: [
            stage('classic', 'assets_served', 'assets', 3),
            stage('valuation', 'assets_served_all_arms', 'assets', 3, [
              drop('reference_not_instrument_scoped', 1, 'definition'),
              drop('supply_unavailable', 1, 'operator'),
            ]),
            stage('valuation', 'assets_reference_valued', 'assets', 1),
          ],
        },
      }),
    );

    await screen.findByText('Whose backing is independently priced');
    present(
      'Whose backing an independent oracle prices',
      'No circulating-supply reading',
      'The feed prices an ounce, not a token',
    );
    atLeast(/ours to fix/, 1);
    atLeast(/the rule working/, 1);
  });

  // The two largest funds are bought and held: no observed market cap, most
  // of the backing. Leading with market cap read as missing data.
  describe('leads with the basis the set actually has', () => {
    const fund = (code: string, name: string, value_usd: string) =>
      asset({
        asset_id: `${code}-${ISSUER}`,
        code,
        slug: code.toLowerCase(),
        name,
        valuation: { status: 'unpriced' },
        premium: { status: 'no_market_price' },
        reference_valuation: { status: 'published', value_usd },
      });

    function liveView(reference: Partial<Summary['reference_valuation']> = {}) {
      return view({
        assets: [
          asset(),
          fund(
            'BENJI',
            'Franklin OnChain U.S. Government Money Fund',
            '439462363.05',
          ),
          fund('USDY', 'Ondo US Dollar Yield', '535897722.98'),
        ],
        summary: wideSummary(reference),
      });
    }

    it('puts the reference total first, states its basis as text, and calls it a floor', async () => {
      show(liveView());

      const backing = await screen.findByText('Value of the backing');
      const market = screen.getByText('Market cap (observed trades)');
      expect(
        backing.compareDocumentPosition(market) &
          Node.DOCUMENT_POSITION_FOLLOWING,
      ).toBeTruthy();
      atLeast('$992.49M', 1);
      atLeast('$17.13M', 1);
      // Page text from the SERVED basis, not a tooltip.
      expect(
        screen.getByText(
          /THIS IS NOT A MARKET CAPITALISATION AND NOT AN OBSERVED PRICE/,
        ).textContent,
      ).toMatch(/nobody was seen paying it/);
      atLeast('What this figure is, in full', 1);
      present(
        /A floor, not a total\./,
        /4 unvalued/,
        /of 11 assets in the set carry no reference/,
        /at least this much, not exactly this much/,
      );
      // The unpriced funds still publish backing; the market column says withheld.
      present('$439.46M', '$535.9M');
      atLeast('at reference price', 2);
      atLeast('Unavailable', 1);
      absent('$0.00');
    });

    it('never renders a withheld headline as zero', async () => {
      show(
        liveView({
          value_usd: undefined,
          assets_valued: 0,
          assets_unvalued: 11,
          lower_bound: false,
          sources: undefined,
        }),
      );

      await screen.findAllByText('Not published');
      absent('$0.00');
      present('No member carries an independent valuation of its instrument');
    });

    // The total can mix oracle NAVs, listing prices and prospectus CNAVs;
    // naming "an independent oracle" is only true for a pure-oracle total.
    it.each([
      [
        'names the published reference valuation when provenances mix',
        ['redstone', 'issuer-nav-page'],
        ['oracle_instrument_nav', 'listing_platform_price'],
        [
          /Circulating supply × the published reference valuation/,
          /2 different kinds of claim, not/,
        ],
        [
          /Circulating supply × an independent oracle/,
          /each row names its own feed/,
        ],
      ],
      [
        'keeps "an independent oracle" and "feed" when every row is oracle-priced',
        ['redstone'],
        ['oracle_instrument_nav'],
        [
          /Circulating supply × an independent oracle/,
          /each row names its own feed/,
        ],
        [],
      ],
    ] as const)('%s', async (_, sources, provenances, shown, hidden) => {
      show(liveView({ sources: [...sources], provenances: [...provenances] }));

      await screen.findByText(shown[0]);
      present(...shown);
      absent(...hidden);
    });

    it('splits the sources by provenance and drops every oracle claim when the total mixes kinds', async () => {
      const cnavSource =
        'Constant NAV money-market fund; issuer NAV page https://example.test/nav (read 2026-09-16; review by 2026-12-15)';
      const ref = (
        source: string,
        provenance: Schemas['RWAReference']['provenance'],
      ) => ({
        price_usd: '1.00000000',
        source,
        feed: 'rwa:X',
        quote: 'fiat:USD',
        as_of: '2026-09-09T15:08:40Z',
        provenance,
      });
      const mixed = view({
        assets: [
          asset({ reference: ref('redstone', 'oracle_instrument_nav') }),
          asset({
            asset_id: `USDT0-${ISSUER}`,
            code: 'USDT0',
            slug: 'usdt0',
            reference: ref('coingecko', 'listing_platform_price'),
          }),
          asset({
            asset_id: `BENJI-${ISSUER}`,
            code: 'BENJI',
            slug: 'benji',
            reference: ref(cnavSource, 'prospectus_constant_nav'),
          }),
        ],
        funnel: {
          balanced: true,
          basis: 'Every served asset.',
          stages: [stage('valuation', 'assets_reference_valued', 'assets', 3)],
        },
        summary: {
          ...view().summary,
          assets: 3,
          assets_with_reference: 3,
          reference_valuation: {
            ...LIVE_REFERENCE,
            value_usd: '3974870.07',
            assets_valued: 3,
            assets_unvalued: 0,
            lower_bound: false,
            sources: ['coingecko', cnavSource, 'redstone'],
            provenances: [
              'listing_platform_price',
              'oracle_instrument_nav',
              'prospectus_constant_nav',
            ],
          },
        },
      });
      const { container } = show(mixed);

      const origin = (
        await screen.findByText('Where the reference comes from.')
      ).parentElement;
      expect(origin).toHaveTextContent(
        'listing-platform prices for the token: coingecko; oracle feeds: redstone; constant NAV fixed by the fund’s prospectus: 1 asset, each row naming its NAV page',
      );
      expect(origin).not.toHaveTextContent('issuer NAV page https://');
      expect(origin).toHaveTextContent('carry a published reference valuation');
      expect(origin).not.toHaveTextContent(/independent oracle/);
      present('Whose backing carries a published reference valuation');
      absent(
        'Whose backing an independent oracle prices',
        /what an independent oracle says/,
      );
      expect(
        screen.getAllByText(/a listing platform's price for the token/),
      ).toHaveLength(2);

      await screen.findByText('Where the population went');
      // "an independent oracle publishes a feed" is the admission rule, not a valuation.
      expect(document.body).not.toHaveTextContent(
        /independent oracle(’s|'s)? (says|valuation|prices)|independently priced|oracle’s valuation of the backing/i,
      );
      const titles = [...container.querySelectorAll('[title]')].map(
        (el) => el.getAttribute('title') ?? '',
      );
      expect(titles.some((t) => t.includes('issuer NAV page'))).toBe(false);
      expect(titles).toContain(
        'Circulating supply times the NAV fixed by the fund’s prospectus. Not a market capitalisation: nobody was observed paying this, and the liquidity and price gates behind the market-cap column cannot check it.',
      );
      expect(
        titles.some((t) => t.includes('coingecko’s price for the token')),
      ).toBe(true);
    });
  });
});

// The page carries RWA, stablecoins and their sum so a combined figure
// published elsewhere compares like-for-like, without stablecoins in the RWA total.
describe('RWAView — the wider sector', () => {
  const USDC = { code: 'USDC', market_cap_usd: '354959662.86' };
  const sectorView = () => view({ summary: wideSummary() });

  it('publishes the stablecoin total and a combined figure, keeping stablecoins out of the RWA headline', async () => {
    apiGet.mockResolvedValue(
      stablecoinPage([
        USDC,
        { code: 'PYUSD', market_cap_usd: '11780447.97' },
        { code: 'EURC', market_cap_usd: '3595985.29' },
        { code: 'yUSDC', market_cap_usd: '3368263.79' },
      ]),
    );
    show(sectorView());

    await screen.findByText('Stablecoins');
    present('$373.7M', '4 fiat-backed tokens issued on Stellar');
    // 992,488,360.69 + 373,704,359.91, exactly.
    await screen.findByText('Combined');
    present(
      '$1.37B',
      /Two bases, added\./,
      /not the real-world-asset figure/,
      '$992.49M',
      /Not a real-world asset\./,
      /the definition above refuses the whole class/,
    );
  });

  it('withholds the combined figure when the stablecoin arm fails', async () => {
    apiGet.mockRejectedValue(new Error('stablecoin catalogue unreachable'));
    show(sectorView());

    await screen.findByText('$992.49M');
    present('Unavailable');
    // A smaller claim must never wear the bigger name.
    present('Not published');
  });

  // Only an observed market cap or a `published` listing valuation counts,
  // and a listing-priced row turns the tile into a mixed-basis total.
  it.each([
    [
      'marks the totals as floors when a token has no supply reading',
      [USDC, { code: 'USDT0' }],
      ['1 fiat-backed token issued on Stellar'],
      [],
    ],
    [
      'sums a listing-priced stablecoin and says the total mixes bases',
      [
        USDC,
        {
          code: 'USDT0',
          listing_valuation: { status: 'published', value_usd: '2581052.90' },
        },
      ],
      [
        '$357.54M',
        '2 fiat-backed tokens issued on Stellar, 1 of them listing-priced',
        /This total mixes two bases\./,
        /provenance: listing_platform_price/,
        /Three bases, added\./,
      ],
      [/Two bases, added\./],
    ],
    [
      'prefers an observed market cap over a listing price on the same row',
      [
        {
          ...USDC,
          listing_valuation: { status: 'published', value_usd: '999999999.00' },
        },
      ],
      ['$354.96M', /Two bases, added\./],
      [/This total mixes two bases\./],
    ],
    [
      'ignores a listing valuation that is not published',
      [
        USDC,
        { code: 'USDT0', listing_valuation: { status: 'listing_unavailable' } },
      ],
      ['$354.96M', '1 fiat-backed token issued on Stellar'],
      [/This total mixes two bases\./],
    ],
  ])('%s', async (name, rows, shown, hidden) => {
    apiGet.mockResolvedValue(stablecoinPage(rows));
    show(sectorView());

    await screen.findByText('Combined');
    present(...shown);
    absent(...hidden);
    if (name.startsWith('marks')) {
      // The backing, the market cap, and the sum of both.
      atLeast('≥', 3);
    }
  });
});

describe('RWAView — curated arm', () => {
  const PUBLISHED = {
    total_usd: '4004795860.00',
    as_of: '2025-08-31',
    executed_at: '2026-09-17T04:58:12Z',
    by_subclass: [
      { subclass: 'US Treasuries', value_usd: '3100000000.00' },
      { subclass: 'Private Credit', value_usd: '904795860.00' },
    ],
    series: [
      { month_end: '2025-07-31', value_usd: '3900000000.10' },
      { month_end: '2025-08-31', value_usd: '4004795860.00' },
    ],
    source: 'dune query 6961845 / 6961847',
  };
  const curated = (over: Partial<NonNullable<View['curated']>>) =>
    ({
      curator: 'dune:stellar',
      status: 'served',
      assets: 0,
      also_verified: 0,
      assets_valued: 0,
      census: census(0),
      basis:
        'Rows a named third-party curator lists as tokenized real-world assets on Stellar.',
      ...over,
    }) as View['curated'];
  function census(n: number) {
    return {
      entries: n,
      contracts: n,
      classic: 0,
      priced: n,
      priced_classic: 0,
      stale: 0,
    };
  }
  const headline = { selector: '.text-3xl, .text-4xl' };

  it('renders served rows apart: three figures, the curator-only badge, and an unchanged verified headline', async () => {
    const VUME = 'CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4';
    show(
      view({
        curated_assets: [
          {
            ...asset(),
            asset_id: VUME,
            code: '',
            issuer: '',
            contract_id: VUME,
            symbol: 'TPT30',
            slug: VUME,
            name: 'VuMe Bond 2030',
            basis: 'third_party_curated',
            recognition: 'third_party_curator',
            anchor_class: undefined,
            anchor_asset: undefined,
            curator: {
              curator: 'dune:stellar',
              company: 'Realiz',
              subclass: 'Corporate Credit',
              also_verified: false,
            },
            reference: {
              ...asset().reference!,
              price_usd: '1.1174',
              source: 'dune',
              provenance: 'curator_uploaded_price',
            },
            reference_valuation: {
              status: 'published',
              value_usd: '558700000.00',
            },
            premium: { status: 'reference_is_a_listing_price' },
          },
        ],
        curated: curated({
          assets: 1,
          assets_valued: 1,
          additional_value_usd: '558700000.00',
          combined_value_usd: '560024956.69',
          verified_value_usd: REF,
          census: census(1),
          published: { ...PUBLISHED, gap_vs_verified_usd: '4003470903.31' },
        }),
      } as Partial<View>),
    );

    await screen.findByText(/As a third-party curator counts it/);
    present(
      'Counted by the curator, not verified here',
      'Published by the curator',
    );
    atLeast('$558.7M', 1);
    present('$560.02M', 'No — curator only', 'Realiz');
    atLeast('$1.32M', 1);
    expect(screen.queryByText('$560.02M', headline)).not.toBeInTheDocument();
  });

  it('draws the comparison without a published panel when no published block is served', async () => {
    show(view({ curated: curated({ verified_value_usd: REF }) }));

    await screen.findByText('Counted by the curator, not verified here');
    absent('Published by the curator');
  });

  it('renders the published total, its month and the signed gap when no per-asset row is readable', async () => {
    show(
      view({
        curated: curated({
          status: 'published_totals',
          verified_value_usd: '345599978.73',
          published: { ...PUBLISHED, gap_vs_verified_usd: '3659195881.27' },
        }),
      }),
    );

    await screen.findByText(/As a third-party curator counts it/);
    present('Published by the curator', '$4B', /for August 2025, computed/);
    // Signed, and published − verified, never the other way round.
    present(
      '+$3.66B',
      'US Treasuries',
      '$904.8M',
      /2 months published · dune query 6961845 \/ 6961847/,
    );
    // An empty "counted by the curator" cell would read as "counts nothing".
    absent('Counted by the curator, not verified here', 'No — curator only');
    expect(screen.queryByText('$4B', headline)).not.toBeInTheDocument();
  });

  it('renders no curated panel when the deployment has no curated reader', async () => {
    show(view({ curated: curated({ status: 'unwired', basis: '' }) }));
    await screen.findByText(/The set/);
    absent(/As a third-party curator counts it/);
  });
});

describe('splitBasis', () => {
  it.each([
    ['One. Two. Three. Four.', 'One. Two.', 'Three. Four.'],
    ['Only one sentence.', 'Only one sentence.', ''],
    // No sentence break: shown whole, never dropped.
    ['no terminator here', 'no terminator here', ''],
    [undefined, '', ''],
    [null, '', ''],
    ['   ', '', ''],
  ])('splits %j into lead %j and rest %j', (basis, lead, rest) => {
    expect(splitBasis(basis)).toEqual({ lead, rest });
  });
});

// Money is summed in integer cents; null, never zero, when nothing was read.
describe('sumUsd', () => {
  it.each([
    [
      ['354959662.86', '11780447.97', '3595985.29', '3368263.79'],
      { total: '373704359.91', valued: 4, unvalued: 0 },
    ],
    // 0.1 + 0.2 === 0.30000000000000004 in float64.
    [['0.10', '0.20'], { total: '0.30', valued: 2, unvalued: 0 }],
    [
      ['10.00', null, undefined, 'n/a', '5.50'],
      { total: '15.50', valued: 2, unvalued: 3 },
    ],
    [[null, undefined], { total: null, valued: 0, unvalued: 2 }],
    [[], { total: null, valued: 0, unvalued: 0 }],
  ])('sums %j', (values, expected) => {
    expect(sumUsd(values)).toEqual(expected);
  });
});

describe('sumStablecoins', () => {
  it.each([
    [
      [
        { market_cap_usd: '1.00' },
        { listing_valuation: { status: 'published', value_usd: '2.50' } },
        { listing_valuation: { status: 'not_listed' } },
        {},
      ],
      { total: '3.50', valued: 2, unvalued: 2, listingPriced: 1 },
    ],
    [
      [{ listing_valuation: { status: 'no_supply' } }],
      { total: null, valued: 0, unvalued: 1, listingPriced: 0 },
    ],
  ])('totals %j', (rows, expected) => {
    expect(sumStablecoins(rows)).toEqual(expected);
  });
});

describe('rwaCsvRow', () => {
  it('carries both valuations as served strings with their statuses', () => {
    const row = rwaCsvRow(asset());
    expect(row.market_cap_usd).toBe(CAP);
    expect(row.reference_value_usd).toBe(REF);
    expect(row.premium_pct).toBe('-3.0574');
    expect(row.circulating_supply).toBe('12336218000000');
    expect(row.market_cap_status).toBe('published');
  });

  it('leaves a withheld value blank and keeps the reason', () => {
    const row = rwaCsvRow(
      asset({
        valuation: { status: 'withheld_low_liquidity' },
        reference: undefined,
      }),
    );
    expect(row.market_cap_usd).toBe('');
    expect(row.market_cap_status).toBe('withheld_low_liquidity');
    expect(row.reference_source).toBe('');
  });
});
