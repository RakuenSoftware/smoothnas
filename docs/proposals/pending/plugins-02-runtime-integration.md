# Proposal: SmoothNAS Plugins — Runtime Integration (LXC2Docker)

**Status:** Pending
**Part of:** smoothnas-plugins (Step 2 of 9)
**Depends on:** plugins-01-foundation

---

## Problem

Phase 01 records a plugin in the database but does not run it. This
phase makes a plugin actually start: bundle the LXC2Docker daemon
into the SmoothNAS appliance, talk to it from tierd over a private
unix socket, and implement container lifecycle (create / start /
stop / remove) for both `oci-image` and `lxc-distro` artifact
shapes. Tier-bound volumes still wait for phase 03; bridge
networking and nginx still wait for phase 04. This phase brings
plugins to "running, with flat volumes, on whatever default network
LXC2Docker provides".

---

## Specification

### LXC2Docker packaging

LXC2Docker is built and shipped as a separate apt package
`smoothnas-runtime` from the SmoothNAS apt repo. The package:

- installs `/usr/lib/smoothnas/docker-lxc-daemon` (the binary);
- installs the systemd unit
  `/lib/systemd/system/smoothnas-runtime.service`;
- depends on `liblxc1`, `lxc-templates`, `skopeo`, `umoci`,
  `iptables`, `iproute2`;
- pre-creates `/run/smoothnas-runtime/` with mode 0750 owned by
  `root:smoothnas`.

The unit:

```ini
[Unit]
Description=SmoothNAS plugin runtime (LXC2Docker)
After=network.target
Before=tierd.service

[Service]
Type=notify
ExecStart=/usr/lib/smoothnas/docker-lxc-daemon \
    --socket=/run/smoothnas-runtime/docker.sock \
    --lxcpath=/var/lib/smoothnas/runtime/lxc \
    --statepath=/var/lib/smoothnas/runtime/state
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

`Before=tierd.service` ensures the socket exists before tierd
tries to talk to it. The `iso/hooks/packages.sh` installer hook
adds `smoothnas-runtime` to the appliance package list (built and
hosted by SmoothNAS CI; we do not depend on the GoW release
channel for the appliance build).

We pin a specific LXC2Docker upstream commit in the
`smoothnas-runtime` package and update via PR + CI smoke tests.
SmoothNAS-specific patches that must land for a given release go
upstream first; if upstream is slow, the package is built from a
SmoothNAS branch with the patches and a tracking issue against
GoW.

### Runtime client

`tierd/internal/plugin/runtime/` is a small Docker-Engine-API
client over the unix socket. It is not a full Docker SDK — it
implements only the endpoints tierd uses. Initial set:

```
POST   /containers/create
POST   /containers/{id}/start
POST   /containers/{id}/stop
DELETE /containers/{id}?v=1
GET    /containers/{id}/json
GET    /containers/{id}/logs?stdout=1&stderr=1&follow=1
GET    /events?filters=...
POST   /images/create?fromImage=...&tag=...     // OCI pulls
GET    /images/{name}/json
DELETE /images/{name}
POST   /_ping
GET    /info
```

Plus two endpoints that LXC2Docker exposes for distro templates
(its image resolver accepts the same `POST /images/create` for
`ubuntu:22.04`-style refs and routes them through the LXC
download template path internally — no separate API needed).

The client is hand-written because the official Docker SDK is
heavy and pulls in many dependencies tierd does not want.
~600 LoC including streaming response handling for `logs` and
`events`.

### Lifecycle

`tierd/internal/plugin/lifecycle.go` exposes:

```go
type Lifecycle interface {
    Materialise(ctx context.Context, name string) error  // pull image / template, create all instance containers
    Start(ctx context.Context, name string) error        // start all instances
    Stop(ctx context.Context, name string) error         // stop all instances
    Restart(ctx context.Context, name string) error
    Demolish(ctx context.Context, name string) error     // stop, remove all containers, drop image
    Status(ctx context.Context, name string) (Status, error)
}

type Status struct {
    State      string             // aggregate per phase 01: running | degraded | stopped | failed | ...
    Instances  []InstanceStatus
    LastChange time.Time
    LastError  string             // most recent failure across instances
}

type InstanceStatus struct {
    Instance     int
    State        string    // installed | pulling | creating | starting | running | stopped | failed
    ContainerID  string    // empty before Materialise
    BridgeIP     string    // populated by phase 04
    LastChange   time.Time
    LastError    string    // last failure reason, if any
}
```

All verbs operate across every row in `plugin_instances` for the
named plugin. A single-instance plugin has one row; a
multi-instance plugin has N. There is no separate "instance
lifecycle" interface — the plugin is the unit, instances are
parallel replicas.

`Materialise` is the new step inserted between phase 01's DB-only
install and `Start`. Image / template resolution happens **once**
per plugin (shared across instances); container creation happens
per instance.

Per artifact type:

- **`oci-image`:** call `POST /images/create?fromImage=<image>`,
  block on the streaming response, verify the resolved digest
  against `manifest.artifact.digest` if present, write the
  resolved full image ref (with `@sha256:...`) to
  `plugins.image_ref`. Then for each instance, `POST
  /containers/create` with a per-instance Docker create payload
  built from the manifest + `plugin_volume_paths` rows for that
  instance. Container name is `<plugin>-<instance>` (or just
  `<plugin>` for single-instance plugins).
- **`lxc-distro`:** call `POST /images/create?fromImage=<distro>:<release>`
  (e.g. `ubuntu:jammy`); LXC2Docker resolves to a distro template.
  If `packages` or `setup` are non-empty, `POST /containers/create`
  with the base image + a one-shot `Cmd` that runs apt/apk install
  followed by the setup script, start the container, wait for it
  to exit cleanly, then `POST /commit` to capture the resulting
  rootfs as a new template image
  `smoothnas-plugin-<name>:<version>`. The setup container runs
  exactly once regardless of `instance_count`. Finally for each
  instance, `POST /containers/create` against that committed
  template with the manifest's `container.command`.

The setup-script container is removed on success and on failure;
its logs are tee'd to the host journal under
`smoothnas-plugin-setup@<name>` for operator inspection.

Setup-script idempotency: tierd records the manifest version and
a hash of the `(packages, setup)` tuple in `plugins.updated_at`
metadata. On manifest re-install with the same hash, the existing
committed template is reused. On hash change, the old template is
removed and a fresh setup container is run.

Container ID and per-instance state land in
`plugin_instances.container_id` and `plugin_instances.state`.
The aggregate `plugins.state` is recomputed (per the table in
phase 01) on every per-instance state change.

### Container create payload

The Docker create payload tierd renders per instance includes:

- `Image`: resolved image ref
- `Cmd`: from `manifest.container.command`
- `WorkingDir`, `User`: from `manifest.container`
- `Env`: rendered from `plugin_config` rows (shared across
  instances)
- `HostConfig.Binds`: rendered by joining `plugin_volumes` with
  `plugin_volume_paths` filtered to the current instance — each
  entry is `<host_path>:<bind_path>`. Per-instance volumes
  contribute their per-instance host_path; shared volumes
  contribute their single host_path to every instance.
- `HostConfig.RestartPolicy`: from `manifest.container.restartPolicy`
- `HostConfig.Devices`: empty in this phase; phase 05 (profiles)
  populates GPU devices
- `Labels`: at minimum
  `io.smoothnas.plugin=<name>`,
  `io.smoothnas.plugin.version=<version>`,
  `io.smoothnas.plugin.instance=<n>`,
  `io.smoothnas.managed=true`

The label set is what tierd uses to discover its own containers
on startup — a `GET /containers/json?filters={"label":["io.smoothnas.managed=true"]}`
gives the full inventory, and the `instance` label routes each
container back to its `plugin_instances` row.

### Status reporting

`Status` reads container state via
`GET /containers/{id}/json` and maps:

| Docker `State` | tierd `state` |
|-----------------|---------------|
| `created`        | `installed` (briefly) |
| `running`        | `running` |
| `exited` (code 0) | `stopped` |
| `exited` (code !=0) | `failed` |
| `restarting`     | `starting` |
| `paused`         | `stopped` (treated as operator-initiated) |
| `dead`           | `failed` |

Plus, during `Materialise`: `pulling` and `creating` are
tierd-internal states streamed back over the API for UI progress.

### Event subscription

A long-running goroutine subscribes to
`GET /events?filters={"label":["io.smoothnas.managed=true"]}`
and updates the matching `plugin_instances.state` on
`start`/`die`/`oom`/`destroy`, then recomputes the aggregate
`plugins.state`. This keeps tierd's view consistent without
polling.

### Uninstall extension

Phase 01's stub uninstall now does, in order:

1. `Stop` every instance container (graceful, with 10s timeout
   each, in parallel).
2. `DELETE /containers/<id>?v=1&force=1` for each instance —
   removes container and anonymous volumes (we don't use
   anonymous volumes; named binds are untouched).
3. For `lxc-distro` plugins: `DELETE /images/smoothnas-plugin-<name>:<version>`
   — drops the committed setup template (one image regardless
   of instance count).
4. For `oci-image` plugins: `DELETE /images/<image_ref>` —
   drops the cached pull. (Future optimization: ref-count and
   only delete when no other plugin uses the same image.)
5. Delete flat-volume directories — both shared
   (`/var/lib/smoothnas/plugins/<name>/<volume>/`) and
   per-instance
   (`/var/lib/smoothnas/plugins/<name>/instance-<n>/<volume>/`).
6. Delete DB rows (cascades from `plugins` to all child
   tables).

Tier-bound volume deletion still no-ops here — phase 03 wires it
in. nginx + bridge teardown are added in phase 04.

If any step fails, uninstall returns the error and leaves state
as-is for operator inspection. The operator retries `uninstall`
after fixing the underlying issue.

### Reconciliation on tierd startup

On boot, tierd:

1. Pings the runtime daemon socket; waits up to 30s for
   `_ping` to succeed (handles the race during system start).
2. Lists containers with the `io.smoothnas.managed=true` label.
3. For each `plugin_instances` row with `container_id` populated:
   verify the container still exists and its state matches what
   we expect. If a container is gone but the row says `running`,
   set the instance state to `failed` with
   `last_error = "container missing on startup"` and recompute
   the aggregate `plugins.state`.
4. For each container with the `io.smoothnas.managed=true` label
   but no matching `plugin_instances` row: log a warning. These
   are ghosts (uninstall partially failed before); operator
   action required.

---

## Out of scope

- Tier-bound volume resolution (phase 03)
- Bridge network configuration (phase 04 — until then plugins
  use LXC2Docker's default bridge)
- nginx reverse-proxy (phase 04)
- Profile resolution including GPU device passthrough (phase 05)
- Any UI (phase 06)

---

## Acceptance

- `smoothnas-runtime` package builds in CI and installs cleanly
  on a fresh Debian 12 VM; `systemctl status smoothnas-runtime`
  shows the daemon active and listening on
  `/run/smoothnas-runtime/docker.sock`.
- `tierd-cli plugin install testdata/llama.yaml && tierd-cli plugin start llama-cpp`
  pulls the OCI image and runs the container; `tierd-cli plugin
  status llama-cpp` reports `running`; `tierd-cli plugin stop`
  cleanly stops it.
- `tierd-cli plugin install testdata/ubuntu-python.yaml &&
  tierd-cli plugin start py-app` runs the setup container,
  commits the template, and starts the final container; the
  committed template appears as
  `smoothnas-plugin-py-app:<version>` in
  `GET /images/json`.
- Killing the runtime daemon while a plugin is running and
  restarting it surfaces the container in tierd's reconciliation
  walk; tierd's view of state matches the runtime's after restart.
- Uninstall removes container + image (or committed template) +
  flat volumes; `GET /containers/json` shows no managed
  containers and `GET /images/json` shows no plugin images
  remaining.
