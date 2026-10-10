'use client';

import { Button } from '@/components/ui';
import { downloadText, toCsv, type CsvCell } from '@/lib/export';

export type PanelDownloadData = {
  /** File name without extension. */
  name: string;
  columns: readonly string[];
  /** Rows as served; amounts stay decimal strings (ADR-0003). */
  rows: readonly Record<string, CsvCell>[];
};

/** CSV + JSON download of the rows a panel already holds. */
export function PanelDownload({ name, columns, rows }: PanelDownloadData) {
  if (rows.length === 0) return null;
  return (
    <div
      role="group"
      aria-label="Download panel data"
      className="flex items-center gap-1"
    >
      <Button
        variant="ghost"
        size="sm"
        onClick={() =>
          downloadText(
            `${name}.csv`,
            'text/csv;charset=utf-8',
            toCsv(columns, rows),
          )
        }
      >
        CSV
      </Button>
      <Button
        variant="ghost"
        size="sm"
        onClick={() =>
          downloadText(
            `${name}.json`,
            'application/json',
            JSON.stringify(rows, null, 2),
          )
        }
      >
        JSON
      </Button>
    </div>
  );
}
