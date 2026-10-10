import { screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

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

const listPriceAlerts = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  listPriceAlerts,
}));

import { renderSignedInPage } from '../../../../test/dashboard-page';
import PriceAlertsPage from './page';

afterEach(() => {
  listPriceAlerts.mockReset();
});

const renderPriceAlertsPage = () => renderSignedInPage(<PriceAlertsPage />);

describe('/dashboard/price-alerts', () => {
  it('shows the empty state when the account has no alerts', async () => {
    listPriceAlerts.mockResolvedValue({ alerts: [], maxAlerts: 10 });

    renderPriceAlertsPage();

    await waitFor(() => {
      expect(screen.getByText('No price alerts yet')).toBeInTheDocument();
    });
  });

  it('renders an existing alert', async () => {
    listPriceAlerts.mockResolvedValue({
      alerts: [
        {
          id: 'alert_1',
          base_asset: 'crypto:BTC',
          quote_asset: 'fiat:USD',
          condition: 'above',
          threshold: '65000',
          cooldown_seconds: 300,
          enabled: true,
          created_at: '2026-01-01T00:00:00Z',
          updated_at: '2026-01-01T00:00:00Z',
        },
      ],
      maxAlerts: 10,
    });

    renderPriceAlertsPage();

    await waitFor(() => {
      expect(screen.queryByText('No price alerts yet')).not.toBeInTheDocument();
    });
  });
});
