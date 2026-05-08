# Proposal: SmoothNAS Plugins — Profiles

**Status:** Pending
**Part of:** smoothnas-plugins (Step 5 of 9)
**Depends on:** plugins-02-runtime-integration

---

## Problem

The plugin manifest declares per-plugin specifics (image, ports,
volumes), but the *policy* bits — which host devices a GPU plugin
needs, what the default memory cap should be, whether to attach the
LAN network — are the same across many plugins and have wrong
defaults at the manifest level. A llama.cpp manifest shouldn't
need to know the path of the host's NVIDIA device nodes; it just
needs to say "I need a GPU."

This phase introduces Incus-style **profiles**: declarative,
composable bundles of container-create policy that the manifest
opts into by name. tierd ships a small built-in catalog
(`gpu-amd`, `gpu-nvidia`, `gpu-intel`, `default-limits`,
`lan-discoverable`); operators can add custom profiles in
`/etc/smoothnas/plugin-profiles.d/`.

---

## Specification

### Profile shape

A profile is a YAML document that contributes fragments of a
Docker create payload:

```yaml
apiVersion: smoothnas.io/v1
kind: PluginProfile
metadata:
  name: gpu-amd
  description: AMD GPU passthrough via /dev/dri
container:
  hostConfig:
    devices:
      - { path: "/dev/dri", cgroupPermissions: "rwm" }
    capAdd: []
  env: {}
lxc:
  rawConfig:
    - "lxc.cgroup2.devices.allow = c 226:* rwm"
    - "lxc.mount.entry = /dev/dri dev/dri none bind,optional,create=dir 0 0"
preflight:
  hostHas:
    - path: /dev/dri
      requirement: optional   # warn-only if missing
```

The schema is intentionally a strict subset of the create payload
plus an `lxc.rawConfig` escape hatch (LXC2Docker maps these to
the underlying LXC container config — needed for fine-grained
device control that Docker's `Devices` array doesn't express).

### Profile catalog

Built-in profiles ship under `/usr/share/smoothnas/profiles/`:

| Name | Purpose |
|------|---------|
| `default-limits` | Sane default `MemoryMax`, `PidsLimit`, `OOMScoreAdj`. Applied to all plugins unless overridden. |
| `gpu-amd` | Bind `/dev/dri/*`, AMD cgroup device permissions. |
| `gpu-nvidia` | Bind `/dev/nvidia*`, `/dev/nvidiactl`, `/dev/nvidia-uvm*`, NVIDIA cgroup permissions. |
| `gpu-intel` | Bind `/dev/dri/*`, Intel cgroup device permissions (same `/dev/dri` as AMD; identical for current kernels — separate name for forward compat). |
| `lan-discoverable` | Attach a second NIC to the host's LAN bridge so mDNS works (uses LXC2Docker's dual-NIC mode). |
| `privileged` | Disable all user-namespace remapping; full host access. **Strongly discouraged**, exists for completeness. |

Operator-supplied profiles live under
`/etc/smoothnas/plugin-profiles.d/<name>.yaml` and override
built-ins by name (operator wins). tierd watches this directory
and reloads on change; running plugins are not recreated, but
new installs see the new profile.

### Resolution and merge order

A plugin's effective create payload is built by deep-merging in
this order (later wins on conflict):

1. **`default-limits` profile** (always applied first if not
   explicitly excluded via `profiles: ["!default-limits", ...]`).
2. **Each profile listed in `manifest.profiles`**, in order.
3. **The plugin manifest's own fields** (`container`, `volumes`,
   `ports`, etc.).

Merge rules:

- Scalars: replace.
- Maps: deep-merge.
- Arrays of scalars: concatenate, deduplicate.
- Arrays of objects (`devices`, `binds`): concatenate (no dedup —
  duplicates would be a manifest bug worth surfacing).
- The `lxc.rawConfig` array: concatenate; tierd writes the
  combined list as `lxc.raw=` directives in the create payload's
  `Labels` (LXC2Docker reads
  `io.smoothnas.lxc.raw.<n>=<directive>` labels and applies them).

### Preflight

A profile may declare `preflight.hostHas`. tierd runs these
checks at install time and surfaces failures the same way as
phase 03 tier preflight:

- `requirement: required` → install fails if path missing
- `requirement: optional` → install warns; profile applies
  whatever it can

For GPU profiles, this is how "operator installed `gpu-amd` on a
host with no AMD GPU" surfaces as a warning, not a hard failure
(the plugin may still work in CPU mode).

### CLI surface

```
tierd-cli profile list
tierd-cli profile show <name>
tierd-cli profile validate <path-to-yaml>
```

No add/remove verbs — operator profiles are filesystem-managed.

### API surface

```
GET  /api/plugin-profiles                 # list with source (builtin|operator)
GET  /api/plugin-profiles/<name>          # full body + resolved fields
POST /api/plugin-profiles/preview
{ "manifest": "<yaml>", "profiles": [...] }
→ resolved create payload preview
```

The preview endpoint is what the install UI (phase 06) uses to
show "this is what will run, with these profiles applied" before
the operator commits.

### Schema field added in phase 01

Phase 01's `plugins` table has no profile column. This phase adds:

```sql
ALTER TABLE plugins ADD COLUMN profiles_json TEXT NOT NULL DEFAULT '[]';
```

Storing the resolved profile list (after `default-limits` injection
and operator overrides) so a future tierd doesn't have to
re-resolve against a possibly-changed catalog to know what was
applied. Manifest stays the source of truth for *requested*
profiles; this column is the *applied* set.

### Effects on phase 02

The container-create logic from phase 02 grows a single call:

```go
payload := profiles.Resolve(manifest, catalog)
runtime.CreateContainer(ctx, payload)
```

Everything else in phase 02 stays as written. The lifecycle
verbs are unchanged.

---

## Out of scope

- Resource quota enforcement beyond what cgroups already provide.
- Per-profile UI in the SmoothNAS browser (operators edit YAML
  files; viewing is in phase 06's plugin install flow).
- Profile inheritance (`extends:`). Composition by listing
  multiple profiles is enough for v1.
- Per-plugin profile overrides expressed inline in the manifest.
  If a plugin needs a one-off, it ships its own profile.

---

## Acceptance

- Built-in profiles resolve and validate; tests cover the
  built-in catalog parses without errors.
- A llama.cpp manifest declaring `profiles: [gpu-amd]` produces
  a create payload with `/dev/dri` in `Devices` and the
  corresponding `lxc.cgroup2.devices.allow` raw-config label.
- Operator-supplied profile in
  `/etc/smoothnas/plugin-profiles.d/test.yaml` is discovered
  on tierd start; reloading the file (touch) is picked up
  within 5s.
- `default-limits` is applied automatically; opting out via
  `profiles: ["!default-limits"]` removes it from the resolved
  payload.
- Preflight: GPU profile installed on a host with no `/dev/dri`
  surfaces a warning; the plugin still installs.
- `tierd-cli profile show gpu-amd` prints the source path and
  body.
