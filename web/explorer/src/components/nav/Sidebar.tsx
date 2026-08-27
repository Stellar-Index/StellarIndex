'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import {
  Activity,
  BarChart3,
  BellRing,
  BookOpen,
  Boxes,
  Building2,
  Code2,
  Coins,
  ExternalLink,
  FileCode,
  Gauge,
  Globe,
  KeyRound,
  Layers,
  LayoutDashboard,
  LogOut,
  Radio,
  Receipt,
  Settings,
  ShieldCheck,
  User,
  Wallet,
  Zap,
  type LucideIcon,
} from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';

import { useMe, useStatus } from '@/api/hooks';
import { API_BASE_URL } from '@/api/client';
import { cn } from '@/lib/cn';
import { useDialog } from '@/lib/useDialog';
import { StellarMark } from '@/components/StellarMark';
import { NetworkSwitcher } from './NetworkSwitcher';
import { CURRENT_NETWORK, CURRENT_NETWORK_ID } from '@/lib/networks';
import { SearchModal } from './SearchModal';

type NavItem = {
  href: string;
  label: string;
  icon: LucideIcon;
  external?: boolean;
  exact?: boolean;
  /** Render the live status tone dot (the Status row — A5-03 revival). */
  statusDot?: boolean;
};
type NavGroup = { title?: string; items: NavItem[] };

// The console IA (nav revision 2026-08-24): three sections — Stellar
// (on-chain entities + protocol surfaces), External (off-chain reference
// data), Developers. One entry per entity class; sub-surfaces live on
// their hub pages (Network hosts operations/ledgers, Insights hosts
// anomalies/divergence/MEV, Protocols hosts the per-category venue
// views). Secondary/marketing pages (Pricing, Methodology, Diagnostics,
// Sources, Issuers, AMM boards) stay reachable via hubs, footer +
// search — the rail is deliberately one screen tall.
const NAV: NavGroup[] = [
  {
    title: 'Stellar',
    items: [
      { href: '/network', label: 'Network', icon: Gauge },
      { href: '/ledgers', label: 'Ledgers', icon: Layers },
      { href: '/transactions', label: 'Transactions', icon: Receipt },
      { href: '/accounts', label: 'Accounts', icon: Wallet },
      { href: '/assets', label: 'Assets', icon: Coins },
      { href: '/contracts', label: 'Contracts', icon: FileCode, exact: true },
      { href: '/sdex', label: 'SDEX', icon: BarChart3 },
      { href: '/protocols', label: 'Protocols', icon: Boxes },
      { href: '/oracles', label: 'Oracles', icon: Radio },
      { href: '/insights', label: 'Insights', icon: Zap },
    ],
  },
  {
    title: 'External',
    items: [
      { href: '/exchanges', label: 'Markets', icon: Building2 },
      { href: '/external/assets', label: 'Assets', icon: Globe },
    ],
  },
  {
    title: 'Developers',
    items: [
      { href: 'https://docs.stellarindex.io', label: 'API Docs', icon: BookOpen, external: true },
      { href: '/sdk', label: 'SDK', icon: Code2 },
      { href: '/status', label: 'Status', icon: Activity, statusDot: true },
    ],
  },
];

// The lean test-net explorers (SDEX-only, no aggregator/pricing) carry no
// bespoke Soroban protocols, oracles, aggregator-derived insights, or external
// CEX/FX markets/assets — hide those rail surfaces so the nav reflects what the
// network actually has. The whole External group drops once it is empty.
const TESTNET_HIDDEN_HREFS = new Set([
  '/protocols',
  '/oracles',
  '/insights',
  '/exchanges',
  '/external/assets',
]);

function navForNetwork(groups: NavGroup[]): NavGroup[] {
  if (CURRENT_NETWORK_ID === 'mainnet') return groups;
  return groups
    .map((g) => ({ ...g, items: g.items.filter((it) => !TESTNET_HIDDEN_HREFS.has(it.href)) }))
    .filter((g) => g.items.length > 0);
}

// Shown only when signed in — the logged-in "Account" section (the former
// standalone dashboard, now part of the site). The Admin row is appended
// only for staff sessions (see SidebarNav).
const ACCOUNT_GROUP: NavGroup = {
  title: 'Account',
  items: [
    // LC-020: link the ACTUAL served routes (/dashboard/*). These used to
    // point at /account/* and relied on a Cloudflare 301 — so the active
    // state never matched the served URL and the links 404'd under `next dev`.
    { href: '/dashboard', label: 'Dashboard', icon: LayoutDashboard, exact: true },
    { href: '/dashboard/keys', label: 'API keys', icon: KeyRound },
    { href: '/dashboard/price-alerts', label: 'Price alerts', icon: BellRing },
    { href: '/dashboard/usage', label: 'Usage', icon: Gauge },
    { href: '/dashboard/settings', label: 'Settings', icon: Settings },
  ],
};

const ADMIN_ITEM: NavItem = {
  href: '/dashboard/admin',
  label: 'Admin',
  icon: ShieldCheck,
};

function isActive(pathname: string | null, href: string): boolean {
  if (!pathname) return false;
  if (href === '/') return pathname === '/';
  return pathname === href || pathname.startsWith(href + '/');
}

function Row({ item, onNavigate }: { item: NavItem; onNavigate?: () => void }) {
  const pathname = usePathname();
  const active =
    !item.external && (item.exact ? pathname === item.href : isActive(pathname, item.href));
  const Icon = item.icon;
  const cls = cn(
    'group flex items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-sm font-medium transition-colors',
    active
      ? 'bg-surface text-ink shadow-xs ring-1 ring-line'
      : 'text-ink-body hover:bg-surface-subtle hover:text-ink',
  );
  const inner = (
    <>
      <Icon className={cn('h-4 w-4 shrink-0', active ? 'text-brand-600' : 'text-ink-faint group-hover:text-ink-muted')} />
      <span className="truncate">{item.label}</span>
      {item.statusDot && <StatusDot />}
      {item.external && <ExternalLink className="ml-auto h-3 w-3 text-ink-faint" />}
    </>
  );
  if (item.external) {
    return (
      <a href={item.href} className={cls} onClick={onNavigate}>
        {inner}
      </a>
    );
  }
  return (
    <Link href={item.href} className={cls} onClick={onNavigate} aria-current={active ? 'page' : undefined}>
      {inner}
    </Link>
  );
}

/**
 * StatusDot — the Status row's live tone dot (green/amber/red), revived
 * per FEC A5-03/D2: the navbar pill was lost in the console-shell redesign
 * (36e0a3c7, Navbar deleted without re-homing it) and its data hook
 * orphaned. Reads the SAME shared useStatus query as DegradedBanner and
 * the /status page — one poll loop, one truth per viewport. WB-04
 * honesty: when the latest poll failed (or none has succeeded yet) we
 * make NO claim — a muted "unknown" dot, never a stale green.
 */
function StatusDot() {
  const feed = useStatus().data;
  const overall =
    feed && feed.error === null ? feed.status?.overall : undefined;
  const tone =
    overall === 'ok'
      ? { cls: 'bg-ok-500', label: 'all systems operational' }
      : overall === 'degraded'
        ? { cls: 'bg-warn-500', label: 'degraded performance' }
        : overall === 'down'
          ? { cls: 'bg-bad-500', label: 'major incident' }
          : { cls: 'bg-line-strong', label: 'status unknown' };
  return (
    <span className="ml-auto flex items-center" title={tone.label}>
      <span aria-hidden className={cn('h-2 w-2 rounded-full', tone.cls)} />
      <span className="sr-only">({tone.label})</span>
    </span>
  );
}

/** The console nav body — shared by the desktop rail + the mobile drawer. */
export function SidebarNav({ onNavigate }: { onNavigate?: () => void }) {
  const me = useMe();
  const signedIn = !!(me.data && (me.data.user?.email || me.data.key_id));
  const isStaff = !!me.data?.user?.is_staff;
  // Logged-in users get the Account section at the BOTTOM of the rail (just
  // above the account card), after the explorer/protocol/analytics groups;
  // staff sessions also get the Admin cockpit row.
  const accountGroup: NavGroup = isStaff
    ? { ...ACCOUNT_GROUP, items: [...ACCOUNT_GROUP.items, ADMIN_ITEM] }
    : ACCOUNT_GROUP;
  const groups = navForNetwork(signedIn ? [...NAV, accountGroup] : NAV);
  return (
    <div className="flex h-full flex-col bg-surface-muted">
      {/* Logo row — the Stellar mark + wordmark on the left, and the odometer
          (a single hoverable control: network name + chevron over THIS network's
          live ledger; click anywhere on it to open the network switcher) floated
          to the right, next to the wordmark. */}
      <div className="flex h-14 shrink-0 items-center gap-2 px-4">
        <Link
          href="/"
          onClick={onNavigate}
          className="flex min-w-0 items-center gap-2 font-sans text-base font-semibold tracking-tight text-ink"
        >
          <StellarMark className="h-5 w-5 shrink-0 text-ink" />
          {/* Wordmark weight contrast (2026-08-24): "Stellar" carries the
              brand weight, "Index" sits lighter — same ink, thinner cut. */}
          <span className="truncate">
            Stellar
            <span className="font-light">Index</span>
          </span>
        </Link>
        <div className="ml-auto shrink-0">
          <NetworkSwitcher onNavigate={onNavigate} />
        </div>
      </div>

      {/* Search — directly below the logo */}
      <div className="px-3 pb-3">
        <SearchModal />
      </div>

      {/* Nav */}
      <nav className="flex-1 space-y-5 overflow-y-auto px-3 pb-4">
        {groups.map((group, gi) => (
          <div key={group.title ?? `g${gi}`} className="space-y-0.5">
            {group.title && (
              <div className="px-2.5 pb-1 text-[11px] font-semibold uppercase tracking-wider text-ink-faint">
                {group.title}
              </div>
            )}
            {group.items.map((it) => (
              <Row key={it.href} item={it} onNavigate={onNavigate} />
            ))}
          </div>
        ))}
      </nav>

      {/* Account — bottom-left */}
      <div className="shrink-0 border-t border-line p-3">
        <AccountCard onNavigate={onNavigate} />
      </div>
    </div>
  );
}

/** The persistent desktop left rail (hidden on mobile; drawer handles small screens). */
export function Sidebar() {
  return (
    <aside className="sticky top-0 hidden h-screen w-64 shrink-0 border-r border-line lg:block">
      <SidebarNav />
    </aside>
  );
}

// ─── Account (bottom-left) ─────────────────────────────────────────────────

function AccountCard({ onNavigate }: { onNavigate?: () => void }) {
  const me = useMe();
  const signedIn = !!(me.data && (me.data.user?.email || me.data.key_id));
  const email = me.data?.user?.email;

  // No accounts backend on the lean test nets — hide the sign-in / account
  // surface entirely rather than showing CTAs that lead to a 503.
  if (!CURRENT_NETWORK.accounts) return null;

  return (
    <div className="space-y-2">
      {signedIn ? (
        <AccountMenu email={email} />
      ) : (
        <div className="grid grid-cols-2 gap-2">
          <Link
            href="/signin"
            onClick={onNavigate}
            className="rounded-lg border border-line bg-surface px-3 py-1.5 text-center text-sm font-medium text-ink-body shadow-xs hover:bg-surface-subtle"
          >
            Sign in
          </Link>
          <Link
            href="/signup"
            onClick={onNavigate}
            className="rounded-lg bg-brand-600 px-3 py-1.5 text-center text-sm font-medium text-white hover:bg-brand-700"
          >
            Sign up
          </Link>
        </div>
      )}
    </div>
  );
}

function AccountMenu({ email }: { email?: string }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    function onDoc(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);
  // ACC-06: focus-trap + focus-move-in + focus-restore (was missing —
  // Tab could escape the open menu into the rest of the page, and
  // closing never returned focus to the trigger button). Escape-to-close
  // is now owned by useDialog too, hence dropping the separate onEsc
  // listener above. Deliberately NOT role="dialog"/aria-modal — see the
  // role comment below: this is a disclosure, not a modal, so useDialog
  // is used here only for its focus mechanics.
  const close = useCallback(() => setOpen(false), []);
  const panelRef = useDialog<HTMLDivElement>(open, close);

  async function signOut() {
    try {
      await fetch(`${API_BASE_URL}/v1/auth/logout`, { method: 'POST', credentials: 'include' });
    } catch {
      /* best-effort */
    }
    window.location.href = '/';
  }

  const initials = (email ?? 'A').slice(0, 1).toUpperCase();

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        aria-controls="sidebar-account-menu"
        className="flex w-full items-center gap-2.5 rounded-lg border border-line bg-surface px-2.5 py-2 text-left shadow-xs hover:bg-surface-subtle"
      >
        <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-brand-600 text-xs font-semibold text-white">
          {initials}
        </span>
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm font-medium text-ink">{email ?? 'Account'}</span>
          <span className="block truncate text-[11px] text-ink-muted">Signed in</span>
        </span>
      </button>
      {open && (
        // Disclosure, not an APG menu: we don't implement the menu
        // keyboard model (Arrow/Home/End), so declaring role="menu"/
        // "menuitem" would promise a behaviour we don't provide and
        // mislead AT. The trigger's aria-expanded + aria-controls
        // describe it correctly; items are plain links/buttons (Tab-
        // reachable), and Escape closes it (handled above).
        <div
          id="sidebar-account-menu"
          ref={panelRef}
          tabIndex={-1}
          className="absolute bottom-full left-0 z-50 mb-1 w-full rounded-lg border border-line bg-surface p-2 shadow-elevated outline-hidden"
        >
          <Link
            href="/dashboard"
            onClick={() => setOpen(false)}
            className="flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-surface-subtle"
          >
            <User className="h-3.5 w-3.5 text-ink-faint" />
            Your account
          </Link>
          <button
            type="button"
            onClick={signOut}
            className="flex w-full items-center gap-2 rounded-md px-3 py-2 text-left text-sm text-ink-body hover:bg-surface-subtle"
          >
            <LogOut className="h-3.5 w-3.5 text-ink-faint" />
            Sign out
          </button>
        </div>
      )}
    </div>
  );
}
