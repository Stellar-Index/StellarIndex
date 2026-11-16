import { protocolMeta } from '@/app/protocols/registry';

export interface PoolMeta {
  protocol: 'blend' | 'blnt';
  name: string;
  note?: string;
  deployedAt?: string;
  initiator?: string;
}

// Curated annotations for every lending mainnet contract we track, keyed by
// contract id. Blend rows: docs/operations/wasm-audits/blend.md (Phase 4 walk).
export const LENDING_POOL_META: Record<string, PoolMeta> = {
  CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7: {
    protocol: 'blend',
    name: 'Backstop V2',
    note: 'Holds the protocol-wide BLND-USDC LP shares that backstop pool insolvency. Receives auction proceeds when borrower positions liquidate at a loss.',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU: {
    protocol: 'blend',
    name: 'Pool Factory V2',
    note: 'Spawns new isolated lending-pool contracts. Each user-facing pool (with its own reserves and risk parameters) is a child of this factory.',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD: {
    protocol: 'blend',
    name: 'Pool #1 (genesis)',
    note: 'First pool deployed by the Pool Factory V2, ~4 minutes after the factory itself. Initiator overlaps with the protocol team — likely a reference/genesis pool.',
    deployedAt: '2025-04-14',
    initiator: 'GAX2VVWVHU5YQY5J3NJBXKHI3FFKZN54BE6GRJCWSIKSBZTQWJJNJMPC',
  },
  CBNR7PYFY775UG7W37B4OJG2OBBUKLFW6VIBHFDKKLR2HECPRMRZMDK3: {
    protocol: 'blend',
    name: 'Pool #2',
    deployedAt: '2025-04-15',
    initiator: 'GBCAS7XIGDRZY4BMABJMGGW7J3YTITRRV5BTEMFQE5ZZSSVWHHX2ZSS4',
  },
  CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS: {
    protocol: 'blend',
    name: 'Pool #3',
    deployedAt: '2025-04-17',
    initiator: 'GBCAS7XIGDRZY4BMABJMGGW7J3YTITRRV5BTEMFQE5ZZSSVWHHX2ZSS4',
  },
  CB4OFHAY2TAEYUVPOJS36S657C6NYMSIFUNCCA5AHYT46Y5XUID3O2ED: {
    protocol: 'blend',
    name: 'Pool #4',
    deployedAt: '2025-05-01',
    initiator: 'GBIWJGAOSFC4KUPHXM573TKTWHMI7VW7D4GCHYZYH243Q6HVBV7ORBIT',
  },
  CAE7QVOMBLZ53CDRGK3UNRRHG5EZ5NQA7HHTFASEMYBWHG6MDFZTYHXC: {
    protocol: 'blend',
    name: 'Pool #5',
    deployedAt: '2025-05-01',
    initiator: 'GBIWJGAOSFC4KUPHXM573TKTWHMI7VW7D4GCHYZYH243Q6HVBV7ORBIT',
  },
  CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF: {
    protocol: 'blend',
    name: 'Pool #6',
    deployedAt: '2025-07-13',
    initiator: 'GCCI7K6QU6FVVIXWSLKRPTBKJCFBLEJKPTZMP27A2KL37N4ZL3OCM3GI',
  },
  CALRF5I2OCJCU577R6MZBCY5IIXNMAAG6PNMN7GUKEYIXBJCJN2FJRVI: {
    protocol: 'blend',
    name: 'Pool #7',
    deployedAt: '2025-11-22',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },
  CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY: {
    protocol: 'blend',
    name: 'Pool #8',
    deployedAt: '2025-11-25',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },
  CDMAVJPFXPADND3YRL4BSM3AKZWCTFMX27GLLXCML3PD62HEQS5FPVAI: {
    protocol: 'blend',
    name: 'Pool #9',
    deployedAt: '2025-11-25',
    initiator: 'GDH3FRHOOWXYXEASH43N2VOVFOPJSVJF3EQFSLBLJYFPHOUAF4N4AETH',
  },

  CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7: {
    protocol: 'blnt',
    name: 'BLNT Fixed pool',
  },
  CAUTPSQDAJEUV5XEU3EXFVYRGDJLEJGVTFJCFZCSPTDCREAMZ573VF4D: {
    protocol: 'blnt',
    name: 'BLNT Yield pool',
  },
  CAKLFWAHB2RIYRBSPUL6EH43AIQ5ILBEM7HTMWYSJRL7PHZQ676KMCV5: {
    protocol: 'blnt',
    name: 'BLNT Pool Factory',
  },
  CAHCUKBPTW5DR5RNXOHCTJD5U76A6EEZXYZAQFRQEEQRXQ3CRUZ6Z3MO: {
    protocol: 'blnt',
    name: 'BLNT Backstop',
  },
};

export interface ProtocolDisplay {
  slug: string;
  name: string;
  website?: string;
}

/** Display name and links for a row's `protocol`; null when it is absent or not in the registry. */
export function lendingProtocol(
  protocol: string | null | undefined,
): ProtocolDisplay | null {
  const meta = protocol ? protocolMeta(protocol) : undefined;
  if (!meta) return null;
  return { slug: meta.name, name: meta.label, website: meta.links?.website };
}

/** Curated protocol first, else the listing row's `protocol` field. */
export function poolProtocol(
  pool: string,
  rowProtocol?: string | null,
): ProtocolDisplay | null {
  return lendingProtocol(LENDING_POOL_META[pool]?.protocol ?? rowProtocol);
}
