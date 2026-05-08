# Proposal: SmoothNAS Plugins — Iframe Embed + Bearer Auth

**Status:** Pending
**Part of:** smoothnas-plugins (Step 7 of 9)
**Depends on:** plugins-04-bridge-and-proxy, plugins-06-ui

---

## Problem

Phase 04 reverse-proxies `/plugins/<name>/` to the plugin's
container, and phase 06 adds an "Open" button on the plugins page.
But the operator clicking "Open" today either lands on a bare
plugin UI in a new tab (loses SmoothNAS chrome) or — if we naively
iframe — gets a plugin that doesn't know it's authenticated.

This phase adds:

- the `/plugins/<name>` route in tierd-ui that iframes the proxied
  plugin UI inside the SmoothNAS chrome,
- bearer-token auth injection in the nginx proxy so the plugin
  sees an authenticated request without doing its own login, and
- the CSP relaxations needed for the iframe to actually render.

Plugin authors who declare `ui.embed.auth: bearer-injected` get
single-sign-on for free; plugins that prefer their own login
(`auth: none`) just get the iframe.

---

## Specification

### `/plugins/<name>` route

`tierd-ui/src/pages/Plugins/PluginEmbedPage.tsx`:

- Route: `/plugins/:name`
- Renders the SmoothNAS shell (sidebar, header) with the main
  content area filled by a single `<iframe>` whose `src` is
  `/plugins/<name>/<embed-path>` (the `embed.path` from the
  manifest, default `/`).
- iframe attributes:
  - `sandbox="allow-scripts allow-same-origin allow-forms allow-downloads allow-popups"`
  - no `allow-top-navigation` — a malicious plugin cannot
    navigate the parent away.
  - `referrerpolicy="no-referrer"`
  - 100% width and height of the content area; no decoration.
- 404 on `/plugins/<unknown>` and on plugins without
  `ui.embed`.

The header within the SmoothNAS chrome shows: plugin name +
version, status pill (live via SSE), small "Manage" button
linking to `/plugins/manage/<name>`.

### Bearer-token auth injection

When a plugin's manifest declares `ui.embed.auth: bearer-injected`,
tierd issues a per-plugin bearer token at install time and rotates
it on operator demand. The token is stored in the
`plugin_secrets` table:

```sql
CREATE TABLE plugin_secrets (
    plugin_name  TEXT    PRIMARY KEY REFERENCES plugins(name) ON DELETE CASCADE,
    bearer_token TEXT    NOT NULL,                  -- 256 bits, hex
    issued_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);
```

(Added in this phase; phase-01 schema migration is amended.)

The nginx route generated in phase 04 fills the previously-commented
auth line:

```nginx
proxy_set_header Authorization "Bearer <token>";
```

— with the live token string. `nginx -t && reload` applies it.

The plugin sees an `Authorization: Bearer <token>` header on
every proxied request. What the plugin does with it is the
plugin's problem — typically: validate against a static config
value (passed via `manifest.config[]` with `secret: true` and
the same value as `bearer_token`).

Token rotation: a `POST /api/plugins/<name>/rotate-token` endpoint
issues a new token, rewrites the nginx route, and reloads. A
plugin that hard-codes the token in its config will need its
config updated — tierd offers a "rotate and update config" flow
in the UI (single click rewrites the matching `config` key).

For `ui.embed.auth: none` plugins, no token is generated; the
proxy passes through whatever auth headers the operator's
browser sends (typically nothing useful — the plugin handles its
own login).

### CSP

The SmoothNAS Content-Security-Policy header (set by the parent
nginx config) gets a relaxation for the iframe ancestor and
frame-src:

```
Content-Security-Policy:
    default-src 'self';
    frame-src   'self';      // /plugins/<name>/ is same-origin
    frame-ancestors 'self';
```

Because the iframe target is same-origin (proxied through nginx
on the same host:port), no `frame-src` widening to external
origins is needed. The plugin's own response can set its own
CSP that further restricts; tierd does not strip plugin CSP
headers.

`X-Frame-Options: SAMEORIGIN` is set on the parent SmoothNAS
response. Plugin responses passing through the proxy may set
their own `X-Frame-Options`; tierd does not rewrite it (a plugin
that sets `DENY` will not embed — that's the plugin author's
choice).

### Cookie isolation

Cookies set by the plugin (path `/plugins/<name>/`) and cookies
set by SmoothNAS (path `/`) coexist without collision because of
path scoping. tierd does not forward SmoothNAS session cookies
to the plugin (the bearer token replaces them), and plugins do
not see the SmoothNAS session cookie because of the path-scoped
proxy.

### Operator opt-out

The detail page (phase 06) gets an extra toggle in the Overview
tab: "Embed in SmoothNAS UI" (default on, only shown for plugins
with `ui.embed`). Off → the "Open" button on the list and the
"Open in SmoothNAS" link on the detail page open
`/plugins/<name>/` in a new browser tab instead of the embed
page. State persisted per-plugin in `plugin_config` under
`SMOOTHNAS_EMBED_DISABLED=true`.

### Refresh and error states

- iframe load failure (container down, route 502): the embed
  page shows an inline error matching SmoothNAS chrome
  ("Plugin is not running. Start it from the Manage page.")
  with a Manage link. Detected via a 5s no-load timeout +
  fetching `/plugins/<name>/` directly to read the status code.
- Plugin restart while iframe is open: the page subscribes to
  the same SSE event stream as the detail page; on `restart`
  events it shows a "Reconnecting…" toast and reloads the
  iframe.

---

## Out of scope

- Cross-plugin SSO (token sharing across plugins). Each plugin
  has its own token.
- OAuth-style flows where the plugin redirects to a SmoothNAS
  consent page. Bearer injection is the only auth mode in v1.
- Per-user permission gating on which plugins which users can
  embed. v1 is admin-only end-to-end.

---

## Acceptance

- Installing the llama.cpp test plugin and clicking "Open" in
  the Plugins list lands on `/plugins/llama-cpp` with the
  llama-server UI rendered inside the SmoothNAS chrome.
- A test plugin that validates `Authorization: Bearer <token>`
  receives the expected header and accepts requests.
- `POST /api/plugins/llama-cpp/rotate-token` issues a new token,
  the nginx route reloads, the next iframe request carries the
  new token. The "rotate and update config" UI flow updates the
  plugin's config in the same transaction.
- Stopping a plugin with the embed page open surfaces the
  "Plugin is not running" inline error within 5s.
- A plugin returning `X-Frame-Options: DENY` fails to embed and
  shows a clear "This plugin refuses embedding" message instead
  of a blank iframe.
- CSP is correctly set; browser console shows no CSP violations
  on the embed page during normal operation.
