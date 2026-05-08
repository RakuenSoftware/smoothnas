# Proposal: SmoothNAS Plugins — GitHub Actions Runner Reference Plugin

**Status:** Pending
**Part of:** smoothnas-plugins (Step 9 of 9)
**Depends on:** plugins-07-iframe-embed

---

## Problem

The second reference plugin: GitHub Actions self-hosted runners.
This exercises three things llama.cpp doesn't:

1. **Multiple instances** of the same plugin — operators commonly
   run several runners in parallel for queue throughput.
2. **No SmoothNAS UI to embed.** The plugin's "UI" is GitHub.com.
   Tests the path where `ui.embed` is omitted entirely.
3. **Outbound-network-only workload.** Tests the
   `host_expose: false` baseline with no need for inbound nginx
   routing — runners just need NAT egress, which the bridge
   network already provides.

Plus it surfaces a gap in the v1 design: plugins that genuinely
need multiple instances. This phase adds the small primitive that
covers it without dragging in Kubernetes-flavoured replicaset
machinery.

---

## Specification

### Repository

`RakuenSoftware/smoothnas-plugin-gh-runner` — same shape as the
llama-cpp repo: README, manifest, release CI.

### Manifest

```yaml
apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: gh-runner
  version: 0.1.0
  description: GitHub Actions self-hosted runner
  vendor: smoothnas
  homepage: https://github.com/RakuenSoftware/smoothnas-plugin-gh-runner

artifact:
  type: oci-image
  image: ghcr.io/rakuensoftware/smoothnas-plugin-gh-runner:0.1.0
  digest: sha256:...

container:
  command: ["/entrypoint.sh"]
  restartPolicy: unless-stopped

instances:                       # NEW in this phase; see "Multiple instances"
  count: 2
  configurable: true             # operator may change at install / via UI

volumes:
  - name: workspace
    mode: tier-bound
    slot: SSD                    # workspaces churn — fast tier, not the fastest
    minSize: 100G
    bind: /home/runner/_work
    perInstance: true            # one workspace per instance, not shared

profiles:
  - default-limits
  # No GPU profile by default — operator adds gpu-* if their builds need it.

config:
  - key: GH_REPO_URL
    type: string
    description: Full repo URL (e.g. https://github.com/owner/repo) or org URL
  - key: GH_RUNNER_TOKEN
    type: string
    secret: true
    description: Registration token from the repo/org runner settings
  - key: GH_RUNNER_LABELS
    type: string
    default: "self-hosted,linux,x64,smoothnas"
    description: Comma-separated runner labels
  - key: GH_RUNNER_GROUP
    type: string
    default: "default"
    description: Runner group name

# No `ports`: runner is outbound-only.
# No `ui`: GitHub is the UI.
```

### Multiple instances

The `instances` block, the `plugin_instances` table, the
`plugin_volume_paths` table, and `volumes[].perInstance` are all
already in the system as of phase 01 and exercised end-to-end
since phase 02 (with default `count: 1`). This phase is the first
plugin where `count > 1` matters in practice.

The gh-runner manifest declares:

```yaml
instances:
  count: 2
  configurable: true
volumes:
  - name: workspace
    perInstance: true
    ...
```

…and the existing code paths produce two containers
(`gh-runner-1`, `gh-runner-2`), each with its own workspace
directory under
`/mnt/<tier>/.plugins/gh-runner/instance-<n>/workspace/`.

What this phase newly introduces:

- **Operator-side scaling.** Two new endpoints in
  `tierd/internal/api/plugins.go`:
  - `GET  /api/plugins/<name>/instances` — list per-instance
    state (already-existing data; new endpoint surfacing it).
  - `POST /api/plugins/<name>/instances` (`{count: N}`) —
    accepted only when `instances.configurable: true`; scales
    up by creating the additional `plugin_instances` and
    `plugin_volume_paths` rows then materialising the new
    containers, or scales down by stopping + removing the
    top-numbered instances and deleting their per-instance
    volume directories. All-or-none: a partial scale failure
    rolls back to the prior count.
- **Instances UI tab.** Phase 06's detail page gains an
  "Instances" tab when `instances.count > 1` or
  `instances.configurable: true`. Shows per-instance status pills
  and, for configurable plugins, a scale slider that POSTs to the
  endpoint above with a confirm dialog enumerating which
  instances will be removed.

### Auth wrapper

The wrapper image is the runner-registration shim. Its
`/entrypoint.sh`:

1. Uses `GH_RUNNER_TOKEN` to call GitHub's API and fetch a
   registration token (if the provided token is a PAT) or uses
   it directly (if it's already a runner registration token).
2. Runs `./config.sh --url $GH_REPO_URL --token $REG_TOKEN
   --labels $GH_RUNNER_LABELS --runnergroup $GH_RUNNER_GROUP
   --name "smoothnas-${HOSTNAME}" --unattended --replace`.
3. Traps SIGTERM to call `./config.sh remove --token ...`
   before exit (so an uninstall deregisters the runner from
   GitHub cleanly).
4. Execs `./run.sh`.

The wrapper image lives in the gh-runner plugin repo and is
built/published the same way as llama.cpp's.

### Token rotation

GitHub registration tokens expire (currently ~1 hour). The
wrapper handles this on container start; a long-running runner
that re-registers (e.g. after restart) needs a fresh token.

For PATs (which are long-lived), the operator pastes the PAT in
config and the wrapper handles re-registration each start. For
direct registration tokens (short-lived), the operator must
re-paste before each restart. The README documents both modes;
we recommend PAT for set-and-forget operation.

A future enhancement (out of v1 scope): a SmoothNAS-side OAuth
flow with GitHub that issues short-lived tokens on demand. v1
keeps this manual.

### CI / release flow

Same shape as llama-cpp: tag push → build wrapper image →
resolve digest → publish release with manifest → SmoothNAS-side
nightly smoke test. The smoke test:

- installs the gh-runner manifest with `count: 1`;
- supplies a test PAT (stored in CI secrets) for a private test
  repo;
- starts the plugin;
- triggers a workflow run on the test repo with a one-line job;
- asserts the runner picks up and completes the job within 5
  minutes;
- uninstalls (which deregisters via the SIGTERM trap).

### `host_expose` is unused here

The runner is outbound-only — it polls GitHub's API and pulls
job assignments, then makes its own outbound network calls for
the work. No inbound listener. `ports` is empty,
`host_expose` is moot. This is the v1 case the
`host_expose: true` field was *reserved* for in the parent doc;
gh-runner doesn't actually need it, so the field stays unused
in v1 and the implementation is deferred.

---

## Out of scope

- A SmoothNAS-side OAuth flow with GitHub.
- Per-instance config (different repos per runner).
- Auto-scaling based on queue depth.
- Cleanup of stale GitHub-side runner registrations (the
  SIGTERM trap handles graceful removal; orphaned entries from
  ungraceful exits are operator-cleaned in GitHub UI).

---

## Acceptance

- The smoothnas-plugin-gh-runner repo exists with manifest and
  release CI.
- Sideloading the manifest with `count: 2` creates two runner
  containers, each with its own workspace under
  `/mnt/<tier>/.plugins/gh-runner/instance-N/workspace/`.
- Both runners register with GitHub and appear in the repo's
  runner list with `smoothnas` in their labels.
- A workflow targeting the `smoothnas` label is picked up by
  one of the runners and completes.
- `POST /api/plugins/gh-runner/instances {count: 4}` scales up
  to four runners; `count: 1` scales back down, deregistering
  the removed runners cleanly.
- Uninstall stops all instances, deregisters them via SIGTERM,
  removes containers, deletes per-instance workspace
  directories, and removes the wrapper image.
- Nightly smoke test passes.
