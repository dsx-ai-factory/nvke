# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.
#
# Custom KinD/CAPD node image for local development. SPIRE agent.conf and
# initial-trust-bundle.pem are supplied at bootstrap by CWE (capi_bootstrap_userdata.go),
# not baked into this image. See README.md in this directory.
#
# The local TPM-backed Forge flow depends on this image satisfying three local
# TPM requirements:
#   1. present a Linux TPM device (`/dev/tpm0` or `/dev/tpmrm0`) so the
#      SPIRE TPM attestor can use its normal device-oriented path
#   2. leave a DER-encoded EK certificate on disk at `/var/lib/swtpm/ek.cert`
#      so nico-mock can surface Forge-compatible `tpmEkCertificate` data
#   3. start that vTPM plumbing before `spire-agent` and `nke-agent`, so CWE
#      preflight sees a SPIFFE-enabled node whose TPM identity is already ready

ARG KINDEST_NODE_TAG=v1.34.8
ARG CAPD_BASE_PLATFORM=linux/arm64
FROM --platform=${CAPD_BASE_PLATFORM} kindest/node:${KINDEST_NODE_TAG}

ARG NODE_AGENT_VERSION=v0.1.29
ARG NODE_AGENT_DIR_ARCH=arm64
ARG NODE_AGENT_PORT=15005
ARG CAPD_SPIRE_ATTESTATION_METHOD=tpm
ARG BUNDLE_SERVER_URL=https://d3mq564cn7sth7.cloudfront.net/
ARG INDEX_FILE_URL=https://d3mq564cn7sth7.cloudfront.net/nke_bundles/Ubuntu_24.04/index.json

RUN set -eux; \
    printf '%s\n' \
      '#!/bin/sh' \
      '# /sbin/reboot shim: reboot(2) sends SIGTERM to PID 1 inside a container, which' \
      '# systemd ignores. Instead, route through D-Bus so systemd shuts down gracefully' \
      '# and exits, triggering a Docker container restart (RestartPolicy: always).' \
      '# On restart, journalctl records a new boot entry, satisfying WaitForNodeUptime.' \
      'echo "reboot shim: sleeping 1s then systemctl poweroff for container restart"' \
      'sleep 1' \
      'systemctl poweroff' \
      > /usr/local/sbin/reboot-shim; \
    chmod 0755 /usr/local/sbin/reboot-shim; \
    ln -sf /usr/local/sbin/reboot-shim /sbin/reboot

# Install a kubeadm wrapper that applies container-node configuration on the
# fly, then delegates to the real kubeadm binary.
COPY scripts/kubeadm-systemverification-shim scripts/update-kubeadm-config-for-containers /usr/local/sbin/
RUN set -eux; \
    chmod 0755 \
      /usr/local/sbin/kubeadm-systemverification-shim \
      /usr/local/sbin/update-kubeadm-config-for-containers; \
    test -x /usr/bin/kubeadm; \
    mv /usr/bin/kubeadm /usr/bin/kubeadm.real; \
    ln -s /usr/local/sbin/kubeadm-systemverification-shim /usr/bin/kubeadm

# node-agent uses `cloud-init status` as its bootstrap completion contract.
# Mock NICo simulates cloud init by creating files and executing commands
# directly through docker after the container starts. It creates a text file
# with the state of this process. Our cloud-init mock script just prints this
# state so the node-agent can perform its check.
COPY scripts/cloud-init-status-shim /usr/local/bin/cloud-init
RUN chmod 0755 /usr/local/bin/cloud-init

# nke-agent CNI install runs scripts that expect `python` on PATH. CWE's
# kubeadm config runbooks also require PyYAML. The local TPM workflow
# additionally needs:
#   - `swtpm` to emulate the TPM itself
#   - `swtpm_setup` to initialize TPM state and emit an EK cert
#   - `tpm2-tools`/`openssl`/`xxd` for local inspection and helper scripts
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssl \
      python3 \
      python-is-python3 \
      python3-yaml \
      swtpm \
      swtpm-tools \
      tpm2-tools \
      xxd; \
    rm -rf /var/lib/apt/lists/*

# Docker build has no systemd as PID 1; agent-installer.sh still lays out units and binaries
# but may exit non-zero when enabling services. Enable/disable is done in a final RUN
# with SYSTEMD_OFFLINE=1 (see below). The build helper resolves the selected
# bundle into the Docker context before this build starts.
COPY agent.tgz /tmp/agent-src/
RUN set -eux; \
    NODE_AGENT_DIR="agent_${NODE_AGENT_VERSION}_${NODE_AGENT_DIR_ARCH}"; \
    cd /tmp/agent-src; \
    if ls ./*.tgz >/dev/null 2>&1; then tar xf ./*.tgz; fi; \
    cd "${NODE_AGENT_DIR}"; \
    ./agent-installer.sh || true; \
    test -f /etc/systemd/system/nke-agent.service; \
    cd /tmp && rm -rf /tmp/agent-src

# Install the SPIRE agent bundle, including the TPM attestor plugin.
COPY spire-agent.tgz /tmp/spire-agent-src/
RUN set -eux; \
    mkdir -p /tmp/spire-agent-bundle; \
    tar -xzf /tmp/spire-agent-src/spire-agent.tgz --strip-components=1 -C /tmp/spire-agent-bundle; \
    cd /tmp/spire-agent-bundle; \
    ./install.sh || true; \
    test -x /usr/local/bin/spire-agent || test -x /opt/spire/bin/spire-agent; \
    cd /tmp && rm -rf /tmp/spire-agent-src /tmp/spire-agent-bundle

# The bundle installer assumes a non-root agent user, but these disposable
# nodes receive their runtime config as root-owned cloud-init files. Run the
# agent as root. TPM-backed nodes also depend on the local vTPM; join-token
# nodes deliberately omit that dependency because CI runners do not expose
# the Colima-specific /dev/vtpmx device.
RUN set -eux; \
    mkdir -p /etc/systemd/system/spire-agent.service.d; \
    case "${CAPD_SPIRE_ATTESTATION_METHOD}" in \
      tpm) \
        printf '%s\n' \
          '[Unit]' \
          'Requires=swtpm-local.service' \
          'After=swtpm-local.service' \
          '' \
          '[Service]' \
          'User=root' \
          'Group=root' \
          'SupplementaryGroups=' \
          > /etc/systemd/system/spire-agent.service.d/override.conf \
        ;; \
      joinToken) \
        printf '%s\n' \
          '[Service]' \
          'User=root' \
          'Group=root' \
          'SupplementaryGroups=' \
          > /etc/systemd/system/spire-agent.service.d/override.conf \
        ;; \
      *) \
        echo "Unsupported CAPD_SPIRE_ATTESTATION_METHOD=${CAPD_SPIRE_ATTESTATION_METHOD}" >&2; \
        exit 1 \
        ;; \
    esac

COPY scripts/local-tpm-char-device scripts/local-start-vtpm-proxy scripts/local-init-tpm-state /usr/local/bin/
COPY systemd/swtpm-local.service /etc/systemd/system/
RUN set -eux; \
    install -d -m 0755 /var/lib/swtpm; \
    chmod 0755 \
      /usr/local/bin/local-tpm-char-device \
      /usr/local/bin/local-start-vtpm-proxy \
      /usr/local/bin/local-init-tpm-state; \
    chmod 0644 /etc/systemd/system/swtpm-local.service

# Host swap is visible inside the container (Docker doesn't namespace
# /proc/swaps), and we can't `swapoff` from inside an unprivileged container.
# Tell kubelet to ignore the host's swap state. The upstream kind image bakes
# this same flag via /etc/default/kubelet for the same reason.
RUN echo 'KUBELET_EXTRA_ARGS=--runtime-cgroups=/system.slice/containerd.service --fail-swap-on=false' \
      > /etc/default/kubelet

# Configure nke-agent.
RUN set -eux; \
    NKE_AGENT_SPIFFE_ARG=""; \
    if /usr/local/bin/nke-agent --help 2>&1 | grep -q -- "-enable-spiffe"; then \
      NKE_AGENT_SPIFFE_ARG=" --enable-spiffe"; \
    fi; \
    printf '%s\n' \
      "AGENT_ARGS=\"--disable-mlx5-rescan${NKE_AGENT_SPIFFE_ARG} --secure-server-port ${NODE_AGENT_PORT} --bundle-server-url ${BUNDLE_SERVER_URL} --index-file-url ${INDEX_FILE_URL}\"" \
      'NKE_LOCAL_DEV=true' \
      > /etc/default/nke-agent

# Route docker.io pulls through a local pull-through cache (started by `make registry-mirror`)
# to avoid Docker Hub rate limits and transient connectivity failures. Falls back to upstream
# if the mirror is unreachable.
RUN mkdir -p /etc/containerd/certs.d/docker.io && \
    cat > /etc/containerd/certs.d/docker.io/hosts.toml <<'EOF'
server = "https://registry-1.docker.io"

[host."http://registry-mirror:5000"]
  capabilities = ["pull", "resolve"]
EOF

# Route Kubernetes image pulls through a local registry.k8s.io pull-through
# cache. Mixed-version upgrades need source-version addons on target-version
# nodes before kubeadm upgrades the cluster-scoped addons.
RUN mkdir -p /etc/containerd/certs.d/registry.k8s.io && \
    cat > /etc/containerd/certs.d/registry.k8s.io/hosts.toml <<'EOF'
server = "https://registry.k8s.io"

[host."http://registry-k8s-mirror:5000"]
  capabilities = ["pull", "resolve"]
EOF

# Install scripts cannot enable units without a running systemd; offline mode
# creates /etc/systemd/system/multi-user.target.wants/ symlinks so first boot
# preserves the same startup order we need for the local TPM-backed flow.
RUN set -eux; \
    SYSTEMD_OFFLINE=1 systemctl daemon-reload; \
    SYSTEMD_OFFLINE=1 systemctl disable nke-attestation.service 2>/dev/null || true; \
    SYSTEMD_OFFLINE=1 systemctl enable nke-agent.service spire-agent.service; \
    if [ "${CAPD_SPIRE_ATTESTATION_METHOD}" = tpm ]; then \
      SYSTEMD_OFFLINE=1 systemctl enable swtpm-local.service; \
    else \
      SYSTEMD_OFFLINE=1 systemctl disable swtpm-local.service 2>/dev/null || true; \
    fi
