// Shared Atom (RFC-4287) serialisation helpers for /blog.atom and
// /changelog.atom. Both feeds hand-build XML with template literals;
// this module is the one place "what must be escaped where" is decided,
// so a fix to one feed's escaping can't silently miss the other.

/** Escape text for use inside an XML element or attribute value. */
export function escapeXml(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&apos;');
}

/**
 * Wrap text in a CDATA section, neutralising any embedded `]]>` (the
 * CDATA terminator) by splitting it across two adjacent CDATA sections.
 * Without this, a single `]]>` in the source text closes the section
 * early and the remainder of the document is parsed as markup.
 */
export function toCdata(s: string): string {
  return `<![CDATA[${s.replace(/]]>/g, ']]]]><![CDATA[>')}]]>`;
}
