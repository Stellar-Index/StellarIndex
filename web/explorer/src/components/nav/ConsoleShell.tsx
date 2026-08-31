'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { Menu, X } from 'lucide-react';
import { useCallback, useState, type ReactNode } from 'react';
import { StellarMark } from '@/components/StellarMark';

import { useDialog } from '@/lib/useDialog';

import { DegradedBanner } from './DegradedBanner';
import { Sidebar, SidebarNav } from './Sidebar';

/**
 * ConsoleShell is the app frame: a single persistent left Sidebar (logo →
 * search → grouped nav → account) over a scrolling content column — no top
 * bar. On small screens the sidebar collapses; a minimal mobile header
 * (logo + menu) opens it as a drawer. Embed routes (/embed/*) render
 * chrome-free for iframing.
 */
export function ConsoleShell({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const [drawer, setDrawer] = useState(false);
  // Close the drawer whenever the route changes. Adjusting state during
  // render (comparing against the previous pathname) rather than in an
  // effect — no commit/paint of the open drawer on navigation.
  const [prevPathname, setPrevPathname] = useState(pathname);
  if (pathname !== prevPathname) {
    setPrevPathname(pathname);
    setDrawer(false);
  }
  // LC-051: the mobile drawer is the primary mobile nav — give it the full
  // modal contract (Escape + focus trap + focus move-in/restore), not just
  // Escape. The shared hook handles all of it.
  const closeDrawer = useCallback(() => setDrawer(false), []);
  const drawerRef = useDialog<HTMLDivElement>(drawer, closeDrawer);

  if (pathname?.startsWith('/embed/')) return <>{children}</>;

  return (
    <div className="flex min-h-screen">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        {/* Mobile-only header — the desktop layout has no top bar. */}
        <div className="border-line bg-surface flex h-14 items-center justify-between border-b px-4 lg:hidden">
          <Link
            href="/"
            className="text-ink flex items-center gap-2 text-sm font-semibold tracking-tight"
          >
            {/* Must match the desktop sidebar's brand exactly (Sidebar.tsx):
                the official Stellar mark plus the two-weight wordmark. This
                was a generic TrendingUp glyph on a brand-colour square — the
                pre-2026-08-24 placeholder — so the mobile header showed a
                different logo from every other surface. */}
            <StellarMark className="h-5 w-5 shrink-0 text-ink" />
            <span className="truncate">
              Stellar
              <span className="font-light">Index</span>
            </span>
          </Link>
          <button
            type="button"
            onClick={() => setDrawer(true)}
            aria-label="Open navigation"
            aria-expanded={drawer}
            aria-controls="mobile-nav-drawer"
            className="text-ink-body hover:bg-surface-subtle -mr-1 inline-flex items-center justify-center rounded-md p-2"
          >
            <Menu className="h-5 w-5" />
          </button>
        </div>

        <DegradedBanner />
        <main id="main" className="flex-1">
          {children}
        </main>
      </div>

      {/* Mobile drawer */}
      {drawer && (
        <div className="fixed inset-0 z-50 lg:hidden">
          <div
            className="absolute inset-0 bg-black/60 backdrop-blur-sm"
            onClick={() => setDrawer(false)}
            aria-hidden
          />
          <div
            ref={drawerRef}
            id="mobile-nav-drawer"
            tabIndex={-1}
            role="dialog"
            aria-modal="true"
            aria-label="Navigation"
            className="border-line bg-surface shadow-elevated absolute top-0 left-0 h-full w-72 max-w-[85vw] border-r outline-hidden"
          >
            <button
              type="button"
              onClick={() => setDrawer(false)}
              aria-label="Close navigation"
              className="text-ink-body hover:bg-surface-subtle absolute top-3 right-2 z-10 inline-flex items-center justify-center rounded-md p-2"
            >
              <X className="h-5 w-5" />
            </button>
            <SidebarNav onNavigate={() => setDrawer(false)} />
          </div>
        </div>
      )}
    </div>
  );
}
