import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { LivePrice } from './LivePrice';

afterEach(() => {
  vi.unstubAllGlobals();
});

// REGRESSION: a withheld price (404/403 from /v1/price) left
// the widget silently showing the build-time `initial` price with no
// on-screen hint that it is stale — only a hover-only `title` tooltip,
// invisible on the touch/iframe surfaces these widgets embed into.
describe('LivePrice (RLT-386)', () => {
  // /v1/price refuses, and the two answers /v1/assets/{id} can give.
  function stubRefused(detail: { status: number; body?: unknown }) {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) =>
        String(input).includes('/v1/assets/')
          ? {
              ok: detail.status === 200,
              status: detail.status,
              json: async () => detail.body ?? {},
            }
          : { ok: false, status: 404, json: async () => ({}) },
      ),
    );
  }

  it('keeps the build-time price, marked stale, when the catalogue did not answer', async () => {
    stubRefused({ status: 503 });

    render(<LivePrice assetId="native" initial="$0.1000" />);

    expect(await screen.findByRole('status')).toHaveTextContent(/stale/i);
    expect(screen.getByText('$0.1000')).toBeInTheDocument();
  });

  it('shows no price when neither endpoint publishes one', async () => {
    stubRefused({ status: 200, body: { data: { price_usd: null } } });

    render(<LivePrice assetId="native" initial="$0.1000" />);

    expect(await screen.findByRole('status')).toHaveTextContent(
      /not published/i,
    );
    expect(screen.queryByText('$0.1000')).not.toBeInTheDocument();
    expect(screen.getByText('—')).toBeInTheDocument();
  });

  it('shows the catalogue price, marked stale, when only the catalogue publishes one', async () => {
    stubRefused({ status: 200, body: { data: { price_usd: '0.2500' } } });

    render(<LivePrice assetId="native" initial="$0.1000" />);

    expect(await screen.findByRole('status')).toHaveTextContent(/stale/i);
    expect(screen.queryByText('$0.1000')).not.toBeInTheDocument();
  });

  it('shows no stale marker once a live price is fetched', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          data: { price: '0.2500', observed_at: '2026-09-22T00:00:00Z' },
        }),
      }),
    );

    render(<LivePrice assetId="native" initial="$0.1000" />);

    expect(await screen.findByText('$0.250000')).toBeInTheDocument();
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('bounds the live price request with an abort signal', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 404,
      json: async () => ({}),
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<LivePrice assetId="native" initial="$0.1000" />);

    await screen.findByRole('status');
    expect(fetchMock.mock.calls[0][1]?.signal).toBeInstanceOf(AbortSignal);
  });
});

describe('LivePrice rounding', () => {
  it('rounds the fetched wire string, not a float of it', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({
          data: { price: '2.00005', observed_at: '2026-09-22T00:00:00Z' },
        }),
      }),
    );

    render(<LivePrice assetId="native" initial="$2.0000" />);

    expect(await screen.findByText('$2.0001')).toBeInTheDocument();
  });
});

describe('LivePrice without an asset', () => {
  it('makes no request and keeps the initial price', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    render(<LivePrice assetId="" initial="$0.1000" />);

    expect(fetchMock).not.toHaveBeenCalled();
    expect(screen.getByText('$0.1000')).toBeTruthy();
  });
});
