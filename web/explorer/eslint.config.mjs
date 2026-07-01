// Flat ESLint config (ESLint 9+). Replaces the legacy .eslintrc.json — `next
// lint` was removed in Next 16, and eslintrc support is going away in ESLint 10.
// eslint-config-next 16 ships native flat-config arrays, so we spread them
// directly (no FlatCompat shim needed).
import coreWebVitals from 'eslint-config-next/core-web-vitals';
import typescript from 'eslint-config-next/typescript';

const eslintConfig = [
  { ignores: ['.next/**', 'out/**', 'node_modules/**', 'next-env.d.ts'] },
  ...coreWebVitals,
  ...typescript,
  {
    rules: {
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
      ],
      // eslint-config-next 16 turns on the React Compiler react-hooks rules.
      // Adopt them as ADVISORY (warn) for now: they flag ~21 pre-existing
      // patterns (mostly intentional setState-in-effect for client-hydration
      // reads). Promoting to error + fixing each site is a tracked
      // code-quality pass, out of scope for the dependency upgrade.
      'react-hooks/set-state-in-effect': 'warn',
      'react-hooks/static-components': 'warn',
      'react-hooks/purity': 'warn',
      'react-hooks/immutability': 'warn',
      'react-hooks/refs': 'warn',
    },
  },
];

export default eslintConfig;
