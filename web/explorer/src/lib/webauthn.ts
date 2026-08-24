// Browser WebAuthn ceremony helpers for passkey sign-in.
//
// The API (go-webauthn) speaks W3C JSON: every binary field
// (challenge, credential ids, attestation/assertion payloads) is
// base64url-encoded. `navigator.credentials` speaks ArrayBuffers.
// These helpers convert in both directions, hand-rolled rather than
// relying on `PublicKeyCredential.parseCreationOptionsFromJSON` /
// `.toJSON()`, which are still missing from browsers we support.

export function supportsPasskeys(): boolean {
  return typeof window !== 'undefined' && !!window.PublicKeyCredential;
}

function base64urlToBuffer(s: string): ArrayBuffer {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/');
  const padded = b64 + '='.repeat((4 - (b64.length % 4)) % 4);
  const bin = atob(padded);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes.buffer;
}

function bufferToBase64url(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// Server-side option shapes: opaque JSON with base64url binaries at
// known positions. Typed loosely on purpose — the wire shape is the
// W3C dictionary, owned by the server library.
interface ServerCredentialDescriptor {
  type: string;
  id: string;
  transports?: string[];
}

export interface ServerCreationOptions {
  publicKey: {
    challenge: string;
    user: { id: string; name: string; displayName: string };
    excludeCredentials?: ServerCredentialDescriptor[];
    [k: string]: unknown;
  };
}

export interface ServerRequestOptions {
  publicKey: {
    challenge: string;
    allowCredentials?: ServerCredentialDescriptor[];
    [k: string]: unknown;
  };
}

function decodeDescriptors(
  list: ServerCredentialDescriptor[] | undefined,
): PublicKeyCredentialDescriptor[] | undefined {
  if (!list) return undefined;
  return list.map((d) => ({
    type: d.type as PublicKeyCredentialType,
    id: base64urlToBuffer(d.id),
    transports: d.transports as AuthenticatorTransport[] | undefined,
  }));
}

/**
 * Run `navigator.credentials.create()` from server creation options
 * and serialize the attestation back to the JSON shape the server's
 * finish-register endpoint parses.
 */
export async function createPasskey(
  options: ServerCreationOptions,
): Promise<Record<string, unknown>> {
  const pk = options.publicKey;
  const cred = (await navigator.credentials.create({
    publicKey: {
      ...(pk as unknown as PublicKeyCredentialCreationOptions),
      challenge: base64urlToBuffer(pk.challenge),
      user: {
        ...(pk.user as unknown as PublicKeyCredentialUserEntity),
        id: base64urlToBuffer(pk.user.id),
      },
      excludeCredentials: decodeDescriptors(pk.excludeCredentials),
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error('no credential returned');

  const resp = cred.response as AuthenticatorAttestationResponse;
  return {
    id: cred.id,
    rawId: bufferToBase64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    response: {
      attestationObject: bufferToBase64url(resp.attestationObject),
      clientDataJSON: bufferToBase64url(resp.clientDataJSON),
      transports:
        typeof resp.getTransports === 'function'
          ? resp.getTransports()
          : undefined,
    },
  };
}

/**
 * Run `navigator.credentials.get()` from server request options and
 * serialize the assertion for the server's finish-login endpoint.
 */
export async function getPasskeyAssertion(
  options: ServerRequestOptions,
): Promise<Record<string, unknown>> {
  const pk = options.publicKey;
  const cred = (await navigator.credentials.get({
    publicKey: {
      ...(pk as unknown as PublicKeyCredentialRequestOptions),
      challenge: base64urlToBuffer(pk.challenge),
      allowCredentials: decodeDescriptors(pk.allowCredentials),
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error('no credential returned');

  const resp = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: bufferToBase64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    response: {
      authenticatorData: bufferToBase64url(resp.authenticatorData),
      clientDataJSON: bufferToBase64url(resp.clientDataJSON),
      signature: bufferToBase64url(resp.signature),
      userHandle: resp.userHandle
        ? bufferToBase64url(resp.userHandle)
        : undefined,
    },
  };
}

/** True when the user dismissed/cancelled the authenticator prompt. */
export function isCeremonyCancelled(err: unknown): boolean {
  return (
    err instanceof DOMException &&
    (err.name === 'NotAllowedError' || err.name === 'AbortError')
  );
}
