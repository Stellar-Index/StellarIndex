import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { PartialDetailCallout } from './PartialDetailCallout';

vi.setConfig({ testTimeout: 30_000 });

function answer(body: unknown, cacheControl: string) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: {
      'content-type': 'application/json',
      'cache-control': cacheControl,
    },
  });
}

// The page reads the detail's degraded verdict, not just its data.
describe('fetchAssetDetail', () => {
  beforeEach(() => {
    vi.resetModules();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('reports a detail still partial after the re-asks', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        answer({ data: { asset_id: 'native', ath: null } }, 'no-store'),
      ),
    );
    const { fetchAssetDetail } = await import('./page');
    const p = fetchAssetDetail('native');
    await vi.runAllTimersAsync();
    const got = await p;
    expect(got.degraded).toBe(true);
    expect(got.detail?.asset_id).toBe('native');
  });

  it('reports a complete detail as complete', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        answer({ data: { asset_id: 'native' } }, 'public, max-age=30'),
      ),
    );
    const { fetchAssetDetail } = await import('./page');
    const got = await fetchAssetDetail('native');
    expect(got.degraded).toBe(false);
  });
});

describe('PartialDetailCallout', () => {
  it('says the blank sections are unknown, not absent', () => {
    render(<PartialDetailCallout code="USDC" />);
    expect(screen.getByText(/USDC's detail partial/)).toBeInTheDocument();
    expect(screen.getByText(/unknown, not absent/)).toBeInTheDocument();
  });
});
