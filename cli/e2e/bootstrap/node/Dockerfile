# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# K8S_VERSION selects the upstream `kindest/node` base image, which bakes in
# matching kubeadm/kubelet/container-runtime binaries. Bumping this is how
# the e2e suite tests against a different Kubernetes minor. Anything kind has
# published works (verify with `docker manifest inspect kindest/node:v${K8S_VERSION}`);
# set via the K8S_VERSION env var, consumed by both the Makefile and
# docker-compose.yaml.
ARG K8S_VERSION=1.32.0
FROM kindest/node:v${K8S_VERSION}

# Install grpcurl for ad-hoc gRPC debugging from inside the container (not on
# the critical path). Pick the matching grpcurl archive for the build arch —
# the upstream release tarballs are named
# `grpcurl_<ver>_linux_{x86_64,arm64}.tar.gz`. TARGETARCH is set automatically
# by `docker buildx build --platform`; defaults to amd64 for plain
# `docker build` (no platform flag).
#
# python3 isn't in the kindest/node base since v1.36 (was in v1.35 and
# earlier), and the calico bundle's install script is python — install it
# here so the CNI install path works across K8s versions.
ARG TARGETARCH
RUN GRPCURL_ARCH=$(case "${TARGETARCH:-amd64}" in arm64) echo arm64 ;; amd64|"") echo x86_64 ;; *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; esac) && \
    TARBALL="grpcurl_1.9.1_linux_${GRPCURL_ARCH}.tar.gz" && \
    apt-get update && \
    apt-get install -y --no-install-recommends wget ca-certificates python3 && \
    DL=$(mktemp -d) && \
    wget -q -P "${DL}" "https://github.com/fullstorydev/grpcurl/releases/download/v1.9.1/${TARBALL}" && \
    wget -q -P "${DL}" "https://github.com/fullstorydev/grpcurl/releases/download/v1.9.1/grpcurl_1.9.1_checksums.txt" && \
    (cd "${DL}" && grep " ${TARBALL}\$" grpcurl_1.9.1_checksums.txt | sha256sum -c -) && \
    tar xz -C /usr/local/bin -f "${DL}/${TARBALL}" grpcurl && \
    rm -rf "${DL}" && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Copy the nke-agent binary fetched by fetch-node-agent.sh.
COPY nke-agent /usr/local/bin/nke-agent
RUN chmod +x /usr/local/bin/nke-agent

# Install nke-agent.service. systemctl can't enable units during build (no
# running systemd), so symlink directly into multi-user.target.wants/.
COPY nke-agent.service /etc/systemd/system/nke-agent.service
RUN ln -s /etc/systemd/system/nke-agent.service \
          /etc/systemd/system/multi-user.target.wants/nke-agent.service

# Bake the CNI bundle into the image and serve it over loopback at runtime, so
# the nodes need no outbound DNS or internet during kubeadm init. Nested
# inside CI's Docker they may have neither, and the CNI install fails after
# kubeadm init has already succeeded — a failure the node-agent reports only
# in its journal.
#
# CNI_BUNDLE_VERSION must match the cniVersion in the inventory fixture used
# for the run: only the baked version can be served. Changing one means
# rebuilding this image with a matching value.
ARG CNI_BUNDLE_VERSION=3.31.4-1
ARG BUNDLE_SERVER_URL=https://nke-dev.s3.us-west-2.amazonaws.com
RUN BUNDLE_ARCH=$(case "${TARGETARCH:-amd64}" in arm64) echo aarch64 ;; amd64|"") echo x86_64 ;; *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; esac) && \
    BUNDLE_REL="nke_bundles/Ubuntu_22.04/${BUNDLE_ARCH}/calico_v${CNI_BUNDLE_VERSION}_${BUNDLE_ARCH}.tgz" && \
    mkdir -p "/opt/nke-bundles/nke_bundles/Ubuntu_22.04/${BUNDLE_ARCH}" && \
    wget -q -O /opt/nke-bundles/nke_bundles/ubuntu_22_index.json \
      "${BUNDLE_SERVER_URL}/nke_bundles/ubuntu_22_index.json" && \
    wget -q -O "/opt/nke-bundles/${BUNDLE_REL}" "${BUNDLE_SERVER_URL}/${BUNDLE_REL}" && \
    test -s "/opt/nke-bundles/${BUNDLE_REL}"

# Loopback file server for the baked bundles, plus the drop-in that points the
# agent at it. systemctl can't enable units during build, so symlink.
COPY nke-bundle-server.service /etc/systemd/system/nke-bundle-server.service
RUN ln -s /etc/systemd/system/nke-bundle-server.service \
          /etc/systemd/system/multi-user.target.wants/nke-bundle-server.service
COPY nke-agent-bundle-url.conf /etc/systemd/system/nke-agent.service.d/bundle-url.conf

# Keep kindest/node's entrypoint. It runs the iptables / cgroup / mount setup
# kubeadm + containerd depend on, then exec's systemd.
ENTRYPOINT ["/usr/local/bin/entrypoint", "/sbin/init"]
