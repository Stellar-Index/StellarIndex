import { describe, it, expect } from 'vitest';

import { hrefFor } from './hrefFor';

describe('hrefFor', () => {
  it('encodes a source name containing a path separator', () => {
    expect(hrefFor.source('band/v2')).toBe('/sources/band%2Fv2');
  });

  it('encodes an exchange name containing a space', () => {
    expect(hrefFor.exchange('Kraken Pro')).toBe('/exchanges/Kraken%20Pro');
  });

  it('encodes a protocol slug containing a space', () => {
    expect(hrefFor.protocol('sd ex')).toBe('/protocols/sd%20ex');
  });

  it('encodes an incident slug and keeps the trailing slash', () => {
    expect(hrefFor.incident('2026-09 outage#1')).toBe(
      '/status/incident/2026-09%20outage%231/',
    );
  });
});
