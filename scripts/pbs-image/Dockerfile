# Experimental manifest-durability candidate; build from the repository root:
# docker build -f scripts/pbs-image/Dockerfile -t pbs:4.2.0-manifest-fsync .
# The live validation used these commands in a disposable Debian 13 guest,
# then inserted the resulting binaries into this same clean runtime image.
FROM debian:13 AS build
ARG DEBIAN_FRONTEND=noninteractive
ARG BUILD_JOBS=1
RUN test "$(dpkg --print-architecture)" = amd64 \
 && apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git dpkg-dev \
 && curl -fsSL https://enterprise.proxmox.com/debian/proxmox-release-trixie.gpg \
      -o /usr/share/keyrings/proxmox.gpg \
 && printf '%s\n' \
      'deb [signed-by=/usr/share/keyrings/proxmox.gpg] http://download.proxmox.com/debian/pbs trixie pbs-no-subscription' \
      'deb [signed-by=/usr/share/keyrings/proxmox.gpg] http://download.proxmox.com/debian/devel trixie main' \
      > /etc/apt/sources.list.d/proxmox.list
# PBS 4.2.0 needs the older apt API; newer devel packages use incompatible majors.
RUN printf '%s\n' \
      'Package: librust-proxmox-apt-dev librust-proxmox-apt+*' 'Pin: version 0.99.7-1' 'Pin-Priority: 1001' '' \
      'Package: librust-proxmox-apt-api-types*' 'Pin: version 2.0.5-1' 'Pin-Priority: 1001' '' \
      'Package: librust-pbs-api-types*' 'Pin: version 1.0.15-1' 'Pin-Priority: 1001' '' \
      'Package: librust-proxmox-upgrade-checks*' 'Pin: version 1.0.1-1' 'Pin-Priority: 1001' '' \
      'Package: librust-proxmox-openid*' 'Pin: version 1.0.2-1' 'Pin-Priority: 1001' '' \
      'Package: librust-proxmox-subscription*' 'Pin: version 1.0.1-1' 'Pin-Priority: 1001' \
      > /etc/apt/preferences.d/pbs-build \
 && git clone --branch v4.2.0 --single-branch https://git.proxmox.com/git/proxmox-backup.git /build
WORKDIR /build
RUN test "$(git rev-parse HEAD)" = 035c449897fafc228c8bbf3a5b5ba38564478ac7 \
 && apt-get update \
 && apt-get --solver 3.0 -Pnodoc --no-install-recommends -y build-dep .
COPY docs/patches/pbs-manifest-fsync.patch /manifest-fsync.patch
RUN git apply --check /manifest-fsync.patch && git apply /manifest-fsync.patch \
 && export DEB_VERSION="$(dpkg-parsechangelog -SVersion)" \
 && export DEB_VERSION_UPSTREAM="${DEB_VERSION%-*}" \
 && export REPOID=035c449897fafc228c8bbf3a5b5ba38564478ac7-manifest-fsync \
 && CARGO_BUILD_JOBS="$BUILD_JOBS" CARGO_PROFILE_RELEASE_DEBUG=0 \
      cargo build --offline --release --package proxmox-backup \
      --bin proxmox-backup-api --bin proxmox-backup-proxy --bin proxmox-backup-manager \
 && mkdir /out \
 && cp target/release/proxmox-backup-api target/release/proxmox-backup-proxy \
      target/release/proxmox-backup-manager /out/
FROM ayufan/proxmox-backup-server@sha256:01f685a010025233b649417a317cc175b27920cd64f0429abfdc46a0d8d2e88a
COPY --from=build /out/proxmox-backup-api /out/proxmox-backup-proxy /usr/lib/x86_64-linux-gnu/proxmox-backup/
COPY --from=build /out/proxmox-backup-manager /usr/sbin/proxmox-backup-manager
LABEL pbs.candidate="manifest-fsync" \
      pbs.source="035c449897fafc228c8bbf3a5b5ba38564478ac7" \
      pbs.base="sha256:01f685a010025233b649417a317cc175b27920cd64f0429abfdc46a0d8d2e88a"
