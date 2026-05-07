# Proposal: SmoothNAS Plugin System

**Status:** Pending

---

## Problem

SmoothNAS is a complete storage appliance, but the box has spare CPU,
RAM, GPU, and tier-backed storage that operators reasonably want to
spend on co-located workloads — local LLM inference, GitHub Actions
runners, media transcoders, scrapers, sync agents. Today the only way
to run such workloads is to log into the host, install something via
apt or `curl | sh`, and hand-write systemd units. That path:

- bypasses the tier model (data lands wherever the operator
  remembers to point it),
- bypasses the firewall subsystem (ports get opened by hand),
- bypasses the UI (no status, no config, no logs view),
- leaves no clean uninstall (operator has to remember every file
  they touched), and
- gives third parties no way to ship a SmoothNAS-aware workload.

This proposal defines a first-class plugin system so additional
workloads install, configure, and uninstall through tierd the same way
the built-in storage subsystems do.

---

## Goals

1. A declarative manifest format (`smoothnas-plugin.yaml`) that fully
   describes a plugin's image, network, volumes, ports, config,
   hardware needs, and UI.
2. Real isolation per plugin — plugins run as LXC system containers,
   not bare host processes.
3. **Two equally first-class plugin shapes:**
   - **OCI-image plugins** that point at any Docker Hub / GHCR image
     and run inside an LXC, no custom packaging.
   - **LXC-distro plugins** that start from a base distro template
     (Ubuntu, Debian, Alpine) plus a package overlay and a setup
     script — a full system container with real init, real systemd
     inside, shaped however the plugin author wants.
   Both shapes surface to tierd, the UI, and the operator as the
   same kind of object: a managed container with the same lifecycle,
   the same volume bindings, the same networking, the same logs.
4. First-class integration with the named-tier model: plugin volumes
   may bind to a specific slot of a specific tier instance, so
   "models on the NVME slot of `media`" is a one-line manifest entry.
5. UI parity with built-in subsystems: install / start / stop /
   configure / uninstall / view logs / open the plugin's own UI from
   the SmoothNAS browser.
6. Hardware-level access where the manifest asks for it — GPU, raw
   block devices, host net. The appliance's hardware is the point.
7. Clean uninstall: removing a plugin removes its container, image
   cache, network endpoints, nginx route, firewall holes, **and**
   persistent volumes. A plugin is an all-or-none object.
8. Sideload first (manifest URL or local file). A curated registry
   is a v2 concern explicitly parked.
9. Incus-style depth — tierd owns the runtime, the network, the
   volume drivers, the profiles, and the image cache. The operator
   never reaches around tierd to talk to the runtime directly.

---

## Non-goals

- Running the upstream Docker daemon. The appliance does not install
  or depend on `docker.io`/`containerd`.
- Running rootless Podman. The runtime is LXC, not OCI-native.
- A `.deb`-based plugin path. Reserved as a future escape hatch only
  if a workload genuinely cannot be containerised.
- A micro-frontend extension API. Plugins surface UI through an
  iframe of their own HTTP server; tierd-ui does not load
  plugin-shipped JavaScript bundles.
- Multi-host plugins, plugin scheduling, or any clustering concept.
- A curated marketplace in v1. Manifest stability is a prerequisite
  for that and is what this series is here to prove.
- Tier-aware data placement *inside* a plugin's volume — once a
  plugin owns a volume, what it does with the bytes is its problem.

---

## Decision

Plugins are **LXC system containers** managed by the
[`LXC2Docker`](https://github.com/games-on-whales/LXC2Docker) daemon
(a Games-on-Whales project that speaks the Docker Engine API on a
unix socket and backs every "Docker container" with an LXC system
container). tierd talks to LXC2Docker over a private socket and
treats it the same way existing tierd code treats `lvm`, `mdadm`, or
`nginx` — an external binary with a stable interface.

This is the Incus-shaped approach: SmoothNAS owns the runtime, the
network, the volume drivers, the image cache, the profiles, and the
UI. Operators never `docker run` anything. The Docker API surface
is an implementation detail of how tierd talks to its own runtime,
not something a plugin author or operator interacts with.

### Two artifact shapes, one container model

LXC2Docker resolves three kinds of image refs:

1. **Distro template** (`ubuntu:22.04`, `debian:bookworm`, `alpine:3.19`)
   → straight to an LXC download-template container.
2. **Distro + package overlay** → distro template with apt/apk
   packages layered on, captured as a reusable template.
3. **Arbitrary OCI image** → pulled via `skopeo copy` into an OCI
   layout, flattened with `umoci unpack`, rootfs imported into an
   LXC container.

The SmoothNAS plugin manifest exposes both ends of this:

- `artifact.type: oci-image` for case 3 (and 1/2 if the operator
  prefers to consume a published image rather than build via
  distro+packages).
- `artifact.type: lxc-distro` for cases 1 and 2 — the manifest
  declares the base distro, release, optional package list, and
  optional setup commands. tierd asks LXC2Docker to materialise
  the template + overlay + run-once setup, and what comes out is
  an LXC container that behaves identically to one that started
  from an OCI image.

Crucially: **once the container exists, all operations go through
the same Docker API surface regardless of which artifact type built
it.** Volumes, networks, exec, logs, stats, events, lifecycle —
identical code paths. There is no "is this an LXC plugin or a
Docker plugin" branch in tierd above the install step. From the
operator's view, both are just plugins, and an LXC-distro plugin
gets every Docker-API capability (volumes, networks, exec, logs,
events, stats) the same way an OCI-image plugin does.

### Why LXC + LXC2Docker, not the alternatives

- **vs. Docker proper:** dockerd is a long-running root daemon with
  its own opinions about networks, storage, and lifecycle that
  overlap with the appliance. LXC2Docker is *our* daemon (in the
  GoW ecosystem the user already maintains), and the underlying
  runtime is LXC, which the kernel ships and Debian packages
  cleanly.
- **vs. Podman + quadlets:** Podman is rootless-OCI-native; LXC is
  system-containers-with-real-init. For appliance plugins (GPU
  passthrough, systemd inside, persistent state, long-lived
  services) LXC is the better primitive. Podman wins on the
  ephemeral-build-runner case; LXC wins everywhere else SmoothNAS
  cares about.
- **vs. systemd portable services:** loses the OCI image ecosystem.
  Custom packaging per plugin is a cost we don't need to pay now
  that LXC2Docker exists.
- **vs. raw LXC:** we'd reinvent OCI image pulls, Docker-API
  ergonomics, exec/logs streaming, and event subscriptions. All of
  that already exists in LXC2Docker.

### Honest costs

- LXC2Docker is itself a runtime daemon SmoothNAS now owns
  end-to-end. Its bugs become tierd bugs. Mitigation: contribute
  upstream rather than fork; tierd version-pins the LXC2Docker
  binary in its release channel.
- LXC has a steeper learning curve than Docker for operators who
  ssh into the host. Mitigation: tierd is the supported entry
  point; LXC under the hood is documented but not promoted.
- Image pulls via `skopeo` + `umoci` are slower than a real
  registry-aware engine. Mitigation: tierd-managed image cache;
  operator-visible "pulling…" state during install.

### Plugin manifest

```yaml
apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: llama-cpp                  # DNS-1123 label, globally unique on host
  version: 0.1.0                   # semver
  description: llama.cpp inference server
  vendor: smoothnas
  homepage: https://github.com/...

# --- One of two artifact shapes ---

artifact:
  type: oci-image                  # pull a published image
  image: ghcr.io/ggml-org/llama.cpp:server-b3500
  digest: sha256:ab12...           # optional but strongly recommended; pins the pull

# OR:

artifact:
  type: lxc-distro                 # build from a distro template
  distro: ubuntu                   # ubuntu | debian | alpine | ...
  release: jammy                   # release name as LXC2Docker resolves it
  arch: amd64                      # default: host arch
  packages:                        # optional apt/apk overlay
    - python3
    - python3-pip
  setup:                           # optional one-shot setup, run before first start
    - pip install --break-system-packages mything==1.2.3
    - useradd -m worker

container:                         # applies to both artifact types
  workingDir: /
  user: ""                         # default: image's USER (oci) or root (lxc-distro)
  restartPolicy: unless-stopped    # see "Lifecycle policy" below
  command: []                      # overrides image CMD; required for lxc-distro

instances:                         # optional; default: { count: 1, configurable: false }
  count: 1                         # how many container replicas to create
  configurable: false              # if true, operator may scale via UI / API

volumes:
  - name: models
    mode: tier-bound               # or "flat"
    slot: NVME                     # required when tier-bound
    minSize: 50G                   # advisory; tierd warns if slot smaller
    bind: /models                  # mountpoint inside the container
    perInstance: false             # if true and count > 1, one volume dir per instance

ports:
  - name: http
    port: 8080                     # container port; tierd never publishes to host
    protocol: tcp
    expose: true                   # punch firewall + nginx route to bridge IP

ui:
  embed:
    path: /                        # path on the plugin's HTTP server
    auth: bearer-injected          # or "none"

profiles:                          # see plugins-05-profiles
  - gpu-amd
  - default-limits

config:
  - key: MODEL_PATH
    type: string
    default: /models/default.gguf
    description: Default model file to load on start
    secret: false
```

A plugin author who knows the manifest can ship to SmoothNAS by
either (a) publishing a Docker image and pointing the manifest at
it, or (b) writing a 30-line manifest that builds from a distro
template. Both are first-class.

### Where plugin state lives

- **Runtime daemon socket:** `/run/smoothnas-runtime/docker.sock`
  (private to tierd; not `/var/run/docker.sock` so it doesn't
  collide if an operator independently installs Docker).
- **Image / template cache:** under LXC2Docker's `--lxcpath`, which
  tierd configures to `/var/lib/smoothnas/runtime/lxc`. Distro
  templates and OCI-derived rootfs templates are reused across
  pulls.
- **Container rootfs:** managed by LXC2Docker under
  `/var/lib/smoothnas/runtime/lxc/<container-id>/`.
- **Flat volumes:** `/var/lib/smoothnas/plugins/<name>/<volume>/`,
  bind-mounted into the container.
- **Tier-bound volumes:** under the resolved tier slot path,
  e.g. `/mnt/media/.plugins/<plugin-name>/<volume>/`. Path
  resolution is the job of phase 03.
- **Bridge network:** `smoothnas-plugins` bridge (managed by
  tierd via LXC2Docker), `10.66.0.0/16`, one stable IP per
  container.
- **Database:** new tables in tierd's SQLite (phase 01).

---

## Phasing

Each phase below is a separate proposal under
`docs/proposals/pending/plugins-NN-*.md`.

| # | Slug | Scope |
|---|------|-------|
| 01 | foundation | manifest schema (both artifact types) + parser + DB tables + `tierd-cli plugin` stubs |
| 02 | runtime-integration | bundle LXC2Docker, talk to its socket, container lifecycle for both artifact types |
| 03 | tier-bound-volumes | resolve and preflight tier-slot volumes |
| 04 | bridge-and-proxy | tierd-managed bridge net + nginx route to container bridge IP |
| 05 | profiles | Incus-style declarative bundles for GPU / limits / network |
| 06 | ui | Plugins page, install flow, config form, logs view |
| 07 | iframe-embed | `/plugins/<name>` route + bearer-injection auth |
| 08 | llama-cpp | reference plugin: OCI image, GPU profile, tier-bound model volume |
| 09 | gh-runner | reference plugin: OCI image, multi-instance, registration tokens |

A phase 10 (curated registry) is **explicitly parked** until the
manifest format has stabilised through both reference plugins.

A future phase exercising the `lxc-distro` artifact shape end-to-end
with a worked example (likely an Ubuntu+Python custom-app plugin) is
out of scope for v1, but the manifest schema and runtime path
support it from phase 02 onward.

---

## Cross-cutting policies

These are decided at the parent level so individual phases can lean
on them without re-litigating.

### GPU passthrough

Plugins requesting GPU access via the `gpu-amd` / `gpu-nvidia` /
`gpu-intel` profiles get hardware-level access: tierd configures
the container's LXC config to bind `/dev/dri/*` and grant the
relevant cgroup device permissions, gated only on the device
existing on the host. Sharing among plugins is the operator's
problem; the appliance gives full access because that is the point
of running the workload here in the first place.

NVIDIA passthrough additionally binds `/dev/nvidia*`,
`/dev/nvidia-uvm*`, and `/dev/nvidiactl`. The
`nvidia-container-toolkit` device-discovery dance is **not** used;
SmoothNAS hosts have known hardware and tierd queries it directly.

### Logs

tierd does not manage plugin log retention. Container stdout/stderr
is captured by LXC2Docker (Docker `/containers/<id>/logs` API) and
streamed to the host journal via the runtime daemon's journald
output. Operators control retention through the existing journald
config. The UI's logs view (phase 06) is an SSE stream of that
endpoint, nothing more.

### Uninstall

A plugin is one object. `DELETE /api/plugins/<name>` always:

1. stops every container declared by the manifest
2. removes containers (Docker API `DELETE /containers/<id>?v=1`)
3. removes generated nginx route + reloads nginx
4. closes firewall holes for declared ports
5. detaches network endpoints and frees the bridge IP
6. deletes flat **and** tier-bound persistent volumes
7. removes the cached image / template from LXC2Docker's store
8. deletes DB rows

There is no `?purge=false` flag. A plugin that the operator wants
to keep data for is a plugin they should not be uninstalling.

### Networking

Plugins are attached to a tierd-managed `smoothnas-plugins` bridge
with a stable per-container IP. tierd does **not** publish ports
to the host (the LXC2Docker `-p` / nftables-DNAT path is unused);
nginx instead reverse-proxies `/plugins/<name>/` and the
`/plugins/<name>` iframe route directly to the container's bridge
IP and container port. This eliminates host port collisions
between plugins entirely.

The exception: a plugin manifest may declare
`ports[].hostExpose: true` to opt into a direct host-bound listener
(intended for cases like GitHub runner egress or LAN-discoverable
services that don't go through the SmoothNAS UI). In that case
tierd asks LXC2Docker to publish the port and opens the host
firewall hole. v1 ships without this; reserved for when phase 09
demands it.

### Lifecycle policy

Plugin containers run with `restart: unless-stopped` semantics by
default (LXC2Docker maps this to LXC's `lxc.start.auto = 1` plus a
restart watcher). An operator-issued stop is sticky across reboots;
a crash triggers restart with exponential backoff capped at 60s.

### Multiple instances

Every plugin has at least one container instance. Plugins that
declare `instances.count > 1` get N containers named
`<plugin>-<n>` for n in 1..N, all from the same manifest, sharing
non-`perInstance` volumes and config. Single-instance plugins are
the default and require no `instances:` block.

Volumes with `perInstance: true` are duplicated per instance:
each gets its own host path suffixed with `/instance-<n>/`. Volumes
without it (the default) are shared across all instances. Config
is always shared across instances; per-instance config is out of
scope for v1.

Operator-side scaling is gated on `instances.configurable: true`.
The aggregate plugin state is `running` if all instances are
running, `degraded` if some are failed, `stopped` if all are
stopped, `failed` if all failed.

### Security model

Plugins are trusted code installed by the operator. tierd verifies
the manifest's `digest` (when present, OCI plugins only) matches
the pulled image's digest and refuses on mismatch — that is the
only gate. LXC-distro plugins inherit the trust of their distro
template registry; tierd does not pin or verify those independently.
Plugins run as LXC system containers with their image's default
user; manifests may override via `container.user`. The appliance
is single-tenant; defense against malicious plugins is out of scope.

---

## Open questions deferred to phase docs

- Exact SQLite schema (phase 01).
- LXC2Docker version pinning, packaging, and upstream extension
  policy (phase 02).
- Setup-script idempotency and re-run rules for `lxc-distro`
  plugins on manifest version bump (phase 02).
- Tier-removal behaviour when a plugin volume binds to that tier
  (phase 03).
- Bridge subnet collision avoidance with operator-defined networks
  (phase 04).
- Profile precedence and merge semantics (phase 05).
- nginx auth-bearer injection mechanism — header injection vs.
  signed cookie (phase 07).
