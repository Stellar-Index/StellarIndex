import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';

import { AssetIdentityLine } from './AssetIdentityLine';

const ID = 'sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH';
const SAC = 'CDFZUVS5YNLXU7VENKOUDEOHCJGKQNVUBWD7KMN6E7ZROKPYPFLRUJFG';

describe('AssetIdentityLine', () => {
  it('shortens both ids, keeps the full value on hover, and offers a copy button each', () => {
    render(<AssetIdentityLine identity={ID} sac={SAC} />);
    expect(screen.getByText('Identity').tagName).toBe('DT');
    expect(screen.getByText('sUSD-GCHW…JYIH')).toHaveAttribute('title', ID);
    expect(screen.getByText('SAC').tagName).toBe('DT');
    expect(screen.getByText('CDFZU…RUJFG')).toHaveAttribute('title', SAC);
    expect(screen.getAllByRole('button')).toHaveLength(2);
  });

  it('omits the SAC row when there is none', () => {
    render(<AssetIdentityLine identity={ID} sac={null} />);
    expect(screen.queryByText('SAC')).toBeNull();
  });

  it('shows only the SAC for XLM, never the word native', () => {
    render(<AssetIdentityLine identity={null} sac={SAC} />);
    expect(screen.queryByText('Identity')).toBeNull();
    expect(screen.queryByText(/native/)).toBeNull();
    expect(screen.getByText('SAC')).toBeInTheDocument();
  });
});
