# Proposal: SmoothNAS Plugins — Foundation

**Status:** Pending
**Part of:** smoothnas-plugins (Step 1 of 9)

---

## Problem

Nothing in the tree models a plugin. Before any LXC2Docker
integration or UI work can happen, tierd needs the manifest schema
(supporting both `oci-image` and `lxc-distro` artifact shapes), a
parser, the SQLite tables that record installed plugins, and a thin
CLI surface that exercises those pieces end-to-end on a developer
machine.

This phase delivers exactly that and nothing more. There is no
runtime daemon talked to here, no container created, no tier
resolution, no bridge network, no nginx, no UI. A plugin
"installed" by this phase is a row in the DB; it does not run.

---

## Specification

### Manifest schema

`tierd/internal/plugin/manifest.go` defines the v1 schema as Go
structs and provides `Parse([]byte) (*Manifest, error)` and
`Validate(*Manifest) error`.

```go
type Manifest struct {
    APIVersion string         `yaml:"apiVersion"` // must be "smoothnas.io/v1"
    Kind       string         `yaml:"kind"`       // must be "Plugin"
    Metadata   Metadata       `yaml:"metadata"`
    Artifact   Artifact       `yaml:"artifact"`   // tagged union; see below
    Container  Container      `yaml:"container"`
    Instances  Instances      `yaml:"instances"`  // optional; defaults to {Count:1, Configurable:false}
    Volumes    []Volume       `yaml:"volumes"`
    Ports      []Port         `yaml:"ports"`
    UI         *UI            `yaml:"ui,omitempty"`
    Profiles   []string       `yaml:"profiles"`   // resolved in phase 05
    Config     []ConfigField  `yaml:"config"`
}

type Instances struct {
    Count        int  `yaml:"count"`        // default 1
    Configurable bool `yaml:"configurable"` // default false
}

// Volume gains a perInstance flag (default false) — when count > 1
// and perInstance is true, the volume's host_path is suffixed
// with /instance-<n>/ per replica; when false (default), the
// host_path is shared across all replicas.

// Artifact is a tagged union: Type selects which of the two
// embedded sub-structs is read. The unselected struct is ignored.
type Artifact struct {
    Type   string         `yaml:"type"`   // "oci-image" | "lxc-distro"
    OCI    OCIArtifact    `yaml:",inline"`
    Distro DistroArtifact `yaml:",inline"`
}

type OCIArtifact struct {
    Image  string `yaml:"image"`            // e.g. ghcr.io/foo/bar:tag
    Digest string `yaml:"digest,omitempty"` // sha256:...
}

type DistroArtifact struct {
    Distro   string   `yaml:"distro"`             // ubuntu | debian | alpine | ...
    Release  string   `yaml:"release"`            // jammy, bookworm, 3.19, ...
    Arch     string   `yaml:"arch,omitempty"`     // default: host arch
    Packages []string `yaml:"packages,omitempty"` // apt/apk overlay
    Setup    []string `yaml:"setup,omitempty"`    // one-shot setup commands
}

type Container struct {
    Command       []string `yaml:"command,omitempty"`
    WorkingDir    string   `yaml:"workingDir,omitempty"`
    User          string   `yaml:"user,omitempty"`
    RestartPolicy string   `yaml:"restartPolicy"` // unless-stopped | on-failure | no
}
```

Field-level rules enforced by `Validate`:

- `metadata.name` must match `^[a-z]([-a-z0-9]{0,38}[a-z0-9])?$`
  (DNS-1123 label, ≤40 chars).
- `metadata.version` must parse as semver.
- `artifact.type` must be `oci-image` or `lxc-distro`.
- For `oci-image`: `image` must be a valid Docker image reference
  (registry/path:tag form); `digest`, when present, must match
  `^sha256:[a-f0-9]{64}$`.
- For `lxc-distro`: `distro` and `release` are required, both must
  match `^[a-z0-9._-]+$`. `arch` defaults to the host arch when
  empty; valid values are LXC2Docker's accepted arches
  (`amd64`, `arm64`, `armhf`, `i386`).
- For `lxc-distro`: `command` is required (the distro template has
  no default CMD). For `oci-image`: `command` is optional and
  overrides the image's CMD when present.
- `container.restartPolicy` must be one of `unless-stopped`,
  `on-failure`, `no`.
- `instances.count` must be ≥ 1. When omitted entirely, the
  manifest is treated as `{count: 1, configurable: false}`. When
  `count > 1`, at least one volume should declare
  `perInstance: true` or all instances will share state — the
  validator emits a warning, not an error, since some workloads
  legitimately want shared state.
- `volumes[].mode` must be `tier-bound` or `flat`. Tier-bound
  volumes must declare `slot` (one of `NVME`, `SSD`, `HDD` in v1;
  validation against the dynamic `tier_levels` table is phase 03).
- `volumes[].perInstance` is a boolean (default false). Has no
  effect when `instances.count == 1`.
- `volumes[].bind` must be an absolute path inside the container.
- `volumes[].name` must be unique within the manifest and match
  `^[a-z][a-z0-9-]{0,30}$`.
- `ports[].port` must be 1–65535; `protocol` must be `tcp` or `udp`.
- `ui.embed.auth` must be `none` or `bearer-injected` when present.
- `profiles[]` entries are recorded but not validated against the
  profile catalog in this phase (phase 05 owns that).
- `config[].key` must match `^[A-Z][A-Z0-9_]{0,63}$`.

`Validate` returns a `*ValidationError` that lists every problem
found, not just the first one — operators sideloading a manifest
should see all errors at once.

### SQLite tables

Migration `2026_05_07_plugins.sql` adds:

```sql
CREATE TABLE plugins (
    name             TEXT    PRIMARY KEY,
    version          TEXT    NOT NULL,
    state            TEXT    NOT NULL,         -- aggregate; see "Aggregate state" below
    manifest         TEXT    NOT NULL,         -- raw YAML, source of truth
    artifact_type    TEXT    NOT NULL,         -- oci-image | lxc-distro
    image_ref        TEXT,                     -- oci-image: full ref pulled (with resolved digest)
    distro_summary   TEXT,                     -- lxc-distro: 'ubuntu/jammy/amd64' for display
    instance_count   INTEGER NOT NULL DEFAULT 1,
    instance_configurable INTEGER NOT NULL DEFAULT 0,
    installed_at     TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at       TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE plugin_instances (
    plugin_name   TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    instance      INTEGER NOT NULL,            -- 1..instance_count
    container_id  TEXT,                        -- LXC2Docker container ID; NULL until phase 02 creates it
    state         TEXT    NOT NULL,            -- installed | pulling | creating | starting | running | stopped | failed
    bridge_ip     TEXT,                        -- populated by phase 04
    last_change   TEXT    NOT NULL DEFAULT (datetime('now')),
    last_error    TEXT,                        -- last failure reason, if any
    PRIMARY KEY (plugin_name, instance)
);

CREATE TABLE plugin_volumes (
    plugin_name  TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    volume_name  TEXT    NOT NULL,
    mode         TEXT    NOT NULL,             -- tier-bound | flat
    slot         TEXT,                         -- NULL when flat
    tier_pool    TEXT,                         -- resolved in phase 03; NULL here
    per_instance INTEGER NOT NULL DEFAULT 0,
    bind_path    TEXT    NOT NULL,             -- where it appears in the container
    PRIMARY KEY (plugin_name, volume_name)
);

CREATE TABLE plugin_volume_paths (
    plugin_name  TEXT    NOT NULL,
    volume_name  TEXT    NOT NULL,
    instance     INTEGER NOT NULL,             -- always 1 for non-perInstance volumes
    host_path    TEXT    NOT NULL,             -- where on host the data lives
    PRIMARY KEY (plugin_name, volume_name, instance),
    FOREIGN KEY (plugin_name, volume_name) REFERENCES plugin_volumes(plugin_name, volume_name) ON DELETE CASCADE
);

CREATE TABLE plugin_ports (
    plugin_name  TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    port_name    TEXT    NOT NULL,
    container_port INTEGER NOT NULL,           -- port inside the container
    protocol     TEXT    NOT NULL,
    expose       INTEGER NOT NULL,             -- 0/1; if 1, nginx /plugins/<name>/ proxies here
    host_expose  INTEGER NOT NULL DEFAULT 0,   -- v1: always 0; phase 09 may set 1
    PRIMARY KEY (plugin_name, port_name)
);

CREATE TABLE plugin_config (
    plugin_name  TEXT    NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    key          TEXT    NOT NULL,
    value        TEXT    NOT NULL,
    PRIMARY KEY (plugin_name, key)
);
```

The `container_id` lives on `plugin_instances`, not `plugins`,
from the start — single-instance plugins simply have one row in
`plugin_instances` with `instance = 1`. This avoids a schema
migration in phase 09 and keeps the multi-instance code path
the same as the single-instance one.

The `plugin_volume_paths` table similarly handles single- and
multi-instance volumes uniformly: a non-`perInstance` volume has
one row with `instance = 1`; a `perInstance` volume has N rows.

There is no exposed-port uniqueness constraint at the DB level —
plugins do not bind to host ports in v1; nginx proxies to each
plugin's container bridge IP, so two plugins can both listen on
internal port 8080 without conflict. Phase 04 owns the bridge-IP
allocation and the nginx route uniqueness (which is keyed on plugin
name, not port).

### Aggregate state

The `plugins.state` column is denormalised from `plugin_instances`:

| Per-instance states           | Aggregate `plugins.state` |
|-------------------------------|----------------------------|
| All `running`                 | `running`                  |
| All `stopped`                 | `stopped`                  |
| Some `running`, some `failed` | `degraded`                 |
| All `failed`                  | `failed`                   |
| Any in `pulling`/`creating`/`starting` | matching transitional state |
| Mixed otherwise               | `degraded`                 |

tierd updates `plugins.state` whenever any `plugin_instances.state`
changes; the column exists for fast list queries and exists as a
materialised view of the per-instance rows.

`plugin_instances.state` and `plugin_instances.container_id` are
both `NULL`/`installed` after a phase-01 install; phase 02
populates them when LXC2Docker actually creates the containers.

### Package layout

```
tierd/internal/plugin/
    manifest.go        // structs + Parse + Validate
    manifest_test.go
    state.go           // DB CRUD against the four tables
    state_test.go
    install.go         // install(manifest, artifactReader) — phase 01 stub
    install_test.go
    errors.go
```

`install.go` in this phase performs:

1. `Parse` + `Validate` the manifest.
2. In one transaction, insert rows across the tables:
   - `plugins`: one row, with `instance_count` and
     `instance_configurable` from the manifest's `instances`
     block (defaulting to `1` and `false` when the block is
     omitted). `state = 'installed'`.
   - `plugin_instances`: one row per instance (1..N), all with
     `state = 'installed'` and `container_id = NULL`.
   - `plugin_volumes`: one row per declared volume.
   - `plugin_volume_paths`: rows expanded per instance — for a
     `per_instance = 0` volume, one row with `instance = 1`; for
     `per_instance = 1` volumes, N rows.
   - `plugin_ports`, `plugin_config`: as per the manifest.
3. For tier-bound `plugin_volume_paths` rows: write `host_path = ""`
   and a sentinel `tier_pool = "<unresolved>"` on the parent
   `plugin_volumes` row; phase 03 fills both in.
4. For flat `plugin_volume_paths` rows: write the real path
   immediately and `os.MkdirAll` it. The path is:
   - `/var/lib/smoothnas/plugins/<name>/<volume>/` for shared volumes
   - `/var/lib/smoothnas/plugins/<name>/instance-<n>/<volume>/`
     for `perInstance` volumes
5. For `oci-image` plugins: record the manifest's
   `artifact.image` and `digest` (if present) in `image_ref`. The
   actual image pull happens in phase 02 when LXC2Docker exists;
   no network I/O in this phase.
6. For `lxc-distro` plugins: record a display-friendly
   `distro_summary` such as `ubuntu/jammy/amd64` in the row.

There is no container creation, no LXC2Docker call, no image
pull. All instance states stay at `installed`. `container_id`
stays `NULL` on every instance row.

### CLI surface

`tierd/cmd/tierd-cli/main.go` gains a `plugin` subcommand:

```
tierd-cli plugin install <manifest.yaml>
tierd-cli plugin install --url <manifest-url>
tierd-cli plugin list
tierd-cli plugin show <name>
tierd-cli plugin uninstall <name>
```

Note the difference from prior drafts: there is no separate
artifact file argument. The manifest is the only input — for
`oci-image` plugins the image lives in a registry, for
`lxc-distro` plugins the distro template lives in LXC's download
template repo. Both are pulled by LXC2Docker in phase 02.

`uninstall` in this phase deletes flat-volume directories and the
four DB rows. It is a stub for the full uninstall flow that later
phases extend with container removal, image cache eviction, nginx
route removal, firewall revocation, and tier-bound volume
deletion.

`start` and `stop` exist but print
`"plugin lifecycle requires phase 02"` and exit non-zero. This
keeps the CLI shape stable across the series.

---

## Filesystem layout

Created by this phase:

```
/var/lib/smoothnas/plugins/
    <name>/<volume>/            # flat-mode volume data (per plugin, per volume)
```

Tier-bound volume paths are not created here — phase 03 owns those.
The runtime/image-cache tree under `/var/lib/smoothnas/runtime/`
is not created here — phase 02 owns that.

---

## Out of scope

- LXC2Docker daemon integration (phase 02)
- Container creation, image pulls, lifecycle (phase 02)
- Tier-bound volume resolution (phase 03)
- Bridge network + nginx reverse-proxy (phase 04)
- Profile resolution (phase 05)
- Any UI (phase 06)
- Reference plugins (phases 08, 09)

---

## Acceptance

- `go test ./internal/plugin/...` passes with table-driven tests
  for at least: valid llama.cpp `oci-image` manifest, valid
  gh-runner `oci-image` manifest with `instances: {count: 2,
  configurable: true}` and a `perInstance` workspace volume,
  valid Ubuntu+Python `lxc-distro` manifest, every individual
  `Validate` failure mode (including `lxc-distro` missing
  `command`, `oci-image` malformed `digest`, unknown
  `artifact.type`, `instances.count` < 1).
- `tierd-cli plugin install testdata/llama.yaml` succeeds, lists
  the plugin in `tierd-cli plugin list` with state `installed`,
  and `uninstall` removes the row and any flat-volume directories
  it created. The `plugin_instances` table contains exactly one
  row for the plugin with `instance = 1`.
- `tierd-cli plugin install testdata/gh-runner.yaml` lands two
  rows in `plugin_instances` (`instance = 1, 2`), and the
  `perInstance` workspace volume produces two `plugin_volume_paths`
  rows with distinct `instance-1` / `instance-2` host paths under
  `/var/lib/smoothnas/plugins/gh-runner/`.
- `tierd-cli plugin install testdata/ubuntu-python.yaml` (an
  `lxc-distro` manifest) lands rows with `artifact_type =
  'lxc-distro'` and a populated `distro_summary`.
- Schema migration round-trips: applying the migration on an
  existing dev DB and rolling it back leaves no plugin tables.
