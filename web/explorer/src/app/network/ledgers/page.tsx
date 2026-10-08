import { permanentRedirect } from 'next/navigation';

// Same shape as ../operations — the Network hub
// links here; the ledgers directory stays canonical at /ledgers.
export default function NetworkLedgersRedirect() {
  permanentRedirect('/ledgers');
}
