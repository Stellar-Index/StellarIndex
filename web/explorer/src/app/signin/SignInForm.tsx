'use client';

import { useState, useSyncExternalStore } from 'react';
import { AlertCircle, KeyRound, Loader2, Mail } from 'lucide-react';

import { API_BASE_URL } from '@/api/client';
import {
  ApiError,
  beginPasskeyLogin,
  finishPasskeyLogin,
  verifyCode,
} from '@/api/account';
import {
  getPasskeyAssertion,
  isCeremonyCancelled,
  supportsPasskeys,
  type ServerRequestOptions,
} from '@/lib/webauthn';

// Stable no-op subscription for useSyncExternalStore — the passkey
// capability of a browser never changes mid-session.
const emptySubscribe = () => () => {};

type State =
  | { kind: 'email' }
  | { kind: 'sendingEmail' }
  | { kind: 'code' } // email accepted; awaiting the 6-digit code
  | { kind: 'verifying' }
  | { kind: 'success' };

export function SignInForm({ mode = 'signin' }: { mode?: 'signin' | 'signup' }) {
  const [email, setEmail] = useState('');
  const [code, setCode] = useState('');
  const [state, setState] = useState<State>({ kind: 'email' });
  const [error, setError] = useState<string | null>(null);
  // Feature-detected client-side (static export — the server snapshot
  // renders the passkey-less first paint, then hydration reveals the
  // button; useSyncExternalStore avoids a setState-in-effect and a
  // hydration mismatch, matching useLastPathSegment's pattern).
  const passkeyReady = useSyncExternalStore(
    emptySubscribe,
    supportsPasskeys,
    () => false,
  );
  const [passkeyBusy, setPasskeyBusy] = useState(false);

  async function onPasskey() {
    setError(null);
    setPasskeyBusy(true);
    try {
      const options = (await beginPasskeyLogin()) as ServerRequestOptions;
      const assertion = await getPasskeyAssertion(options);
      await finishPasskeyLogin(assertion);
      // Full-page navigation so the freshly-set session cookie applies.
      window.location.assign('/dashboard');
      return;
    } catch (err) {
      if (!isCeremonyCancelled(err)) {
        setError(
          err instanceof ApiError
            ? (err.detail ?? 'Passkey sign-in failed — try again or use email.')
            : 'Passkey sign-in failed — try again or use email.',
        );
      }
      setPasskeyBusy(false);
    }
  }

  async function onSubmitEmail(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = email.trim().toLowerCase();
    if (!trimmed) return;
    setError(null);
    setState({ kind: 'sendingEmail' });
    try {
      const res = await fetch(`${API_BASE_URL}/v1/auth/login`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: trimmed }),
        // REQUIRED. This is a cross-origin call (stellarindex.io ->
        // api.stellarindex.io) and fetch defaults to credentials:
        // 'same-origin', so without this the browser DISCARDS the
        // response's Set-Cookie. The cookie in question is the
        // login-intent witness the callback demands (C3-030), so every
        // emailed sign-in link 403'd with "this sign-in link must be
        // opened in the browser that requested it" — verified live
        // 2026-08-04. The server-side binding landed in 5b99ebbd, which
        // touched no web source; the Go tests replay the cookie a real
        // browser "would have stored", which is exactly the assumption
        // this call broke. accountFetch has always had it.
        credentials: 'include',
      });
      if (!res.ok) {
        let detail: string | undefined;
        try {
          const body = (await res.json()) as { detail?: string; title?: string };
          detail = body.detail ?? body.title;
        } catch {
          // ignore
        }
        setError(detail ?? `Request failed (${res.status} ${res.statusText})`);
        setState({ kind: 'email' });
        return;
      }
      setEmail(trimmed);
      setState({ kind: 'code' });
    } catch {
      setError('Network error — please try again.');
      setState({ kind: 'email' });
    }
  }

  async function onSubmitCode(e: React.FormEvent) {
    e.preventDefault();
    const digits = code.replace(/\D/g, '');
    if (digits.length !== 6) {
      setError('Enter the 6-digit code from the email.');
      return;
    }
    setError(null);
    setState({ kind: 'verifying' });
    try {
      await verifyCode(email, digits);
      // Full-page navigation so the freshly-set session cookie applies
      // and the cookie-authed dashboard renders signed in.
      setState({ kind: 'success' });
      window.location.assign('/dashboard');
    } catch (err) {
      const detail =
        err instanceof ApiError
          ? (err.detail ?? 'That code didn’t work. Check the email or request a new one.')
          : 'Network error — please try again.';
      setError(detail);
      setState({ kind: 'code' });
    }
  }

  // ─── Step 2: enter the code (also accept the magic link) ──────────
  if (state.kind === 'code' || state.kind === 'verifying' || state.kind === 'success') {
    const busy = state.kind === 'verifying' || state.kind === 'success';
    return (
      <form onSubmit={onSubmitCode} className="space-y-4">
        <div className="rounded-lg border border-line bg-surface-muted p-4 text-sm text-ink-body">
          We emailed a sign-in code to{' '}
          <span className="font-mono font-medium">{email}</span>. Enter it
          below, or just click the link in that email — either signs you in
          {mode === 'signup' ? ' and creates your account if it’s new' : ''}.
        </div>

        <label className="block space-y-1">
          <span className="text-sm font-medium text-ink-body">6-digit code</span>
          <input
            type="text"
            inputMode="numeric"
            autoComplete="one-time-code"
            pattern="[0-9]*"
            maxLength={6}
            value={code}
            onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
            autoFocus
            placeholder="123456"
            className="w-full rounded-md border border-line bg-surface px-3 py-2 text-center font-mono text-lg tracking-[0.4em] placeholder:tracking-[0.4em] placeholder:text-ink-faint focus:border-brand-500 focus:outline-hidden focus:ring-1 focus:ring-brand-500"
          />
        </label>

        {error && (
          <div role="alert" className="flex items-start gap-2 rounded-md border border-bad-300 bg-bad-50 p-3 text-sm text-bad-700">
            <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
            <span>{error}</span>
          </div>
        )}

        <button
          type="submit"
          disabled={busy || code.length !== 6}
          className="inline-flex w-full items-center justify-center gap-2 rounded-md bg-brand-600 px-4 py-2 text-sm font-medium text-white hover:bg-brand-700 disabled:cursor-not-allowed disabled:opacity-60"
        >
          {busy && <Loader2 className="h-4 w-4 animate-spin" />}
          {state.kind === 'success' ? 'Signing you in…' : 'Verify code'}
        </button>

        <p className="text-xs text-ink-muted">
          Didn&apos;t get it? Check spam, or{' '}
          <button
            type="button"
            onClick={() => {
              setCode('');
              setError(null);
              setState({ kind: 'email' });
            }}
            className="underline hover:no-underline"
          >
            use a different email
          </button>
          .
        </p>
      </form>
    );
  }

  // ─── Step 1: enter the email ──────────────────────────────────────
  return (
    <form onSubmit={onSubmitEmail} className="space-y-4">
      <label className="block space-y-1">
        <span className="text-sm font-medium text-ink-body">Email</span>
        <div className="relative">
          <Mail className="absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-ink-faint" />
          <input
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
            autoComplete="email"
            placeholder="you@example.com"
            className="w-full rounded-md border border-line bg-surface py-2 pl-8 pr-3 text-sm placeholder:text-ink-faint focus:border-brand-500 focus:outline-hidden focus:ring-1 focus:ring-brand-500"
          />
        </div>
      </label>

      {error && (
        <div role="alert" className="flex items-start gap-2 rounded-md border border-bad-300 bg-bad-50 p-3 text-sm text-bad-700">
          <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
          <span>{error}</span>
        </div>
      )}

      <button
        type="submit"
        disabled={state.kind === 'sendingEmail' || !email.trim()}
        className="inline-flex w-full items-center justify-center gap-2 rounded-md bg-brand-600 px-4 py-2 text-sm font-medium text-white hover:bg-brand-700 disabled:cursor-not-allowed disabled:opacity-60"
      >
        {state.kind === 'sendingEmail' && <Loader2 className="h-4 w-4 animate-spin" />}
        {mode === 'signup' ? 'Create account' : 'Send sign-in code'}
      </button>

      {passkeyReady && (
        <>
          <div className="flex items-center gap-3 text-xs text-ink-faint">
            <span className="h-px flex-1 bg-line" aria-hidden />
            or
            <span className="h-px flex-1 bg-line" aria-hidden />
          </div>
          <button
            type="button"
            onClick={onPasskey}
            disabled={passkeyBusy}
            className="inline-flex w-full items-center justify-center gap-2 rounded-md border border-line bg-surface px-4 py-2 text-sm font-medium text-ink-body hover:bg-surface-muted disabled:cursor-not-allowed disabled:opacity-60"
          >
            {passkeyBusy ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <KeyRound className="h-4 w-4" />
            )}
            Sign in with a passkey
          </button>
        </>
      )}

      <p className="text-xs text-ink-muted">
        Passwordless sign-in — we email a 6-digit code (and a one-click
        link), valid for 15 minutes. New emails create an account on
        first sign-in. Passkeys can be added from the dashboard once
        you&apos;re in.
      </p>
    </form>
  );
}
