import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { HomeTryAPI } from './HomeTryAPI';

afterEach(() => {
  vi.unstubAllGlobals();
});

function deferredFetch() {
  const pending: ((body: string) => void)[] = [];
  const fetchMock = vi.fn().mockImplementation(
    () =>
      new Promise((resolve) => {
        pending.push((body) =>
          resolve({ ok: true, status: 200, text: async () => body }),
        );
      }),
  );
  vi.stubGlobal('fetch', fetchMock);
  return { fetchMock, pending };
}

describe('HomeTryAPI live runner', () => {
  it('drops a slow response once the visitor has picked another example', async () => {
    const { fetchMock, pending } = deferredFetch();
    render(<HomeTryAPI />);

    fireEvent.click(screen.getByRole('button', { name: 'Run live' }));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0]![0])).toContain('/v1/price?');

    fireEvent.click(screen.getByRole('button', { name: 'XLM asset detail' }));

    await act(async () => {
      pending[0]!('{"stale":"price-body"}');
    });

    expect(screen.queryByText(/price-body/)).toBeNull();
    expect(screen.queryByText('response')).toBeNull();
    expect(screen.getByRole('button', { name: 'Run live' })).toBeEnabled();

    fireEvent.click(screen.getByRole('button', { name: 'Run live' }));
    expect(String(fetchMock.mock.calls[1]![0])).toContain('/v1/assets/XLM');
    await act(async () => {
      pending[1]!('{"fresh":"asset-body"}');
    });
    expect(screen.getByText(/asset-body/)).toBeInTheDocument();
  });
});
