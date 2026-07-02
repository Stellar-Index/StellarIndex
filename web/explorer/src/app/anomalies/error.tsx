'use client';

import { RouteError } from '@/components/RouteError';

// Segment error boundary — catches render throws anywhere under
// /anomalies and degrades to the shared retry surface instead of a
// white screen. See src/components/RouteError.tsx.
export default function Error(props: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return <RouteError {...props} section="anomalies" />;
}
