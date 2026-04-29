/**
 * Custom ESLint rule: no-literal-jsx-strings.
 *
 * Flags JSX text content and a handful of attribute values that
 * look like user-facing English strings, but aren't routed through
 * the i18n `t()` helper.
 *
 * What it catches:
 *   - JSX text: `<h1>Some Title</h1>`     →  `<h1>{t('key')}</h1>`
 *   - Attributes on a small allowlist of attribute names that
 *     almost always carry user-facing copy:
 *       placeholder, title, aria-label, alt
 *
 * What it ignores (correct silence):
 *   - JSX expression containers: `<h1>{t('key')}</h1>` — no JSXText
 *     to flag.
 *   - Anything inside `<code>...</code>` — protocol/path content.
 *   - Single character punctuation like "—", "▲", "—".
 *   - Anything matching the configurable `allow` list (default
 *     covers product names: SmoothNAS, smoothfs, ZFS, RAID, etc.).
 *   - Lines with `// i18n-allow: <reason>` on the same line.
 *
 * Options (all optional):
 *   allow:   string[]   — extra exact-match allowlist entries.
 *   allowRegex: string  — extra regex pattern; if a string matches,
 *                         it's allowed. Combined with the built-in
 *                         pattern.
 */

const DEFAULT_ALLOW_SET = new Set([
  // Product / project names
  'SmoothNAS', 'smoothfs', 'tierd', 'tierd-ui', 'smoothgui',
  // Storage protocol identifiers
  'ZFS', 'ZFS:', 'RAID', 'RAIDZ', 'JBOD', 'LVM', 'mdadm',
  'SMB', 'NFS', 'iSCSI', 'IQN', 'LUN', 'SLOG', 'L2ARC', 'ARC',
  // Hardware / network
  'NVME', 'SSD', 'HDD', 'MAC', 'IP', 'IPv4', 'IPv6', 'MTU',
  'VLAN', 'DHCP', 'CIDR', 'TLS', 'SSH', 'TCP', 'UDP',
  'NIC', 'LACP', 'MPIO',
  // OS / system
  'CPU', 'GPU', 'GRUB', 'UEFI', 'BIOS', 'UID', 'GID', 'RX', 'TX',
  // Common UI bits
  'OK', 'N/A',
  // SmoothGUI is a brand
  'SmoothGUI',
]);

// Built-in regex for things we always allow regardless of options:
//   - Pure numbers, percentages, byte sizes, paths, URLs, emails.
//   - Single character (punctuation, arrows, em-dash).
//   - Whitespace-only.
const BUILTIN_ALLOW_REGEX = new RegExp(
  '^\\s*$' +                                  // pure whitespace
  '|^[^a-zA-Z]+$' +                           // no alpha at all
  '|^[a-zA-Z]$' +                             // single char
  '|^[/\\.\\-\\w]+\\.[a-z]+$' +               // file.ext / path/file.ext
  '|^https?://' +                             // URLs
  '|^[^@\\s]+@[^@\\s]+$' +                    // emails
  '|^v?\\d+\\.\\d+(\\.\\d+)?[\\w.+-]*$' +     // version strings
  '|^/[/\\w.\\-]*$',                          // absolute paths
);

function isAllowed(text, opts) {
  const trimmed = text.trim();
  if (DEFAULT_ALLOW_SET.has(trimmed)) return true;
  if (BUILTIN_ALLOW_REGEX.test(trimmed)) return true;
  if (opts.allow && opts.allow.includes(trimmed)) return true;
  if (opts.allowRegex) {
    try {
      if (new RegExp(opts.allowRegex).test(trimmed)) return true;
    } catch {
      // Invalid regex — let other gates decide.
    }
  }
  // Strings that are mostly whitespace + one allowlisted token also pass.
  // E.g. "ZFS Pool" — "Pool" is the lint, not "ZFS". This rule is
  // specifically about whether the string needs t(). "ZFS Pool" does.
  return false;
}

const ATTR_NAMES_FLAGGED = new Set([
  'placeholder',
  'title',
  'aria-label',
  'alt',
]);

function looksUserFacingText(text) {
  // For JSXText nodes — content rendered between tags.
  const trimmed = text.trim();
  if (!trimmed) return false;
  const alphaRuns = trimmed.match(/[a-zA-Z]+/g) || [];
  if (alphaRuns.length === 0) return false;
  const alphaTotal = alphaRuns.join('').length;
  if (alphaTotal < 4) return false;
  return alphaRuns.length >= 2 || alphaTotal >= 5;
}

function looksUserFacingAttr(text) {
  // For attribute values (placeholder="...", title="..." etc.).
  // Stricter than JSXText: placeholders frequently carry example
  // values that aren't user copy:
  //   - "tank/vol0"  — protocol path example
  //   - "100G"       — sample byte size
  //   - "media"      — sample identifier
  //   - "192.168.1.1" — sample IP
  // We skip those and only flag values that look like real English
  // sentences ("Enter your hostname", "Filter by name…").
  const trimmed = text.trim();
  if (!trimmed) return false;
  // Skip path-like (slashes) and dotted (file.ext / version / IP).
  if (trimmed.includes('/') || /\d+\./.test(trimmed)) return false;
  // Skip single-token lowercase identifiers ("media", "tank").
  if (/^[a-z][a-z0-9_-]*$/.test(trimmed)) return false;
  // Skip strings that are mostly digits + unit suffix ("100G", "4K").
  if (/^\d+\s*[A-Za-z]?$/.test(trimmed)) return false;
  // Skip CIDR/IP-like sequences with commas.
  if (/^[\d.,/\s]+$/.test(trimmed)) return false;
  // Otherwise: needs both a space (multi-word) AND alphabetic content.
  return /\s/.test(trimmed) && /[a-zA-Z]/.test(trimmed);
}

function hasAllowComment(node, sourceCode) {
  // Same-line trailing comment is the obvious case
  // (`<h1>Hello</h1>  // i18n-allow: ...`).
  // We also check the line immediately above, because JSX has no
  // valid syntax for an inline comment between attributes inside
  // an opening tag — putting the marker on the previous line is
  // the cleanest workaround.
  const line = node.loc?.start?.line;
  if (line == null) return false;
  const comments = sourceCode.getAllComments();
  return comments.some(c =>
    /i18n-allow\b/.test(c.value) &&
    (c.loc.start.line === line || c.loc.start.line === line - 1),
  );
}

export default {
  meta: {
    type: 'suggestion',
    docs: {
      description:
        'flag hard-coded user-facing English strings in JSX (use t() instead)',
    },
    schema: [
      {
        type: 'object',
        properties: {
          allow: { type: 'array', items: { type: 'string' } },
          allowRegex: { type: 'string' },
        },
        additionalProperties: false,
      },
    ],
    messages: {
      jsxText:
        'JSX text "{{ text }}" should go through t() (or be added to the allow list / annotated with `// i18n-allow: <reason>`).',
      attribute:
        'JSX attribute {{ attr }}="{{ text }}" should go through t() (or be annotated with `// i18n-allow: <reason>`).',
    },
  },

  create(context) {
    const opts = context.options[0] || {};
    const sourceCode = context.sourceCode;

    return {
      JSXText(node) {
        const text = node.value;
        if (!looksUserFacingText(text)) return;
        if (isAllowed(text, opts)) return;
        if (hasAllowComment(node, sourceCode)) return;
        // Skip when the parent JSXElement is <code>: protocol bits.
        const parent = node.parent;
        if (parent && parent.openingElement?.name?.name === 'code') return;
        context.report({
          node,
          messageId: 'jsxText',
          data: { text: text.trim().slice(0, 60) },
        });
      },
      JSXAttribute(node) {
        const name = node.name?.name;
        if (typeof name !== 'string') return;
        if (!ATTR_NAMES_FLAGGED.has(name)) return;
        const value = node.value;
        if (!value || value.type !== 'Literal') return;
        if (typeof value.value !== 'string') return;
        if (!looksUserFacingAttr(value.value)) return;
        if (isAllowed(value.value, opts)) return;
        if (hasAllowComment(node, sourceCode)) return;
        context.report({
          node,
          messageId: 'attribute',
          data: { attr: name, text: value.value.slice(0, 60) },
        });
      },
    };
  },
};
