import { loadArchitectureDoc, loadArchitectureDocs } from '@/lib/architecture';
import { buildDocPage } from '@/lib/docPage';

// Each curated architecture doc rendered as a static page.
// Reuses the same loader/renderer pattern as ADRs and incident
// postmortems — the underlying markdown is the source of truth,
// the page just layers on Stellar Index chrome.

export const dynamic = 'error';
export const dynamicParams = false;

export function generateStaticParams() {
  return loadArchitectureDocs().map((d) => ({ slug: d.slug }));
}

const { generateMetadata, DocPage } = buildDocPage({
  category: 'architecture',
  label: 'Architecture',
  pillLabel: 'Architecture',
  loadDoc: loadArchitectureDoc,
});

export { generateMetadata };
export default DocPage;
