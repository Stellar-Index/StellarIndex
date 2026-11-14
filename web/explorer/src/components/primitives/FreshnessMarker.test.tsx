import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';

import { FreshnessMarker } from './FreshnessMarker';

describe('FreshnessMarker', () => {
  it('shows an operator hold with its reason', () => {
    render(
      <FreshnessMarker
        flags={{ under_review: true }}
        underReviewReason="supply reconciliation in progress"
      />,
    );
    const badge = screen.getByText('Under review');
    expect(badge).toHaveAttribute(
      'title',
      expect.stringContaining('supply reconciliation in progress'),
    );
  });

  it('renders nothing when no flag is set', () => {
    const { container } = render(<FreshnessMarker flags={{}} />);
    expect(container).toBeEmptyDOMElement();
  });
});
