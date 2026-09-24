import { describe, it, expect } from 'vitest';

import { escapeXml, toCdata } from './atom';

// A body containing a literal `]]>` (e.g. a blog post quoting XML/XSLT, or
// a fenced code block showing a CDATA example) must not be able to close
// the CDATA section early — that would emit malformed XML to every
// subscriber and the parser does not recover for the rest of the document.
describe('toCdata', () => {
  it('neutralises an embedded ]]> by splitting it across two CDATA sections', () => {
    const body = 'before ]]> after';
    const wrapped = toCdata(body);

    expect(wrapped).toBe('<![CDATA[before ]]]]><![CDATA[> after]]>');
    // Exactly one CDATA-open/close pair still ends the whole section.
    expect(wrapped.endsWith(']]>')).toBe(true);
    expect(wrapped.startsWith('<![CDATA[')).toBe(true);
  });

  it('round-trips plain text with no terminator unchanged', () => {
    expect(toCdata('plain text, no markers')).toBe(
      '<![CDATA[plain text, no markers]]>',
    );
  });

  it('produces XML that a DOM parser accepts and recovers the original text', () => {
    const body = 'code example: <foo><![CDATA[inner]]></foo> done';
    const xml = `<root><content>${toCdata(body)}</content></root>`;
    const doc = new DOMParser().parseFromString(xml, 'application/xml');
    expect(doc.getElementsByTagName('parsererror').length).toBe(0);
    expect(doc.getElementsByTagName('content')[0]?.textContent).toBe(body);
  });
});

describe('escapeXml', () => {
  it('escapes all five XML-significant characters', () => {
    expect(escapeXml(`& < > " '`)).toBe('&amp; &lt; &gt; &quot; &apos;');
  });
});
