import { describe, it, expect, vi } from 'vitest';

import { fetchAllCursors } from './hooks';

const row = (source: string) =>
  ({ source }) as unknown as Awaited<ReturnType<typeof fetchAllCursors>>[0];

describe('fetchAllCursors', () => {
  it('follows pagination.next until the list ends', async () => {
    const get = vi
      .fn()
      .mockResolvedValueOnce({ data: [row('a')], pagination: { next: 'p2' } })
      .mockResolvedValueOnce({ data: [row('b')], pagination: {} });

    const rows = await fetchAllCursors(get);

    expect(rows.map((r) => r.source)).toEqual(['a', 'b']);
    expect(get).toHaveBeenCalledTimes(2);
    expect(get.mock.calls[1][1]).toMatchObject({ cursor: 'p2' });
  });

  it('refuses a list that never ends, so a partial set is not read as whole', async () => {
    const get = vi
      .fn()
      .mockResolvedValue({ data: [row('a')], pagination: { next: 'again' } });

    await expect(fetchAllCursors(get)).rejects.toThrow(/page budget/);
  });
});
