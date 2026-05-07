# Proposal: SmoothNAS Plugins — UI

**Status:** Pending
**Part of:** smoothnas-plugins (Step 6 of 9)
**Depends on:** plugins-04-bridge-and-proxy, plugins-05-profiles

---

## Problem

Plugins are installable, runnable, networked, and proxied — but
the only way to install one is via `tierd-cli`, and the only way
to see plugin status is to read DB rows. Operators expect a
SmoothNAS-native UI for the same actions.

This phase delivers the Plugins page, the install flow (sideload
URL or upload), the per-plugin detail page (status, logs, config),
and the API endpoints they consume. The iframe page that opens
the plugin's own UI is phase 07.

---

## Specification

### API endpoints

`tierd/internal/api/plugins.go`:

| Method | Path | Purpose |
|--------|------|---------|
| `GET`    | `/api/plugins`                      | list installed plugins (name, version, state, profiles, ports, brief health) |
| `GET`    | `/api/plugins/<name>`               | full detail incl. resolved create payload, bridge IP, last error |
| `POST`   | `/api/plugins/preflight`            | parse manifest, return placement preview + errors/warnings (also used during install wizard) |
| `POST`   | `/api/plugins/install`              | install (manifest URL or uploaded YAML; tier_assignments per phase 03) |
| `POST`   | `/api/plugins/<name>/start`         | |
| `POST`   | `/api/plugins/<name>/stop`          | |
| `POST`   | `/api/plugins/<name>/restart`       | |
| `PUT`    | `/api/plugins/<name>/config`        | update `plugin_config`; triggers restart |
| `DELETE` | `/api/plugins/<name>`               | full uninstall (per parent doc — all-or-none) |
| `GET`    | `/api/plugins/<name>/logs`          | SSE stream of container logs (Docker `/containers/<id>/logs?follow=1`) |
| `GET`    | `/api/plugins/<name>/events`        | SSE stream of state changes (subset of Docker `/events`) |
| `GET`    | `/api/plugin-profiles`              | (per phase 05) |
| `GET`    | `/api/plugin-profiles/<name>`       | (per phase 05) |
| `POST`   | `/api/plugin-profiles/preview`      | (per phase 05) |

The install endpoint accepts both `multipart/form-data` (file
upload + JSON metadata) and `application/json` (manifest URL).

### React routes

`tierd-ui/src/pages/Plugins/`:

```
PluginsListPage.tsx          // /plugins
PluginInstallPage.tsx        // /plugins/install
PluginDetailPage.tsx         // /plugins/manage/<name>
```

(`/plugins/<name>` — the iframe route — is added in phase 07; the
detail page lives at `/plugins/manage/<name>` to keep the path
clear.)

### Plugins list page

A card per installed plugin:

- name + version + vendor (left)
- status pill: `running` (green) / `stopped` (gray) /
  `failed` (red) / `pulling`/`creating`/`starting` (yellow)
- profiles shown as chips
- exposed ports (bridge port → `/plugins/<name>/`)
- actions: Start / Stop / Restart / Configure / Open / Uninstall
- "Open" is disabled until phase 07 lands (the route returns 404
  before then; UI hides the button if `ui.embed` is absent in
  the manifest).

Top of the page: "Install plugin" CTA → `/plugins/install`.

Empty state: brief explanation of what a plugin is + link to
docs + the same CTA.

### Install wizard

Single-page wizard with step indicator:

1. **Source.** "Manifest URL" or "Upload file". Paste/upload,
   click Continue.
2. **Preview.** tierd parses + validates; shows manifest summary
   (name, version, image / distro, ports, volumes, profiles,
   config keys with defaults). Validation errors block; warnings
   are visible.
3. **Tier assignments.** For each tier-bound volume, show a
   dropdown of compatible tiers (from `GET /api/tiers` filtered
   by the volume's `slot`). Disabled if the manifest has no
   tier-bound volumes.
4. **Configure.** Form generated from `manifest.config[]`:
   text input per string key, toggle per bool, masked field for
   `secret: true`. Defaults pre-filled.
5. **Confirm.** Final summary including resolved profiles
   (calls `/api/plugin-profiles/preview`). Operator clicks
   Install.
6. **Progress.** SSE-driven progress view during install:
   "Pulling image…" / "Setting up template…" / "Creating
   container…" / "Starting…" — each a discrete event from
   tierd's install pipeline. On success, redirect to the detail
   page. On failure, show the error and offer Retry / Cancel.

### Detail page

Tabs:

- **Overview.** Status, image / distro, profiles, ports, volumes
  (including resolved tier paths), uptime, last restart cause.
- **Logs.** SSE-streamed `journalctl`-style view backed by
  `/api/plugins/<name>/logs`. Pause / clear / download buttons.
  Follow-tail is on by default; pause when operator scrolls up.
- **Config.** Same form as the install wizard's step 4; saving
  PUTs to `/api/plugins/<name>/config` and prompts a restart.
- **Profiles.** Read-only view of resolved profiles with link
  to `/plugin-profiles/<name>` for each.
- **Danger Zone.** Uninstall button with a confirm dialog that
  enumerates exactly what will be removed (containers, volumes,
  cached image, nginx route, DB rows) — wording matches the
  parent doc's all-or-none policy.

### i18n

All operator-visible strings go in
`tierd-ui/src/i18n/locales/en.ts` under the `plugins.*`
namespace. Per the existing eslint rule (i18n-10), no JSX
literals.

### SSE client

The existing tierd-ui SSE utility (used for benchmarks live
telemetry) is reused. No new wire-format work.

### Permission integration

Plugins are admin-only in v1. The existing role gating around
the Updates page is reused — same gate, different page. Future
per-plugin permissions are out of scope.

---

## Out of scope

- The iframe page that displays the plugin's own UI (phase 07).
- Marketplace / curated registry (parked).
- Plugin upgrade flow (re-install with same name + new version
  works manually; an "update available" badge is future work).
- Profile editor in UI (operators edit YAML; the detail page
  links to the file path read from
  `/api/plugin-profiles/<name>`).

---

## Acceptance

- All endpoints in the table return the documented shapes;
  contract tests against `tierd/internal/api/plugins_test.go`.
- Install wizard happy path works end-to-end against a test
  plugin (the llama.cpp manifest from phase 08 testdata).
- Install wizard surfaces validation errors at step 2 and
  blocks Continue.
- Logs SSE stream stays open across container restart and
  resumes streaming the new container's logs.
- Uninstall confirm dialog enumerates the correct removal
  list and the operation completes from the UI.
- Existing Cypress / Vitest suites pass; new component tests
  cover the wizard steps in isolation.
- New eslint-rule pass: no JSX literals introduced under
  `tierd-ui/src/pages/Plugins/`.
