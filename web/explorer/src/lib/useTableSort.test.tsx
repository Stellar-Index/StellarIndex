import { describe, it, expect, vi } from 'vitest';
import { render, screen, renderHook } from '@testing-library/react';

import { SortableTh, useTableSort } from './useTableSort';

// ACC-01: SortableTh's <th> must carry scope="col" (matching ui/Table.tsx's
// own <Th>) so screen readers announce the column header for each data
// cell in the column, not just the clicked one.
describe('SortableTh', () => {
  it('sets scope="col" on the interactive (sortable) header', () => {
    render(
      <table>
        <thead>
          <tr>
            <SortableTh
              label="Price"
              sortKey="price"
              sort={{ key: null, dir: 'desc' }}
              onSort={() => {}}
              ariaSort={() => 'none'}
            />
          </tr>
        </thead>
      </table>,
    );
    expect(screen.getByRole('columnheader', { name: /Price/ })).toHaveAttribute(
      'scope',
      'col',
    );
  });

  it('sets scope="col" on the non-interactive (unsortable) header', () => {
    render(
      <table>
        <thead>
          <tr>
            <SortableTh label="#" />
          </tr>
        </thead>
      </table>,
    );
    expect(screen.getByRole('columnheader', { name: '#' })).toHaveAttribute(
      'scope',
      'col',
    );
  });
});

// T282: lib/format.ts's every formatter "passes 'en-US' explicitly so
// ... [output] match[es] between SSG and hydration" — compareValues'
// string fallback must carry the same pin, or a non-ASCII column's sort
// order can differ between the server-rendered order and the client's
// runtime default locale.
describe('compareValues (via useTableSort)', () => {
  it('pins string comparison to en-US explicitly', () => {
    const spy = vi.spyOn(String.prototype, 'localeCompare');
    const rows = [{ name: 'banana' }, { name: 'apple' }];
    const { result } = renderHook(() =>
      useTableSort(rows, [{ key: 'name', value: (r) => r.name }], 'name', 'asc'),
    );
    expect(result.current.sorted.map((r) => r.name)).toEqual([
      'apple',
      'banana',
    ]);
    expect(
      spy.mock.calls.some(([, locale]) => locale === 'en-US'),
    ).toBe(true);
    spy.mockRestore();
  });
});

// CA2-A35-correct-2: nulls/NaN must sort last regardless of direction. The
// default direction is 'desc' (useTableSort's own `initialDir` default and
// every column's toggle fallback), so a regression here surfaces as blank
// rows rising to the top of a descending sort — e.g. unpriced assets above
// priced ones on /assets.
describe('null handling under both directions', () => {
  it('keeps nulls last on a descending sort', () => {
    const rows = [
      { price: null },
      { price: 5 },
      { price: 10 },
      { price: null },
      { price: 3 },
    ];
    const { result } = renderHook(() =>
      useTableSort(rows, [{ key: 'price', value: (r) => r.price }], 'price', 'desc'),
    );
    expect(result.current.sorted.map((r) => r.price)).toEqual([
      10,
      5,
      3,
      null,
      null,
    ]);
  });

  it('keeps nulls last on an ascending sort', () => {
    const rows = [
      { price: null },
      { price: 5 },
      { price: 10 },
      { price: null },
      { price: 3 },
    ];
    const { result } = renderHook(() =>
      useTableSort(rows, [{ key: 'price', value: (r) => r.price }], 'price', 'asc'),
    );
    expect(result.current.sorted.map((r) => r.price)).toEqual([
      3,
      5,
      10,
      null,
      null,
    ]);
  });
});
