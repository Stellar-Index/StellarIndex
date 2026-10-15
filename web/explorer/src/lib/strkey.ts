// Minimal Stellar strkey codec (SEP-23): just enough to resolve a muxed
// M-address to its base G-account and derive an asset's SAC address without
// pulling in the SDK.
const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
const VERSION_G = 6 << 3;
const VERSION_M = 12 << 3;
const VERSION_C = 2 << 3;

function crc16xmodem(bytes: Uint8Array): number {
  let crc = 0;
  for (const b of bytes) {
    crc ^= b << 8;
    for (let i = 0; i < 8; i++) {
      crc = crc & 0x8000 ? ((crc << 1) ^ 0x1021) & 0xffff : (crc << 1) & 0xffff;
    }
  }
  return crc;
}

function base32Decode(s: string): Uint8Array | null {
  const out: number[] = [];
  let bits = 0;
  let value = 0;
  for (const ch of s) {
    const idx = ALPHABET.indexOf(ch);
    if (idx < 0) return null;
    value = (value << 5) | idx;
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  return Uint8Array.from(out);
}

function base32Encode(bytes: Uint8Array): string {
  let out = '';
  let bits = 0;
  let value = 0;
  for (const b of bytes) {
    value = (value << 8) | b;
    bits += 8;
    while (bits >= 5) {
      out += ALPHABET[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
  }
  if (bits > 0) out += ALPHABET[(value << (5 - bits)) & 31];
  return out;
}

function encodeCheck(version: number, payload: Uint8Array): string {
  const body = new Uint8Array(1 + payload.length);
  body[0] = version;
  body.set(payload, 1);
  const crc = crc16xmodem(body);
  const full = new Uint8Array(body.length + 2);
  full.set(body);
  full[body.length] = crc & 0xff;
  full[body.length + 1] = crc >> 8;
  return base32Encode(full);
}

/** Base G-account of a muxed M-address, or null when it isn't a valid one. */
export function muxedBaseAccount(m: string): string | null {
  if (!/^M[A-Z2-7]{68}$/.test(m)) return null;
  const raw = base32Decode(m);
  // version(1) + ed25519(32) + id(8) + crc(2)
  if (!raw || raw.length !== 43 || raw[0] !== VERSION_M) return null;
  const crc = crc16xmodem(raw.subarray(0, 41));
  if (raw[41] !== (crc & 0xff) || raw[42] !== crc >> 8) return null;
  return encodeCheck(VERSION_G, raw.subarray(1, 33));
}

function int32(n: number): number[] {
  return [(n >>> 24) & 0xff, (n >>> 16) & 0xff, (n >>> 8) & 0xff, n & 0xff];
}

async function sha256(bytes: Uint8Array<ArrayBuffer>): Promise<Uint8Array> {
  return new Uint8Array(
    await globalThis.crypto.subtle.digest('SHA-256', bytes),
  );
}

/**
 * The Stellar Asset Contract address of a classic asset (`code` + G-issuer)
 * or of native XLM (`code` 'native'), or null when the input isn't a valid
 * asset. sha256(ENVELOPE_TYPE_CONTRACT_ID ‖ networkId ‖ FROM_ASSET ‖ Asset).
 */
export async function sacAddress(
  code: string,
  issuer: string | null | undefined,
  passphrase: string,
): Promise<string | null> {
  let asset: number[];
  if (code === 'native' && !issuer) {
    asset = int32(0);
  } else {
    if (!/^[A-Za-z0-9]{1,12}$/.test(code) || !issuer) return null;
    if (!/^G[A-Z2-7]{55}$/.test(issuer)) return null;
    const raw = base32Decode(issuer);
    if (!raw || raw.length !== 35 || raw[0] !== VERSION_G) return null;
    const crc = crc16xmodem(raw.subarray(0, 33));
    if (raw[33] !== (crc & 0xff) || raw[34] !== crc >> 8) return null;
    const width = code.length <= 4 ? 4 : 12;
    const padded = Array.from({ length: width }, (_, i) =>
      i < code.length ? code.charCodeAt(i) : 0,
    );
    asset = [
      ...int32(width === 4 ? 1 : 2),
      ...padded,
      ...int32(0),
      ...raw.subarray(1, 33),
    ];
  }
  const networkId = await sha256(new TextEncoder().encode(passphrase));
  const preimage = Uint8Array.from([
    ...int32(8),
    ...networkId,
    ...int32(1),
    ...asset,
  ]);
  return encodeCheck(VERSION_C, await sha256(preimage));
}
