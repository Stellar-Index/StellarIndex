import { describe, expect, it } from 'vitest';

import { PROTOCOLS } from './registry';

describe('protocol links', () => {
  it('are absolute https URLs, since they render as raw hrefs', () => {
    for (const p of PROTOCOLS) {
      for (const href of Object.values(p.links ?? {})) {
        expect(new URL(href).protocol, `${p.name}: ${href}`).toBe('https:');
      }
    }
  });
});
