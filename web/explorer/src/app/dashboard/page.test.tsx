import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return { ...actual, useMe: vi.fn() };
});

const listKeys = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  listKeys,
}));

vi.mock('next/navigation', async () => {
  const actual =
    await vi.importActual<typeof import('next/navigation')>('next/navigation');
  return {
    ...actual,
    useRouter: () => ({ ...actual.useRouter(), replace: vi.fn() }),
  };
});

import { useMe, type MeResponse } from '@/api/hooks';

import AccountOverviewPage from './page';

afterEach(() => {
  listKeys.mockReset();
});

function renderDashboard(me: MeResponse) {
  vi.mocked(useMe).mockReturnValue({
    isLoading: false,
    isError: false,
    data: me,
  } as ReturnType<typeof useMe>);
  listKeys.mockResolvedValue([]);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AccountOverviewPage />
    </QueryClientProvider>,
  );
}

// GH-1074, render side. A partner comped to 5,000/min is what the API
// serves on `account.rate_limit_per_min` (platform.Account.EffectiveRateLimitPerMin
// for override 5000); the 100,000 partner tier number may only appear
// labelled "Plan ceiling", never as the account's limit.
describe('AccountOverviewPage — GH-1074 enforced rate limit', () => {
  it('renders the served limit and labels the tier number as the plan ceiling', async () => {
    renderDashboard({
      user: { id: 'u1', email: 'owner@acme.example' },
      account: {
        id: 'acct-1',
        slug: 'acme',
        tier: 'partner',
        status: 'active',
        rate_limit_per_min: 5000,
      },
    } as MeResponse);

    expect(await screen.findByText('5,000')).toBeInTheDocument();
    expect(
      screen.getByText('5,000 req/min default key limit'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('Plan ceiling: 100,000 req/min'),
    ).toBeInTheDocument();
    expect(screen.queryByText('100,000')).not.toBeInTheDocument();
    expect(screen.queryByText('100,000 req/min')).not.toBeInTheDocument();
  });

  it('never falls back to the tier ceiling when no limit is served', async () => {
    renderDashboard({
      user: { id: 'u1', email: 'owner@acme.example' },
      account: {
        id: 'acct-1',
        slug: 'acme',
        tier: 'partner',
        status: 'active',
      },
    } as MeResponse);

    expect(await screen.findByText('Custom limits')).toBeInTheDocument();
    expect(screen.queryByText('100,000')).not.toBeInTheDocument();
  });
});

// GH-1073: an expired key no longer authenticates, so it is not an
// "Active key" even though it is unrevoked.
describe('AccountOverviewPage — GH-1073 expired keys are not active', () => {
  it('counts an expired, unrevoked key as expired, not active', async () => {
    vi.mocked(useMe).mockReturnValue({
      isLoading: false,
      isError: false,
      data: {
        user: { id: 'u1', email: 'owner@acme.example' },
        account: { id: 'acct-1', slug: 'acme', tier: 'free', status: 'active' },
      } as MeResponse,
    } as ReturnType<typeof useMe>);
    listKeys.mockResolvedValue([
      {
        id: 'k1',
        name: 'old',
        key_prefix: 'sip_oldoldol',
        tier: 'apikey',
        rate_limit_per_min: 60,
        created_at: '2026-01-01T00:00:00Z',
        expires_at: '2026-02-01T00:00:00Z',
      },
    ]);
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <AccountOverviewPage />
      </QueryClientProvider>,
    );

    expect(await screen.findByText('1 expired')).toBeInTheDocument();
    expect(screen.queryByText('all active')).not.toBeInTheDocument();
  });
});
