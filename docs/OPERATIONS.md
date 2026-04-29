# SmoothNAS Operations Guide

This page covers the practical side of working on or running SmoothNAS: build, test, install, release, and branch workflow.

## Build and Test

### Backend

```bash
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0=url.git@github.com:.insteadOf
export GIT_CONFIG_VALUE_0=https://github.com/
export GOPRIVATE=github.com/RakuenSoftware/*
export GONOSUMDB=github.com/RakuenSoftware/*
cd tierd
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go build -ldflags "-X main.version=<version>" -o ../bin/tierd ./cmd/tierd/
```

For the smoothfs host-gated fixture added in Phase 2.5:

```bash
cd tierd
SMOOTHFS_KO=/absolute/path/to/smoothfs.ko \
GOCACHE=/tmp/gocache \
go test -tags=e2e ./internal/tiering/smoothfstest -run TestE2EMountReadyAutoDiscovery
```

That test requires `root`, `losetup`, `mkfs.xfs`, `mount`, `umount`, `insmod`, and `rmmod`. Without those prerequisites it skips cleanly.

Phase 2.6 extends that same harness with live validation cases:

```bash
cd tierd
SMOOTHFS_KO=/absolute/path/to/smoothfs.ko \
GOCACHE=/tmp/gocache \
go test -tags=e2e ./internal/tiering/smoothfstest \
  -run 'TestE2E(HeatFlowsIntoPlanner|RestartReplayPreCutoverRollback|RestartReplayPostCutoverForward)'
```

### Frontend

```bash
cd tierd-ui
npm install
npm run build
npm test
```

For day-to-day UI iteration, the active frontend dev server is:

```bash
cd tierd-ui
npm run dev
```

Frontend verification is now a TypeScript check via `npm test`, and the only supported frontend runtime/build path is React + Vite.

### Full build

```bash
make build
```

For development on a shared workstation, prefer the low-impact wrapper:

```bash
make build-low
```

That wrapper lowers CPU and I/O priority for the nested build so multiple
SmoothNAS or aimee sessions do not contend as aggressively for the same disk.

### Full test

```bash
make test
```

### Kernel and OpenZFS

SmoothNAS ships a custom kernel (`6.18.22-smoothnas-lts`, `LOCALVERSION=-smoothnas-lts`) and a matching OpenZFS DKMS build. Both are produced by the shared appliance-kernel harness at [`RakuenSoftware/smoothkernel`](https://github.com/RakuenSoftware/smoothkernel); SmoothNAS no longer carries inline `bindeb-pkg` / `deb-dkms` recipes.

To rebuild the appliance kernel plus ZFS stack:

```bash
# from the SmoothNAS checkout
make kernel-low
make zfs-low
```

These targets delegate into the sibling `../smoothkernel` checkout through
`scripts/build-smoothkernel.sh`, which:

- runs the build under reduced CPU / I/O priority
- defaults `BUILD_THREADS` to at most half the host CPUs, capped at 8
- still respects an explicit `BUILD_THREADS=...` override

If you need the old direct path, the underlying harness is still the same:

```bash
# in a checkout of RakuenSoftware/smoothkernel
./recipes/build-kernel.sh \
    KERNEL_VERSION=6.18.22 LOCALVERSION=-smoothnas-lts \
    CONFIG_SOURCE=/path/to/smoothnas.config
./recipes/build-zfs.sh ZFS_VERSION=2.4.1
```

To build the installer ISO with the same scheduling guard:

```bash
make iso-low VERSION=2026.0424.1
```

The resulting `.deb` packages (kernel + headers + zfs-dkms) install into SmoothNAS like any Debian kernel package; DKMS rebuilds the `smoothfs` module from the pinned `RakuenSoftware/smoothfs` source checkout embedded by the installer/build flow. The bump-the-kernel-pin runbook — when to take point bumps vs LTS jumps, the OpenZFS/Linux-Maximum compatibility rule — lives at [`smoothkernel/docs/bumping-kernel.md`](https://github.com/RakuenSoftware/smoothkernel/blob/main/docs/bumping-kernel.md).

SmoothNAS's per-OS bits are the `smoothfs` module source, the `.config` seed, and the `LOCALVERSION` string. Everything else is upstream or in `smoothkernel`.

## Local Install Layout

The project installs into a conventional appliance layout:

| Path | Purpose |
| --- | --- |
| `/usr/local/bin/tierd` | backend binary |
| `/usr/share/tierd-ui` | built static frontend assets |
| `/etc/systemd/system/tierd-host-init.service` | one-shot host repair/tuning before `tierd` |
| `/etc/systemd/system/tierd.service` | backend service |
| `/etc/nginx/sites-available/tierd` | nginx config |
| `/var/lib/tierd/tierd.db` | SQLite database |
| `/etc/tierd/update-channel` | persisted update channel |

The Makefile already captures the expected deployment shape.

## Installer Flow

[`iso/build-iso.sh`](../iso/build-iso.sh) builds the SmoothNAS ISO by wrapping
the generic [smoothiso](https://github.com/RakuenSoftware/smoothiso) builder
and embedding the [SmoothGUI](https://github.com/RakuenSoftware/smoothgui)
React installer frontend. At install time the smoothiso initrd:

1. brings up the loopback + console display
2. starts an Xorg session and launches `firefox-esr --app=...` against the
   embedded SmoothGUI bundle on `http://127.0.0.1:8080`
3. drives the install (network, disk selection, password, partition,
   debootstrap, packages, GRUB) by sending JSON requests to the GUI
4. sources the SmoothNAS hooks under [`iso/hooks/`](../iso/hooks):
   - `embed.sh` stages the SmoothNAS payload (tierd, frontend, .deb repo,
     smoothfs source, tests) into the installer initrd
   - `packages.sh` installs the DKMS toolchain, NAS tooling, SmoothKernel,
     and service packages into the target chroot
   - `configure.sh` writes sysctl/udev tuning, the nftables ruleset, the
     tierd binary + UI + systemd units, and the nginx site
   - `firstboot.sh` runs once on the first boot to build the OpenZFS,
     smoothfs, and smoothfs Samba VFS DKMS modules and to generate the
     TLS certificate consumed by nginx

The key design rule is that the OS lives on a separate disk selection from
managed storage disks.

## Runtime Services

| Service | Role |
| --- | --- |
| `nginx` | TLS termination, static UI hosting, API proxy |
| `tierd-host-init` | one-shot backup cleanup, package healing, and host tuning before `tierd` |
| `tierd` | backend API and orchestration |

Backend defaults:

- bind address: `127.0.0.1:8420`
- database path: `/var/lib/tierd/tierd.db`

## Agent and MCP Setup

SmoothNAS exposes a repo-local `aimee` MCP server through [../.mcp.json](../.mcp.json).

That surface is for engineering agents, not for appliance runtime services.

Recommended agent startup sequence:

1. load the MCP config from [../.mcp.json](../.mcp.json)
2. call `get_help`
3. inspect `git_status`
4. read the relevant docs and source entrypoints
5. run `git_verify` before handoff when configured

See [AIMEE.md](AIMEE.md) for the agent-focused workflow and tool map.

## Release Gate

Before updating a release PR, run the shared gate from the repo root:

```bash
SMOOTHNAS_HOST=192.168.0.204 SMOOTHNAS_PASS='...' scripts/release-gate.sh
```

The default gate is non-destructive. It checks service health, generated SMB/NFS
defaults, failed units, and quick protocol smoke tests when the test mounts are
present. The full checklist is in [RELEASE_CHECKLIST.md](RELEASE_CHECKLIST.md).

## Release and Update Model

SmoothNAS currently supports three update channels plus the manual upload path:

- `main`: public stable releases from `RakuenSoftware/smoothnas`
- `testing`: public prereleases from `RakuenSoftware/smoothnas`
- `jbailes`: private source builds from `JBailes/SmoothNAS` over SSH using the host `JBailes` account keys
- local artifact upload and apply flow

The `jbailes` channel is intentionally documented as transitional. It currently clones and builds from source because authenticated private release artifacts are not wired up yet. That should be replaced with a private release-artifact flow once the repo-auth and packaging path exists.

## Branch Workflow

Recommended branch roles:

- `main`: stable promoted branch
- `testing`: integration branch for work that is ready to soak
- short-lived feature or fix branches: PR into `testing`
- promotion PRs: `testing -> main`

That workflow matters for this repo because storage changes are broad and often touch backend, UI, installer, and update behavior at the same time.

## Documentation and Architecture Follow-Ups

Two specific cleanup tracks are still open and should be treated as engineering work, not just prose work:

1. unify the tier implementation around the named-tier-instance model
2. replace the remaining old `JBailes/SmoothNAS` module-path references with `RakuenSoftware/smoothnas`
3. replace the temporary `jbailes` SSH source-build updater path with authenticated private release artifacts

The source deep dive calls both out in more detail:

- [../src/README.md](../src/README.md)
