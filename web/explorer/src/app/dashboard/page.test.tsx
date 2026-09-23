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
    await vi.importActual<typeof import('next/navigation')>(
      'next/navigation',
    );
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

// TestAccountMe_SessionEffectiveLimits (Go) pins the API side of
// GH-1074; this pins the render side: the dashboard's "Rate limit"
// stat must show the EFFECTIVE (override-resolved) limit the API now
// serves on `account.rate_limit_per_min`, not the partner tier
// ceiling (100,000) computed from `tierCeiling`. Pre-fix, MetricStrip
// read only the tier and derived the ceiling client-side — a partner
// comped to 5,000/min would have rendered "100,000".
describe('AccountOverviewPage — GH-1074 effective rate limit', () => {
  it('renders the effective limit from account.rate_limit_per_min, not the tier ceiling', async () => {
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

    const value = await screen.findByText('5,000');
    expect(value).toBeInTheDocument();
    expect(screen.queryByText('100,000')).not.toBeInTheDocument();
  });
});
