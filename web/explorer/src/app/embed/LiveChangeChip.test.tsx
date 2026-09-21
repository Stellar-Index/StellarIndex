import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactElement } from 'react';

import { LiveChangeChip } from './LiveChangeChip';

function renderChip(ui: ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
});
afterEach(() => {
  vi.unstubAllGlobals();
});

// REGRESSION (K061): the /embed/pair and /embed/asset widgets baked
// their change chip ONCE at build time (last-vs-first chart point, or
// the coin row's change_*_pct) and rendered it as a static number
// beside <LivePrice>, which refreshes on a 60s poll. The baked chip
// never refreshed, so it could silently disagree with the live price
// for as long as the iframe stayed open. LiveChangeChip must re-derive
// the pill from the live change-summary worker (GET
// /v1/changes/{entity_type}/{id}), overriding the baked figure once
// the worker reports a fresher one.
describe('LiveChangeChip (K061)', () => {
  function mockChangesFetch(h24DeltaPct: number) {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((input: string | URL) => {
        const url = String(input);
        if (url.includes('/v1/changes/')) {
          return Promise.resolve({
            ok: true,
            status: 200,
            json: async () => ({
              data: {
                entity_type: 'coin',
                entity_id: 'native',
                refreshed_at: '2026-09-19T15:00:00Z',
                current_value: '0.170',
                h24_delta_pct: h24DeltaPct,
              },
            }),
          });
        }
        return Promise.resolve({
          ok: false,
          status: 404,
          json: async () => ({}),
        });
      }),
    );
  }

  it('overrides a stale build-time UP chip with a live DOWN figure from the change-summary worker', async () => {
    mockChangesFetch(-5.2);
    renderChip(
      <LiveChangeChip
        entityType="coin"
        entityID="native"
        window="24h"
        initialPct={2.1}
      />,
    );
    expect(await screen.findByText(/-5\.20% 24h/)).toBeInTheDocument();
    expect(screen.queryByText(/\+2\.10% 24h/)).not.toBeInTheDocument();
  });

  it('keeps the baked chip when the change-summary worker has no row yet', () => {
    // Default beforeEach fetch stub rejects every request — no worker row.
    renderChip(
      <LiveChangeChip
        entityType="coin"
        entityID="native"
        window="24h"
        initialPct={2.1}
      />,
    );
    expect(screen.getByText(/\+2\.10% 24h/)).toBeInTheDocument();
  });
});
