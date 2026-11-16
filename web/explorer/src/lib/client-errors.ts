const CLIENT_ERRORS_ENDPOINT = '/client-errors';
const MAX_FIELD_LENGTH = 500;

/**
 * Beacons a client-side error to the `client-errors` CF Pages function
 * (web/explorer/functions/client-errors.js) so it lands in server-side
 * logs instead of only the reporting user's own browser console.
 * Best-effort: a reporting failure must never surface as a second crash.
 */
export function reportClientError(report: {
  message: string;
  digest?: string;
  section?: string;
}) {
  try {
    const payload = JSON.stringify({
      message: (report.message ?? '').slice(0, MAX_FIELD_LENGTH),
      digest: report.digest,
      section: report.section,
      path: window.location.pathname,
    });
    const blob = new Blob([payload], { type: 'application/json' });
    const sent = navigator.sendBeacon?.(CLIENT_ERRORS_ENDPOINT, blob);
    if (!sent) {
      void fetch(CLIENT_ERRORS_ENDPOINT, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: payload,
        keepalive: true,
      }).catch(() => {});
    }
  } catch {
    // Reporting is best-effort — must never throw out of error handling.
  }
}
