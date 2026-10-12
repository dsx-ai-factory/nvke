# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Node image for the nvkectl bootstrap-site E2E substrate.
#
# This is a deliberately thin derivation of the CAPD node image
# (controller/hack/capd-kindest-node/Dockerfile, built by `make capd-image`),
# which already provides everything a pre-provisioned NVKE host needs:
#
#   * nke-agent installed with its systemd unit, configured via
#     /etc/default/nke-agent
#   * spire-agent including the TPM attestor plugin
#   * a software TPM with a DER-encoded EK certificate at /var/lib/swtpm/ek.cert
#   * a kubeadm wrapper that skips the container-only SystemVerification failure
#   * kubelet --fail-swap-on=false, a /sbin/reboot shim, and pull-through
#     registry mirrors for docker.io and registry.k8s.io
#
# Rebuilding that from scratch would duplicate a lot of hard-won detail and
# immediately start drifting, so this image only adds the site SPIRE config.

ARG BASE_IMAGE=cwe-capd-kindest-node:local
FROM ${BASE_IMAGE}

ARG NODE_AGENT_PORT=15005

# Fail the build loudly if the base image ever stops serving on the port the
# substrate (and the controller's NODE_AGENT_PORT default) expects, rather than letting it
# surface as an unexplained readiness timeout.
RUN grep -q -- "--secure-server-port ${NODE_AGENT_PORT}" /etc/default/nke-agent
RUN ! grep -q -- '--disable-mtls' /etc/default/nke-agent

# These hosts are already provisioned. The CAPD base image has a cloud-init
# status shim (controller/hack/capd-kindest-node/scripts/cloud-init-status-shim)
# that reads /var/lib/nico-containers/cloud-init/status. This suite has no
# nico-sim process to write that file, so mark setup complete here or the controller's
# NodePreflight stays on "cloud-init not done: status: running" forever.
RUN install -d /var/lib/nico-containers/cloud-init && \
    printf 'done\n' >/var/lib/nico-containers/cloud-init/status

# This suite runs Calico; the base image's OVS uplink bridge is for OVN Geneve
# and leaves these nodes unreachable on the substrate network.
RUN SYSTEMD_OFFLINE=1 systemctl disable nke-container-network.service

# SPIRE.
#
# The base image already installs spire-agent, the TPM attestor plugin and a
# software TPM, because CAPD nodes attest via TPM. All this has to do is stop
# disabling them and drop in the config and trust anchor.
#
# The result is a host that already has a hardware identity and is already
# looking for its SPIRE server — which is what a pre-provisioned machine looks
# like by the time nvkectl hears about it. The server does not exist yet;
# spire-agent retries until `nvkectl bootstrap site` puts one where agent.conf
# says. That is why the E2E has nothing to configure and nothing to start: it
# reads the fingerprints, lets the command deploy SPIRE, and checks the agents
# got in.
#
#   agent.conf         rendered from node/agent.conf.tmpl by image.go, which is
#                      where the server address and trust domain are decided
#   bootstrap-ca.crt   the CA the suite generated and also handed to the
#                      command, which chains SPIRE's own CA to it — so the
#                      agent verifies the server rather than trusting it on
#                      first use
#   get_tpm_pubhash    the upstream spire-tpm-plugin tool, used to read the EK
#                      fingerprint the server's allowlist is keyed on. The same
#                      tool the DSX ansible runbook reads over SSH.
#
# data_dir is on /run, a tmpfs, so no agent state survives a restart: a cached
# SVID is signed by a SPIRE CA that will not exist next run, and the agent would
# fail rather than re-attest.
COPY agent.conf /etc/spire-agent/agent.conf
COPY tools/ca.crt /etc/spire-agent/bootstrap-ca.crt
COPY tools/get_tpm_pubhash /usr/local/bin/get_tpm_pubhash
RUN set -eux; \
    chmod 0644 /etc/spire-agent/agent.conf /etc/spire-agent/bootstrap-ca.crt; \
    chmod 0755 /usr/local/bin/get_tpm_pubhash; \
    grep -q TPM_PLUGIN_CHECKSUM= /etc/spire-agent/spire-agent.env; \
    SYSTEMD_OFFLINE=1 systemctl enable swtpm-local.service spire-agent.service

# Keep the base image's entrypoint. kindest/node's entrypoint performs the
# iptables / cgroup / mount setup kubeadm and containerd depend on before
# exec'ing systemd; replacing it surfaces much later as "no route to host"
# during kubeadm join.
