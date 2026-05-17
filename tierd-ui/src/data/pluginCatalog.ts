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
    - "--temp"
    - "\${LLAMA_ARG_TEMP}"
    - "--n-gpu-layers"
    - "\${N_GPU_LAYERS}"
    - "--ctx-size"
    - "\${CTX_SIZE}"
    - "--parallel"
    - "\${PARALLEL_SLOTS}"
  restartPolicy: unless-stopped
  resources:
    memory: "\${MEMORY_LIMIT}"

${llamaCppCommonVolumes}
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
  - default-limits
  - gpu-nvidia

config:
  - key: MODEL_URL
    type: string
    label: Model download URL
    default: ""
    description: HTTP(S) URL for the GGUF model. The container downloads it into its private /models/model.gguf path on start.
  - key: LLAMA_ARG_TEMP
    type: number
    label: Temperature
    default: "0.8"
    min: "0"
    max: "2"
    step: "0.01"
    description: Sampling temperature passed to llama.cpp. Lower values are more deterministic.
  - key: N_GPU_LAYERS
    type: number
    label: GPU layers
    default: "999"
    min: "0"
    step: "1"
    description: Number of model layers to offload to GPU. 999 = all.
  - key: CTX_SIZE
    type: number
    label: Context window
    default: "524288"
    min: "1024"
    step: "1024"
    unit: tokens
    description: Total server context. With 4 request slots, 524288 gives 128K per slot.
  - key: PARALLEL_SLOTS
    type: number
    label: Request slots
    default: "4"
    min: "1"
    max: "16"
    step: "1"
    description: llama.cpp parallel request slots passed to --parallel.
  - key: MEMORY_LIMIT
    type: string
    label: Container memory
    default: "64GiB"
    description: LXC memory limit for the llama.cpp container.
  - key: LLAMA_ARG_FLASH_ATTN
    type: select
    label: Flash attention
    default: "on"
    options:
      - value: "on"
      - value: "off"
    description: Enables llama.cpp flash attention.
  - key: LLAMA_ARG_CACHE_TYPE_K
    type: select
    label: K cache type
    default: q8_0
    options:
      - value: q8_0
      - value: q5_0
      - value: q4_0
    description: KV cache K quantization. Q8 preserves quality.
  - key: LLAMA_ARG_CACHE_TYPE_V
    type: select
    label: V cache type
    default: q8_0
    options:
      - value: q8_0
      - value: q5_0
      - value: q4_0
    description: KV cache V quantization. Q8 preserves quality.
  - key: LLAMA_ARG_N_CPU_MOE
    type: number
    label: CPU MoE layers
    default: "10"
    min: "0"
    step: "1"
    description: Number of MoE layers left on CPU/system RAM so active experts can fit VRAM.
  - key: LLAMA_ARG_FIT
    type: select
    label: Fit model to VRAM
    default: "off"
    options:
      - value: "off"
      - value: "on"
    description: Leave off to allow overfill/GTT when model weights exceed VRAM.
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
    - "--temp"
    - "\${LLAMA_ARG_TEMP}"
    - "--n-gpu-layers"
    - "\${N_GPU_LAYERS}"
    - "--ctx-size"
    - "\${CTX_SIZE}"
    - "--parallel"
    - "\${PARALLEL_SLOTS}"
  restartPolicy: unless-stopped
  resources:
    memory: "\${MEMORY_LIMIT}"

${llamaCppCommonVolumes}
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
  - default-limits
  - gpu-amd

config:
  - key: MODEL_URL
    type: string
    label: Model download URL
    default: ""
    description: HTTP(S) URL for the GGUF model. The container downloads it into its private /models/model.gguf path on start.
  - key: LLAMA_ARG_TEMP
    type: number
    label: Temperature
    default: "0.8"
    min: "0"
    max: "2"
    step: "0.01"
    description: Sampling temperature passed to llama.cpp. Lower values are more deterministic.
  - key: N_GPU_LAYERS
    type: number
    label: GPU layers
    default: "999"
    min: "0"
    step: "1"
    description: Number of model layers to offload to GPU. 999 = all.
  - key: CTX_SIZE
    type: number
    label: Context window
    default: "524288"
    min: "1024"
    step: "1024"
    unit: tokens
    description: Total server context. With 4 request slots, 524288 gives 128K per slot.
  - key: PARALLEL_SLOTS
    type: number
    label: Request slots
    default: "4"
    min: "1"
    max: "16"
    step: "1"
    description: llama.cpp parallel request slots passed to --parallel.
  - key: MEMORY_LIMIT
    type: string
    label: Container memory
    default: "64GiB"
    description: LXC memory limit for the llama.cpp container.
  - key: LLAMA_ARG_FLASH_ATTN
    type: select
    label: Flash attention
    default: "on"
    options:
      - value: "on"
      - value: "off"
    description: Enables llama.cpp flash attention.
  - key: LLAMA_ARG_CACHE_TYPE_K
    type: select
    label: K cache type
    default: q8_0
    options:
      - value: q8_0
      - value: q5_0
      - value: q4_0
    description: KV cache K quantization. Q8 preserves quality.
  - key: LLAMA_ARG_CACHE_TYPE_V
    type: select
    label: V cache type
    default: q8_0
    options:
      - value: q8_0
      - value: q5_0
      - value: q4_0
    description: KV cache V quantization. Q8 preserves quality.
  - key: LLAMA_ARG_N_CPU_MOE
    type: number
    label: CPU MoE layers
    default: "10"
    min: "0"
    step: "1"
    description: Number of MoE layers left on CPU/system RAM so active experts can fit VRAM.
  - key: LLAMA_ARG_FIT
    type: select
    label: Fit model to VRAM
    default: "off"
    options:
      - value: "off"
      - value: "on"
    description: Leave off to allow overfill/GTT when model weights exceed VRAM.
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
    - "--temp"
    - "\${LLAMA_ARG_TEMP}"
    - "--ctx-size"
    - "\${CTX_SIZE}"
    - "--parallel"
    - "\${PARALLEL_SLOTS}"
    - "--threads"
    - "\${THREADS}"
  restartPolicy: unless-stopped
  resources:
    memory: "\${MEMORY_LIMIT}"

${llamaCppCommonVolumes}
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
  - default-limits

config:
  - key: MODEL_URL
    type: string
    label: Model download URL
    default: ""
    description: HTTP(S) URL for the GGUF model. The container downloads it into its private /models/model.gguf path on start.
  - key: LLAMA_ARG_TEMP
    type: number
    label: Temperature
    default: "0.8"
    min: "0"
    max: "2"
    step: "0.01"
    description: Sampling temperature passed to llama.cpp. Lower values are more deterministic.
  - key: CTX_SIZE
    type: number
    label: Context window
    default: "524288"
    min: "1024"
    step: "1024"
    unit: tokens
    description: Total server context. With 4 request slots, 524288 gives 128K per slot; CPU mode may need a smaller value.
  - key: PARALLEL_SLOTS
    type: number
    label: Request slots
    default: "4"
    min: "1"
    max: "16"
    step: "1"
    description: llama.cpp parallel request slots passed to --parallel.
  - key: MEMORY_LIMIT
    type: string
    label: Container memory
    default: "64GiB"
    description: LXC memory limit for the llama.cpp container.
  - key: LLAMA_ARG_FLASH_ATTN
    type: select
    label: Flash attention
    default: "on"
    options:
      - value: "on"
      - value: "off"
    description: Enables llama.cpp flash attention.
  - key: LLAMA_ARG_CACHE_TYPE_K
    type: select
    label: K cache type
    default: q8_0
    options:
      - value: q8_0
      - value: q5_0
      - value: q4_0
    description: KV cache K quantization. Q8 preserves quality.
  - key: LLAMA_ARG_CACHE_TYPE_V
    type: select
    label: V cache type
    default: q8_0
    options:
      - value: q8_0
      - value: q5_0
      - value: q4_0
    description: KV cache V quantization. Q8 preserves quality.
  - key: THREADS
    type: number
    label: CPU threads
    default: "0"
    min: "0"
    step: "1"
    description: CPU threads. 0 = let llama.cpp auto-detect.
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
