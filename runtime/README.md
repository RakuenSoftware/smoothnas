# SmoothNAS Runtime

SmoothNAS plugins run through `smoothnas-runtime`, a systemd service
that exposes the Games on Whales LXC2Docker Docker-compatible API on
`/run/smoothnas-runtime/docker.sock`.

Build the pinned daemon with:

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

The upstream source defaults to the latest `main` branch:

```text
https://github.com/games-on-whales/LXC2Docker.git
main
```

Set `LXC2DOCKER_REF=<commit-or-tag>` when a reproducible pinned build is
needed.

SmoothNAS applies the patches in `runtime/patches/` on top of that
commit to keep OCI template names LXC-safe, preserve stopped SmoothNAS
plugin containers from upstream's legacy raw-LXC garbage collector,
provide a legacy directory-copy fallback on hosts where `lxc-copy`
cannot perform its mount-based clone, and reattach started raw-LXC
veth devices when LXC leaves the host-side interface detached from the
bridge. The managed bridge name is provided by upstream LXC2Docker.
