# Proposal: SmoothNAS Plugins — deploy via real docker-compose (not a tierd re-implementation)

**Status:** Pending
**Part of:** smoothnas-plugins
**Supersedes / reconsiders:** `plugins-10-compose-services.md`

---

## Problem

`plugins-10-compose-services.md` answers "real plugins are multi-container" by
**re-implementing docker-compose inside tierd**: a `services:` schema, a
`plugin_services` table, per-service ports/volumes/config/instances, dependency
ordering, health-gated startup, reconcile/teardown — and the follow-ons it
implies (conditional/profile-gated services, `depends_on` conditions, etc.). That
is a large, ongoing re-build of an orchestrator that already exists and is
battle-tested: **docker-compose**.

LXC2Docker is, by design, a faithful Docker-engine drop-in (`docker.sock`
replacement). Real `docker-compose` is a *client* that talks the engine API and
does all the orchestration (service graph, `depends_on`, health gating,
**profiles**, `--scale`) **client-side**. So the orchestration tierd is
re-implementing is available for free — *if* SmoothNAS just uses compose against
LXC2Docker.

## Evidence (validated, 2026-06-25)

Against the live LXC2Docker on `.254`:

- `docker compose up/down` works end-to-end (creates the bridge network,
  builds/pulls images, creates + health-gates + starts containers).
- **Profiles** gate services correctly (`--profile X`) — the conditional-service
  feature, **client-side, zero engine support** (so neither LXC2Docker nor tierd
  needs to implement it).
- **GPU passthrough** works via compose `devices: ["/dev/dri:/dev/dri"]` — a
  container enumerated `Vulkan0 : AMD Radeon Graphics (RADV GFX1100)`.
- The **entire aimee split stack** (postgres + GPU `aimee-llm` + `aimee-kb` +
  `aimee-server`) now runs on `.254` as a docker-compose project against
  LXC2Docker, **migrated off the tierd plugins** — health-gated deps, `/dev/dri`
  GPU, webchat, all green. (`aimee/deploy/compose/aimee.yaml` + `.gpu.yaml`.)

Two LXC2Docker fidelity items surfaced (engine-level, the right layer): the
Docker-Hub library-image → OCI fix (games-on-whales/LXC2Docker#94), and
operational notes (re-`umoci unpack` of an already-cached image stalls `up` — use
`--pull never`; container-list eventual consistency right after start). These are
engine/ops bugs to fix, not reasons to re-implement orchestration.

## Proposal

**SmoothNAS orchestrates plugins by driving real `docker compose` against
LXC2Docker, and layers its value-adds around compose rather than re-implementing
the orchestrator.**

- **Plugin format = a compose project.** A plugin ships (or its manifest compiles
  1:1 to) a `compose.yaml`. `services`, `depends_on`, `healthcheck`, `profiles`,
  `devices`, `ports`, `volumes` are compose's, not a parallel tierd schema.
- **tierd's job shrinks to**: install/start/stop/uninstall = `docker compose
  up -d --pull never` / `down` / `ps` / `logs`; plus the genuinely
  SmoothNAS-specific concerns that compose does *not* own:
  - **Tiered volumes** (smoothfs slots): a compose volume `driver`/driver-opts
    (a smoothfs volume plugin) or a resolved bind path injected at deploy — the
    one piece worth keeping from tierd's volume machinery.
  - **Host-port conflict guard** (smoothnas#380): keep it as a pre-`up`
    validation across installed projects.
  - **Profiles catalog** (`gpu-amd`, `default-limits`, `aimee-stack`): express as
    reusable compose fragments / `x-` extensions / override files merged at
    deploy, instead of a bespoke profile system. (`gpu-amd` ≈ `devices:
    /dev/dri` + cgroup rules.)
  - **Config injection**: operator config → an `.env` / `env_file` for the
    project (replaces `plugin_config` env templating).
  - **UI**: read `docker compose ps`/`logs` (compose writes the standard
    `com.docker.compose.*` labels LXC2Docker already supports for Portainer).

## Why this over plugins-10

- **Less bespoke code, less drift.** No `plugin_services` table, no re-built
  dependency/health/profile/scale engine to maintain and debug.
- **Faithful Docker.** Operators (and existing tooling: compose, Portainer) work
  unmodified — the stated LXC2Docker goal.
- **Conditional services / profiles come free** (the exact ask that triggered
  this), with zero engine or tierd work.
- **Upstream stacks deploy as-is.** aimee already ships compose files; "requires
  external Postgres/embedder" friction disappears without a manifest re-spec.

## Phases (proposed)

1. **Compose backend in tierd** — install/start/stop/uninstall a plugin by
   invoking compose against LXC2Docker; smoothnas owns only validation +
   config/.env + the host-port guard. Run the aimee stack through it (already
   proven by hand on `.254`).
2. **Tiered-volume integration** — smoothfs volume driver (or bind resolution)
   so compose volumes land on the right tier.
3. **Profiles-as-fragments** — `gpu-amd`/limits as compose override fragments;
   retire the bespoke profile catalog.
4. **UI over compose** — ps/logs/stop/start through the compose project.
5. **Migrate manifests** — publish plugins as compose projects (or a thin
   manifest→compose compiler); deprecate the parallel `services:` schema from
   plugins-10.

## Open questions / risks

- **Tiered volumes** are the one thing compose doesn't model natively — needs a
  smoothfs volume driver or a deploy-time bind-resolution shim (the main design
  work).
- **GPU / privileged fragments**: confirm cgroup device rules (not just the
  device node) are expressed cleanly in compose for every accelerator.
- **LXC2Docker engine bugs** (Hub-image#94; re-unpack stalls; list eventual
  consistency) must be fixed for a smooth `compose up` — but they're engine
  fixes, aligned with "copy Docker faithfully."
- **Lifecycle bookkeeping**: tierd still records what's installed; it now tracks
  compose projects rather than synthesizing the run itself.
