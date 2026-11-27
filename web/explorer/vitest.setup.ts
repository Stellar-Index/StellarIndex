// Registers jest-dom matchers (toBeInTheDocument, toHaveAttribute, …) on
// vitest's expect, and unmounts rendered trees between tests.
import '@testing-library/jest-dom/vitest';
import { afterEach, vi } from 'vitest';
import { cleanup } from '@testing-library/react';
import { notifyManager } from '@tanstack/react-query';

afterEach(async () => {
  cleanup();
  // react-query delivers updates on a later setTimeout(0); one left queued at file end fires into
  // React after jsdom is gone. Under fake timers it never fires, and awaiting it would hang.
  if (!vi.isFakeTimers()) {
    await new Promise<void>((resolve) => notifyManager.schedule(resolve));
  }
});
