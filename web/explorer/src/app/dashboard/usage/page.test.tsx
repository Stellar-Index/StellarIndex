import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '@/api/account';

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return { ...actual, useMe: vi.fn() };
});

vi.mock('next/navigation', async () => {
  const actual =
    await vi.importActual<typeof import('next/navigation')>('next/navigation');
  return {
    ...actual,
    useRouter: () => ({ ...actual.useRouter(), replace: vi.fn() }),
  };
});

const fetchUsage = vi.hoisted(() => vi.fn());
const listKeys = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  fetchUsage,
  listKeys,
}));

import { useMe } from '@/api/hooks';
import UsagePage from './page';

afterEach(() => {
  fetchUsage.mockReset();
  listKeys.mockReset();
});

function renderUsagePage() {
  vi.mocked(useMe).mockReturnValue({
    isLoading: false,
    isError: false,
    data: { user: { email: 'a@b.com' } },
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useMe>);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <UsagePage />
    </QueryClientProvider>,
  );
}

const EMPTY_COPY = /No tracked requests yet for this account/;

// A failed /v1/account/usage read (expired session 401, 5xx) must surface
// as an error, never as the "no requests yet" empty state: that copy is a
// claim of zero traffic, and rendering it for a failed read is a false
// statement about the customer's usage.
describe('/dashboard/usage request history', () => {
  it('renders a failed usage read as an error, not as zero usage', async () => {
    listKeys.mockResolvedValue([]);
    fetchUsage.mockRejectedValue(
      new ApiError(401, 'Authentication required', 'session expired'),
    );

    renderUsagePage();

    expect(
      await screen.findByText("Couldn't load request history"),
    ).toBeInTheDocument();
    expect(screen.getByText('session expired')).toBeInTheDocument();
    expect(screen.queryByText(EMPTY_COPY)).not.toBeInTheDocument();
    expect(
      screen.queryByText(/No per-endpoint data yet/),
    ).not.toBeInTheDocument();
  });

  it('renders the empty state only for a successful empty read', async () => {
    listKeys.mockResolvedValue([]);
    fetchUsage.mockResolvedValue([]);

    renderUsagePage();

    expect(await screen.findByText(EMPTY_COPY)).toBeInTheDocument();
    expect(
      screen.queryByText("Couldn't load request history"),
    ).not.toBeInTheDocument();
  });

  it('renders the tracked total for a successful read', async () => {
    listKeys.mockResolvedValue([]);
    fetchUsage.mockResolvedValue([
      {
        date: '2026-09-20',
        endpoint: '/v1/assets',
        requests: 1200,
        billable: 1200,
        errors: 3,
        throttled: 0,
      },
      {
        date: '2026-09-21',
        endpoint: '/v1/assets',
        requests: 34,
        billable: 34,
        errors: 0,
        throttled: 1,
      },
    ]);

    renderUsagePage();

    await waitFor(() =>
      expect(screen.getByText(/1,234 requests/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(EMPTY_COPY)).not.toBeInTheDocument();
  });

  // GH-1280: the enforced quota window is monthly, not daily, and the
  // rendered month-to-date figure must reconcile against `billable` —
  // never the 5xx-inclusive `requests` column.
  it('surfaces month-to-date billable usage against the monthly quota, not the 5xx-inclusive request count', async () => {
    listKeys.mockResolvedValue([]);
    // Day 1 of the real current UTC month — MonthlyQuota buckets by the
    // real clock, not a fixture date, so this must track it.
    const today = new Date().toISOString().slice(0, 7) + '-01';
    fetchUsage.mockResolvedValue([
      {
        date: today,
        endpoint: '/v1/assets',
        requests: 120,
        billable: 100,
        errors: 20,
        throttled: 0,
      },
    ]);

    renderUsagePage();

    expect(await screen.findByText('Month-to-date')).toBeInTheDocument();
    expect(await screen.findByText('100')).toBeInTheDocument();
    expect(screen.getByText('Monthly quota')).toBeInTheDocument();
    expect(screen.queryByText(/daily window/)).not.toBeInTheDocument();
  });
});
