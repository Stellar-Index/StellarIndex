// Minimal Stellar strkey codec (SEP-23): just enough to resolve a muxed
// M-address to its base G-account without pulling in the SDK.
const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
const VERSION_G = 6 << 3;
const VERSION_M = 12 << 3;

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
