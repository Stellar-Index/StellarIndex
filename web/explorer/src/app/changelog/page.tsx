import type { Metadata } from 'next';
import Link from 'next/link';

import { loadReleases, versionSlug, type Release } from '@/lib/changelog';
import { Inline } from '@/lib/markdown';

// Cap the rendered changelog to the most recent N releases. The full
// history (242+ sections) inlined to a ~4.4 MB page (audit 2026-06-19);
// older entries remain in CHANGELOG.md / GitHub releases.
const RECENT_RELEASES = 40;

export const metadata: Metadata = {
  title: 'Changelog',
  description:
    'Every release of Stellar Index — features added, bugs fixed, and the architectural changes behind them. Source: CHANGELOG.md.',
  alternates: { canonical: '/changelog' },
};

export default function ChangelogPage() {
  const releases: Release[] = loadReleases();
  return (
    <div className="mx-auto max-w-4xl space-y-8 px-6 py-10">
      <header className="space-y-3">
        <div className="flex items-baseline justify-between">
          <p className="text-brand-600 font-mono text-xs tracking-widest uppercase">
            Changelog
          </p>
          <a
            href="/changelog.atom"
            target="_blank"
            rel="noreferrer noopener"
            className="text-ink-muted hover:text-brand-600 text-xs"
            title="Atom feed — subscribe in Feedly, Slack RSS bot, etc."
          >
            Subscribe (Atom) ↗
          </a>
        </div>
        <h1 className="text-4xl font-semibold tracking-tight">
          Every release, every change.
        </h1>
        <p className="text-ink-body max-w-2xl text-base">
          Pulled at build time from{' '}
          <code className="bg-surface-subtle rounded-sm px-1.5 py-0.5 font-mono text-sm">
            CHANGELOG.md
          </code>{' '}
          on{' '}
          <a
            href="https://github.com/Stellar-Index/StellarIndex/blob/main/CHANGELOG.md"
            target="_blank"
            rel="noreferrer noopener"
            className="text-brand-600 hover:underline"
          >
            main
          </a>
          . Format follows{' '}
          <a
            href="https://keepachangelog.com/en/1.1.0/"
            target="_blank"
            rel="noreferrer noopener"
            className="text-brand-600 hover:underline"
          >
            Keep a Changelog
          </a>
          ; SemVer for the public Go SDK, CalVer for binary releases.
        </p>
      </header>

      {releases.length === 0 ? (
        <div className="border-warn-300 bg-warn-50 text-warn-700 rounded-md border p-6 text-sm">
          CHANGELOG.md not found at build time — this page is a stub. See the{' '}
          <a
            href="https://github.com/Stellar-Index/StellarIndex/blob/main/CHANGELOG.md"
            target="_blank"
            rel="noreferrer noopener"
            className="underline"
          >
            canonical changelog on GitHub
          </a>
          .
        </div>
      ) : (
        <div className="space-y-10">
          {/* Cap the rendered set to the most recent releases — the full
              242-section history inlined to ~4.4 MB of HTML (audit
              2026-06-19). Older releases stay one click away on GitHub. */}
          {releases.slice(0, RECENT_RELEASES).map((r) => (
            <ReleaseCard key={r.version} release={r} />
          ))}
          {releases.length > RECENT_RELEASES && (
            <p className="text-ink-muted text-sm">
              Showing the {RECENT_RELEASES} most recent of {releases.length}{' '}
              releases.{' '}
              <a
                href="https://github.com/Stellar-Index/StellarIndex/blob/main/CHANGELOG.md"
                target="_blank"
                rel="noreferrer noopener"
                className="text-brand-600 hover:underline"
              >
                Full changelog →
              </a>
            </p>
          )}
        </div>
      )}

      <div className="border-line text-ink-muted border-t pt-6 text-sm">
        <Link href="/" className="text-brand-600 hover:underline">
          ← Home
        </Link>
        <span className="mx-2">·</span>
        <a
          href="https://github.com/Stellar-Index/StellarIndex/releases"
          target="_blank"
          rel="noreferrer noopener"
          className="text-brand-600 hover:underline"
        >
          GitHub Releases ↗
        </a>
      </div>
    </div>
  );
}

function ReleaseCard({ release }: { release: Release }) {
  const isUnreleased = release.version.toLowerCase() === 'unreleased';
  // `id` lets the atom feed's `#<slug>` anchors actually scroll
  // here — without this, feed-reader subscribers land on the
  // changelog page with no scroll target. The slug shape mirrors
  // changelog.atom/route.ts via the shared `versionSlug` helper.
  const id = versionSlug(release.version);
  return (
    <article
      id={id}
      className="border-line bg-surface scroll-mt-20 rounded-lg border p-6 shadow-sm"
    >
      <header className="border-line-subtle mb-4 flex flex-wrap items-baseline justify-between gap-2 border-b pb-3">
        <h2 className="font-mono text-2xl font-semibold tracking-tight">
          <a href={`#${id}`} className="hover:text-brand-600">
            {release.version}
          </a>
        </h2>
        <div className="flex items-center gap-2 text-xs">
          {isUnreleased ? (
            <span className="bg-warn-50 text-warn-700 rounded-sm px-2 py-0.5 font-mono tracking-wider uppercase">
              unreleased
            </span>
          ) : (
            release.date && (
              <span className="text-ink-muted font-mono tabular-nums">
                {release.date}
              </span>
            )
          )}
          {!isUnreleased && (
            <a
              href={`https://github.com/Stellar-Index/StellarIndex/releases/tag/${release.version}`}
              target="_blank"
              rel="noreferrer noopener"
              className="border-line hover:border-brand-500 hover:text-brand-600 rounded-sm border px-2 py-0.5 font-mono text-xs"
            >
              GitHub ↗
            </a>
          )}
        </div>
      </header>
      <div className="space-y-4">
        {release.blocks.map((b, i) => (
          <BlockSection key={i} block={b} />
        ))}
      </div>
    </article>
  );
}

function BlockSection({ block }: { block: { kind: string; lines: string[] } }) {
  const tone =
    block.kind === 'Added'
      ? 'text-up'
      : block.kind === 'Fixed'
        ? 'text-brand-700'
        : block.kind === 'Changed'
          ? 'text-warn-700'
          : block.kind === 'Removed' || block.kind === 'Deprecated'
            ? 'text-down'
            : 'text-ink-body';

  // Strip the leading "- " bullet from list items so we can group
  // by sub-item; preserve everything else as raw markdown the
  // browser doesn't have to parse heavily — bullet lines start a
  // new entry, indented continuation lines glue to the previous.
  const items: string[] = [];
  let buf = '';
  for (const line of block.lines) {
    if (/^- /.test(line)) {
      if (buf) items.push(buf);
      buf = line.replace(/^- /, '');
    } else if (buf) {
      buf += '\n' + line;
    }
  }
  if (buf) items.push(buf);

  return (
    <section>
      <h3
        className={`mb-2 text-xs font-semibold tracking-wider uppercase ${tone}`}
      >
        {block.kind}
      </h3>
      <ul className="text-ink-body space-y-2 text-sm">
        {items.map((it, i) => (
          <li key={i}>
            <Inline text={it} />
          </li>
        ))}
      </ul>
    </section>
  );
}
