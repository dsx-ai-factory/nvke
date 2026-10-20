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
: "${NODE_COUNT:=1}"
# Where import puts the CSC's CAPI objects; CWE writes them to nke-system in the bootstrap cluster.
: "${MGMT_NAMESPACE:=nke-system}"
# Where import puts them: kind (a fresh kind cluster) or csc (the CSC itself, self-managed).
: "${IMPORT_TARGET:=kind}"
: "${K8S_VERSION:=1.34.8}"

BOOTSTRAP_CLUSTER=nvkectl-bootstrap
BOOTSTRAP_NAMESPACE=nke-system
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
k_boot() { sudo kubectl --context "kind-${BOOTSTRAP_CLUSTER}" "$@"; }
k_mgmt() {
  if [[ "${IMPORT_TARGET}" == csc ]]; then k_csc "$@"; else sudo kubectl --context "kind-${MGMT_CLUSTER}" "$@"; fi
}
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
node_containers() {
  {
    echo "${NODE_NAME}"
    sudo nerdctl ps -a --filter "label=local-csc.cluster=${CLUSTER_NAME}" --format '{{.Names}}'
  } | sort -u
}
# Host i is NODE_NAME/NODE_IP with the trailing index and last octet advanced by i.
host_name() { if (($1 == 0)); then echo "${NODE_NAME}"; else echo "${NODE_NAME%-*}-$1"; fi; }
host_ip() {
  local a b c d
  IFS=. read -r a b c d <<<"${NODE_IP}"
  echo "${a}.${b}.${c}.$((d + $1))"
}
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
  import      copy the CSC's CAPI objects into a fresh management cluster '${MGMT_CLUSTER}',
              or into the CSC itself with IMPORT_TARGET=csc
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
  local i
  for ((i = 0; i < NODE_COUNT; i++)); do
    in_cidr "$(host_ip "${i}")" "${subnet}" || die "host IP $(host_ip "${i}") is outside the kind network ${subnet}"
  done
}

# A lease outlives its container across VM restarts and blocks a static --ip.
release_stale_lease() {
  local ip="$1" lease="${CNI_LEASE_DIR}/$1" owner
  sudo test -f "${lease}" || return 0
  owner="$(sudo head -1 "${lease}")"
  if [[ -n "${owner}" ]] && container_exists "${owner}"; then
    die "${ip} is held by container ${owner}; set NODE_IP or run down"
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
  local name="$1" ip="$2"
  log "starting ${name} at ${ip}"
  sudo nerdctl run -d --name "${name}" --hostname "${name}" \
    --label "local-csc.cluster=${CLUSTER_NAME}" \
    --privileged --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
    --tty --restart unless-stopped \
    --network kind --ip "${ip}" \
    --volume /lib/modules:/lib/modules:ro \
    --volume "${name}-var:/var" --volume "${name}-containerd:/var/lib/containerd" \
    --volume /dev/null:/proc/swaps:ro \
    --volume "${SCRIPT_DIR}/node-init.sh:/local-csc-node-init.sh:ro" \
    --tmpfs /tmp --tmpfs /run --tmpfs /var/lib/etcd:size=1g \
    --env "NODE_NAME=${name}" \
    --entrypoint /bin/bash \
    "${NODE_IMAGE}" /local-csc-node-init.sh >/dev/null
}

wait_node() {
  local name="$1" state=""
  local -a ex=(sudo nerdctl exec "${name}")
  for _ in $(seq 60); do
    state="$("${ex[@]}" systemctl is-system-running 2>/dev/null || true)"
    [[ "${state}" == running || "${state}" == degraded ]] && break
    sleep 2
  done
  if [[ "${state}" != running ]]; then
    "${ex[@]}" systemctl --failed --no-legend || true
    die "${name} systemd is '${state:-unreachable}', not running"
  fi
  # systemd can report running a moment before nke-agent binds its port.
  local i
  for ((i = 0; i < 15; i++)); do
    "${ex[@]}" sh -c "ss -tln | grep -q ':${AGENT_PORT} '" && break
    sleep 2
  done
  ((i < 15)) || die "nke-agent is not listening on ${AGENT_PORT} in ${name}"
  [[ "$("${ex[@]}" cloud-init status)" == *done* ]] || die "cloud-init status is not done in ${name}"
  [[ "$("${ex[@]}" cat /proc/sys/net/ipv4/ip_forward)" == 1 ]] || die "ip_forward is not 1 in ${name}"
  "${ex[@]}" dpkg -s "${CALICO_PACKAGE}" >/dev/null 2>&1 || die "${CALICO_PACKAGE} is not installed in the node image"
  log "${name} is ready"
}

machine_name() {
  sudo kubectl --context "$1" -n "${BOOTSTRAP_NAMESPACE}" get machines.cluster.x-k8s.io \
    -o jsonpath='{.items[0].metadata.name}'
}

# CAPI can look for the Node a second before the provider stamps its
# providerID and not look again; a metadata change requeues the Machine.
wait_noderef() {
  local kubectl_fn="$1" namespace="$2" machine="$3" ref="" i
  local -a k=("${kubectl_fn}" -n "${namespace}")
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
  [[ -n "${ref}" ]] || die "Machine ${machine} still has no nodeRef"
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
  local i
  for ((i = 0; i < NODE_COUNT; i++)); do
    container_exists "$(host_name "${i}")" && die "$(host_name "${i}") already exists; run down first"
    release_stale_lease "$(host_ip "${i}")"
  done
  write_sitespec
  for ((i = 0; i < NODE_COUNT; i++)); do start_node "$(host_name "${i}")" "$(host_ip "${i}")"; done
  for ((i = 0; i < NODE_COUNT; i++)); do wait_node "$(host_name "${i}")"; done
  if ((NODE_COUNT > 1)); then
    # bootstrap site requires a controlPlaneVIP for more than one node; DSX CSCs have none.
    log "bootstrapping ${NODE_NAME} only; hosts 1..$((NODE_COUNT - 1)) are left for an out-of-band join"
  fi

  log "bootstrapping ${CLUSTER_NAME} (10-20 minutes; log: ${STATE_DIR}/bootstrap.log)"
  (cd "${STATE_DIR}" && sudo "${NVKECTL}" bootstrap site -f sitespec.yaml \
    --disable-mtls --keep-bootstrap-cluster \
    --kubeconfig-out "${KUBECONFIG_OUT}" --encryption-key-out "${ETCD_KEY_OUT}" \
    --diagnostics-dir "${STATE_DIR}/diagnostics") 2>&1 | tee "${STATE_DIR}/bootstrap.log"

  wait_noderef k_boot "${BOOTSTRAP_NAMESPACE}" "$(machine_name "kind-${BOOTSTRAP_CLUSTER}")"
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
  local cluster ns
  for cluster in "${BOOTSTRAP_CLUSTER}" "${MGMT_CLUSTER}"; do
    kind_cluster_exists "${cluster}" || continue
    ns="${BOOTSTRAP_NAMESPACE}"
    [[ "${cluster}" == "${MGMT_CLUSTER}" ]] && ns="${MGMT_NAMESPACE}"
    log "CAPI objects in ${cluster}/${ns}"
    sudo kubectl --context "kind-${cluster}" -n "${ns}" \
      get clusters.cluster.x-k8s.io,machines.cluster.x-k8s.io,provisionedmachines 2>/dev/null || true
  done
  if [[ "${IMPORT_TARGET}" == csc && -f "${KUBECONFIG_OUT}" ]]; then
    log "CAPI objects on the CSC in ${MGMT_NAMESPACE}"
    k_csc -n "${MGMT_NAMESPACE}" get clusters.cluster.x-k8s.io,machines.cluster.x-k8s.io,provisionedmachines 2>/dev/null || true
  fi
}

# -------------------------------------------------------------------- import

# IMPORT_EXTRA names a JSON file, {"hosts": [...], "items": [...]}, describing
# hosts joined outside CWE: inventory entries for the ProvisionedCluster, and a
# Machine, ProvisionedMachine and <machine>-bootstrap-data Secret per host.
# Their Machines are created before the CWE-built node's so it, the
# controlPlaneEndpoint holder, is the newest: KCP updates the oldest first.
import_extra() {
  local capi="$1" m
  local -a machines
  local node_objects='.kind == "Machine" or .kind == "ProvisionedMachine" or (.kind == "Secret" and (.metadata.name | endswith("-bootstrap-data")))'
  jq --slurpfile extra "${IMPORT_EXTRA}" "(.items |= map(select(${node_objects} | not)))
    | (.items[] | select(.kind == \"ProvisionedCluster\") | .spec.hosts) += \$extra[0].hosts" "${capi}" |
    k_mgmt apply --server-side -f -
  mapfile -t machines < <(jq -r '.items[] | select(.kind == "Machine") | .metadata.name' "${IMPORT_EXTRA}")
  for m in "${machines[@]}"; do
    jq --arg m "${m}" --arg ns "${MGMT_NAMESPACE}" '{apiVersion: "v1", kind: "List",
      items: [.items[] | select(.metadata.name == $m or .metadata.name == ($m + "-bootstrap-data")) | .metadata.namespace = $ns]}' \
      "${IMPORT_EXTRA}" | k_mgmt apply --server-side -f -
    # creationTimestamp has one-second resolution.
    sleep 2
  done
  jq "(.items |= map(select(${node_objects})))" "${capi}" | k_mgmt apply --server-side -f -
}

# A pod can land on any node, so every node's containerd gets the image.
load_image_into_csc() {
  local node
  while read -r node; do
    sudo nerdctl save "$1" | sudo nerdctl exec -i "${node}" ctr -n k8s.io images import - >/dev/null
  done < <(node_containers)
}

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
  k_boot -n "${BOOTSTRAP_NAMESPACE}" get "cluster.cluster.x-k8s.io/${CLUSTER_NAME}" "provisionedcluster/${CLUSTER_NAME}" \
    "machine.cluster.x-k8s.io/${machine}" "provisionedmachine/${machine}" \
    "secret/${CLUSTER_NAME}-kubeconfig" "secret/${machine}-bootstrap-data" -o json |
    jq --arg ip "${NODE_IP}" --arg ns "${MGMT_NAMESPACE}" '.items |= map(
        del(.status, .metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp,
            .metadata.generation, .metadata.managedFields, .metadata.ownerReferences, .metadata.finalizers)
        | .metadata.namespace = $ns
        | if .kind == "Cluster"
          then .spec.paused = true | .spec.controlPlaneEndpoint = {host: $ip, port: 6443}
          else . end)' >"${dir}/${CLUSTER_NAME}-capi.json"

  log "pausing the source Cluster so only one management cluster acts on the node"
  k_boot -n "${BOOTSTRAP_NAMESPACE}" patch cluster.cluster.x-k8s.io "${CLUSTER_NAME}" --type merge -p '{"spec":{"paused":true}}'

  local -a clusterctl_target=(--kubeconfig-context "kind-${MGMT_CLUSTER}")
  if [[ "${IMPORT_TARGET}" == csc ]]; then
    clusterctl_target=(--kubeconfig "${KUBECONFIG_OUT}")
  elif ! kind_cluster_exists "${MGMT_CLUSTER}"; then
    log "creating ${MGMT_CLUSTER}"
    kind_ create cluster --name "${MGMT_CLUSTER}" --wait 3m
  fi
  if ! k_mgmt -n capi-system get deployment capi-controller-manager >/dev/null 2>&1; then
    log "installing Cluster API ${capi_version} (${IMPORT_TARGET})"
    sudo clusterctl init "${clusterctl_target[@]}" \
      --core "cluster-api:${capi_version}" --bootstrap "kubeadm:${capi_version}" \
      --control-plane "kubeadm:${capi_version}" --wait-providers
  fi
  log "installing the provisioned provider"
  if [[ "${IMPORT_TARGET}" == csc ]]; then
    load_image_into_csc "${PROVIDER_IMAGE}"
  else
    # Root creates it: fs.protected_regular stops root opening a user's file in /tmp.
    archive="$(sudo mktemp)"
    sudo nerdctl save -o "${archive}" "${PROVIDER_IMAGE}"
    kind_ load image-archive "${archive}" --name "${MGMT_CLUSTER}"
    sudo rm -f "${archive}"
  fi
  k_mgmt apply --server-side -f "${REPO}/cli/internal/bootstrap/controllerdeploy/manifests/provisioned-provider.yaml" >/dev/null
  k_mgmt -n provisioned-system rollout status deploy/provisioned-controller-manager --timeout 3m

  log "importing ${CLUSTER_NAME}"
  k_mgmt create namespace "${MGMT_NAMESPACE}" --dry-run=client -o yaml | k_mgmt apply -f - >/dev/null
  local -a extra_machines=()
  if [[ -n "${IMPORT_EXTRA:-}" ]]; then
    import_extra "${dir}/${CLUSTER_NAME}-capi.json"
    mapfile -t extra_machines < <(jq -r '.items[] | select(.kind == "Machine") | .metadata.name' "${IMPORT_EXTRA}")
  else
    k_mgmt apply --server-side -f "${dir}/${CLUSTER_NAME}-capi.json"
  fi
  k_mgmt -n "${MGMT_NAMESPACE}" patch cluster.cluster.x-k8s.io "${CLUSTER_NAME}" --type merge -p '{"spec":{"paused":false}}'
  local m
  for m in "${extra_machines[@]}" "${machine}"; do wait_noderef k_mgmt "${MGMT_NAMESPACE}" "${m}"; done

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
  local name i
  local -a names
  mapfile -t names < <(node_containers)
  for name in "${names[@]}"; do
    log "removing ${name}"
    remove_container "${name}"
    sudo nerdctl volume rm "${name}-var" "${name}-containerd" >/dev/null 2>&1 || true
  done
  for ((i = 0; i < NODE_COUNT; i++)); do release_stale_lease "$(host_ip "${i}")"; done
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
