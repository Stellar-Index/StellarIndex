import { TabNav } from '@/components/ui/Tabs';
import { availableRoutes } from '@/lib/network-routes';

const ITEMS = [
  { label: 'Status', href: '/status' },
  { label: 'Diagnostics', href: '/diagnostics' },
  { label: 'Sources', href: '/sources' },
  { label: 'Methodology', href: '/methodology' },
  { label: 'Service level', href: '/sla' },
];

/** The shared strip that ties the pages a reader uses to judge the data into one section. */
export function DataTrustTabs({ active }: { active: string }) {
  return <TabNav items={availableRoutes(ITEMS)} activeHref={active} />;
}
