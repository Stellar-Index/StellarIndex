import { permanentRedirect } from 'next/navigation';

// Nav revision 2026-08-24: same shape as ../operations — the Network hub
// links here; the ledgers directory stays canonical at /ledgers.
export default function NetworkLedgersRedirect() {
  permanentRedirect('/ledgers');
}
