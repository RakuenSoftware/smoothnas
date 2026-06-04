# Proposal: SmoothNAS Plugins — Compose-style Multi-Service Plugins

## Problem

A SmoothNAS plugin is single-image: one OCI image fanned out to N identical
replica *instances*. Every plugin table (`plugin_instances`, `plugin_ports`,
`plugin_volumes`, `plugin_config`) hangs off `plugins(name)` with no notion of
distinct co-operating containers.

Real-world plugins are not single images. The motivating case is aimee, whose
upstream ships as a docker-compose stack: `aimee-kb` (or the combined
server+kb) plus a `postgres` (pgvector) service and an `embedder` service, wired
together over a network. Because a plugin can only run one image, the published
aimee manifests punt Postgres and the embedder to "external infra" — the
operator must stand them up by hand and paste in `AIMEE_DB2_URL` /
`AIMEE_EMBEDDER_URL`. The manifests advertise this as *"Requires an external
Postgres (pgvector) database and embedder service"*, which is precisely the
friction we want to remove.

The requirement: **a single plugin owns a *set* of containers**, started,
stopped, health-gated, reconciled, and uninstalled as one managed surface — the
docker-compose model, but inside SmoothNAS's existing volume/port/tier/config
machinery rather than alongside it.

## Decisions (locked)

- **Manifest:** `services:` is **required**. The single-image top-level shape is
  retired; every existing manifest (testdata fixtures + the published aimee
  manifests) is migrated to the new form. One internal code path, no
  dual-shape branching.
- **Storage:** a new **`plugin_services`** table is the per-service anchor;
  `plugin_instances` / `plugin_ports` / `plugin_volumes` / `plugin_config` gain
  a `service` dimension referencing `(plugin_name, service)`.

## Specification

### Manifest schema

`metadata`, `instances`, and `profiles` stay plugin-level. Everything that was
per-image moves under a named service. `artifact`, `container`, `volumes`,
`ports`, and `config` become **per-service**.

```yaml
apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: aimee-kb
  version: 0.2.0
  description: >-
    aimee knowledge base — self-contained: bundles its pgvector Postgres and
    embedder. Serves the /v1 HTTP API with shared, vector-backed memory.
  vendor: RakuenSoftware
services:
  - name: postgres
    artifact: { type: oci-image, image: pgvector/pgvector:pg16 }
    env:
      POSTGRES_DB: aimee_shared
      POSTGRES_USER: aimee
      POSTGRES_PASSWORD: aimee        # see "Secrets" below
    volumes:
      - { name: pgdata, mode: flat, bind: /var/lib/postgresql/data }
    health:
      test: ["CMD-SHELL", "pg_isready -U aimee -d aimee_shared"]
      interval: 10s
      timeout: 5s
      retries: 5

  - name: embedder
    artifact: { type: oci-image, image: ghcr.io/rakuensoftware/aimee-embedder:latest }
    health:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      startPeriod: 30s

  - name: kb
    artifact: { type: oci-image, image: ghcr.io/rakuensoftware/aimee-kb:latest }
    dependsOn:
      postgres: { condition: service_healthy }
      embedder: { condition: service_healthy }
    env:
      AIMEE_DB2_URL: postgresql://aimee:aimee@{{service.postgres.host}}:5432/aimee_shared
      AIMEE_EMBEDDER_URL: http://{{service.embedder.host}}:8080
    ports:
      - { name: api, port: 8741, protocol: tcp, hostExpose: true }
    volumes:
      - { name: home, mode: flat, bind: /var/lib/aimee }
```

New / changed types in `plugin/manifest.go`:

- `Manifest.Services []Service` — **required, non-empty**. `Manifest.Artifact`,
  `.Container`, `.Volumes`, `.Ports`, `.Config` are removed from the top level.
- `Service`:
  - `Name` — DNS-label, unique within the plugin.
  - `Artifact` — the existing tagged union (oci-image / lxc-distro), unchanged.
  - `Container` — existing per-container knobs (command, user, restartPolicy,
    resources).
  - `Env map[string]string` — service environment, **after** `{{...}}`
    interpolation (see *Service discovery*).
  - `Volumes []Volume`, `Ports []Port`, `Config []ConfigField` — as today but
    scoped to the service.
  - `DependsOn map[string]DependsCondition` — keys are sibling service names;
    `condition ∈ {service_started, service_healthy}`. Must be acyclic.
  - `Health *Healthcheck` — `test`, `interval`, `timeout`, `retries`,
    `startPeriod` (mirrors compose / the LXC2Docker healthcheck surface).

Validation (`ValidateManifest`) additions:
- `services` non-empty; service names unique, DNS-label-safe.
- `dependsOn` references only sibling services; the graph is acyclic
  (topological sort must succeed) — reject cycles with the offending edge.
- Port/volume name uniqueness is scoped **per service**; host-published ports
  (`hostExpose: true`) must not collide **across services** of the same plugin.
- `service_healthy` dependencies require the target to declare `health`.

### SQLite tables

New migration `00019_plugin_services.sql`.

```sql
CREATE TABLE plugin_services (
    plugin_name   TEXT NOT NULL REFERENCES plugins(name) ON DELETE CASCADE,
    service       TEXT NOT NULL,
    artifact_type TEXT NOT NULL,
    image_ref     TEXT,
    distro_summary TEXT,
    depends_on    TEXT,           -- JSON: {"postgres":"service_healthy",...}
    health        TEXT,           -- JSON: serialized Healthcheck, nullable
    ordinal       INTEGER NOT NULL, -- topological start order, precomputed
    PRIMARY KEY (plugin_name, service)
);
```

`plugin_instances`, `plugin_ports`, `plugin_volumes`, `plugin_volume_paths`,
`plugin_config` each gain a `service TEXT NOT NULL` column folded into their
primary key and their FK now targets `(plugin_name, service)` on
`plugin_services`. `plugins.image_ref` / `plugins.artifact_type` become
nullable/legacy — the authoritative artifact data moves to `plugin_services`.

A plugin's run unit is now `(plugin_name, service, instance)`. Replica fan-out
(`instances`) still applies, per service.

### Service discovery (no embedded DNS)

LXC2Docker's `CreateContainerRequest` exposes only `Env` and `NetworkMode` —
**no network aliases, hostnames, or DNS** — so compose's name-based discovery is
not available. All services share the runtime `veth` bridge (`10.100.0.0/24`),
and each container's bridge IP is already discovered post-start
(`plugin_instances.bridge_ip`, `InspectContainerBridgeIP`).

Discovery is therefore **bridge-IP injection**:

1. Start services in `ordinal` order (topological).
2. After a service's container is up (and healthy, if a dependent needs
   `service_healthy`), record its `bridge_ip`.
3. Before starting a dependent, render `{{service.<name>.host}}` /
   `{{service.<name>.port.<portName>}}` tokens in its `Env` against the
   recorded IPs/ports of already-started siblings.

`dependsOn` ordering is what guarantees a dependency's IP exists before a
dependent renders. This reuses the plugins-04 bridge-IP machinery; no new
network primitives, no per-plugin networks.

> Open item for the implementer: as an alternative to env tokens we can add
> `ExtraHosts []string` to `CreateContainerRequest` and write
> `<service>:<ip>` entries so unmodified `postgres`/`embedder` hostnames resolve.
> Prefer this **if** LXC2Docker honours ExtraHosts — verify against the runtime
> before committing. Env-token interpolation is the fallback that needs nothing
> from the runtime.

### Lifecycle (single managed surface)

The plugin is one unit; operations act on the whole set.

- **Create/Start:** walk services in `ordinal` order. For each, create + start
  every replica instance, then — if any dependent requires it — wait for the
  service's healthcheck to pass (bounded timeout → plugin `failed`). Inject
  discovery env into dependents as they come up.
- **Stop:** reverse `ordinal` order.
- **Demolish/Uninstall:** stop + remove every `(service, instance)` container,
  drop per-service images, then existing volume/port/config teardown.
- **Restart of one service:** allowed, but re-render discovery env for
  downstream dependents whose upstream IP may have changed.

### Aggregate state

`plugins.state` aggregates across **all** `(service, instance)` rows:
- all running → `running`
- some running, some failed/stopped → `degraded` (reuse `StateDegraded`)
- none running, ≥1 failed → `failed`

`plugin_instances.state` remains per-(service,instance); the plugin row rolls
them up. The reconciler walks `plugin_services × plugin_instances` and maps
daemon container states back, with the same ghost-container logging as today.

### API / UI surface

The plugin detail response gains a `services[]` array (name, image, per-service
instance states, health, host-published ports, dependsOn). Install/start/stop/
uninstall remain single plugin-level verbs. The UI renders one plugin card with
a service breakdown — not N plugins.

## Migration of existing manifests

Breaking change → every `kind: Plugin` manifest is rewritten to `services:`:
- Testdata fixtures: `gh-runner.yaml`, `llama.yaml`, `ubuntu-python.yaml`,
  `wolf.yaml` → trivially wrapped as a single-service plugin.
- aimee fixtures (`aimee-server.yaml`, `aimee-kb.yaml`, `aimee-combined.yaml`)
  **and the published RakuenSoftware/smoothnas-plugin-aimee manifests** →
  rewritten to bundle `postgres` + `embedder` as services, dropping the
  "external Postgres / embedder required" language. This is the original ask
  that motivated the feature.

## Out of scope

- Per-plugin Docker networks / network isolation between services (single shared
  `veth` bridge is retained).
- Compose `profiles:`, `extends`, `configs`, `secrets` files, multiple compose
  files. (Plugin-level secrets continue via the existing secret config path.)
- Importing/running a raw user-supplied `docker-compose.yaml` verbatim.
- Cross-plugin service references.

## Acceptance

- A plugin manifest with `services: [postgres, embedder, kb]` installs and runs
  as one plugin; `kb` reaches `postgres`/`embedder` over the bridge with no
  operator-supplied URLs.
- Start order honours `dependsOn`; `kb` does not start until `postgres` and
  `embedder` are healthy; a dependency that never goes healthy fails the plugin
  within the bounded timeout.
- Aggregate state reports `degraded` when one service is down and the rest run.
- Uninstall removes every service container, image, and volume.
- All migrated single-service manifests (gh-runner, llama, ubuntu-python, wolf,
  aimee-server) behave exactly as before.
- The aimee catalog validate/live tests
  (`plugins_catalog_aimee_*_test.go`) pass against the rewritten manifests.
```
