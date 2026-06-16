'use client';

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { Loader2 } from 'lucide-react';
import { useAuth } from '@/lib/auth';
import { AppShell } from './AppShell';

// AuthedRoute is a client wrapper that gates a page on a valid
// session. Loading shows a faint placeholder; anonymous redirects
// to /signin/; authenticated renders the AppShell with the page
// as its main child.
//
// This wraps each (authed) page rather than a Next layout group
// because a layout component can't redirect synchronously — the
// /signin route would briefly flash through the AppShell before
// the bounce. Wrapping per-page keeps the loading state inside
// the wrapper itself and the bounce explicit.
export function AuthedRoute({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const { state } = useAuth();

  useEffect(() => {
    if (state.kind === 'anon') router.replace('/signin/');
  }, [state, router]);

  if (state.kind === 'loading' || state.kind === 'anon') {
    return (
      <div className="flex min-h-screen items-center justify-center gap-2 text-sm text-ink-muted">
        <Loader2 className="h-4 w-4 animate-spin text-brand-600" />
        {state.kind === 'loading' ? 'Loading your dashboard…' : 'Redirecting…'}
      </div>
    );
  }

  return <AppShell me={state.me}>{children}</AppShell>;
}
