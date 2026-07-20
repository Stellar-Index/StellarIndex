// Registers jest-dom matchers (toBeInTheDocument, toHaveAttribute, …) on
// vitest's expect, and unmounts rendered trees between tests.
import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

afterEach(() => {
  cleanup();
});
