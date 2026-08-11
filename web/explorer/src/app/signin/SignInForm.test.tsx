import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { SignInForm } from './SignInForm';

const beginPasskeyLogin = vi.hoisted(() => vi.fn());
const finishPasskeyLogin = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  beginPasskeyLogin,
  finishPasskeyLogin,
}));

const supportsPasskeys = vi.hoisted(() => vi.fn());
const getPasskeyAssertion = vi.hoisted(() => vi.fn());
vi.mock('@/lib/webauthn', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/webauthn')>()),
  supportsPasskeys,
  getPasskeyAssertion,
}));

afterEach(() => {
  beginPasskeyLogin.mockReset();
  finishPasskeyLogin.mockReset();
  supportsPasskeys.mockReset();
  getPasskeyAssertion.mockReset();
});

describe('SignInForm passkey entry', () => {
  it('offers no passkey button when the browser lacks WebAuthn', () => {
    supportsPasskeys.mockReturnValue(false);
    render(<SignInForm />);
    expect(
      screen.getByRole('button', { name: /Send sign-in code/ }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: /Sign in with a passkey/ }),
    ).not.toBeInTheDocument();
  });

  it('runs the WebAuthn ceremony end-to-end on click', async () => {
    supportsPasskeys.mockReturnValue(true);
    const options = { publicKey: { challenge: 'example-challenge' } };
    const assertion = { id: 'example-assertion-id' };
    beginPasskeyLogin.mockResolvedValue(options);
    getPasskeyAssertion.mockResolvedValue(assertion);
    finishPasskeyLogin.mockResolvedValue(undefined);
    // jsdom's location.assign throws "not implemented" — stub it so
    // the post-login navigation is observable instead of fatal.
    const assign = vi.fn();
    Object.defineProperty(window, 'location', {
      value: { ...window.location, assign },
      writable: true,
    });

    render(<SignInForm />);
    fireEvent.click(
      await screen.findByRole('button', { name: /Sign in with a passkey/ }),
    );

    await waitFor(() => expect(finishPasskeyLogin).toHaveBeenCalledWith(assertion));
    expect(beginPasskeyLogin).toHaveBeenCalledTimes(1);
    expect(getPasskeyAssertion).toHaveBeenCalledWith(options);
    await waitFor(() => expect(assign).toHaveBeenCalledWith('/dashboard'));
  });

  it('surfaces a verification failure without leaving the email form', async () => {
    supportsPasskeys.mockReturnValue(true);
    beginPasskeyLogin.mockResolvedValue({ publicKey: {} });
    getPasskeyAssertion.mockResolvedValue({});
    finishPasskeyLogin.mockRejectedValue(new Error('boom'));

    render(<SignInForm />);
    fireEvent.click(
      await screen.findByRole('button', { name: /Sign in with a passkey/ }),
    );

    expect(await screen.findByRole('alert')).toHaveTextContent(
      /Passkey sign-in failed/,
    );
    // The email path is still usable as the fallback.
    expect(
      screen.getByRole('button', { name: /Send sign-in code/ }),
    ).toBeInTheDocument();
  });
});
