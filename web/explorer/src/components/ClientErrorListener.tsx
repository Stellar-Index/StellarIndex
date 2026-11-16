'use client';

import { useEffect } from 'react';

import { reportClientError } from '@/lib/client-errors';

/** One page load reports at most this many errors: a throwing timer must not become a request flood. */
const MAX_REPORTS_PER_PAGE = 5;

function messageOf(reason: unknown): string {
  if (reason instanceof Error) return reason.message;
  return typeof reason === 'string' ? reason : '';
}

/**
 * ClientErrorListener — reports errors no React boundary sees: throws in
 * event handlers, timers and stream listeners (`error`) and rejected
 * promises nobody awaited (`unhandledrejection`). Renders nothing.
 */
export function ClientErrorListener() {
  useEffect(() => {
    const seen = new Set<string>();
    const report = (message: string, section: string) => {
      // "Script error." is all a cross-origin script's throw exposes.
      if (!message || message === 'Script error.') return;
      if (seen.has(message) || seen.size >= MAX_REPORTS_PER_PAGE) return;
      seen.add(message);
      reportClientError({ message, section });
    };
    const onError = (e: ErrorEvent) =>
      report(e.message || messageOf(e.error), 'window-error');
    const onRejection = (e: PromiseRejectionEvent) =>
      report(messageOf(e.reason), 'unhandled-rejection');
    window.addEventListener('error', onError);
    window.addEventListener('unhandledrejection', onRejection);
    return () => {
      window.removeEventListener('error', onError);
      window.removeEventListener('unhandledrejection', onRejection);
    };
  }, []);
  return null;
}
