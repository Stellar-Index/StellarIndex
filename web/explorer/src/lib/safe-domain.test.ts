import { describe, it, expect } from 'vitest';

import { isSafeHomeDomain, isSafePublicImageUrl } from './safe-domain';

describe('isSafeHomeDomain', () => {
  it('accepts a normal registrable domain', () => {
    expect(isSafeHomeDomain('example.com')).toBe(true);
  });
  it('rejects userinfo/path/scheme smuggling', () => {
    expect(isSafeHomeDomain('good.com@evil.com')).toBe(false);
    expect(isSafeHomeDomain('evil.com/login?next=good.com')).toBe(false);
  });
});

// SEC-10: SEP-1 `image` is issuer-controlled; scheme-only validation let a
// hostile issuer's URL point a viewer's browser at an arbitrary host,
// including private/internal addresses (client-side SSRF) or plain http.
describe('isSafePublicImageUrl', () => {
  it('accepts a normal https URL', () => {
    expect(isSafePublicImageUrl('https://issuer.example.com/icon.png')).toBe(true);
  });

  it('rejects http (https-only)', () => {
    expect(isSafePublicImageUrl('http://issuer.example.com/icon.png')).toBe(false);
  });

  it('rejects loopback / localhost', () => {
    expect(isSafePublicImageUrl('https://localhost/icon.png')).toBe(false);
    expect(isSafePublicImageUrl('https://127.0.0.1/icon.png')).toBe(false);
    expect(isSafePublicImageUrl('https://[::1]/icon.png')).toBe(false);
  });

  it('rejects RFC1918 private ranges', () => {
    expect(isSafePublicImageUrl('https://10.0.0.5/icon.png')).toBe(false);
    expect(isSafePublicImageUrl('https://172.16.0.5/icon.png')).toBe(false);
    expect(isSafePublicImageUrl('https://192.168.1.5/icon.png')).toBe(false);
  });

  it('rejects link-local, including the cloud metadata address', () => {
    expect(isSafePublicImageUrl('https://169.254.169.254/latest/meta-data/')).toBe(false);
  });

  it('rejects non-URL garbage and empty/missing values', () => {
    expect(isSafePublicImageUrl('not a url')).toBe(false);
    expect(isSafePublicImageUrl('')).toBe(false);
    expect(isSafePublicImageUrl(null)).toBe(false);
    expect(isSafePublicImageUrl(undefined)).toBe(false);
  });
});
