import { describe, it, expect } from 'vitest';

import { cdata, escapeXml } from './atom';

// A literal `]]>` inside untrusted content (a pasted XML/CDATA snippet in a
// blog post or changelog entry) terminates a naively-built CDATA section
// early, injecting whatever follows as raw markup into the feed. cdata()
// must split it so the section can never close prematurely.
describe('cdata', () => {
  it('splits an embedded ]]> so it cannot close the section early', () => {
    const hostile = 'before]]><script>alert(1)</script>after';
    const wrapped = `<content>${cdata(hostile)}</content>`;

    // The naive form `<![CDATA[${hostile}]]>` would produce this exact
    // substring — assert we did NOT produce it.
    expect(wrapped).not.toContain('<![CDATA[before]]><script>');

    const doc = new DOMParser().parseFromString(wrapped, 'application/xml');
    expect(doc.querySelector('parsererror')).toBeNull();
    // The parsed text content must round-trip to the original hostile
    // string verbatim — i.e. the whole thing stayed data, none of it
    // became markup.
    expect(doc.documentElement.textContent).toBe(hostile);
  });

  it('leaves ordinary content untouched', () => {
    const plain = '### Fixed\n- some bullet\n';
    expect(cdata(plain)).toBe(`<![CDATA[${plain}]]>`);
  });
});

describe('escapeXml', () => {
  it('escapes the five XML special characters', () => {
    expect(escapeXml(`&<>"'`)).toBe('&amp;&lt;&gt;&quot;\'');
  });
});
