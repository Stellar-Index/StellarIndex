import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';

import type { components } from '@/api/types';

import { RWAView } from './RWAView';

type Schemas = components['schemas'];
type View = Schemas['RWAAssetsView'];

const apiGetData = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
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
    valuation: { status: 'published', price_usd: '1.0412', market_cap_usd: '1284500.00' },
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
      recognition_tags: ['anchor', 'custodian', 'defi', 'exchange', 'issuer', 'sdf'],
      scam_flag_tags: ['malicious', 'unsafe', 'fraud', 'scam', 'hack', 'phishing'],
      documentation_url: 'https://stellarindex.io/docs/methodology/rwa-definition',
    },
    summary: {
      assets: 1,
      issuers: 1,
      market_cap_usd: '1284500.00',
      assets_valued: 1,
      assets_unvalued: 0,
      lower_bound: false,
      earliest_first_seen_ledger: 55008233,
      basis: 'Sum of the published market caps of the assets meeting the four-requirement definition.',
    },
    assets: [asset()],
    by_class: [{ class: 'bond', assets: 1, market_cap_usd: '1284500.00', assets_unvalued: 0 }],
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
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
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
    expect(screen.getAllByText('$1,284,500.00').length).toBeGreaterThanOrEqual(3);
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
        by_issuer: [{ issuer: ISSUER, name: 'Etherfuse', assets: 1, assets_unvalued: 1 }],
      }),
    );
    renderView();

    // The money cells say the word, not a figure and not a bare dash: a
    // dash beside real figures reads as zero.
    expect((await screen.findAllByText('Unavailable')).length).toBeGreaterThanOrEqual(2);
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
        assets: [asset(), asset({ asset_id: `TESOURO-${ISSUER}`, code: 'TESOURO', valuation: { status: 'unpriced' } })],
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

    expect((await screen.findAllByText('$1,284,500.00')).length).toBeGreaterThan(0);
    expect(screen.getByText(/At least this\./)).toBeInTheDocument();
    expect(screen.getByText(/publish no\s+valuation and contribute nothing/)).toBeInTheDocument();
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
    expect(screen.getByText('Issuer flagged by the independent directory')).toBeInTheDocument();
    expect(screen.getByText('Issuer recognised by nobody but itself')).toBeInTheDocument();
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
          basis: 'No asset currently meets the definition.',
        },
        by_class: [],
        by_issuer: [],
      }),
    );
    renderView();

    expect(await screen.findByText('No asset currently meets the definition')).toBeInTheDocument();
    expect(screen.getByText('Not published')).toBeInTheDocument();
    expect(screen.queryByText('$0.00')).not.toBeInTheDocument();
  });

  it('surfaces a failed load instead of an empty set', async () => {
    apiGetData.mockRejectedValue(new Error('502 Bad Gateway'));
    renderView();

    expect(await screen.findByText('Failed to load real-world assets')).toBeInTheDocument();
    // An outage must not be presented as "no RWAs exist".
    expect(screen.queryByText(/No asset currently meets the definition/)).not.toBeInTheDocument();
  });
});
