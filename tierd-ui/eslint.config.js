// SmoothNAS frontend ESLint config (flat-config, ESLint 9).
//
// Today this config carries one custom rule: i18n/no-literal-jsx-strings.
// It exists to fail-fast on hard-coded JSX strings that should go through
// the i18n t() helper. Other ESLint hygiene rules (typescript, react, etc.)
// can be layered on later — keeping the surface tight for now.

import jsRecommended from '@eslint/js';
import reactHooks from 'eslint-plugin-react-hooks';
import noLiteralJsxStrings from './eslint-rules/no-literal-jsx-strings.js';

export default [
  {
    ignores: [
      'dist/**',
      'node_modules/**',
      'eslint-rules/**',
      'eslint.config.js',
      // Orphan files superseded by src/pages/Arrays/Arrays.tsx in
      // commit a95ed26 ("merge mdadm and ZFS into unified Arrays
      // page"). Not imported anywhere; left in place per the
      // operator's call. The rule would otherwise flag them.
      'src/pages/Arrays/Mdadm.tsx',
      'src/pages/Arrays/Zfs.tsx',
    ],
  },
  {
    files: ['src/**/*.{ts,tsx}'],
    languageOptions: {
      parser: (await import('typescript-eslint')).default.parser,
      parserOptions: {
        ecmaVersion: 'latest',
        sourceType: 'module',
        ecmaFeatures: { jsx: true },
      },
    },
    plugins: {
      i18n: { rules: { 'no-literal-jsx-strings': noLiteralJsxStrings } },
      'react-hooks': reactHooks,
    },
    rules: {
      // React-hooks. The codebase already has explicit
      // `// eslint-disable-next-line react-hooks/exhaustive-deps`
      // directives in a few places, so the team intends to enforce it.
      'react-hooks/exhaustive-deps': 'warn',
      'react-hooks/rules-of-hooks': 'error',
      // Default rule severity: warn. CI flips to --max-warnings 0 once the
      // existing surface is clean, but we ship at warn first so an
      // accidental new literal doesn't block unrelated PRs.
      'i18n/no-literal-jsx-strings': ['error', {
        allow: [
          // Page-wide breadcrumbs that aren't user copy.
          'admin', 'admin@', 'root',
          // Common nav identifiers / class names that show up as text in
          // some places (CSS class tokens, badge values, debug output).
          'enabled', 'disabled', 'active', 'standby', 'pending',
          // Protocol / unit identifiers shown verbatim as badges or
          // chart suffixes.
          'rsync', 'delete', 'MiB/s', 'Mbps',
        ],
      }],
    },
  },
];
