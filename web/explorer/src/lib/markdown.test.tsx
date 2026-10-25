import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { Markdown } from './markdown';

describe('Markdown sourcePath', () => {
  it('resolves an incident file’s repo-relative link to GitHub', () => {
    // Unresolved, the browser turns this into /docs/operations/… on the explorer: a 404.
    render(
      <Markdown
        source="See [playbook](../../../docs/operations/sev-playbook.md#65-x)."
        sourcePath="internal/incidents/data/2026-09-02-x.md"
      />,
    );
    expect(screen.getByRole('link', { name: 'playbook' })).toHaveAttribute(
      'href',
      'https://github.com/Stellar-Index/StellarIndex/blob/main/docs/operations/sev-playbook.md#65-x',
    );
  });
});
