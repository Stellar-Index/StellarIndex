// Unit tests for the heading-order scanner itself (#486).
//
// The scanner is what keeps the 694-page regression closed, so it needs
// its own tests: a checker that silently stops finding anything reads
// exactly like a fixed codebase. The fixtures below are the real emitted
// shapes — the pre-fix /assets/[slug] and /research outlines, and the
// RSC-payload trap that a naive grep falls into.
import { describe, expect, it } from 'vitest';

import {
  extractHeadings,
  formatOutline,
  formatViolation,
  headingOrderViolations,
} from './heading-order';

describe('extractHeadings', () => {
  it('reads headings in document order with their text', () => {
    expect(
      extractHeadings(
        '<h1 class="x">USDC</h1><p>hi</p><h2>Price</h2><h3>Issuer</h3>',
      ),
    ).toEqual([
      { level: 1, text: 'USDC' },
      { level: 2, text: 'Price' },
      { level: 3, text: 'Issuer' },
    ]);
  });

  it('strips nested markup and collapses whitespace in the text', () => {
    expect(
      extractHeadings('<h2>\n  Volume by <span>source</span> — 24h\n</h2>'),
    ).toEqual([{ level: 2, text: 'Volume by source — 24h' }]);
  });

  it('ignores headings inside <script> (the Next RSC flight payload)', () => {
    // self.__next_f.push serialises the tree into a <script>; a scan that
    // does not strip it double-counts every heading and invents skips.
    const html =
      '<h1>Network</h1><h2>Throughput</h2>' +
      '<script>self.__next_f.push([1,"<h3>Throughput</h3>"])</script>';
    expect(extractHeadings(html).map((h) => h.level)).toEqual([1, 2]);
  });

  it('ignores headings inside HTML comments', () => {
    expect(
      extractHeadings('<h1>A</h1><!-- <h3>commented</h3> --><h2>B</h2>').map(
        (h) => h.level,
      ),
    ).toEqual([1, 2]);
  });
});

describe('headingOrderViolations', () => {
  it('accepts a monotonic outline', () => {
    expect(
      headingOrderViolations('<h1>A</h1><h2>B</h2><h3>C</h3><h2>D</h2>'),
    ).toEqual([]);
  });

  it('accepts jumping back up any number of levels', () => {
    // h4 → h2 is a new section, not a skip.
    expect(
      headingOrderViolations(
        '<h1>A</h1><h2>B</h2><h3>C</h3><h4>D</h4><h2>E</h2>',
      ),
    ).toEqual([]);
  });

  it('does not require the first heading to be an h1', () => {
    // "no h1" is page-has-heading-one, a different rule; #486 is only
    // about skipped levels, and flagging this would mis-scope the guard.
    expect(headingOrderViolations('<h2>A</h2><h3>B</h3>')).toEqual([]);
  });

  it('flags the pre-fix /assets/[slug] outline (h1 → h3)', () => {
    const violations = headingOrderViolations(
      '<h1>USDC</h1><h3>Converter</h3><h3>Issuer</h3>',
    );
    expect(violations).toHaveLength(1);
    expect(violations[0]).toMatchObject({ from: 1, to: 3, text: 'Converter' });
  });

  it('flags the pre-fix /research outline (h2 → h4)', () => {
    const violations = headingOrderViolations(
      '<h1>Research</h1><h2>Architecture narratives</h2><h4>Ingest pipeline</h4>',
    );
    expect(violations).toHaveLength(1);
    expect(violations[0]).toMatchObject({
      from: 2,
      to: 4,
      text: 'Ingest pipeline',
    });
  });

  it('reports every skip on a page, not just the first', () => {
    expect(
      headingOrderViolations('<h1>A</h1><h3>B</h3><h2>C</h2><h4>D</h4>').map(
        (v) => `${v.from}->${v.to}`,
      ),
    ).toEqual(['1->3', '2->4']);
  });

  it('accepts a pre-extracted heading list', () => {
    expect(
      headingOrderViolations([
        { level: 1, text: 'A' },
        { level: 3, text: 'B' },
      ]),
    ).toHaveLength(1);
  });
});

describe('failure-message helpers', () => {
  it('formatViolation names the levels, the position and the text', () => {
    expect(
      formatViolation({ from: 1, to: 3, text: 'Converter', index: 1 }),
    ).toBe('h1 → h3 at heading #2 ("Converter")');
  });

  it('formatOutline renders the whole outline', () => {
    expect(
      formatOutline([
        { level: 1, text: 'USDC' },
        { level: 2, text: 'Converter' },
      ]),
    ).toBe('h1:USDC | h2:Converter');
  });
});
