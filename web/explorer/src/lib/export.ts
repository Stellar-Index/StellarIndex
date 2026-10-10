export type CsvCell = string | number | boolean | null | undefined;

function csvField(v: CsvCell): string {
  if (v === null || v === undefined) return '';
  const s = String(v);
  return /[",\r\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

/**
 * RFC 4180 CSV: a header row of `columns`, then one row per record in that
 * column order. Cells are written as given — callers pass API decimal strings
 * untouched so an exported amount is the served amount (ADR-0003).
 */
export function toCsv<K extends string>(
  columns: readonly K[],
  rows: readonly { readonly [P in K]?: CsvCell }[],
): string {
  const lines = [columns.map(csvField).join(',')];
  for (const row of rows)
    lines.push(columns.map((c) => csvField(row[c])).join(','));
  return lines.join('\r\n') + '\r\n';
}

/** Save `text` as a file through a transient object URL. */
export function downloadText(filename: string, mime: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: mime }));
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoking in the same task can cancel the download in some browsers.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
