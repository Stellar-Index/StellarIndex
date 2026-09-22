'use client';

import { RouteError } from '@/components/RouteError';

// Root error boundary — final backstop for any segment that doesn't
// carry its own error.tsx. The root layout keeps rendering around
// this (unlike global-error.tsx, which only fires above the layout),
// so the shared, styled retry surface applies here too.
// See src/components/RouteError.tsx.
export default function Error(props: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return <RouteError {...props} />;
}
