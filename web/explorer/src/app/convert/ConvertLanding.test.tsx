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

const USDC_ID = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN'; // gitleaks:allow — public Circle issuer
const ASSETS = [
  { ticker: 'USDC', assetId: USDC_ID, name: 'USD Coin', slug: 'usdc' },
];

function renderLanding() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ConvertLanding tickers={['USD', 'EUR']} assets={ASSETS} />
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
  });

  it('swaps XLM → USD into USD → XLM, priced as the inverse', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [{ asset_id: 'native', price: '0.25' }],
    });
    renderLanding();
    fireEvent.click(screen.getByLabelText('Swap currencies'));
    expect(screen.getByLabelText('From')).toHaveValue('USD');
    expect(screen.getByLabelText('To')).toHaveValue('XLM');
    expect(screen.getByTestId('chart')).toHaveTextContent('USD/XLM');
    expect((await screen.findAllByText(/4\.0000/)).length).toBeGreaterThan(0);
  });

  it('preselects the pair the /convert Function redirected with', () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    window.history.replaceState(null, '', '/convert/?from=EUR&to=XLM');
    renderLanding();
    expect(screen.getByLabelText('From')).toHaveValue('EUR');
    expect(screen.getByLabelText('To')).toHaveValue('XLM');
    expect(
      screen.queryByText("That pair isn't available to convert"),
    ).toBeNull();
    window.history.replaceState(null, '', '/convert/');
  });

  it('prices XLM → a verified asset by its (code, issuer) id', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [{ asset_id: 'native', price: '0.5' }],
    });
    window.history.replaceState(null, '', '/convert/?from=XLM&to=USDC');
    renderLanding();
    expect(screen.getByLabelText('From')).toHaveValue('XLM');
    expect(screen.getByLabelText('To')).toHaveValue('USDC');
    expect(
      screen.queryByText("That pair isn't available to convert"),
    ).toBeNull();
    expect((await screen.findAllByText(/0\.500000/)).length).toBeGreaterThan(0);
    expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
      `/v1/price/batch?asset_ids=native&quote=${encodeURIComponent(USDC_ID)}`,
      {},
    );
    window.history.replaceState(null, '', '/convert/');
  });

  it('links a verified asset → hub page under its upper-cased ticker', () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderLanding();
    fireEvent.change(screen.getByLabelText('From'), {
      target: { value: 'USDC' },
    });
    expect(
      screen.getByRole('link', { name: 'USDC → USD page' }),
    ).toHaveAttribute('href', '/convert/USDC/USD');
  });

  it('says so when the requested pair is not offered', () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    window.history.replaceState(null, '', '/convert/?from=XLM&to=SCAM');
    renderLanding();
    expect(screen.getByLabelText('From')).toHaveValue('XLM');
    expect(screen.getByLabelText('To')).toHaveValue('USD');
    expect(
      screen.getByText("That pair isn't available to convert"),
    ).toBeInTheDocument();
    window.history.replaceState(null, '', '/convert/');
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
