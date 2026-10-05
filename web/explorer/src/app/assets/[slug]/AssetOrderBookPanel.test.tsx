import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetOrderBookPanel, orderBookQuote } from './AssetOrderBookPanel';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const AQUA = 'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';

function renderPanel(assetID: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetOrderBookPanel assetID={assetID} />
    </QueryClientProvider>,
  );
}

describe('AssetOrderBookPanel', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockReset();
    vi.mocked(apiGet).mockReturnValue(new Promise(() => {}));
  });

  it('quotes a classic asset against XLM and XLM against USDC', () => {
    expect(orderBookQuote(AQUA)).toBe('native');
    expect(orderBookQuote(USDC)).toBe('native');
    expect(orderBookQuote('native')).toBe(USDC);
  });

  it('requests the asset as the selling side of its SDEX book', () => {
    renderPanel(AQUA);
    expect(
      screen.getByRole('heading', { name: /SDEX order book/i }),
    ).toBeInTheDocument();
    expect(apiGet).toHaveBeenCalledWith('/v1/sdex/orderbook', {
      selling: AQUA,
      buying: 'native',
      depth: '12',
    });
  });

  it('renders nothing for an asset with no SDEX book', () => {
    const { container } = renderPanel(
      'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75',
    );
    expect(container).toBeEmptyDOMElement();
    expect(apiGet).not.toHaveBeenCalled();
  });
});
