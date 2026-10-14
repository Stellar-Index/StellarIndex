import Link from 'next/link';
import type { ComponentProps } from 'react';
import { Badge } from '@/components/ui';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { Inline } from '@/lib/markdown';

interface ChangeItem {
  release: string;
  date?: string;
  kind: string;
  text: string;
}

/**
 * HomeRecentChanges — top-of-fold "Recently shipped" widget.
 *
 * Renders the 3 most recent changelog bullets from the latest
 * non-Unreleased release block in CHANGELOG.md. Helps a returning
 * visitor immediately see "what changed since I was last here?"
 * without clicking through to /changelog.
 *
 * Server-component; reads CHANGELOG.md at build time. The full
 * /changelog page does the same parse for everything; this just
 * keeps the head of the head.
 */
export function HomeRecentChanges() {
  const items = readRecentItems(3);
  if (items.length === 0) return null;
  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <div className="space-y-1">
          <h2 className="text-h3 text-ink font-semibold">Recently shipped</h2>
          <p className="text-ink-body text-sm">
            What landed in the last release. Scrolling history at{' '}
            <Link href="/changelog" className="text-brand-600 hover:underline">
              /changelog
            </Link>
            .
          </p>
        </div>
        <div className="flex items-center gap-3 text-xs">
          <a
            href="/changelog.atom"
            target="_blank"
            rel="noreferrer noopener"
            className="text-ink-muted hover:text-brand-600"
            title="Atom feed — subscribe in Feedly, Slack RSS bot, etc."
          >
            Subscribe (Atom) ↗
          </a>
          <Link href="/changelog" className="text-brand-600 hover:underline">
            Full changelog
          </Link>
        </div>
      </div>
      <ul className="space-y-2">
        {items.map((it, i) => (
          <li key={i} className="border-line bg-surface rounded-md border p-3">
            <div className="mb-1 flex items-baseline gap-2">
              <Badge tone={kindTone(it.kind)}>{it.kind}</Badge>
              <span className="text-ink-faint font-mono text-[11px]">
                {it.release}
                {it.date && ` · ${it.date}`}
              </span>
            </div>
            <p className="text-ink-body text-sm">
              <ChangelogPreview text={it.text} />
            </p>
          </li>
        ))}
      </ul>
    </section>
  );
}

// readRecentItems pulls the first N bullet entries from the most
// recent non-Unreleased release in CHANGELOG.md. Failures (file
// missing, no parseable releases) return [] so the home page
// silently omits the widget rather than blowing up the build.
// Operator-actions blocks are run-book steps, not shipped changes.
const SHIPPED_KINDS = new Set([
  'Added',
  'Changed',
  'Fixed',
  'Removed',
  'Deprecated',
  'Security',
]);

function readRecentItems(n: number): ChangeItem[] {
  let text = '';
  try {
    text = readFileSync(join(process.cwd(), '../../CHANGELOG.md'), 'utf8');
  } catch {
    return [];
  }
  const lines = text.split('\n');
  let release = '';
  let date: string | undefined;
  let kind = '';
  let foundReleased = false;
  const out: ChangeItem[] = [];
  let buf = '';

  function flushBuf() {
    if (buf && release && kind) {
      out.push({ release, date, kind, text: buf.trim() });
      buf = '';
    }
  }

  for (const line of lines) {
    const releaseMatch = line.match(/^##\s+\[([^\]]+)\](?:\s+—\s+(\S+))?/);
    if (releaseMatch) {
      flushBuf();
      if (out.length >= n && foundReleased) break;
      release = releaseMatch[1]!;
      date = releaseMatch[2];
      kind = '';
      foundReleased = release.toLowerCase() !== 'unreleased';
      // We render Unreleased too — fresh main commits should
      // surface. The "last release" framing is fuzzy on purpose.
      continue;
    }
    if (!release) continue;
    const blockMatch = line.match(/^###\s+(.+)$/);
    if (blockMatch) {
      flushBuf();
      kind = blockMatch[1]!.trim();
      continue;
    }
    if (out.length >= n) continue;
    if (/^- /.test(line) && SHIPPED_KINDS.has(kind)) {
      flushBuf();
      buf = line.replace(/^- /, '');
    } else if (buf) {
      buf += '\n' + line;
    }
  }
  flushBuf();
  return out.slice(0, n);
}

// Removals and deprecations are the entries an integrator must act on.
function kindTone(kind: string): ComponentProps<typeof Badge>['tone'] {
  if (kind === 'Added') return 'brand';
  if (kind === 'Removed' || kind === 'Deprecated') return 'warn';
  return 'neutral';
}

// ChangelogPreview renders just the FIRST paragraph of a CHANGELOG
// entry — bold + code + link substitutions, then truncate at the
// first newline-newline (matches the visual rhythm where the bold
// heading is the "what" and the rest is the "why"). Tokenizing is the
// canonical lib/markdown Inline; links render as
// plain spans because this strip is a preview, not a link surface.
function ChangelogPreview({ text }: { text: string }) {
  const firstPara = text.split(/\n\n/)[0]!.trim();
  return <Inline text={firstPara} plainLinks />;
}
