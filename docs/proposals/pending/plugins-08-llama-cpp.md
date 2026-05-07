# Proposal: SmoothNAS Plugins — llama.cpp Reference Plugin

**Status:** Pending
**Part of:** smoothnas-plugins (Step 8 of 9)
**Depends on:** plugins-07-iframe-embed

---

## Problem

Phases 01–07 deliver the plugin system, but nothing exercises it
end-to-end. This phase ships the first reference plugin: a
llama.cpp inference server that uses the GPU, stores models on a
named tier, and embeds its built-in HTTP UI inside the SmoothNAS
browser.

This phase also serves as the worked example for plugin authors:
the manifest, the published image policy, and the release flow are
the template anyone else uses to build a SmoothNAS plugin.

---

## Specification

### Repository

`RakuenSoftware/smoothnas-plugin-llama-cpp` — a small repo
containing only:

```
README.md
smoothnas-plugin.yaml
.github/workflows/release.yml
testdata/
```

No code. The image consumed is upstream `ghcr.io/ggml-org/llama.cpp:server-*`
(or the AMD/CUDA variant), unmodified.

### Manifest

```yaml
apiVersion: smoothnas.io/v1
kind: Plugin
metadata:
  name: llama-cpp
  version: 0.1.0
  description: llama.cpp HTTP inference server
  vendor: smoothnas
  homepage: https://github.com/RakuenSoftware/smoothnas-plugin-llama-cpp

artifact:
  type: oci-image
  image: ghcr.io/ggml-org/llama.cpp:server-cuda-b3500
  digest: sha256:...           # pinned at release time

container:
  command:
    - "./llama-server"
    - "--host"
    - "0.0.0.0"
    - "--port"
    - "8080"
    - "--model"
    - "${MODEL_PATH}"
    - "--n-gpu-layers"
    - "${N_GPU_LAYERS}"
  restartPolicy: unless-stopped

volumes:
  - name: models
    mode: tier-bound
    slot: NVME
    minSize: 50G
    bind: /models

ports:
  - name: http
    port: 8080
    protocol: tcp
    expose: true

ui:
  embed:
    path: /
    auth: bearer-injected

profiles:
  - gpu-nvidia               # operator changes to gpu-amd at install time if AMD
  - default-limits

config:
  - key: MODEL_PATH
    type: string
    default: /models/default.gguf
    description: Model file to load on start (path inside container)
  - key: N_GPU_LAYERS
    type: string
    default: "999"
    description: Number of model layers to offload to GPU (999 = all)
  - key: SMOOTHNAS_BEARER_EXPECTED
    type: string
    default: ""
    secret: true
    description: |
      Auto-populated to match the rotated bearer token. Do not edit
      manually; tierd's "rotate token" flow keeps this in sync.
```

The `SMOOTHNAS_BEARER_EXPECTED` config field is the contract for
the bearer-injected auth: a thin upstream-image wrapper script
(see "Image variants" below) checks the incoming
`Authorization: Bearer ...` header against this env var and 401s
on mismatch. tierd's rotate-token flow updates this config value
atomically with the nginx header.

### Image variants

llama.cpp upstream publishes images for multiple GPU vendors. The
manifest in this repo ships with `cuda` by default; for other
hardware we publish sibling manifests:

```
smoothnas-plugin.yaml          # CUDA (default; gpu-nvidia profile)
smoothnas-plugin-rocm.yaml     # AMD ROCm (gpu-amd profile, image: ...:server-rocm-...)
smoothnas-plugin-cpu.yaml      # CPU-only (no GPU profile, image: ...:server-...)
```

The install wizard (phase 06) picks one when installing from a
URL that points at the repo root (we add a small `index.json` to
the release listing so the UI can offer the variants).

### Auth wrapper

Bearer-injected auth needs the plugin to validate the token. The
upstream llama.cpp server has no built-in auth gate. We solve this
without forking upstream by wrapping the image at SmoothNAS install
time using LXC2Docker's `lxc-distro`-style overlay path? No — that
is build-time; we want runtime. Instead, the manifest's
`container.command` is replaced with a small entrypoint:

The manifest above is updated to:

```yaml
container:
  command:
    - "/bin/sh"
    - "-c"
    - |
      exec /usr/bin/socat \
        TCP-LISTEN:8080,reuseaddr,fork \
        EXEC:'/usr/local/bin/llama-auth-gate /usr/bin/llama-server-real'
```

— except this gets ugly and pulls socat as a dep. Cleaner: ship a
sidecar reverse-proxy *as a separate container in the same plugin
manifest* once `units[]` / multi-container support lands. v1
target: a single-container plugin, and the bearer check lives in
a thin wrapper image we publish:

```
ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:0.1.0-cuda
```

— a 5-line Dockerfile `FROM ghcr.io/ggml-org/llama.cpp:server-cuda-b3500`
+ a small Go binary that:

1. starts the upstream `llama-server` as a child process
2. listens on 8080, validates `Authorization: Bearer
   $SMOOTHNAS_BEARER_EXPECTED` on every request
3. forwards valid requests to the child on 127.0.0.1:8081
4. returns 401 on missing/wrong bearer

The wrapper repo is `RakuenSoftware/smoothnas-plugin-llama-cpp`;
its release CI builds and pushes the wrapper image. The manifest
distributed to operators points at the wrapper, not at upstream
directly.

This means the manifest's `artifact.image` is in fact:

```yaml
artifact:
  type: oci-image
  image: ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:0.1.0-cuda
  digest: sha256:...
```

— a 5MB layer on top of upstream. The README documents this
clearly so operators understand they're consuming a SmoothNAS
wrapper.

(For plugins that *do* support pluggable auth — e.g., Jellyfin,
Immich — the wrapper is unnecessary and the manifest references
upstream directly. llama.cpp is the awkward case.)

### Model staging

`/mnt/<tier>/.plugins/llama-cpp/models/` is empty after install.
The README walks operators through staging models:

```
sudo cp my-model.gguf /mnt/media/.plugins/llama-cpp/models/default.gguf
```

A future enhancement (out of v1 scope) is a "model browser" page
inside the SmoothNAS UI that downloads from Hugging Face. For
now: SCP, browse-via-SMB, or curl-from-inside-the-container via
`tierd-cli plugin exec llama-cpp -- curl -L ... -o ...`
(`exec` exposed via the runtime client; CLI verb added in this
phase).

### CI / release flow

The repo's `.github/workflows/release.yml` on tag push:

1. Builds the wrapper image for `cuda`, `rocm`, `cpu` variants.
2. Pushes to `ghcr.io/rakuensoftware/...`.
3. Resolves digests and writes them into the matching manifest
   files.
4. Creates a GitHub release with the three manifest files
   attached.
5. Updates `index.json` mapping variant → manifest URL.

A SmoothNAS-side CI smoke test (in the SmoothNAS repo, not this
plugin repo) runs nightly:

- Spins up a SmoothNAS dev VM,
- sideloads the latest llama-cpp manifest,
- starts it,
- POSTs a tiny prompt to `/plugins/llama-cpp/completion`,
- asserts a non-empty response within 60s,
- uninstalls.

This is the canary that proves a SmoothNAS release didn't break
the plugin contract.

---

## Out of scope

- Multi-container plugins (sidecar auth gate, queue worker, etc.).
  The wrapper-image workaround is the v1 answer.
- Model browser / Hugging Face integration.
- Model conversion / quantisation pipelines.
- A per-plugin metrics dashboard. The plugin's own UI shows
  inference stats; SmoothNAS doesn't aggregate them in v1.

---

## Acceptance

- The smoothnas-plugin-llama-cpp repo exists with the three
  manifest variants and a working release CI.
- Sideloading the CUDA manifest into a SmoothNAS dev VM with an
  NVIDIA GPU pulls the wrapper image, places models on the NVME
  slot of a chosen tier, exposes port 8080 through nginx, embeds
  the llama.cpp UI in `/plugins/llama-cpp`, and rejects requests
  without a valid bearer.
- The sibling AMD manifest works on a host with `gpu-amd`
  profile applied (different image variant, otherwise identical).
- The CPU manifest works on a host with no GPU.
- Uninstall removes the wrapper image, the model directory, the
  nginx route, and the DB rows. The model files are deleted
  along with everything else (per parent doc all-or-none policy);
  the README warns operators about this loudly.
- Nightly SmoothNAS smoke test passes.
