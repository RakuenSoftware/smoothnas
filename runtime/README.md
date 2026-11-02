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

The upstream source is pinned to the tested LXC2Docker commit that removes
container request and failure-config logging of credentials:

```text
https://github.com/games-on-whales/LXC2Docker.git
ff6d6d8f9f6f62ea4b2fe7592e88a10ed20569bb
```

Set `LXC2DOCKER_REF=<branch-or-commit-or-tag>` to evaluate a newer revision.
Downstream corrections are applied by the same build path used for releases.

SmoothNAS carries runtime corrections in `runtime/patches/`. The build script
applies them after checking out LXC2Docker; a patch that no longer applies fails
the build. The pinned upstream already bounds masquerade rules in one nft transaction.
The bridge correction serializes bridge and port-forward mutations with
`nftMu` and checks that published port DNAT rules survive repeated reconciliation.
