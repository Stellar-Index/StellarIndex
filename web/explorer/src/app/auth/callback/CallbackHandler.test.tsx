import { render } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { CallbackHandler } from './CallbackHandler';

describe('CallbackHandler token exposure', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('scrubs the plaintext magic-link token out of the address bar before redirecting', () => {
    vi.useFakeTimers();
    const secretToken = 'super-secret-magic-link-token';
    Object.defineProperty(window, 'location', {
      value: {
        ...window.location,
        pathname: '/auth/callback',
        search: `?token=${secretToken}&next=/dashboard`,
        replace: vi.fn(),
      },
      writable: true,
    });
    const replaceState = vi
      .spyOn(window.history, 'replaceState')
      .mockImplementation(() => {});

    render(<CallbackHandler />);

    // The token must be stripped from the visible URL synchronously on
    // mount — not deferred behind the redirect's setTimeout — so it never
    // sits resident in the tab's address bar or session-restore history.
    expect(replaceState).toHaveBeenCalledTimes(1);
    const [, , url] = replaceState.mock.calls[0];
    expect(String(url)).not.toContain(secretToken);
    expect(String(url)).toBe('/auth/callback');

    vi.advanceTimersByTime(100);
  });
});
