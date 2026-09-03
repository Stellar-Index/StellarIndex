import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import CareersPage from './page';

// The contributing path for a new on-chain decoder listed a "Five-file
// convention: README + events + decode + consumer + tests". The package
// is SIX files — docs/contributing/add-onchain-source.md §1 and
// docs/contributing/task-recipes.md both require dispatcher_adapter.go,
// the production seam implementing dispatcher.Decoder. It is the one
// file whose absence is silent: without it the source compiles,
// registers nowhere, and emits nothing.
describe('CareersPage contributing paths', () => {
  it('does not describe the on-chain decoder package as five files', () => {
    render(<CareersPage />);
    expect(screen.queryByText(/five-file convention/i)).not.toBeInTheDocument();
  });

  it('names dispatcher_adapter as part of the six-file convention', () => {
    render(<CareersPage />);
    const decoder = screen.getByText(/Six-file convention/i);
    expect(decoder).toHaveTextContent(/dispatcher_adapter/);
  });
});
