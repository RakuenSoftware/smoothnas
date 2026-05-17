# SmoothNAS Runtime

SmoothNAS plugins run through `smoothnas-runtime`, a systemd service
that exposes the Games on Whales LXC2Docker Docker-compatible API on
`/run/smoothnas-runtime/docker.sock`.

Build the runtime daemon with:

```sh
make build-runtime
```

On Debian/Trixie build hosts, install `golang-go`, `pkg-config`,
`build-essential`, and `lxc-dev` first. Runtime hosts need `lxc`,
`lxc-templates`, `skopeo`, `umoci`, `rsync`, `nftables`, `iptables`,
`iproute2`, and `uidmap`.

The build target writes `bin/docker-lxc-daemon`. Appliance installs
place it at `/usr/lib/smoothnas/docker-lxc-daemon` and install
`runtime/smoothnas-runtime.service`.

The upstream source defaults to LXC2Docker `main`:

```text
https://github.com/games-on-whales/LXC2Docker.git
main
```

Set `LXC2DOCKER_REF=<branch-or-commit-or-tag>` when a reproducible
pinned build is needed.

SmoothNAS does not carry local LXC2Docker patches. Runtime fixes must
land upstream in LXC2Docker and are picked up by the next SmoothNAS
runtime build from `main`.
