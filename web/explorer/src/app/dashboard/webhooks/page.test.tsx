import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { DashboardWebhook } from '@/api/account';

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

const listDashboardWebhooks = vi.hoisted(() => vi.fn());
const createDashboardWebhook = vi.hoisted(() => vi.fn());
const deleteDashboardWebhook = vi.hoisted(() => vi.fn());
const updateDashboardWebhook = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  listDashboardWebhooks,
  createDashboardWebhook,
  deleteDashboardWebhook,
  updateDashboardWebhook,
}));

import { useMe } from '@/api/hooks';
import WebhooksPage from './page';

afterEach(() => {
  listDashboardWebhooks.mockReset();
  createDashboardWebhook.mockReset();
  deleteDashboardWebhook.mockReset();
  updateDashboardWebhook.mockReset();
});

function webhook(overrides: Partial<DashboardWebhook> = {}): DashboardWebhook {
  return {
    id: 'a1b2c3d4-0000-4000-8000-000000000001',
    name: 'production-alerts',
    url: 'https://example.com/hooks/stellarindex',
    events: ['price.alert'],
    enabled: true,
    created_at: '2026-08-01T12:00:00Z',
    updated_at: '2026-08-01T12:00:00Z',
    ...overrides,
  };
}

function renderWebhooksPage() {
  vi.mocked(useMe).mockReturnValue({
    isLoading: false,
    isError: false,
    data: { user: { email: 'a@b.com' } },
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useMe>);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <WebhooksPage />
    </QueryClientProvider>,
  );
}

// This is the regression coverage for T262: before this page existed,
// the ONLY way to attach a webhook to price alerts (or any other
// account event) was to email support — even though the dashboard
// session API already served full CRUD at /v1/dashboard/webhooks. The
// page not existing at all is the pre-fix state this test is red
// against (there is no `./page` module to import).
describe('/dashboard/webhooks self-service management', () => {
  it('registers a new webhook through the dashboard session API and reveals its secret once', async () => {
    listDashboardWebhooks.mockResolvedValue([]);
    createDashboardWebhook.mockResolvedValue({
      webhook: webhook(),
      secret:
        'wsec_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdead', // gitleaks:allow
    });

    renderWebhooksPage();

    expect(await screen.findByText('No webhooks yet')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /New webhook/ }));
    fireEvent.change(screen.getByLabelText(/Name/), {
      target: { value: 'production-alerts' },
    });
    fireEvent.change(screen.getByLabelText(/Endpoint URL/), {
      target: { value: 'https://example.com/hooks/stellarindex' },
    });
    fireEvent.click(screen.getByRole('button', { name: /Create webhook/ }));

    await waitFor(() =>
      expect(createDashboardWebhook).toHaveBeenCalledWith({
        name: 'production-alerts',
        url: 'https://example.com/hooks/stellarindex',
        events: ['price.alert'],
      }),
    );

    // The signing secret is surfaced exactly once, from the create
    // response — never re-fetched or re-derived.
    expect(await screen.findByText(/wsec_deadbeef/)).toBeInTheDocument();
  });

  it('lists a registered webhook and its subscribed events', async () => {
    listDashboardWebhooks.mockResolvedValue([
      webhook({ events: ['price.alert', 'incident.sev1'] }),
    ]);

    renderWebhooksPage();

    expect(await screen.findByText('production-alerts')).toBeInTheDocument();
    expect(
      screen.getByText('https://example.com/hooks/stellarindex'),
    ).toBeInTheDocument();
    expect(screen.getByText('price.alert')).toBeInTheDocument();
    expect(screen.getByText('incident.sev1')).toBeInTheDocument();
  });

  it('deletes a webhook via the DELETE endpoint and refreshes the list', async () => {
    listDashboardWebhooks.mockResolvedValue([webhook()]);
    deleteDashboardWebhook.mockResolvedValue(undefined);
    vi.spyOn(window, 'confirm').mockReturnValue(true);

    renderWebhooksPage();
    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));

    await waitFor(() =>
      expect(deleteDashboardWebhook).toHaveBeenCalledWith(
        'a1b2c3d4-0000-4000-8000-000000000001',
      ),
    );
    await waitFor(() =>
      expect(listDashboardWebhooks.mock.calls.length).toBeGreaterThan(1),
    );
  });
});
