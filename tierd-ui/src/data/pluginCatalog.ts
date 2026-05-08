// Built-in plugin catalog displayed by the install wizard's source
// step. Each entry pairs a friendly card with a full smoothnas-plugin
// manifest the wizard hands to the parse → preview → install pipeline,
// so the operator path becomes "click a tile, hit Continue" for the
// plugins SmoothNAS ships canonical manifests for.
//
// Manifests are embedded verbatim (Vite bundles them into the UI
// asset) instead of fetched at runtime: avoids a CORS proxy, works
// without external connectivity, and keeps the catalog deterministic
// across appliance versions. The trade-off is that bumping a plugin's
// manifest in its own repo doesn't roll out to existing appliances
// until they update tierd-ui — same release cadence as everything
// else in the UI.
//
// Adding a plugin: drop a new entry below and import the manifest
// string. Hardware-specific variants (CPU vs CUDA vs Vulkan for
// llama.cpp) are separate entries, because picking the right binary
// for the host's GPU is exactly the choice the wizard is supposed
// to surface.

export type CatalogEntry = {
  id: string;            // unique within the catalog; not surfaced
  name: string;          // headline shown on the card
  vendor: string;        // small label below the name
  description: string;   // 1-2 line summary, plain text
  homepage?: string;     // GitHub or vendor link
  tags?: string[];       // optional hardware/use chips ("AMD Vulkan", "NVIDIA CUDA", ...)
  manifestYaml: string;  // verbatim smoothnas-plugin.yaml content
};

const ghRunnerManifest = `apiVersion: smoothnas.io/v1
kind: Plugin

metadata:
  name: gh-runner
  version: 0.1.0
  description: |
    GitHub Actions self-hosted runner. Each instance registers itself
    with the configured repo or org on container start and deregisters
    cleanly on stop (SIGTERM trap calls config.sh remove). Outbound
    network only: no ports exposed, no SmoothNAS UI to embed —
    GitHub.com is the UI.
  vendor: smoothnas
  homepage: https://github.com/RakuenSoftware/smoothnas-plugin-gh-runner

artifact:
  type: oci-image
  image: ghcr.io/rakuensoftware/smoothnas-plugin-gh-runner:0.1.0

container:
  command: []
  restartPolicy: unless-stopped

instances:
  count: 2
  configurable: true

volumes:
  - name: workspace
    mode: tier-bound
    slot: SSD
    minSize: 100G
    bind: /home/runner/_work
    perInstance: true

profiles:
  - default-limits

config:
  - key: GH_REPO_URL
    type: string
    description: |
      Repository or organisation URL the runner registers against.
      Examples:
        https://github.com/owner/repo
        https://github.com/my-org
  - key: GH_RUNNER_TOKEN
    type: string
    secret: true
    description: |
      Either a personal access token (recommended; token rotates
      registration tokens automatically across restarts) or a
      short-lived registration token from the repo/org runner
      settings page. PATs are detected by the ghp_ / github_pat_
      prefix.
  - key: GH_RUNNER_LABELS
    type: string
    default: "self-hosted,linux,x64,smoothnas"
    description: Comma-separated runner labels.
  - key: GH_RUNNER_GROUP
    type: string
    default: "default"
    description: Runner group name (org-level runners only; ignored at repo scope).
`;

const llamaCppCommonVolumes = `volumes:
  - name: models
    mode: tier-bound
    slot: NVME
    minSize: 50G
    bind: /models
`;

const llamaCppCudaManifest = `apiVersion: smoothnas.io/v1
kind: Plugin

metadata:
  name: llama-cpp
  version: 0.1.0
  description: |
    llama.cpp inference server with NVIDIA CUDA acceleration.
    Models live on the NVME slot of an operator-chosen tier.
    Bearer-injected auth — SmoothNAS injects a per-plugin token via
    the nginx route; the wrapper image validates it before forwarding
    requests to upstream llama-server.
  vendor: smoothnas
  homepage: https://github.com/RakuenSoftware/smoothnas-plugin-llama-cpp

artifact:
  type: oci-image
  image: ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:0.1.0-cuda

container:
  command:
    - "--model"
    - "\${MODEL_PATH}"
    - "--n-gpu-layers"
    - "\${N_GPU_LAYERS}"
    - "--ctx-size"
    - "\${CTX_SIZE}"
  restartPolicy: unless-stopped

${llamaCppCommonVolumes}
ports:
  - name: http
    port: 8080
    protocol: tcp
    expose: true

ui:
  embed:
    path: /
    auth: bearer

profiles:
  - default-limits
  - gpu-nvidia

config:
  - key: MODEL_PATH
    type: string
    default: "/models/model.gguf"
    description: Path to the GGUF model file inside the container.
  - key: N_GPU_LAYERS
    type: string
    default: "999"
    description: Number of model layers to offload to GPU. 999 = all.
  - key: CTX_SIZE
    type: string
    default: "8192"
    description: Maximum context length in tokens.
`;

const llamaCppVulkanManifest = `apiVersion: smoothnas.io/v1
kind: Plugin

metadata:
  name: llama-cpp
  version: 0.1.0
  description: |
    llama.cpp inference server with AMD Vulkan acceleration.
    Models live on the NVME slot of an operator-chosen tier.
  vendor: smoothnas
  homepage: https://github.com/RakuenSoftware/smoothnas-plugin-llama-cpp

artifact:
  type: oci-image
  image: ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:0.1.0-vulkan

container:
  command:
    - "--model"
    - "\${MODEL_PATH}"
    - "--n-gpu-layers"
    - "\${N_GPU_LAYERS}"
    - "--ctx-size"
    - "\${CTX_SIZE}"
  restartPolicy: unless-stopped

${llamaCppCommonVolumes}
ports:
  - name: http
    port: 8080
    protocol: tcp
    expose: true

ui:
  embed:
    path: /
    auth: bearer

profiles:
  - default-limits
  - gpu-amd

config:
  - key: MODEL_PATH
    type: string
    default: "/models/model.gguf"
    description: Path to the GGUF model file inside the container.
  - key: N_GPU_LAYERS
    type: string
    default: "999"
    description: Number of model layers to offload to GPU. 999 = all.
  - key: CTX_SIZE
    type: string
    default: "8192"
    description: Maximum context length in tokens.
`;

const llamaCppCpuManifest = `apiVersion: smoothnas.io/v1
kind: Plugin

metadata:
  name: llama-cpp
  version: 0.1.0
  description: |
    llama.cpp inference server, CPU only. For hosts without a GPU,
    or for quick experiments before paying the GPU pull cost.
  vendor: smoothnas
  homepage: https://github.com/RakuenSoftware/smoothnas-plugin-llama-cpp

artifact:
  type: oci-image
  image: ghcr.io/rakuensoftware/smoothnas-plugin-llama-cpp:0.1.0-cpu

container:
  command:
    - "--model"
    - "\${MODEL_PATH}"
    - "--ctx-size"
    - "\${CTX_SIZE}"
    - "--threads"
    - "\${THREADS}"
  restartPolicy: unless-stopped

${llamaCppCommonVolumes}
ports:
  - name: http
    port: 8080
    protocol: tcp
    expose: true

ui:
  embed:
    path: /
    auth: bearer

profiles:
  - default-limits

config:
  - key: MODEL_PATH
    type: string
    default: "/models/model.gguf"
    description: Path to the GGUF model file inside the container.
  - key: CTX_SIZE
    type: string
    default: "8192"
    description: Maximum context length in tokens.
  - key: THREADS
    type: string
    default: "8"
    description: Worker thread count for inference.
`;

export const pluginCatalog: CatalogEntry[] = [
  {
    id: 'gh-runner',
    name: 'GitHub Actions runner',
    vendor: 'smoothnas',
    description:
      'Self-hosted GitHub Actions runner. Registers with a repo or org on start, deregisters on stop. Outbound-only — no ports, no embedded UI.',
    homepage: 'https://github.com/RakuenSoftware/smoothnas-plugin-gh-runner',
    tags: ['CI', 'multi-instance'],
    manifestYaml: ghRunnerManifest,
  },
  {
    id: 'llama-cpp-vulkan',
    name: 'llama.cpp (AMD Vulkan)',
    vendor: 'smoothnas',
    description:
      'llama.cpp inference server with AMD Vulkan acceleration. For hosts with an AMD GPU — uses /dev/dri, no ROCm runtime needed.',
    homepage: 'https://github.com/RakuenSoftware/smoothnas-plugin-llama-cpp',
    tags: ['LLM', 'AMD Vulkan'],
    manifestYaml: llamaCppVulkanManifest,
  },
  {
    id: 'llama-cpp-cuda',
    name: 'llama.cpp (NVIDIA CUDA)',
    vendor: 'smoothnas',
    description:
      'llama.cpp inference server with NVIDIA CUDA acceleration. For hosts with an NVIDIA GPU.',
    homepage: 'https://github.com/RakuenSoftware/smoothnas-plugin-llama-cpp',
    tags: ['LLM', 'NVIDIA CUDA'],
    manifestYaml: llamaCppCudaManifest,
  },
  {
    id: 'llama-cpp-cpu',
    name: 'llama.cpp (CPU only)',
    vendor: 'smoothnas',
    description:
      'llama.cpp inference server, CPU only. For hosts without a GPU or for quick experiments before paying the GPU pull cost.',
    homepage: 'https://github.com/RakuenSoftware/smoothnas-plugin-llama-cpp',
    tags: ['LLM', 'CPU'],
    manifestYaml: llamaCppCpuManifest,
  },
];
