import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { HBarList } from '@/components/charts/Bars';
import type { Source } from '@/api/hooks';
import { peerRows } from './SourcePeerBars';

const src = (name: string, trades?: number, klass = 'exchange') =>
  ({ name, class: klass, trade_count_24h: trades }) as Source;

describe('peerRows', () => {
  const all = [
    src('a', 10),
    src('b', 500),
    src('c', 50, 'oracle'),
    src('d', undefined),
  ];

  it('keeps same-class sources with counts, ranked desc, highlighting self', () => {
    const rows = peerRows('a', all);
    expect(rows.map((r) => r.label)).toEqual(['b', 'a']);
    expect(rows[1].color).toBe('var(--color-brand-500)');
  });

  it('renders as a labelled bar list', () => {
    render(<HBarList items={peerRows('a', all)} ariaLabel="peer trades" />);
    expect(screen.getByLabelText('peer trades').textContent).toContain('500');
  });
});
