import { loadOperationsDoc, loadOperationsDocs } from '@/lib/operations';
import { buildDocPage } from '@/lib/docPage';

// Each curated operations doc rendered as a static page. Same
// shape as the ADR / architecture / discovery browsers.

export const dynamic = 'error';
export const dynamicParams = false;

export function generateStaticParams() {
  return loadOperationsDocs().map((d) => ({ slug: d.slug }));
}

const { generateMetadata, DocPage } = buildDocPage({
  category: 'operations',
  label: 'Operations',
  pillLabel: 'Operations runbook',
  loadDoc: loadOperationsDoc,
});

export { generateMetadata };
export default DocPage;
