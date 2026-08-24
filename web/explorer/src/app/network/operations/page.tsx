import { permanentRedirect } from 'next/navigation';

// Nav revision 2026-08-24: the rail's Network entry owns the network
// sub-surfaces, and /network/operations is the address the hub links
// under. The operations directory itself stays canonical at
// /operations (deep links, sitemap, breadcrumbs all point there), so
// this route is a server-side permanent redirect, not a duplicate page.
export default function NetworkOperationsRedirect() {
  permanentRedirect('/operations');
}
