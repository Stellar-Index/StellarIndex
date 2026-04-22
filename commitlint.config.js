// commitlint — enforces conventional commits across the repo.
//
// We're a Go project but this config has zero Go-specific constraints;
// commitlint runs in CI via a small action and local hook.
// Pattern borrowed from loop-app/commitlint.config.js with our own
// type + scope enumerations.

export default {
  extends: ['@commitlint/config-conventional'],
  rules: {
    'type-enum': [
      2,
      'always',
      [
        'feat',     // new feature (user-visible)
        'fix',      // bug fix
        'refactor', // code change that neither adds feature nor fixes bug
        'perf',     // performance improvement
        'test',     // test additions / changes
        'docs',     // docs-only change
        'chore',    // tooling, housekeeping
        'ci',       // CI config / pipelines
        'build',    // build system / external deps
        'revert',   // revert a previous commit
      ],
    ],
    'scope-enum': [
      2,
      'always',
      [
        // components
        'indexer',
        'aggregator',
        'api',
        'ops',
        'migrate',

        // cross-cutting packages
        'canonical',
        'consumer',
        'extract',
        'storage',
        'auth',
        'ratelimit',
        'metadata',
        'supply',
        'obs',
        'divergence',

        // source families
        'sources',
        'sdex',
        'soroswap',
        'aquarius',
        'phoenix',
        'comet',
        'blend',
        'reflector',
        'redstone',
        'band',
        'chainlink',
        'cex',
        'fx',

        // repo-wide concerns
        'deploy',
        'ci',
        'docs',
        'deps',
        'adr',
        'infra',
        'security',
      ],
    ],
    'subject-max-length': [2, 'always', 72],
    'body-max-line-length': [2, 'always', 100],
  },
};
