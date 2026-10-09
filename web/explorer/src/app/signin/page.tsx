import type { Metadata } from 'next';
import Link from 'next/link';

import { PageHeader } from '@/components/ui';

import { SignInForm } from './SignInForm';

export const metadata: Metadata = {
  // Auth form — no SEO value; keep it out of the index (and the sitemap).
  robots: { index: false, follow: false },
  title: 'Sign in',
  description:
    'Sign in to your Stellar Index account. Magic-link email auth — no passwords.',
};

export default function SignInPage() {
  return (
    // Route-frame record :
    // max-w-md is DELIBERATE — the bare magic-link form is an auth
    // micro-surface; /signup stays max-w-4xl for its tier table. Vertical
    // rhythm is harmonized with /signup (py-12 sm:py-16); widths diverge
    // on purpose. Allowlisted for the census-2 route-frame tripwire.
    <div className="mx-auto max-w-md space-y-6 px-6 py-12 sm:py-16">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Sign in' }]}
        title="Sign in"
        description="Magic-link email — no passwords."
      />
      <SignInForm mode="signin" />
      <p className="text-ink-muted text-center text-sm">
        Don&apos;t have an account?{' '}
        <Link href="/signup" className="text-brand-600 hover:underline">
          Create one
        </Link>
      </p>
    </div>
  );
}
