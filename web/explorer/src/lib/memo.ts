/**
 * memoDisplay — the memo value to show and copy. A MEMO_TEXT that is not valid
 * UTF-8 reaches the wire with U+FFFD in place of its bytes, so the lossless
 * base64 copy is shown instead of the damaged text.
 */
export function memoDisplay(
  memo?: string | null,
  memoBase64?: string | null,
): { value: string | undefined; base64: boolean } {
  if (memoBase64 && (memo == null || memo.includes('�'))) {
    return { value: memoBase64, base64: true };
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
