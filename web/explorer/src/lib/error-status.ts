/** HTTP status parsed from an `apiGet` error message, or null when it has none. */
export function errorStatus(err: unknown): number | null {
  if (!(err instanceof Error)) return null;
  const m = /^(\d{3}) /.exec(err.message);
  return m === null ? null : Number(m[1]);
}
