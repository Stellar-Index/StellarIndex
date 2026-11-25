import { Callout } from '@/components/ui';

/** The asset detail was still partial when the page was built: its blank sections are unknown, not absent. */
export function PartialDetailCallout({ code }: { code: string }) {
  return (
    <Callout tone="warn" title="Some reads behind this page did not answer">
      <p>
        The API marked {code}&apos;s detail partial when this page was built, so
        sections shown blank (top markets, all-time high, price history, counts)
        are unknown, not absent, until the next build.
      </p>
    </Callout>
  );
}
