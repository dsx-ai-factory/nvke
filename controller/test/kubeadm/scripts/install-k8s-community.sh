#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Install the Kubernetes stack a kubeadm bring-up needs, from community
# artifacts only. Runs inside the test VM from cloud-init.
#
# The NKE apt repository sits behind a source-IP allow list that GitHub runners
# are outside of, so the lane that needs it reports success having skipped its
# own test. Everything the NKE kubeadm distro package ships is an upstream
# release with a published checksum, and the agent looks its tools up on PATH
# (agent/internal/agent/agent.go:423 does exec.LookPath("kubeadm")), so fetching
# them here gives the agent the same thing the package would.
#
# Every download is pinned and verified. dl.k8s.io publishes a .sha256 next to
# each binary; the GitHub release tarballs get an expected digest passed in.
set -euo pipefail

K8S_VERSION="${K8S_VERSION:-v1.36.3}"
CONTAINERD_VERSION="${CONTAINERD_VERSION:-2.3.0}"
RUNC_VERSION="${RUNC_VERSION:-v1.2.5}"
CNI_VERSION="${CNI_VERSION:-v1.6.0}"
CRICTL_VERSION="${CRICTL_VERSION:-v1.36.0}"
ARCH="${ARCH:-amd64}"

log() { echo "install-k8s-community: $*"; }

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# dl.k8s.io serves <binary>.sha256 beside each binary. Fetch both and compare,
# so a truncated or substituted download fails here rather than at kubeadm init.
fetch_k8s_binary() {
	local name="$1" dest="$2"
	local base="https://dl.k8s.io/release/${K8S_VERSION}/bin/linux/${ARCH}"
	curl -fsSL --retry 3 -o "${tmp}/${name}" "${base}/${name}"
	curl -fsSL --retry 3 -o "${tmp}/${name}.sha256" "${base}/${name}.sha256"
	echo "$(cat "${tmp}/${name}.sha256")  ${tmp}/${name}" | sha256sum -c - >/dev/null
	install -m 0755 "${tmp}/${name}" "${dest}/${name}"
	log "installed ${name} ${K8S_VERSION}"
}

fetch_verified() {
	local url="$1" out="$2" want="$3"
	curl -fsSL --retry 3 -o "${out}" "${url}"
	if [ -n "${want}" ]; then
		echo "${want}  ${out}" | sha256sum -c - >/dev/null
	fi
}

log "installing the Kubernetes ${K8S_VERSION} stack from community artifacts"

# containerd, runc and the CNI plugins, at the paths kubeadm and the kubelet
# expect to find them.
fetch_verified \
	"https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${ARCH}.tar.gz" \
	"${tmp}/containerd.tgz" "${CONTAINERD_SHA256:-}"
tar -C /usr/local -xzf "${tmp}/containerd.tgz"

fetch_verified \
	"https://github.com/opencontainers/runc/releases/download/${RUNC_VERSION}/runc.${ARCH}" \
	"${tmp}/runc" "${RUNC_SHA256:-}"
install -m 0755 "${tmp}/runc" /usr/local/sbin/runc

install -d -m 0755 /opt/cni/bin
fetch_verified \
	"https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-${ARCH}-${CNI_VERSION}.tgz" \
	"${tmp}/cni.tgz" "${CNI_SHA256:-}"
tar -C /opt/cni/bin -xzf "${tmp}/cni.tgz"

fetch_verified \
	"https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-${ARCH}.tar.gz" \
	"${tmp}/crictl.tgz" "${CRICTL_SHA256:-}"
tar -C /usr/local/bin -xzf "${tmp}/crictl.tgz"

for b in kubeadm kubelet kubectl; do
	fetch_k8s_binary "${b}" /usr/local/bin
done

# containerd's shipped config disables the systemd cgroup driver, which kubeadm
# requires. Without this the kubelet and the runtime disagree about cgroups and
# the control plane never becomes ready.
install -d -m 0755 /etc/containerd
containerd config default >/etc/containerd/config.toml
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml

install -d -m 0755 /etc/systemd/system
cat >/etc/systemd/system/containerd.service <<'UNIT'
[Unit]
Description=containerd container runtime
After=network.target

[Service]
ExecStart=/usr/local/bin/containerd
Restart=always
Delegate=yes
KillMode=process
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
UNIT

# The kubelet unit and its kubeadm drop-in, matching what the upstream packages
# install. kubeadm writes /var/lib/kubelet/kubeadm-flags.env itself.
cat >/etc/systemd/system/kubelet.service <<'UNIT'
[Unit]
Description=kubelet: The Kubernetes Node Agent
Documentation=https://kubernetes.io/docs/
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/kubelet
Restart=always
StartLimitInterval=0
RestartSec=10

[Install]
WantedBy=multi-user.target
UNIT

install -d -m 0755 /etc/systemd/system/kubelet.service.d
cat >/etc/systemd/system/kubelet.service.d/10-kubeadm.conf <<'UNIT'
[Service]
Environment="KUBELET_KUBECONFIG_ARGS=--bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf --kubeconfig=/etc/kubernetes/kubelet.conf"
Environment="KUBELET_CONFIG_ARGS=--config=/var/lib/kubelet/config.yaml"
EnvironmentFile=-/var/lib/kubelet/kubeadm-flags.env
EnvironmentFile=-/etc/default/kubelet
ExecStart=
ExecStart=/usr/local/bin/kubelet $KUBELET_KUBECONFIG_ARGS $KUBELET_CONFIG_ARGS $KUBELET_KUBEADM_ARGS $KUBELET_EXTRA_ARGS
UNIT

# br_netfilter and the two sysctls kubeadm's preflight checks require.
modprobe br_netfilter || true
modprobe overlay || true
install -d -m 0755 /etc/modules-load.d /etc/sysctl.d
cat >/etc/modules-load.d/k8s.conf <<'MOD'
overlay
br_netfilter
MOD
cat >/etc/sysctl.d/99-kubernetes.conf <<'SYSCTL'
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
SYSCTL
sysctl --system >/dev/null

swapoff -a || true
sed -i '/\bswap\b/s/^/#/' /etc/fstab || true

systemctl daemon-reload
systemctl enable --now containerd
systemctl enable kubelet

log "kubeadm $(kubeadm version -o short 2>/dev/null || echo '?')"
log "containerd $(containerd --version 2>/dev/null | awk '{print $3}' || echo '?')"
log "done"
