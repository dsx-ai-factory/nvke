# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# The Linux-only steps of an image build -- the node packages and the patched
# installer ISO -- for hosts that cannot run them natively, such as an
# Apple-silicon Mac. scripts/builder-container.sh builds and runs it; Packer
# and QEMU stay on the host. See README.md, "Build on an Apple-silicon Mac".
#
# Bookworm, not the default golang tag: the agent links against this image's
# glibc (2.36), and its .deb declares a bare `Depends: libc6`, so the builder's
# glibc must not be newer than the oldest target image's (Ubuntu 24.04, 2.39).
# The Go version matches go.work, and the digest matches
# agent/internal-scripts/docker/Dockerfile.proto; move them together.
FROM docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea

# The package targets download their own pinned crane and nfpm. These are the
# remaining tools fetch-artifacts.sh and prepare-installer-iso.sh call. gawk
# replaces bookworm's mawk (1.3.4-20200120) as awk: the bundle scripts match
# checksum lines with interval expressions such as {64}, which that mawk lacks
# and Ubuntu's newer mawk and the Nix builder's gawk both support.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl dpkg-dev file gawk git gnupg jq make p7zip-full python3 unzip xorriso xz-utils \
    && rm -rf /var/lib/apt/lists/*

# cni-ovn builds its image from source. builder-container.sh hands the podman
# machine's service socket to this container (CONTAINER_HOST, OCI_BIN=podman),
# and this static client, installed as `podman`, drives it. Keep it no newer
# than the podman you run; the digests are from that release's shasums file.
ARG PODMAN_REMOTE_VERSION=5.1.2
ARG TARGETARCH
RUN set -eu; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
      arm64) sha=5f19c964f77f9066d2c0454c87db5287c906f72468eb74242aa60bc7dff2de3b ;; \
      amd64) sha=0d10e6da619e2682749eb782bfced52c30e70ef8d1c9e85c184e3f1a7e09d155 ;; \
      *) echo "no podman-remote digest for $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 8 -o /tmp/podman-remote.tar.gz \
      "https://github.com/containers/podman/releases/download/v${PODMAN_REMOTE_VERSION}/podman-remote-static-linux_${arch}.tar.gz"; \
    echo "$sha  /tmp/podman-remote.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/podman-remote.tar.gz -C /tmp; \
    install -m 0755 "/tmp/bin/podman-remote-static-linux_${arch}" /usr/local/bin/podman; \
    rm -rf /tmp/podman-remote.tar.gz /tmp/bin
