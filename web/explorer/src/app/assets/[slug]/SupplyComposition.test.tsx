import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { SupplyComposition } from './SupplyComposition';

describe('SupplyComposition', () => {
  it('shows exact shares and a declared-max headroom segment', () => {
    render(
      <SupplyComposition
        circulating="500"
        total="750"
        max="1000"
        decimals={0}
        maxDeclared
      />,
    );
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain(
      'Headroom to declared max 250',
    );
    expect(screen.getByText('50%')).toBeInTheDocument();
    expect(screen.queryByText(/is a floor/)).toBeNull();
  });

  it('says floor when circulating is a lower bound', () => {
    render(
      <SupplyComposition
        circulating="10"
        total="10"
        max={null}
        decimals={0}
        floor
      />,
    );
    expect(screen.getByText(/is a floor/)).toBeInTheDocument();
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain(
      'floor',
    );
  });

  it('marks the not-circulating share as a ceiling under a floor', () => {
    render(
      <SupplyComposition
        circulating="60"
        total="100"
        max={null}
        decimals={0}
        floor
      />,
    );
    expect(screen.getByText(/≥\s*60/)).toBeInTheDocument();
    expect(screen.getByText(/≤\s*40/)).toBeInTheDocument();
  });

  it('keeps >2^53 amounts exact', () => {
    render(
      <SupplyComposition
        circulating="9007199254740993"
        total="9007199254740995"
        max={null}
        decimals={0}
      />,
    );
    expect(screen.getByRole('img').getAttribute('aria-label')).toContain(
      '9,007,199,254,740,993',
    );
    expect(screen.getByText('100%')).toBeInTheDocument();
  });

  it('renders nothing with no supply', () => {
    const { container } = render(
      <SupplyComposition
        circulating={null}
        total={null}
        max={null}
        decimals={7}
      />,
    );
    expect(container.firstChild).toBeNull();
  });
});
