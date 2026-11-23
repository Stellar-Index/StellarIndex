'use client';

import { useCursors } from '@/api/hooks';
import { asExample } from '@/api/client';
import { Panel } from '@/components/reveal';
import {
  Badge,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';

// The cursor's `source` is the cursor TYPE (backfill / projector /
// ledgerstream); the venue lives in `sub_source` — projector rows carry it
// bare ("soroswap"), backfill rows prefix a ledger range ("<from-to>:sdex").
function cursorVenue(c: { sub_source?: string }): string {
  const ss = (c.sub_source ?? '').trim();
  const colon = ss.lastIndexOf(':');
  return colon >= 0 ? ss.slice(colon + 1) : ss;
}

/** The ingest cursors for one source, read live: a lag baked at build time would vouch for a stalled cursor. */
export function SourceCursorsPanel({ source }: { source: string }) {
  const { data, isLoading, error } = useCursors();
  const cursors = data?.filter((c) => cursorVenue(c) === source);

  return (
    <Panel
      headingLevel={2}
      title="Ingest cursors"
      hint={`${cursors ? cursors.length : '—'} entries · live ingest position per sub-source · refreshes every 15s`}
      source={asExample('/v1/diagnostics/cursors')}
      bodyClassName={cursors && cursors.length > 0 ? '-mx-4 -mb-4' : undefined}
    >
      {isLoading && <Skeleton className="h-[5.5rem]" />}
      {error != null && !isLoading && (
        <p className="text-ink-muted text-sm">
          Ingest cursors unavailable right now.
        </p>
      )}
      {cursors && cursors.length === 0 && (
        <p className="text-ink-muted text-sm">
          No cursor recorded for this source.
        </p>
      )}
      {cursors && cursors.length > 0 && (
        // WCAG 1.4.10 Reflow: wide mono columns scroll inside the panel
        // instead of pushing the whole page sideways at 320px.
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Sub-source</Th>
                <Th align="right">Last ledger</Th>
                <Th align="right">Updated</Th>
                <Th align="right">Lag</Th>
              </tr>
            </THead>
            <TBody>
              {cursors.map((c, i) => (
                <TR key={`${c.sub_source ?? ''}|${i}`}>
                  <Td className="font-mono text-xs">{c.sub_source || '—'}</Td>
                  <Td align="right" className="font-mono">
                    #{c.last_ledger}
                  </Td>
                  <Td
                    align="right"
                    className="text-ink-muted font-mono text-xs"
                  >
                    {c.last_updated.replace('T', ' ').slice(0, 19)} UTC
                  </Td>
                  <Td align="right">
                    <LagBadge seconds={c.lag_seconds} />
                  </Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </div>
      )}
    </Panel>
  );
}

function LagBadge({ seconds }: { seconds: number }) {
  const s = Math.max(0, seconds);
  const tone = s <= 60 ? 'ok' : s <= 600 ? 'warn' : 'bad';
  const label =
    s < 60
      ? `${s.toFixed(0)}s`
      : s < 3600
        ? `${(s / 60).toFixed(1)}m`
        : `${(s / 3600).toFixed(1)}h`;
  return (
    <Badge tone={tone} className="tnum">
      {label}
    </Badge>
  );
}
