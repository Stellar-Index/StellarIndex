import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { AssetClientFallback } from './AssetClientFallback';

function mockAssetFetch(assetId: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({
      status: 200,
      ok: true,
      json: async () => ({ data: { asset_id: assetId } }),
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('AssetClientFallback', () => {
  it('shows the recovery panel, not the fetch-error panel, when sessionStorage throws', async () => {
    mockAssetFetch('native');
    // Private-mode / storage-disabled browsers throw a SecurityError on
    // sessionStorage access rather than returning null.
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError');
    });

    render(<AssetClientFallback slug="native" />);

    await waitFor(() => {
      expect(screen.getByText(/Reload the page/i)).toBeInTheDocument();
    });
    expect(
      screen.queryByText(/Couldn't reach the API/i),
    ).not.toBeInTheDocument();
  });
});
