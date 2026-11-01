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

### Plugin runtime

The plugin container runtime is built separately from the appliance backend:

```bash
make build-runtime     # writes bin/docker-lxc-daemon
make install-runtime   # installs the daemon + smoothnas-runtime.service
```

The runtime wraps [LXC2Docker](https://github.com/games-on-whales/LXC2Docker)
(pinned to a tested upstream commit; override with `LXC2DOCKER_REF`). Build hosts need
`golang-go`, `pkg-config`, `build-essential`, and `lxc-dev`; runtime hosts
need `lxc`, `lxc-templates`, `skopeo`, `umoci`, `rsync`, `nftables`,
`iptables`, `iproute2`, and `uidmap`. Runtime fixes are pinned upstream; reviewed downstream corrections in
`runtime/patches/` are applied by the same build path used for releases.
See [../runtime/README.md](../runtime/README.md) for detail.

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
| `/etc/systemd/system/smoothnas-runtime.service` | plugin container runtime (LXC2Docker) |
| `/usr/lib/smoothnas/docker-lxc-daemon` | runtime daemon binary |
| `/etc/nginx/sites-available/tierd` | nginx config |
| `/etc/nginx/conf.d/plugins.d` | generated per-plugin nginx route fragments |
| `/var/lib/tierd/tierd.db` | SQLite database |
| `/var/lib/smoothnas/runtime` | plugin image/template cache and container rootfs |
| `/var/lib/smoothnas/plugins` | flat (non-tier-bound) plugin volumes |
| `/etc/apt/sources.list.d/smoothapt.sources` | the Smooth* archive (smoothnas package) |

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

### Unattended install

The ISO boots interactive by default, but the whole install can run without
an operator. Two equivalent entry points:

- The GRUB menu ships an **"Automated — wipes the first disk"** entry. The
  target disk and admin password are baked in at ISO build time via the
  `AUTO_INSTALL_DISK` (default `/dev/sda`) and `AUTO_INSTALL_PASSWORD`
  (default `changeme`) build-iso.sh env vars.
- Any boot entry becomes unattended by adding kernel cmdline args:
  `smoothiso.disks=/dev/sda` (comma/plus separated for RAID-1 OS mirrors,
  e.g. `smoothiso.disks=/dev/sda+/dev/sdb`) and `smoothiso.password=...`
  (≥ 6 chars).

When both answers are present on the cmdline the installer skips every
prompt — including the final "remove media and continue" confirm — and
reboots on its own after a 10-second countdown. Detach the ISO/media before
that reboot (or rely on boot order) so the machine comes up in the installed
system. This is the path CI and test-VM provisioning use.

## Runtime Services

| Service | Role |
| --- | --- |
| `nginx` | TLS termination, static UI hosting, API proxy, plugin embed routes |
| `tierd-host-init` | one-shot backup cleanup, package healing, and host tuning before `tierd` |
| `tierd` | backend API and orchestration |
| `smoothnas-runtime` | plugin container runtime (LXC2Docker) on `/run/smoothnas-runtime/docker.sock` |

Backend defaults:

- bind address: `127.0.0.1:8420`
- database path: `/var/lib/tierd/tierd.db`

### Plugin catalog: bundled floor + GitHub token

**First-party plugins are bundled with the appliance.** SmoothNAS ships a
snapshot of its own plugin manifests (aimee, gh-runner, llama-cpp, vllm, wolf)
inside `tierd` (`tierd/internal/api/catalogdata/`, regenerated by
`scripts/sync-plugin-catalog.sh`). The "Install plugins" catalog serves these
**locally** — so they browse and install with **no GitHub fetch**, even on an
offline / air-gapped box or when the rate limit is exhausted. First-party cards
show a **Bundled** badge. When online, `tierd` refreshes them from GitHub in the
background so newer versions appear without an appliance update; that refresh is
best-effort and never blocks install.

**GitHub is still used for freshness and third-party repos.** The background
refresh (and any non-bundled repo) fetches each repo's latest release from
`api.github.com`. Unauthenticated, GitHub allows only **60 requests/hr per IP**,
shared across every repo; once exhausted the API returns **403 Forbidden**. This
no longer blocks installing first-party plugins (they serve from the bundle) — it
only means the *newest* upstream version may not show until the limit resets.

To lift the limit to 5000/hr (recommended so background freshness is reliable),
give `tierd` a token via its optional env file (created empty by default, not
overwritten on upgrade):

```sh
# a read-only, public-repo-scoped classic PAT (or fine-grained "public repos" read) is enough
printf 'SMOOTHNAS_GITHUB_TOKEN=%s\n' "$TOKEN" > /etc/tierd/tierd.env
chmod 600 /etc/tierd/tierd.env
systemctl restart tierd
```

`tierd` also accepts `GITHUB_TOKEN` / `GH_TOKEN` as fallbacks. The token is sent
only to the GitHub API host, never to release-asset downloads. Without a token
the fetch still works — it just shares the 60/hr budget, so a 403 means "wait
for the hourly reset or set a token," not a broken repo.

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

### SmoothFS protocol gate (CI)

[`smoothfs-protocol-gate.yml`](../.github/workflows/smoothfs-protocol-gate.yml)
is the CI half of the protocol story, and stable releases require it to pass.
It runs on a stock GitHub runner by building the smoothfs module and Samba VFS
against a clean ≥ 6.18 mainline kernel, booting that kernel under
virtme-ng/QEMU (TCG emulation — nested KVM stalls on GitHub runners), and
executing the *unmodified* ISO-shipped gate scripts inside the guest:
`scripts/smoothfs-protocol-gate.sh` (cthon04 NFS conformance, smbtorture,
tier-spill checks) followed by `scripts/smoothfs-mixed-protocol-soak.sh`
(concurrent local + NFSv4.2 + SMB writers on a two-tier loopback pool, then a
zero-length-file integrity sweep).

The smoothfs source under test is the `SMOOTHFS_REPO_REF` pin in
[`iso/build-iso.sh`](../iso/build-iso.sh) — the same ref the release embeds —
so bumping the pin is what rolls new smoothfs code through the gate.

Everything long-running in the guest is wall-clock bounded so a kernel-side
wedge fails the job in minutes with diagnostics instead of consuming the whole
job timeout invisibly: each cthon04 suite is bounded (900 s, fresh workdir,
retried), the soak's exportfs and NFS/CIFS mounts are bounded, and on any
timeout the harness dumps D-state kernel stacks, all-task stacks, and dmesg
(sysrq w/l/t) before failing. If a gate job produces no output for tens of
minutes and then dies at the guest wall-clock bound, treat it as an in-kernel
smoothfs hang and start from the stack dumps in the job log.

## Release and Update Model

SmoothNAS updates with apt. Everything it runs is one package, `smoothnas`
(tierd, the web UI, the plugin runtime and their units), whose dependencies
bring SmoothKernel, OpenZFS and smoothfs from the signed Smooth* archive
(`https://updates.rakuensoftware.com/public`, suites `common` and `smoothnas`)
and the storage and sharing tools from Debian. unattended-upgrades installs
updates from the archive and Debian security as soon as they are published.
A new kernel takes effect when the operator restarts; the Updates page says
when that is needed.

- Each push to `main` publishes a stable release; smoothapt ingests its
  attested packages into the `smoothnas` suite.
- Each push to `testing` publishes a GitHub prerelease (ISO and packages) for
  testers; the archive does not serve it.
- smoothfs-samba-vfs depends on its exact Samba, so a Samba update installs
  together with a matching module, which smoothfs's pipeline rebuilds daily
  when Debian updates Samba.

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

The source deep dive calls both out in more detail:

- [../src/README.md](../src/README.md)

## Google Drive backups

The **Backups** page supports Google Drive alongside NFS, SMB and rsync. The
appliance needs `rclone >= 1.60`; the SmoothNAS package and installer include it.
For a source checkout on an existing machine, install rclone before creating an
encrypted job or starting a Drive run.

1. Open [Google Cloud Console](https://console.cloud.google.com/), create or
   select a project, and enable the **Google Drive API**.
2. Configure the OAuth consent screen as **External** and publish it to
   **Production**. Tokens issued while the app is in Testing expire after seven
   days. Personal-use apps can proceed through Google's unverified-app warning.
3. Create an OAuth client of type **TVs and Limited Input devices**. Copy its
   client ID and client secret.
4. In **Backups → Google accounts → Connect Google account**, enter a label and
   the client credentials. Open the Google link and approve the displayed code.
   The NAS polls Google; no public callback URL or HTTPS reverse proxy is needed.
5. Add a backup job with method **Google Drive**, choose the account, an absolute
   local path and a relative Drive folder such as `smoothnas/storage`. Choose
   **Push** to back up or **Pull** to restore. Restores always copy and never
   delete local files. Push deletion mode mirrors the selected folder.
6. Enter any exclude patterns, one per line (for example `/video/**` and
   `/models/**`). These are rclone exclusion patterns, not `+`/`-` filter rules.
7. Optionally enable encryption. **Prepare recovery kit** produces a private
   `smoothnas-recovery.conf`; download it and confirm it is safely stored before
   saving. Preparation expires after 30 minutes and is lost on daemon restart.
   If it expires, cancel and reopen the form to prepare another kit.
8. Enable a schedule, select weekdays and set a time in the **NAS's local time
   zone**. Existing jobs remain manual until scheduling is enabled.

Encryption covers file contents, filenames and directory names. Encryption
cannot change after the first run. An encrypted job keeps the Google account
and folder recorded in its recovery kit; create a new job for another target.
The kit includes the OAuth credentials, refresh token and encryption keys, so
keep it private and store it somewhere other than the NAS. The API never
exports saved encryption keys. Losing both the NAS and its kit makes encrypted
backups unrecoverable.

To restore an encrypted job through SmoothNAS, edit the original job and change
its direction to **Pull**, retaining its encryption setting. A newly created
encrypted job generates new keys and cannot decrypt a different job's backup.

To restore without SmoothNAS, install stock rclone on another machine and run:

```bash
rclone --config ./smoothnas-recovery.conf copy sncrypt: /path/to/restore
```

Keep the original Google Cloud project and OAuth client. SmoothNAS requests
`drive.file`, so it can only see files created through that project. Connecting
through a different project cannot discover the old backup. Disconnecting an
unused account removes its credentials from SmoothNAS and leaves Drive files
intact. Google access can be revoked separately in Google Account settings;
revocation may also affect other clients in the same project.

Schedules check every 30 seconds. A missed slot in the last 24 hours is caught
up once, and an already running job causes that slot to be skipped. Repeated
DST hours run once; nonexistent times run at the first valid minute afterward.
A daily Google upload cap ends the run with a continuation summary; the next
scheduled run copies remaining changes. Full Drive storage and other errors
remain failures in run history.

Credentials and encryption keys use the existing SQLite secrets posture. They
are omitted from JSON, command arguments and run errors. Runs use environment
configuration rather than a persistent rclone configuration file.

Implementation checks include mocked Google device-flow tests and real rclone
local-backend backup, restore and independent recovery-kit tests. A real Google
account connection, encrypted upload and recovery on a second machine still
need operator validation before a release is described as live-validated.
