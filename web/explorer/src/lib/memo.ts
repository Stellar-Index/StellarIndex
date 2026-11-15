// Direction controls and invisible format characters: a memo holding one
// renders as different text from its bytes (reversed digits, hidden runs).
const HIDDEN_CHARS = /[\u061C\u200B-\u200F\u202A-\u202E\u2060-\u2069\uFEFF]/;
const HIDDEN_CHARS_ALL = new RegExp(HIDDEN_CHARS.source, 'g');

/**
 * memoDisplay — the memo value to show and copy. A MEMO_TEXT that is not valid
 * UTF-8 reaches the wire with U+FFFD in place of its bytes, and one with
 * direction-control or invisible characters displays as other text; either
 * way the lossless base64 copy is shown. With no base64 copy the hidden
 * characters are written out as escapes.
 */
export function memoDisplay(
  memo?: string | null,
  memoBase64?: string | null,
): { value: string | undefined; base64: boolean } {
  const hidden = memo != null && HIDDEN_CHARS.test(memo);
  if (memoBase64 && (memo == null || memo.includes('�') || hidden)) {
    return { value: memoBase64, base64: true };
  }
  if (hidden) {
    const escaped = memo.replace(
      HIDDEN_CHARS_ALL,
      (c) =>
        `\\u${c.charCodeAt(0).toString(16).toUpperCase().padStart(4, '0')}`,
    );
    return { value: escaped, base64: false };
  }
  return { value: memo ?? undefined, base64: false };
}

/** memoLabel — memoDisplay for a list cell: optional truncation, a base64 tag. */
export function memoLabel(
  memo?: string | null,
  memoBase64?: string | null,
  max?: number,
  prefix = '',
): string {
  const { value, base64 } = memoDisplay(memo, memoBase64);
  if (!value) return '';
  const shown = max && value.length > max ? `${value.slice(0, max)}…` : value;
  return `${prefix}${shown}${base64 ? ' (base64)' : ''}`;
}
