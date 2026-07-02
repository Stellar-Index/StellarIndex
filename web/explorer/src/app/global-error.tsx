'use client';

/**
 * global-error — the boundary of last resort. Renders only when the
 * root layout itself (or something above every segment boundary)
 * throws. Per the Next.js contract it must supply its own
 * <html>/<body>, and because it replaces the root layout it renders
 * OUTSIDE the design system — no Tailwind, no globals.css, no fonts.
 * Inline styles only.
 */
export default function GlobalError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <html lang="en">
      <body
        style={{
          margin: 0,
          minHeight: '100vh',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          background: '#fafaf9',
          color: '#1c1917',
          fontFamily:
            "system-ui, -apple-system, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif",
        }}
      >
        <div style={{ maxWidth: '28rem', padding: '2rem', textAlign: 'center' }}>
          <h1 style={{ fontSize: '1.25rem', fontWeight: 600, margin: '0 0 0.5rem' }}>
            Stellar Index hit an unexpected error
          </h1>
          <p style={{ fontSize: '0.875rem', color: '#57534e', margin: '0 0 1.25rem', lineHeight: 1.5 }}>
            The application shell failed to render. This is usually transient —
            try again, or head back to the homepage.
          </p>
          {error?.digest && (
            <p
              style={{
                fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
                fontSize: '0.75rem',
                color: '#a8a29e',
                margin: '0 0 1.25rem',
              }}
            >
              digest: {error.digest}
            </p>
          )}
          <div style={{ display: 'flex', gap: '0.75rem', justifyContent: 'center' }}>
            <button
              onClick={reset}
              style={{
                background: '#1f4ae0',
                color: '#ffffff',
                border: 'none',
                borderRadius: '0.5rem',
                padding: '0.5rem 1rem',
                fontSize: '0.875rem',
                fontWeight: 500,
                cursor: 'pointer',
              }}
            >
              Try again
            </button>
            {/* Deliberately a plain <a>: when global-error renders, the
                app (and possibly the router) has crashed — a full-page
                navigation is the reliable escape hatch. */}
            {/* eslint-disable-next-line @next/next/no-html-link-for-pages */}
            <a
              href="/"
              style={{
                display: 'inline-block',
                border: '1px solid #d6d3d1',
                borderRadius: '0.5rem',
                padding: '0.5rem 1rem',
                fontSize: '0.875rem',
                fontWeight: 500,
                color: '#1c1917',
                textDecoration: 'none',
              }}
            >
              Back to home
            </a>
          </div>
        </div>
      </body>
    </html>
  );
}
