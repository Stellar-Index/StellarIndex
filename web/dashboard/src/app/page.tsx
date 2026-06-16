'use client';

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { Loader2 } from 'lucide-react';
import { useAuth } from '@/lib/auth';

// Root route — bounce based on auth state.
//
//   loading → render a brief "checking session..." state
//   anon    → push to /signin/
//   authed  → push to /overview/ (the dashboard landing page)
export default function RootPage() {
  const router = useRouter();
  const { state } = useAuth();

  useEffect(() => {
    if (state.kind === 'anon') router.replace('/signin/');
    if (state.kind === 'authed') router.replace('/overview/');
  }, [state, router]);

  return (
    <div className="flex min-h-screen items-center justify-center gap-2 text-sm text-ink-muted">
      <Loader2 className="h-4 w-4 animate-spin text-brand-600" />
      Checking your session…
    </div>
  );
}
