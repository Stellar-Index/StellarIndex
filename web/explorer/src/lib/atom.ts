// Shared helpers for the Atom feed routes (blog.atom, changelog.atom).
// Both feeds interpolate untrusted content (post bodies, changelog entries)
// into hand-built XML strings — keep the escaping in one place.

/** Escape text for use in an XML element or attribute value. */
export function escapeXml(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

/**
 * Wrap text in a CDATA section, splitting any embedded `]]>` so it can never
 * terminate the section early. A literal `]]>` in source content (a code
 * fence closing a CDATA-lookalike, a pasted XML snippet, etc.) would
 * otherwise end the CDATA block mid-content and inject the remainder as raw
 * markup into the feed.
 *
 * Standard technique: close the current section right before the `]]`,
 * emit the `>` as a literal char (via a fresh CDATA section, which is the
 * only way to include it), then reopen a new section for what follows.
 */
export function cdata(s: string): string {
  return `<![CDATA[${s.replace(/]]>/g, ']]]]><![CDATA[>')}]]>`;
}
