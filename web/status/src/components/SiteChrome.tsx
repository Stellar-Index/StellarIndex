import { Activity } from 'lucide-react';

import { Container } from '@/components/ui';

const SITE_URL = 'https://stellarindex.io';

/**
 * SiteHeader echoes the explorer's navbar so the status page reads as part of
 * stellarindex.io: the brand-600 logo tile + "Stellar Index" wordmark on the
 * left, a subtle "Status" label on the right, on a hairline-bordered bar.
 * The wordmark links back to the main site (the status app is a separate
 * static deploy with no internal routes to /).
 */
export function SiteHeader() {
  return (
    <header className="sticky top-0 z-40 border-b border-line bg-surface/80 backdrop-blur-md supports-[backdrop-filter]:bg-surface/70">
      <Container className="flex items-center justify-between py-3">
        <a
          href={SITE_URL}
          className="flex items-center gap-2 text-sm font-semibold tracking-tight text-ink"
        >
          <span className="flex h-6 w-6 items-center justify-center rounded-md bg-brand-600 text-white">
            <Activity className="h-3.5 w-3.5" />
          </span>
          <span>Stellar Index</span>
          <span
            aria-hidden
            className="ml-0.5 hidden h-1 w-1 rounded-full bg-line-strong sm:inline-block"
          />
          <span className="hidden text-sm font-normal text-ink-muted sm:inline">
            Status
          </span>
        </a>
        <nav className="flex items-center gap-1 text-sm">
          <a
            href={SITE_URL}
            className="rounded-md px-3 py-1.5 text-ink-body transition-colors hover:bg-surface-subtle hover:text-brand-600"
          >
            Explorer
          </a>
          <a
            href="https://docs.stellarindex.io"
            className="hidden rounded-md px-3 py-1.5 text-ink-body transition-colors hover:bg-surface-subtle hover:text-brand-600 sm:inline-block"
          >
            API Docs
          </a>
        </nav>
      </Container>
    </header>
  );
}

/**
 * SiteFooter mirrors the explorer footer's lower bar — API host, GitHub,
 * licence — plus a link back to the main site, so the two surfaces feel like
 * one product.
 */
export function SiteFooter() {
  return (
    <footer className="mt-16 border-t border-line bg-surface py-8">
      <Container className="flex flex-wrap items-center justify-between gap-3 text-xs text-ink-muted">
        <div className="flex flex-wrap items-center gap-4">
          <a href={SITE_URL} className="hover:text-ink-body">
            stellarindex.io
          </a>
          <span>
            API:{' '}
            <a
              href="https://api.stellarindex.io"
              className="font-mono hover:text-ink-body"
            >
              api.stellarindex.io
            </a>
          </span>
          <a
            href="https://github.com/StellarIndex/stellar-index"
            target="_blank"
            rel="noopener noreferrer"
            className="hover:text-ink-body"
          >
            GitHub
          </a>
        </div>
        <span>Apache-2.0</span>
      </Container>
    </footer>
  );
}
