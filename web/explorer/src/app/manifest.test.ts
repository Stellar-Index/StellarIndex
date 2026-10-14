import { describe, expect, it } from 'vitest';

import manifest from './manifest';

/**
 * PWA install UIs flash background_color as the splash screen before
 * hydration and paint theme_color on the OS chrome, so both must be the
 * canvas token (globals.css --color-surface-canvas) of the dark-only system.
 */
describe('manifest', () => {
  it('uses the canvas colour for the splash and the chrome', () => {
    const result = manifest();

    expect(result.background_color).toBe('#0b0c0f');
    expect(result.theme_color).toBe('#0b0c0f');
  });
});
