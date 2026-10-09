import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
vi.mock('./[from]/[to]/ConvertChart', () => ({
  ConvertChart: ({ from, to }: { from: string; to: string }) => (
    <div data-testid="chart">{`${from}/${to}`}</div>
  ),
}));

import { apiGet } from '@/api/client';
import { ConvertLanding } from './ConvertLanding';

function renderLanding() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ConvertLanding tickers={['USD', 'EUR']} />
    </QueryClientProvider>,
  );
}

describe('ConvertLanding', () => {
  it('opens on XLM → USD with the chart underneath', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [{ asset_id: 'native', price: '0.25' }],
    });
    renderLanding();
    expect(screen.getByLabelText('From')).toHaveValue('XLM');
    expect(screen.getByLabelText('To')).toHaveValue('USD');
    expect(screen.getByTestId('chart')).toHaveTextContent('XLM/USD');
    expect((await screen.findAllByText(/0\.250000/)).length).toBeGreaterThan(0);
    expect(screen.getByLabelText('Swap currencies')).toBeDisabled();
  });

  it('swaps a fiat pair and links its page', () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderLanding();
    fireEvent.change(screen.getByLabelText('From'), {
      target: { value: 'EUR' },
    });
    fireEvent.click(screen.getByLabelText('Swap currencies'));
    expect(screen.getByLabelText('From')).toHaveValue('USD');
    expect(screen.getByLabelText('To')).toHaveValue('EUR');
    expect(screen.getByTestId('chart')).toHaveTextContent('USD/EUR');
    expect(
      screen.getByRole('link', { name: 'USD → EUR page' }),
    ).toHaveAttribute('href', '/convert/USD/EUR');
  });
});
