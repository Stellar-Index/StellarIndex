/**
 * Wording for a transaction 404. The API says when its index is behind the
 * network; dropping that reads as "the payment did not land".
 */
export function txNotFoundDescription(err: unknown): string {
  const behind =
    err instanceof Error && err.message.includes('may not be indexed yet');
  return behind
    ? 'Not in the index yet. The index is behind the network, so a recent transaction may not be indexed. This does not mean it failed: retry before submitting it again.'
    : 'No transaction with that hash in the indexed range. One submitted in the last few seconds may not be indexed yet.';
}
