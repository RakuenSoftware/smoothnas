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

The pinned upstream source is:

```text
https://github.com/games-on-whales/LXC2Docker.git
483a5851d3f50383a723785a5d80027fb0539d0c
```

SmoothNAS applies the patches in `runtime/patches/` on top of that
commit to keep OCI template names LXC-safe, preserve stopped SmoothNAS
plugin containers from upstream's legacy raw-LXC garbage collector,
provide a legacy directory-copy fallback on hosts where `lxc-copy`
cannot perform its mount-based clone, expose the managed bridge as
`veth0`, and reattach started raw-LXC veth devices when LXC leaves the
host-side interface detached from the bridge.
