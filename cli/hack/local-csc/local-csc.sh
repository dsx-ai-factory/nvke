#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# A local single-node CSC (site controller) for development: one container
# standing in for a pre-provisioned host, bootstrapped by
# `nvkectl bootstrap site --disable-mtls`. See README.md in this directory.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
# shellcheck source=../../../hack/read-tool-version.sh
. "${REPO}/hack/read-tool-version.sh"

: "${COLIMA_PROFILE:=nvke}"
: "${VM_CPUS:=6}"
: "${VM_MEMORY:=12}"
: "${VM_DISK:=100}"
: "${STATE_DIR:=$(cd "${REPO}/.." && pwd)/nvke-dev}"
: "${CLUSTER_NAME:=csc-local}"
: "${NODE_NAME:=csc-node-0}"
: "${NODE_IP:=10.4.1.10}"
: "${K8S_VERSION:=1.34.8}"

BOOTSTRAP_CLUSTER=nvkectl-bootstrap
MGMT_CLUSTER=csc-mgmt
AGENT_PORT=15005
NODE_IMAGE="cwe-capd-kindest-node:local"
CONTROLLER_IMAGE="nke-controller:latest"
PROVIDER_IMAGE="cluster-api-provider-provisioned:latest"
NVKECTL="${REPO}/cli/bin/nvkectl"
# prepare-calico-assets.sh pins this version.
CALICO_VERSION=3.30.1
CALICO_PACKAGE="nke-cni-calico-manifests-${CALICO_VERSION}"
CALICO_DEB="${STATE_DIR}/cni/${CALICO_PACKAGE}_${CALICO_VERSION}_all.deb"
KUBECONFIG_OUT="${STATE_DIR}/${CLUSTER_NAME}.kubeconfig"
ETCD_KEY_OUT="${STATE_DIR}/${CLUSTER_NAME}-etcd-encryption-key.json"
CNI_LEASE_DIR=/var/lib/cni/networks/kind

export PATH="/usr/local/go/bin:${PATH}"

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'local-csc: %s\n' "$*" >&2; exit 1; }
kind_() { sudo env KIND_EXPERIMENTAL_PROVIDER=nerdctl kind "$@"; }
node_exec() { sudo nerdctl exec "${NODE_NAME}" "$@"; }
k_boot() { sudo kubectl --context "kind-${BOOTSTRAP_CLUSTER}" "$@"; }
k_mgmt() { sudo kubectl --context "kind-${MGMT_CLUSTER}" "$@"; }
k_csc() { sudo kubectl --kubeconfig "${KUBECONFIG_OUT}" "$@"; }

require_linux() {
  [[ "$(uname -s)" == Linux ]] ||
    die "'$1' runs inside the VM: colima ssh --profile ${COLIMA_PROFILE}, then run it in tmux"
}

kind_cluster_exists() {
  local clusters
  clusters="$(kind_ get clusters 2>/dev/null || true)"
  grep -qx "$1" <<<"${clusters}"
}
container_exists() { sudo nerdctl inspect "$1" >/dev/null 2>&1; }
image_exists() { sudo nerdctl image inspect "$1" >/dev/null 2>&1; }

usage() {
  cat <<EOF
Usage: $(basename "$0") <command>

On the Mac:
  vm-up       create or start the Colima VM '${COLIMA_PROFILE}', then run 'provision' in it

In the VM (colima ssh --profile ${COLIMA_PROFILE}), preferably in tmux:
  provision   install the repository's pinned tools and kernel settings (idempotent)
  build       build nvkectl, the controller and provider images, and the node image
  up          start the node and run 'nvkectl bootstrap site' against it
  status      show the node, the kind clusters, and the CAPI objects
  import      copy the CSC's CAPI objects into a fresh management cluster '${MGMT_CLUSTER}'
  down        delete the node, both kind clusters, and the CSC credentials

State (site spec, kubeconfig, etcd key, logs) goes to STATE_DIR=${STATE_DIR}.
It holds credentials; keep it outside the repository.
EOF
}

# ---------------------------------------------------------------------- Mac

cmd_vm_up() {
  [[ "$(uname -s)" == Darwin ]] || die "vm-up runs on the Mac"
  mkdir -p "${STATE_DIR}"
  if colima status --profile "${COLIMA_PROFILE}" >/dev/null 2>&1; then
    log "Colima profile ${COLIMA_PROFILE} is already running"
  elif [[ -d "${HOME}/.colima/${COLIMA_PROFILE}" ]]; then
    colima start --profile "${COLIMA_PROFILE}"
  else
    # sshfs rather than virtiofs: virtiofs has truncated large image archives.
    colima start --profile "${COLIMA_PROFILE}" \
      --runtime containerd --arch "$(uname -m | sed 's/arm64/aarch64/')" --vm-type vz \
      --cpu "${VM_CPUS}" --memory "${VM_MEMORY}" --disk "${VM_DISK}" \
      --mount-type sshfs --mount "${REPO}:w" --mount "${STATE_DIR}:w"
  fi
  colima ssh --profile "${COLIMA_PROFILE}" -- bash "${SCRIPT_DIR}/local-csc.sh" provision
  log "VM ready. Next: colima ssh --profile ${COLIMA_PROFILE}; tmux new -s csc; ${SCRIPT_DIR}/local-csc.sh build"
}

# ----------------------------------------------------------------- provision

fetch() {
  local url="$1" sha="$2" dest="$3"
  [[ -n "${sha}" ]] || die "no checksum for ${url}"
  curl -fsSL --retry 5 -o "${dest}" "${url}"
  echo "${sha}  ${dest}" | sha256sum -c --quiet -
}

install_tools() {
  local arch pin_arch tmp
  arch="$(dpkg --print-architecture)"
  pin_arch="${arch^^}"
  tmp="$(mktemp -d)"

  local go_version kind_version kubectl_version capi_version helm_version
  go_version="$(awk '/^go /{print $2; exit}' "${REPO}/go.work")"
  kind_version="$(read_tool_version KIND_VERSION)"
  kubectl_version="$(read_tool_version KUBECTL_VERSION)"
  capi_version="$(read_tool_version CAPI_VERSION)"
  helm_version="$(read_tool_version HELM_VERSION)"

  if [[ "$(/usr/local/go/bin/go version 2>/dev/null)" != *"go${go_version} "* ]]; then
    log "installing Go ${go_version}"
    local go_tgz="go${go_version}.linux-${arch}.tar.gz" go_sha
    go_sha="$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' |
      jq -r --arg f "${go_tgz}" '.[].files[] | select(.filename == $f) | .sha256')"
    fetch "https://go.dev/dl/${go_tgz}" "${go_sha}" "${tmp}/${go_tgz}"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "${tmp}/${go_tgz}"
  fi
  if [[ "$(kind version 2>/dev/null)" != *"${kind_version} "* ]]; then
    log "installing kind ${kind_version}"
    fetch "https://kind.sigs.k8s.io/dl/${kind_version}/kind-linux-${arch}" \
      "$(read_tool_version "KIND_SHA256_${pin_arch}")" "${tmp}/kind"
    sudo install -m 0755 "${tmp}/kind" /usr/local/bin/kind
  fi
  if [[ "$(kubectl version --client -o json 2>/dev/null | jq -r .clientVersion.gitVersion)" != "${kubectl_version}" ]]; then
    log "installing kubectl ${kubectl_version}"
    fetch "https://dl.k8s.io/release/${kubectl_version}/bin/linux/${arch}/kubectl" \
      "$(read_tool_version "KUBECTL_SHA256_${pin_arch}")" "${tmp}/kubectl"
    sudo install -m 0755 "${tmp}/kubectl" /usr/local/bin/kubectl
  fi
  if [[ "$(clusterctl version -o short 2>/dev/null)" != "${capi_version}" ]]; then
    log "installing clusterctl ${capi_version}"
    fetch "https://github.com/kubernetes-sigs/cluster-api/releases/download/${capi_version}/clusterctl-linux-${arch}" \
      "$(read_tool_version "CLUSTERCTL_SHA256_${pin_arch}")" "${tmp}/clusterctl"
    sudo install -m 0755 "${tmp}/clusterctl" /usr/local/bin/clusterctl
  fi
  if [[ "$(helm version --short 2>/dev/null)" != "${helm_version}"* ]]; then
    log "installing helm ${helm_version}"
    local helm_tgz="helm-${helm_version}-linux-${arch}.tar.gz"
    fetch "https://get.helm.sh/${helm_tgz}" \
      "$(curl -fsSL "https://get.helm.sh/${helm_tgz}.sha256sum" | awk '{print $1}')" "${tmp}/${helm_tgz}"
    tar -C "${tmp}" -xzf "${tmp}/${helm_tgz}" "linux-${arch}/helm"
    sudo install -m 0755 "${tmp}/linux-${arch}/helm" /usr/local/bin/helm
  fi
  rm -rf "${tmp}"
}

add_safe_directory() {
  local -a git_cmd=("$@" git config --global)
  local dirs
  dirs="$("${git_cmd[@]}" --get-all safe.directory 2>/dev/null || true)"
  grep -qx "${REPO}" <<<"${dirs}" || "${git_cmd[@]}" --add safe.directory "${REPO}"
}

cmd_provision() {
  require_linux provision
  log "base packages and kernel settings"
  sudo apt-get update -qq
  sudo apt-get install -y -qq make git curl jq ca-certificates tmux >/dev/null
  sudo tee /etc/sysctl.d/99-local-csc.conf >/dev/null <<'EOF'
fs.inotify.max_user_instances=1024
fs.inotify.max_user_watches=1048576
fs.file-max=2097152
net.ipv4.ip_forward=1
EOF
  sudo sysctl --system >/dev/null

  install_tools

  # nerdctl, kind and nvkectl must run as root; sudo has to find Go too.
  local sudoers
  sudoers="$(mktemp)"
  printf 'Defaults secure_path="/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"\n' >"${sudoers}"
  sudo visudo -cf "${sudoers}" >/dev/null
  sudo install -m 0440 "${sudoers}" /etc/sudoers.d/90-local-csc
  rm -f "${sudoers}"
  # Without these, git rejects the mounted repo and make stamps nvkectl v0.0.0.
  add_safe_directory
  add_safe_directory sudo

  sudo nerdctl version >/dev/null || die "rootful nerdctl does not work"
  pgrep -x buildkitd >/dev/null || die "buildkitd is not running"
  log "provisioned: go $(go env GOVERSION), $(kind version | cut -d' ' -f2), clusterctl $(clusterctl version -o short)"
}

# --------------------------------------------------------------------- build

cmd_build() {
  require_linux build
  log "nvkectl"
  make -C "${REPO}/cli" build
  log "controller and provisioned-provider images"
  sudo make -C "${REPO}/cli" images
  if [[ ! -f "${CALICO_DEB}" ]]; then
    log "Calico manifests package"
    bash "${REPO}/controller/hack/ci/prepare-calico-assets.sh" "${STATE_DIR}/cni"
  fi
  if [[ "${REBUILD_NODE_IMAGE:-}" == 1 ]] && image_exists "${NODE_IMAGE}"; then
    sudo nerdctl rmi -f "${NODE_IMAGE}"
  fi
  log "node image (kept if present; REBUILD_NODE_IMAGE=1 rebuilds it)"
  sudo make -C "${REPO}/cli" ensure-capd-image CAPD_CNI_MANIFESTS_DEB_PATH="${CALICO_DEB}"
}

# ------------------------------------------------------------------------ up

ip_to_int() {
  local a b c d
  IFS=. read -r a b c d <<<"$1"
  echo $(((a << 24) | (b << 16) | (c << 8) | d))
}

in_cidr() {
  local ip="$1" net="${2%/*}" bits="${2#*/}" mask
  mask=$(((0xFFFFFFFF << (32 - bits)) & 0xFFFFFFFF))
  (($(($(ip_to_int "${ip}") & mask)) == $(($(ip_to_int "${net}") & mask))))
}

ensure_kind_network() {
  if ! sudo nerdctl network inspect kind >/dev/null 2>&1; then
    # kind creates the network with its own settings; the node must join it before bootstrap.
    log "creating the kind network with a throwaway cluster"
    kind_ create cluster --name local-csc-netinit --wait 0s
    kind_ delete cluster --name local-csc-netinit
  fi
  local subnet
  subnet="$(sudo nerdctl network inspect kind | jq -r '.[0].IPAM.Config[].Subnet' | grep -v : | head -1 || true)"
  [[ -n "${subnet}" ]] || die "kind network has no IPv4 subnet"
  in_cidr "${NODE_IP}" "${subnet}" || die "NODE_IP=${NODE_IP} is outside the kind network ${subnet}"
}

# A lease outlives its container across VM restarts and blocks a static --ip.
release_stale_lease() {
  local lease="${CNI_LEASE_DIR}/${NODE_IP}" owner
  sudo test -f "${lease}" || return 0
  owner="$(sudo head -1 "${lease}")"
  if [[ -n "${owner}" ]] && container_exists "${owner}"; then
    die "${NODE_IP} is held by container ${owner}; set NODE_IP or run down"
  fi
  sudo rm -f "${lease}"
}

write_sitespec() {
  printf 'name,ip,tpm_ek_thumbprint\n%s,%s,\n' "${NODE_NAME}" "${NODE_IP}" >"${STATE_DIR}/hosts.csv"
  "${NVKECTL}" generate sitespec "${CLUSTER_NAME}" --nodes "${STATE_DIR}/hosts.csv" \
    --set "cluster.k8sVersion=${K8S_VERSION}" \
    --set cluster.cniName=calico --set "cluster.cniVersion=${CALICO_VERSION}" \
    --set cluster.podCidr=192.168.0.0/16 --set cluster.serviceCidr=10.96.0.0/12 \
    --set cluster.kubeletConfig.systemReservedMemory=512Mi \
    >"${STATE_DIR}/sitespec.yaml"
}

start_node() {
  log "starting ${NODE_NAME} at ${NODE_IP}"
  sudo nerdctl run -d --name "${NODE_NAME}" --hostname "${NODE_NAME}" \
    --privileged --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
    --tty --restart unless-stopped \
    --network kind --ip "${NODE_IP}" \
    --volume /lib/modules:/lib/modules:ro \
    --volume "${NODE_NAME}-var:/var" --volume "${NODE_NAME}-containerd:/var/lib/containerd" \
    --volume /dev/null:/proc/swaps:ro \
    --volume "${SCRIPT_DIR}/node-init.sh:/local-csc-node-init.sh:ro" \
    --tmpfs /tmp --tmpfs /run --tmpfs /var/lib/etcd:size=1g \
    --env "NODE_NAME=${NODE_NAME}" \
    --entrypoint /bin/bash \
    "${NODE_IMAGE}" /local-csc-node-init.sh >/dev/null
}

wait_node() {
  local state=""
  for _ in $(seq 60); do
    state="$(node_exec systemctl is-system-running 2>/dev/null || true)"
    [[ "${state}" == running || "${state}" == degraded ]] && break
    sleep 2
  done
  if [[ "${state}" != running ]]; then
    node_exec systemctl --failed --no-legend || true
    die "${NODE_NAME} systemd is '${state:-unreachable}', not running"
  fi
  node_exec sh -c "ss -tln | grep -q ':${AGENT_PORT} '" || die "nke-agent is not listening on ${AGENT_PORT}"
  [[ "$(node_exec cloud-init status)" == *done* ]] || die "cloud-init status is not done"
  [[ "$(node_exec cat /proc/sys/net/ipv4/ip_forward)" == 1 ]] || die "ip_forward is not 1 in the node"
  node_exec dpkg -s "${CALICO_PACKAGE}" >/dev/null 2>&1 || die "${CALICO_PACKAGE} is not installed in the node image"
  log "${NODE_NAME} is ready"
}

machine_name() {
  sudo kubectl --context "$1" -n nke-system get machines.cluster.x-k8s.io \
    -o jsonpath='{.items[0].metadata.name}'
}

# CAPI can look for the Node a second before the provider stamps its
# providerID and not look again; a metadata change requeues the Machine.
wait_noderef() {
  local context="$1" machine="$2" ref="" i
  local -a k=(sudo kubectl --context "${context}" -n nke-system)
  for i in $(seq 48); do
    ref="$("${k[@]}" get machine.cluster.x-k8s.io "${machine}" -o jsonpath='{.status.nodeRef.name}' 2>/dev/null || true)"
    [[ -n "${ref}" ]] && break
    if ((i == 12 || i == 30)); then
      log "Machine ${machine} has no nodeRef yet; requeueing it"
      "${k[@]}" annotate machine.cluster.x-k8s.io "${machine}" local-csc/requeue="$(date +%s)" --overwrite >/dev/null
      "${k[@]}" annotate cluster.cluster.x-k8s.io "${CLUSTER_NAME}" local-csc/requeue="$(date +%s)" --overwrite >/dev/null
    fi
    sleep 5
  done
  "${k[@]}" annotate machine.cluster.x-k8s.io "${machine}" local-csc/requeue- >/dev/null 2>&1 || true
  "${k[@]}" annotate cluster.cluster.x-k8s.io "${CLUSTER_NAME}" local-csc/requeue- >/dev/null 2>&1 || true
  [[ -n "${ref}" ]] || die "Machine ${machine} still has no nodeRef in ${context}"
  log "Machine ${machine} -> Node ${ref}"
}

cmd_up() {
  require_linux up
  [[ -x "${NVKECTL}" ]] || die "no ${NVKECTL}; run build"
  for image in "${NODE_IMAGE}" "${CONTROLLER_IMAGE}" "${PROVIDER_IMAGE}"; do
    image_exists "${image}" || die "image ${image} is missing; run build"
  done
  if container_exists "${NODE_NAME}" || kind_cluster_exists "${BOOTSTRAP_CLUSTER}"; then
    die "a CSC already exists; run down first"
  fi
  [[ ! -e "${ETCD_KEY_OUT}" ]] || die "${ETCD_KEY_OUT} exists and bootstrap refuses to overwrite it; run down"
  mkdir -p "${STATE_DIR}"

  ensure_kind_network
  release_stale_lease
  write_sitespec
  start_node
  wait_node

  log "bootstrapping ${CLUSTER_NAME} (10-20 minutes; log: ${STATE_DIR}/bootstrap.log)"
  (cd "${STATE_DIR}" && sudo "${NVKECTL}" bootstrap site -f sitespec.yaml \
    --disable-mtls --keep-bootstrap-cluster \
    --kubeconfig-out "${KUBECONFIG_OUT}" --encryption-key-out "${ETCD_KEY_OUT}" \
    --diagnostics-dir "${STATE_DIR}/diagnostics") 2>&1 | tee "${STATE_DIR}/bootstrap.log"

  wait_noderef "kind-${BOOTSTRAP_CLUSTER}" "$(machine_name "kind-${BOOTSTRAP_CLUSTER}")"
  cmd_status
}

# -------------------------------------------------------------------- status

cmd_status() {
  require_linux status
  log "containers"
  sudo nerdctl ps -a --format '{{.Names}}  {{.Status}}'
  log "kind clusters"
  kind_ get clusters 2>/dev/null || true
  if [[ -f "${KUBECONFIG_OUT}" ]]; then
    log "CSC nodes (${KUBECONFIG_OUT})"
    k_csc get nodes -o wide || true
  fi
  local cluster
  for cluster in "${BOOTSTRAP_CLUSTER}" "${MGMT_CLUSTER}"; do
    kind_cluster_exists "${cluster}" || continue
    log "CAPI objects in ${cluster}"
    sudo kubectl --context "kind-${cluster}" -n nke-system \
      get clusters.cluster.x-k8s.io,machines.cluster.x-k8s.io,provisionedmachines 2>/dev/null || true
  done
}

# -------------------------------------------------------------------- import

cmd_import() {
  require_linux import
  kind_cluster_exists "${BOOTSTRAP_CLUSTER}" || die "no ${BOOTSTRAP_CLUSTER} cluster to import from; run up"
  local machine dir="${STATE_DIR}/import" capi_version archive
  machine="$(machine_name "kind-${BOOTSTRAP_CLUSTER}")"
  capi_version="$(read_tool_version CAPI_VERSION)"
  mkdir -p "${dir}"

  log "saving the source controller's view and the CAPI objects to ${dir}"
  curl -sf http://127.0.0.1:30080/api/clusters | jq . >"${dir}/cwe-clusters.json"
  curl -sf "http://127.0.0.1:30080/api/nodes?clusterName=${CLUSTER_NAME}" | jq . >"${dir}/cwe-nodes.json"
  k_csc get node "${machine}" -o yaml >"${dir}/node-before.yaml"
  k_boot -n nke-system get "cluster.cluster.x-k8s.io/${CLUSTER_NAME}" "provisionedcluster/${CLUSTER_NAME}" \
    "machine.cluster.x-k8s.io/${machine}" "provisionedmachine/${machine}" \
    "secret/${CLUSTER_NAME}-kubeconfig" "secret/${machine}-bootstrap-data" -o json |
    jq --arg ip "${NODE_IP}" '.items |= map(
        del(.status, .metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp,
            .metadata.generation, .metadata.managedFields, .metadata.ownerReferences, .metadata.finalizers)
        | if .kind == "Cluster"
          then .spec.paused = true | .spec.controlPlaneEndpoint = {host: $ip, port: 6443}
          else . end)' >"${dir}/${CLUSTER_NAME}-capi.json"

  log "pausing the source Cluster so only one management cluster acts on the node"
  k_boot -n nke-system patch cluster.cluster.x-k8s.io "${CLUSTER_NAME}" --type merge -p '{"spec":{"paused":true}}'

  if ! kind_cluster_exists "${MGMT_CLUSTER}"; then
    log "creating ${MGMT_CLUSTER}"
    kind_ create cluster --name "${MGMT_CLUSTER}" --wait 3m
  fi
  if ! k_mgmt -n capi-system get deployment capi-controller-manager >/dev/null 2>&1; then
    log "installing Cluster API ${capi_version}"
    sudo clusterctl init --kubeconfig-context "kind-${MGMT_CLUSTER}" \
      --core "cluster-api:${capi_version}" --bootstrap "kubeadm:${capi_version}" \
      --control-plane "kubeadm:${capi_version}" --wait-providers
  fi
  log "installing the provisioned provider"
  # Root creates it: fs.protected_regular stops root opening a user's file in /tmp.
  archive="$(sudo mktemp)"
  sudo nerdctl save -o "${archive}" "${PROVIDER_IMAGE}"
  kind_ load image-archive "${archive}" --name "${MGMT_CLUSTER}"
  sudo rm -f "${archive}"
  k_mgmt apply --server-side -f "${REPO}/cli/internal/bootstrap/controllerdeploy/manifests/provisioned-provider.yaml" >/dev/null
  k_mgmt -n provisioned-system rollout status deploy/provisioned-controller-manager --timeout 3m

  log "importing ${CLUSTER_NAME}"
  k_mgmt create namespace nke-system --dry-run=client -o yaml | k_mgmt apply -f - >/dev/null
  k_mgmt apply --server-side -f "${dir}/${CLUSTER_NAME}-capi.json"
  k_mgmt -n nke-system patch cluster.cluster.x-k8s.io "${CLUSTER_NAME}" --type merge -p '{"spec":{"paused":false}}'
  wait_noderef "kind-${MGMT_CLUSTER}" "${machine}"

  k_csc get node "${machine}" -o yaml >"${dir}/node-after.yaml"
  if diff <(grep -E 'providerID|cluster\.x-k8s\.io' "${dir}/node-before.yaml") \
    <(grep -E 'providerID|cluster\.x-k8s\.io' "${dir}/node-after.yaml"); then
    log "node ${machine} is unchanged by the import"
  else
    log "WARNING: the node's providerID or CAPI annotations changed (diff above)"
  fi
  cmd_status
  log "never kubectl delete the imported objects; run down to remove ${MGMT_CLUSTER}"
}

# ---------------------------------------------------------------------- down

remove_container() {
  container_exists "$1" || return 0
  sudo nerdctl stop -t 30 "$1" >/dev/null 2>&1 || true
  # rm can race runc over a kind node's nested cgroups; the second try lands.
  sudo nerdctl rm -f -v "$1" >/dev/null 2>&1 || { sleep 5; sudo nerdctl rm -f -v "$1" >/dev/null; }
}

delete_kind_cluster() {
  local name="$1" node
  kind_cluster_exists "${name}" || return 0
  log "deleting kind cluster ${name}"
  if ! kind_ delete cluster --name "${name}"; then
    # A node left dead by a VM restart fails `kind delete` with "no running task".
    while read -r node; do
      [[ -n "${node}" ]] && remove_container "${node}"
    done < <(sudo nerdctl ps -a --filter "label=io.x-k8s.kind.cluster=${name}" --format '{{.Names}}')
    sudo kubectl config delete-context "kind-${name}" >/dev/null 2>&1 || true
    sudo kubectl config delete-cluster "kind-${name}" >/dev/null 2>&1 || true
  fi
}

cmd_down() {
  require_linux down
  delete_kind_cluster "${MGMT_CLUSTER}"
  delete_kind_cluster "${BOOTSTRAP_CLUSTER}"
  log "removing ${NODE_NAME}"
  remove_container "${NODE_NAME}"
  sudo nerdctl volume rm "${NODE_NAME}-var" "${NODE_NAME}-containerd" >/dev/null 2>&1 || true
  release_stale_lease
  rm -f "${KUBECONFIG_OUT}" "${ETCD_KEY_OUT}"
  log "down; images, tools and ${STATE_DIR}/sitespec.yaml are kept"
}

case "${1:-}" in
  vm-up) cmd_vm_up ;;
  provision) cmd_provision ;;
  build) cmd_build ;;
  up) cmd_up ;;
  status) cmd_status ;;
  import) cmd_import ;;
  down) cmd_down ;;
  -h | --help | help | "") usage ;;
  *) usage >&2; exit 2 ;;
esac
