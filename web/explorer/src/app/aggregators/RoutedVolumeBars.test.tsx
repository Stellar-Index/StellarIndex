import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { RoutedVolumeBars } from './RoutedVolumeBars';

const row = (name: string, v: string | null, kind = 'router') => ({
  contract_id: `C${name}`,
  name,
  kind,
  routed_volume_24h_usd: v,
});

describe('RoutedVolumeBars', () => {
  it('ranks by exact volume above 2^53 and notes unvalued routers', () => {
    render(
      <RoutedVolumeBars
        rows={[
          row('small', '9007199254740992'),
          row('big', '9007199254740993'),
          row('none', null),
          row('vault', null, 'aggregator-vault'),
        ]}
      />,
    );
    const items = screen.getAllByRole('listitem');
    expect(items[0].textContent).toContain('big');
    expect(items[1].textContent).toContain('small');
    expect(screen.getByTestId('routed-volume-bars').textContent).toContain(
      '1 router with no USD valuation yet is not drawn',
    );
  });

  it('renders nothing for a single valued router', () => {
    const { container } = render(
      <RoutedVolumeBars rows={[row('a', '5'), row('b', null)]} />,
    );
    expect(container.firstChild).toBeNull();
  });
});
