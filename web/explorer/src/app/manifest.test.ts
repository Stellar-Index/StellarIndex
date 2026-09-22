import { describe, expect, it } from 'vitest';

import manifest from './manifest';

/**
 * T315: manifest.ts paired a white background_color (#ffffff) with the
 * dark-only design system's blue theme_color (#1f4ae0). PWA install UIs
 * flash background_color as the splash screen before hydration, so this
 * inverted the dark chrome for every Android/Chrome install.
 * background_color must match the design system's canvas token
 * (globals.css --color-surface-canvas), not white.
 */
describe('manifest', () => {
  it('uses the dark design system canvas colour, not white', () => {
    const result = manifest();

    expect(result.background_color).toBe('#0a0b0d');
    expect(result.background_color).not.toBe('#ffffff');
  });
});
