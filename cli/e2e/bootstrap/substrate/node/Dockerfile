# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Node image for the nkectl bootstrap-site E2E substrate.
#
# This is a deliberately thin derivation of the CAPD node image
# (controller/hack/capd-kindest-node/Dockerfile, built by `make capd-image`),
# which already provides everything a pre-provisioned NKE host needs:
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
# immediately start drifting, so we change exactly one thing.

ARG BASE_IMAGE=cwe-capd-kindest-node:local
FROM ${BASE_IMAGE}

ARG NODE_AGENT_PORT=15005

# The one difference between "a CAPD machine" and "a pre-provisioned host".
#
# A CAPD machine receives SPIRE config through the CAPI bootstrap secret at
# first boot, so its agent can demand mTLS. nkectl's bootstrap-site path talks
# to a pre-provisioned host that has already booted by the time it learns the
# host exists — there is no cloud-init hook left to deliver a trust bundle
# through — so bootstrap runs plaintext and the agent must not require mTLS.
#
# The flag is *injected into* the base image's AGENT_ARGS rather than replacing
# the line. Those other flags are load-bearing and easy to get wrong from
# memory: --index-file-url in particular short-circuits nke-agent's GetOSVersion
# check, which is Ubuntu-only and panics at startup on the Debian-based kindest
# base ("won't start agent without valid index"). Injecting means the base image
# stays the single source of truth for how to run the agent here, and a future
# flag added there is inherited rather than silently dropped.
#
# The port is unchanged: --disable-mtls swaps the listener's credentials, not
# its address (node-agent cmd/agent/main.go).
RUN set -eux; \
    test -f /etc/default/nke-agent; \
    grep -q '^AGENT_ARGS="' /etc/default/nke-agent; \
    sed -i 's/^AGENT_ARGS="/AGENT_ARGS="--disable-mtls /' /etc/default/nke-agent; \
    grep -q -- '--disable-mtls' /etc/default/nke-agent; \
    cat /etc/default/nke-agent

# Fail the build loudly if the base image ever stops serving on the port the
# substrate (and CWE's NODE_AGENT_PORT default) expects, rather than letting it
# surface as an unexplained readiness timeout.
RUN grep -q -- "--secure-server-port ${NODE_AGENT_PORT}" /etc/default/nke-agent

# spire-agent and swtpm are enabled in the base image because CAPD nodes attest
# via TPM. A pre-provisioned bootstrap node has no SPIRE identity, and
# spire-agent would crash-loop without an agent.conf. Disable both so the
# journal stays readable and `systemctl is-active nke-agent` is unambiguous.
RUN set -eux; \
    SYSTEMD_OFFLINE=1 systemctl disable spire-agent.service 2>/dev/null || true; \
    SYSTEMD_OFFLINE=1 systemctl disable swtpm-local.service 2>/dev/null || true

# Keep the base image's entrypoint. kindest/node's entrypoint performs the
# iptables / cgroup / mount setup kubeadm and containerd depend on before
# exec'ing systemd; replacing it surfaces much later as "no route to host"
# during kubeadm join.
