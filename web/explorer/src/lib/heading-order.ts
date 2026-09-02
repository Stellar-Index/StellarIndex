/**
 * heading-order — the WCAG 1.3.1 "heading-order" check, as a pure function.
 *
 * A screen-reader user navigating by heading perceives the page as the
 * outline its `<h1>`…`<h6>` sequence describes. Jumping a level (h1 → h3)
 * asserts a section that isn't there, so the perceived structure and the
 * real one disagree. That is axe's `heading-order` rule and WCAG 1.3.1
 * (Info and Relationships), Level A.
 *
 * Why this lives in the repo rather than in a reviewer's terminal (#486):
 * 694 of the 1,691 built pages that carry a heading skipped a level, and
 * the fix is spread across ~40 call sites that each look individually
 * harmless. Without a check that runs on every build, one `<Panel>` added
 * as a page's top-level section (its title defaults to `<h3>`) silently
 * re-opens the whole finding.
 *
 * Deliberately regex-based, not DOM-based: it runs against the emitted
 * static export in `postbuild` where no DOM is available, and against
 * `container.innerHTML` in component tests — one implementation, one
 * definition of the defect, both layers.
 */

/** One heading found in document order. */
export type Heading = {
  /** 1–6. */
  level: number;
  /** Text content, tags stripped and whitespace collapsed (may be ''). */
  text: string;
};

/** A place where the outline jumps more than one level. */
export type HeadingOrderViolation = {
  /** Level of the preceding heading. */
  from: number;
  /** Level of the heading that skipped. */
  to: number;
  /** Text of the heading that skipped. */
  text: string;
  /** Zero-based index of the offending heading in the document order. */
  index: number;
};

const HEADING_TAG = /<(h[1-6])\b[^>]*>/gi;
const SCRIPT_BLOCK = /<script\b[^>]*>[\s\S]*?<\/script>/gi;
const HTML_COMMENT = /<!--[\s\S]*?-->/g;
const TAG = /<[^>]*>/g;

/**
 * extractHeadings — every heading in document order.
 *
 * `<script>` blocks are stripped first: Next inlines the RSC flight
 * payload into them, and that payload contains escaped heading markup
 * that is NOT part of the rendered outline. HTML comments go too (Next
 * marks Suspense boundaries with them).
 */
export function extractHeadings(html: string): Heading[] {
  const body = html.replace(SCRIPT_BLOCK, '').replace(HTML_COMMENT, '');
  const headings: Heading[] = [];
  HEADING_TAG.lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = HEADING_TAG.exec(body)) !== null) {
    const after = body.slice(match.index + match[0].length);
    const close = after.search(/<\/h[1-6]>/i);
    const inner = close === -1 ? after.slice(0, 200) : after.slice(0, close);
    headings.push({
      level: Number(match[1][1]),
      text: inner.replace(TAG, '').replace(/\s+/g, ' ').trim(),
    });
  }
  return headings;
}

/**
 * headingOrderViolations — every level jump of more than one.
 *
 * The first heading on a page can be any level (a missing `<h1>` is a
 * different rule, `page-has-heading-one`, and is not what #486 is
 * about), so the walk only compares each heading with its predecessor.
 */
export function headingOrderViolations(
  htmlOrHeadings: string | Heading[],
): HeadingOrderViolation[] {
  const headings =
    typeof htmlOrHeadings === 'string'
      ? extractHeadings(htmlOrHeadings)
      : htmlOrHeadings;
  const violations: HeadingOrderViolation[] = [];
  let previous = 0;
  headings.forEach((heading, index) => {
    if (previous > 0 && heading.level > previous + 1) {
      violations.push({
        from: previous,
        to: heading.level,
        text: heading.text,
        index,
      });
    }
    previous = heading.level;
  });
  return violations;
}

/** One-line rendering of a violation, for assertion messages. */
export function formatViolation(v: HeadingOrderViolation): string {
  return `h${v.from} → h${v.to} at heading #${v.index + 1} ("${v.text}")`;
}

/** The whole outline as `h1 > h3 > h3` — for a failure message. */
export function formatOutline(headings: Heading[]): string {
  return headings.map((h) => `h${h.level}:${h.text.slice(0, 40)}`).join(' | ');
}
