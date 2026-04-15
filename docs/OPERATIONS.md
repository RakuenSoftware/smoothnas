# SmoothNAS Operations Guide

This page covers the practical side of working on or running SmoothNAS: build, test, install, release, and branch workflow.

## Build and Test

### Backend

```bash
cd tierd
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go build -ldflags "-X main.version=<version>" -o ../bin/tierd ./cmd/tierd/
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

### Full test

```bash
make test
```

## Local Install Layout

The project installs into a conventional appliance layout:

| Path | Purpose |
| --- | --- |
| `/usr/local/bin/tierd` | backend binary |
| `/usr/share/tierd-ui` | built static frontend assets |
| `/etc/systemd/system/tierd.service` | backend service |
| `/etc/nginx/sites-available/tierd` | nginx config |
| `/var/lib/tierd/tierd.db` | SQLite database |
| `/etc/tierd/update-channel` | persisted update channel |

The Makefile already captures the expected deployment shape.

## Installer Flow

The custom installer in [`../iso/smoothnas-install`](../iso/smoothnas-install) does the following:

1. prepare the initramfs environment
2. bring up networking
3. let the user choose OS disks
4. partition and format the OS target
5. debootstrap Debian
6. install packages and appliance assets
7. configure hostname, services, bootloader, and nginx

The key design rule is that the OS lives on separate disk selection from managed storage disks.

## Runtime Services

| Service | Role |
| --- | --- |
| `nginx` | TLS termination, static UI hosting, API proxy |
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
