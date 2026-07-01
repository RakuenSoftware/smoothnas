# Authoring SmoothNAS plugins as docker-compose projects

SmoothNAS plugins can be **real docker-compose projects** (plugins-11): `tierd`
drives `docker compose` against LXC2Docker (a faithful Docker-engine drop-in)
instead of re-implementing an orchestrator. A compose plugin *is* a
`compose.yaml` with a top-level `name:`; its services, `depends_on`,
`healthcheck`, `profiles`, and `ports` are compose's, not a parallel schema.

Install/start/stop/uninstall/status all route through the compose backend; the
manifest (`smoothnas.io/v1 Plugin`) format continues to work unchanged.

## Minimal example

```yaml
name: my-plugin            # the compose project name (required)
services:
  web:
    image: ghcr.io/acme/web:1.2.3
    ports: ["8080:80"]     # host-port collisions are rejected across plugins
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost/"]
      interval: 10s
    depends_on:
      db: { condition: service_healthy }
  db:
    image: postgres:17
    healthcheck: { test: ["CMD", "pg_isready"], interval: 10s }
```

Install it (HTTP `POST /api/plugins` or the CLI) exactly like a manifest plugin.

## Tiered storage — `x-smoothnas` on a named volume

Compose doesn't model smoothfs storage-pool placement, so annotate a **named**
volume with `x-smoothnas`:

```yaml
services:
  db:
    image: postgres:17
    volumes: ["pgdata:/var/lib/postgresql/data"]
volumes:
  pgdata:
    x-smoothnas:
      tier: fast          # smoothfs pool/tier name
      minSize: 20G        # optional
```

At install, `tierd` resolves the tier to a host path and **rewrites the mount to
a bind** on that path (mechanism B — LXC2Docker's `local` volume driver ignores
`driver_opts device=`, but service bind mounts use the source path directly). The
placement is **pinned**: a later edit of `x-smoothnas.tier` is refused rather than
silently relocating data. Because the volume is a bind, `docker compose down`
never deletes the tiered data. Anonymous volumes can't be tiered — declare a
named volume.

## GPU / devices / cgroup — plain compose, no profile needed

LXC2Docker maps standard compose device syntax to LXC config, so the bespoke
profile catalog (`gpu-amd`, etc.) is **not** required for compose plugins:

```yaml
services:
  llm:
    image: ghcr.io/acme/llm:vulkan
    devices: ["/dev/dri:/dev/dri"]            # AMD/Intel: bind + auto cgroup-allow
    device_cgroup_rules: ["c 226:* rwm"]      # explicit cgroup allow, if you need it
    # NVIDIA (CDI, via nvidia-container-toolkit on the host):
    deploy:
      resources:
        reservations:
          devices:
            - { driver: nvidia, count: all, capabilities: [gpu] }
```

- `devices:` — LXC2Docker bind-mounts the node(s) and emits
  `lxc.cgroup2.devices.allow` for each (an AMD 7900 XTX enumerates
  `Vulkan0 : AMD Radeon Graphics (RADV GFX1100)` this way).
- `device_cgroup_rules:` — passed straight through as `lxc.cgroup2.devices.allow`.
- NVIDIA `deploy.resources.reservations.devices` — translated from the NVIDIA CDI
  spec (driver bind mounts + device nodes + cgroup allow).

## Conditional services & config

- **Conditional services** — use compose `profiles:` and activate with a
  profile; this is client-side, no engine or tierd support needed.
- **Config** — for now a compose plugin brings its own `environment:` /
  `env_file:`. Operator-config injection with secret handling is a follow-up.

## What tierd owns (vs compose)

`tierd` drives `up --pull never` / `down` / `ps` / `logs` and adds only the
SmoothNAS-specific concerns compose doesn't: tiered-volume bind resolution + pin,
the cross-project host-port guard, and treating `docker compose ps` as the source
of truth for state (cached, with a periodic reconcile sweep).
