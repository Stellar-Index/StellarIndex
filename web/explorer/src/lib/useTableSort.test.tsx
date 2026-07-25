import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { SortableTh } from './useTableSort';

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
    expect(screen.getByRole('columnheader', { name: /Price/ })).toHaveAttribute('scope', 'col');
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
    expect(screen.getByRole('columnheader', { name: '#' })).toHaveAttribute('scope', 'col');
  });
});
