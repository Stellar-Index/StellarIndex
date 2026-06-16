'use client';

import { useState } from 'react';
import { toast } from 'sonner';
import { TrendingUp, Mail, Check, Loader2 } from 'lucide-react';
import { ApiError, requestMagicLink } from '@/lib/api';
import { Button, Field, Input, Callout } from '@/components/ui';

export default function SigninPage() {
  const [email, setEmail] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [sent, setSent] = useState(false);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (!email.trim() || submitting) return;
    setSubmitting(true);
    try {
      await requestMagicLink(email.trim().toLowerCase());
      setSent(true);
    } catch (err) {
      const msg =
        err instanceof ApiError
          ? (err.detail ?? err.message)
          : 'Network error — please try again.';
      toast.error(msg);
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="flex min-h-screen flex-col items-center justify-center px-6 py-12">
      <div className="w-full max-w-sm">
        {/* Brand mark */}
        <div className="mb-8 flex items-center justify-center gap-2.5">
          <span className="flex h-9 w-9 items-center justify-center rounded-lg bg-brand-600 text-white">
            <TrendingUp className="h-5 w-5" />
          </span>
          <span className="text-lg font-semibold tracking-tight text-ink">
            Stellar Index
          </span>
        </div>

        <div className="rounded-card border border-line bg-surface p-8 shadow-card">
          <div className="mb-6 space-y-1.5 text-center">
            <h1 className="text-h3 font-semibold text-ink">Sign in to your dashboard</h1>
            <p className="text-sm text-ink-muted">
              We&apos;ll email you a single-use link. New here? The same link
              creates your account.
            </p>
          </div>

          {sent ? (
            <Callout tone="ok" title="Check your email">
              <p className="mt-1">
                A sign-in link is on its way to{' '}
                <span className="font-mono font-medium">{email}</span>. It
                expires in 15 minutes.
              </p>
              <button
                type="button"
                onClick={() => {
                  setSent(false);
                  setEmail('');
                }}
                className="mt-3 text-xs font-medium text-ok-700 underline hover:no-underline"
              >
                Use a different email
              </button>
            </Callout>
          ) : (
            <form onSubmit={handleSubmit} className="space-y-4">
              <Field label="Email" htmlFor="signin-email">
                <div className="relative">
                  <Mail className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-ink-faint" />
                  <Input
                    id="signin-email"
                    type="email"
                    autoFocus
                    required
                    inputMode="email"
                    autoComplete="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    className="pl-9"
                    placeholder="you@example.com"
                  />
                </div>
              </Field>
              <Button
                type="submit"
                className="w-full"
                disabled={submitting || !email.trim()}
              >
                {submitting ? (
                  <Loader2 className="h-4 w-4 animate-spin" />
                ) : (
                  <Check className="h-4 w-4" />
                )}
                {submitting ? 'Sending…' : 'Email me a sign-in link'}
              </Button>
            </form>
          )}
        </div>

        <p className="mt-6 text-center text-xs text-ink-faint">
          By signing in you agree to the{' '}
          <a
            href="https://stellarindex.io/terms"
            className="underline hover:text-ink-muted"
          >
            terms of service
          </a>
          .
        </p>
      </div>
    </div>
  );
}
