import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { DexVolumeShare } from './DexVolumeShare';

describe('DexVolumeShare', () => {
  it('shows exact total above 2^53 and counts unvalued DEXes', () => {
    render(
      <DexVolumeShare
        rows={[
          { name: 'soroswap', volume_24h_usd: '9007199254740993' },
          { name: 'phoenix', volume_24h_usd: '7' },
          { name: 'comet', volume_24h_usd: null },
          { name: 'aquarius' },
        ]}
      />,
    );
    const t = screen.getByTestId('volume-share').textContent ?? '';
    expect(t).toContain('Top 2 of 2 DEXes');
    expect(t).toContain('2 DEXes with no valued volume are not drawn');
    expect(t).toContain('$9007.2T');
  });

  it('renders nothing for a single valued DEX', () => {
    const { container } = render(
      <DexVolumeShare
        rows={[{ name: 'a', volume_24h_usd: '5' }, { name: 'b' }]}
      />,
    );
    expect(container.firstChild).toBeNull();
  });
});
