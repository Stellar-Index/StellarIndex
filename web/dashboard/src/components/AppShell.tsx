'use client';

import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import {
  LayoutDashboard,
  KeyRound,
  BarChart3,
  Settings,
  LogOut,
  ShieldCheck,
  TrendingUp,
  ExternalLink,
  type LucideIcon,
} from 'lucide-react';
import { useState, type ReactNode } from 'react';
import { useAuth } from '@/lib/auth';
import { logout, type AccountMe } from '@/lib/api';
import { tierLabel } from '@/lib/format';
import { cn } from '@/lib/cn';

interface NavItem {
  href: string;
  label: string;
  icon: LucideIcon;
}

const nav: NavItem[] = [
  { href: '/overview/', label: 'Overview', icon: LayoutDashboard },
  { href: '/keys/', label: 'API keys', icon: KeyRound },
  { href: '/usage/', label: 'Usage', icon: BarChart3 },
  { href: '/settings/', label: 'Settings', icon: Settings },
];

const staffNav: NavItem[] = [{ href: '/admin/', label: 'Staff', icon: ShieldCheck }];

const DOCS_URL = 'https://docs.stellarindex.io';

export function AppShell({ me, children }: { me: AccountMe; children: ReactNode }) {
  const router = useRouter();
  const [signingOut, setSigningOut] = useState(false);

  async function handleLogout() {
    setSigningOut(true);
    try {
      await logout();
    } finally {
      // Either way, push to /signin and let the AuthProvider re-resolve.
      // Even a network failure here doesn't block the user from leaving —
      // the next /me call will 401 if the cookie did get cleared, and if
      // it didn't, /signin will quietly bounce them back to /overview.
      router.replace('/signin/');
    }
  }

  return (
    <div className="flex min-h-screen">
      {/* ── Sidebar ───────────────────────────────────────────────── */}
      <aside className="sticky top-0 flex h-screen w-64 shrink-0 flex-col border-r border-line bg-surface">
        {/* Brand mark — mirrors the explorer navbar */}
        <div className="flex h-16 items-center gap-2.5 border-b border-line px-5">
          <Link href="/overview/" className="flex items-center gap-2.5">
            <span className="flex h-7 w-7 items-center justify-center rounded-md bg-brand-600 text-white">
              <TrendingUp className="h-4 w-4" />
            </span>
            <span className="flex flex-col leading-none">
              <span className="text-sm font-semibold tracking-tight text-ink">
                Stellar Index
              </span>
              <span className="mt-0.5 text-[11px] font-medium text-ink-faint">
                Dashboard
              </span>
            </span>
          </Link>
        </div>

        {/* Primary nav */}
        <nav className="flex-1 overflow-y-auto px-3 py-4">
          <ul className="space-y-0.5">
            {nav.map((item) => (
              <li key={item.href}>
                <NavLink item={item} />
              </li>
            ))}
          </ul>

          {me.user.is_staff && (
            <>
              <div className="mt-6 px-3 pb-1.5 text-[11px] font-medium uppercase tracking-wider text-ink-faint">
                Staff
              </div>
              <ul className="space-y-0.5">
                {staffNav.map((item) => (
                  <li key={item.href}>
                    <NavLink item={item} />
                  </li>
                ))}
              </ul>
            </>
          )}

          <div className="mt-6 px-3 pb-1.5 text-[11px] font-medium uppercase tracking-wider text-ink-faint">
            Resources
          </div>
          <ul className="space-y-0.5">
            <li>
              <a
                href={DOCS_URL}
                target="_blank"
                rel="noopener noreferrer"
                className="flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm text-ink-muted transition-colors hover:bg-surface-muted hover:text-ink"
              >
                <ExternalLink className="h-[18px] w-[18px] shrink-0" />
                <span className="flex-1">API docs</span>
              </a>
            </li>
          </ul>
        </nav>

        {/* Account footer */}
        <div className="border-t border-line p-3">
          <div className="flex items-center gap-2.5 rounded-lg px-2 py-1.5">
            <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-brand-50 text-xs font-semibold uppercase text-brand-700">
              {initials(me.user.display_name || me.user.email)}
            </span>
            <div className="min-w-0 flex-1">
              <div className="truncate text-[13px] font-medium text-ink">
                {me.user.email}
              </div>
              <div className="truncate text-xs text-ink-faint">
                {me.account.name} · {tierLabel(me.account.tier)}
              </div>
            </div>
          </div>
          <button
            type="button"
            onClick={handleLogout}
            disabled={signingOut}
            className="mt-1 flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-sm text-ink-muted transition-colors hover:bg-surface-muted hover:text-ink disabled:opacity-60"
          >
            <LogOut className="h-[18px] w-[18px] shrink-0" />
            {signingOut ? 'Signing out…' : 'Sign out'}
          </button>
        </div>
      </aside>

      {/* ── Content ───────────────────────────────────────────────── */}
      <main className="min-w-0 flex-1">{children}</main>
    </div>
  );
}

function NavLink({ item }: { item: NavItem }) {
  const pathname = usePathname();
  const active = pathname.startsWith(item.href);
  const Icon = item.icon;
  return (
    <Link
      href={item.href}
      aria-current={active ? 'page' : undefined}
      className={cn(
        'flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm font-medium transition-colors',
        active
          ? 'bg-brand-50 text-brand-700'
          : 'text-ink-muted hover:bg-surface-muted hover:text-ink',
      )}
    >
      <Icon className="h-[18px] w-[18px] shrink-0" />
      {item.label}
    </Link>
  );
}

/** Two-letter initials from a name or email local-part. */
function initials(s: string): string {
  const local = s.includes('@') ? s.split('@')[0] : s;
  const parts = local.split(/[.\s_-]+/).filter(Boolean);
  if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
  return local.slice(0, 2).toUpperCase();
}

export function useAuthGate(): AccountMe | null {
  const { state } = useAuth();
  if (state.kind !== 'authed') return null;
  return state.me;
}
