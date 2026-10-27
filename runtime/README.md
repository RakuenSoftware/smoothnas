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

The upstream source is pinned to the reviewed LXC2Docker commit:

```text
https://github.com/games-on-whales/LXC2Docker.git
7171568494541a1c9a776670a1623f3db64d7e46
```

Set `LXC2DOCKER_REF=<branch-or-commit-or-tag>` to evaluate a newer revision.
Downstream corrections are applied by the same build path used for releases.


SmoothNAS carries runtime corrections in `runtime/patches/`. The build script
applies them after checking out LXC2Docker; a patch that no longer applies fails
the build. The bridge correction replaces only the owned postrouting chain in
one nft transaction, preserving published port DNAT rules and removing old
duplicate masquerade rules. Bridge and port-forward mutations share `nftMu`.
