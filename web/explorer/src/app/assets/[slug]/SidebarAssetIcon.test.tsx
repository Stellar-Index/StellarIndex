import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';

import { SidebarAssetIcon } from './SidebarAssetIcon';

// SEC-10: SEP-1 `image` is issuer-controlled. The old scheme-only check
// (/^https:\/\//) accepted ANY https host, including private/internal
// ones — a hostile issuer could point a viewer's browser at their own
// router or a cloud metadata endpoint via a plain <img src>.
describe('SidebarAssetIcon', () => {
  it('renders the real <img> for a normal public https icon URL', () => {
    const { container } = render(
      <SidebarAssetIcon image="https://issuer.example.com/icon.png" code="USDC" />,
    );
    expect(container.querySelector('img')).not.toBeNull();
  });

  it('falls back to the letter glyph (no <img>) for a private/internal icon URL', () => {
    const { container } = render(
      <SidebarAssetIcon image="https://169.254.169.254/latest/meta-data/" code="EVIL" />,
    );
    expect(container.querySelector('img')).toBeNull();
    expect(container.textContent).toBe('E');
  });
});
