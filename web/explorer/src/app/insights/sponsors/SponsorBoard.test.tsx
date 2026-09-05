import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SponsorBoard } from './SponsorBoard';

// Coverage floor is protocol 14's activation — a real chain fact, and
// deliberately not genesis. The page must present it as the feature's
// start rather than as a gap.
const sponsorsFixture = {
  data: {
    sponsors: [
      {
        rank: 1,
        account: 'GAUA7XL5K54CC2DDGP77FJ2YBHRJLT36CPZDXWPM6MP7MANOGG77PNJU',
        sponsorships_started: 30_512,
        distinct_sponsored: 28_011,
        revocations_issued: 0,
        first_ledger: 32_747_295,
        last_ledger: 64_277_239,
        first_seen_at: '2021-02-16T18:21:00Z',
        last_seen_at: '2026-09-05T00:14:59Z',
      },
      {
        // Re-sponsors heavily and revokes: started is 10x distinct.
        rank: 2,
        account: 'GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU',
        sponsorships_started: 20_360,
        distinct_sponsored: 2_036,
        revocations_issued: 261,
        first_ledger: 64_000_032,
        last_ledger: 64_277_224,
        first_seen_at: '2026-08-17T19:43:29Z',
        last_seen_at: '2026-09-05T00:13:36Z',
      },
    ],
    totals: {
      sponsors: 41_208,
      sponsorships_started: 11_487_143,
      distinct_sponsored: 9_120_044,
      revocations_issued: 88_525,
    },
    coverage: {
      from_ledger: 32_747_295,
      thru_ledger: 64_277_243,
      from_time: '2021-02-16T18:21:00Z',
      thru_time: '2026-09-05T00:15:11Z',
      ambiguous_transactions: 0,
    },
    computed_at: '2026-09-05T04:30:00Z',
  },
  as_of: '2026-09-05T04:45:00Z',
  flags: {},
};

vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return {
    ...mod,
    apiGet: vi.fn(async () => sponsorsFixture),
  };
});

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe('SponsorBoard', () => {
  beforeEach(() => vi.clearAllMocks());

  it('keeps sponsorships started and distinct accounts as separate columns', async () => {
    renderWithQuery(<SponsorBoard />);

    expect(await screen.findByText('Sponsors')).toBeInTheDocument();
    expect(screen.getByText('30,512')).toBeInTheDocument();
    expect(screen.getByText('28,011')).toBeInTheDocument();
    // The heavy re-sponsor: 20,360 started over 2,036 accounts = 10x.
    expect(screen.getByText('20,360')).toBeInTheDocument();
    expect(screen.getByText('2,036')).toBeInTheDocument();
    expect(screen.getByText('10.0×')).toBeInTheDocument();
    expect(screen.getByText('261')).toBeInTheDocument();
  });

  it('presents the protocol-14 floor as the feature’s start, not a gap', async () => {
    renderWithQuery(<SponsorBoard />);

    const strip = await screen.findByText(/where sponsorship began/i);
    expect(strip.textContent).toContain('32,747,295');
    expect(strip.textContent).toContain('64,277,243');
    expect(strip.textContent).toMatch(/whole history of the feature/i);
  });

  it('shows no live-sponsorship figure anywhere on the board', async () => {
    const { container } = renderWithQuery(<SponsorBoard />);
    await screen.findByText('Sponsors');

    const text = (container.textContent ?? '').toLowerCase();
    for (const banned of [
      'currently sponsoring',
      'active sponsorships',
      'sponsoring now',
    ]) {
      expect(text).not.toContain(banned);
    }
  });

  it('falls back to a warming message rather than an empty board', async () => {
    const mod = await import('@/api/client');
    vi.mocked(mod.apiGet).mockRejectedValueOnce(new Error('503'));

    renderWithQuery(<SponsorBoard />);

    expect(await screen.findByText(/warming/i)).toBeInTheDocument();
  });
});
