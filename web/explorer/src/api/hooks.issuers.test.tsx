import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { renderHook, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { ISSUER_LOOKUP_LIMIT, useIssuerLookup, useIssuers } from './hooks';

afterEach(() => vi.unstubAllGlobals());

describe('issuer hooks', () => {
  it('share one /v1/issuers request when a page uses both', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      statusText: 'OK',
      json: async () => ({
        data: [
          { g_strkey: 'GA', home_domain: 'a.example', org_verified: true },
          { g_strkey: 'GB' },
        ],
      }),
    });
    vi.stubGlobal('fetch', fetchMock);
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );

    const { result } = renderHook(
      () => ({
        list: useIssuers(ISSUER_LOOKUP_LIMIT),
        lookup: useIssuerLookup(),
      }),
      { wrapper },
    );

    await waitFor(() => expect(result.current.lookup.data).toBeDefined());
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(result.current.list.data?.map((r) => r.g_strkey)).toEqual([
      'GA',
      'GB',
    ]);
    expect(result.current.lookup.data).toEqual({
      GA: {
        home_domain: 'a.example',
        org_name: undefined,
        org_verified: true,
      },
    });
  });
});
