#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Prepare a machine for the local site, so the operator flow can be typed as
# it is on real hardware: nvkectl generate sitespec, nvkectl bootstrap site,
# clusterctl init, deploy/nvke-deploy-local.sh install, the release
# resources, a tenant cluster. This script stands in for what a data
# center and a build pipeline hand an operator, and for the lab's simulator;
# it runs none of the operator's commands.
#
#   hack/local-site.sh setup    the host, a container node with its hosts.csv and
#                               SPIRE CA, the networks the bootstrap and the
#                               tenant nodes need, the controller and profile
#                               images built from this checkout and staged into
#                               the node, the stack config and credentials,
#                               nico-sim, the files the flow needs; writes a
#                               workspace and its env.sh
#   hack/local-site.sh status   what is up
#   hack/local-site.sh down     delete tenant clusters, nico-sim, the node and the workspace
#
# `make local-site-setup`, `local-site-status` and `local-site-down` run
# these; deploy/nvke-deploy-local.sh is the install with the lab's steps. The
# workspace is NVKE_LOCAL_SITE_DIR (default ~/nvke-local-site): hosts.csv,
# spire-ca.crt and .key, stack-config.json, nvke-api-clients.yaml with the
# lab's networks filled in, releases/, tenant.yaml, credentials.env, and
# env.sh, which exports the NVKECTL_* credentials, the CA files and the
# NVKE_* input paths for the flow and for deploy/nvke-deploy-local.sh.
# Rerunning setup skips what is there. docs/get-started/local-site.md is the
# page.
#
# Linux, as root, with nerdctl's containerd (cli/hack/bootstrap-e2e-host.sh
# install-runtime) for the node and Docker for nico-sim's tenant nodes. On
# macOS, inside the Colima machine `make -C controller host-up` creates.
set -Eeuo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKSPACE="${NVKE_LOCAL_SITE_DIR:-${HOME}/nvke-local-site}"
SITE="local-site"
NODE="nvkectl-e2e-node1"
NODE_NETWORK="${NVKECTL_E2E_NETWORK:-nvkectl-e2e-nodes}"
# The node's address on the substrate's default subnet. NVKECTL_E2E_NETWORK
# may name another network of that subnet; the substrate's subnet and name
# prefix overrides are not supported here, since nico-sim's endpoint and the
# NetworkPolicy are written for this one.
NODE_IP="172.26.0.10"
TENANT_NETWORK="kind"
NICO_SIM_PORT=18080
NVKECTL="${NVKECTL:-${REPO}/cli/bin/nvkectl}"
KUBECONFIG_FILE="${WORKSPACE}/${SITE}.kubeconfig"
# The SPIRE server the node's agent dials during the bootstrap: the seed
# cluster publishes it on this address, which goes onto the node bridge.
SPIRE_VIP="${NVKECTL_E2E_SPIRE_SERVER_ADDRESS:-172.26.0.2}"
# nerdctl's kind network, where the bootstrap's seed cluster runs. Pinned
# away from Docker's kind network and the node network.
KIND_NETWORK_SUBNET="10.26.0.0/16"
ARCH="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
EXAMPLES="${REPO}/deploy/examples/local-site"
# The controller image and the profile content image, built from this
# checkout and staged into the node's containerd under names no registry
# serves: the chart pulls with IfNotPresent, so the node resolves them from
# its own store, and the stack config names them. The other images and the
# charts come from the public registries and repositories deploy/nico pins.
CONTROLLER_IMAGE="nvke.local/nke-controller:local"
PROFILES_IMAGE="nvke.local/controller-profiles:local"
# The image `make -C cli images` builds for the bootstrap's seed cluster; the
# site's controller is the same build under the name above.
BOOTSTRAP_CONTROLLER_IMAGE="${NVKECTL_NVKE_CONTROLLER_IMAGE:-nke-controller:latest}"
# The node image, for the site node and, copied into Docker, the tenant nodes,
# under the name `make -C cli nodes-up` builds it with.
NODE_IMAGE="${CAPD_KINDEST_NODE_IMAGE:-cwe-capd-kindest-node:local}"
# The OVN chart stream the node image carries, as cli/e2e/bootstrap/run.sh
# builds it, and where its packages are kept: outside the workspace, which
# down removes, since building them takes minutes. The stream is fixed because
# prepare-capd-bundles.sh builds only this one.
CNI_STREAM="2.4.2"
CNI_CHARTS_PACKAGE="nvke-cni-ovn-charts-${CNI_STREAM}"
CNI_BUNDLE_DIR="${CNI_BUNDLE_DIR:-${XDG_CACHE_HOME:-${HOME}/.cache}/nvke-local-site/cni-$(uname -m)}"

log() { printf '[local-site] %s\n' "$*" >&2; }
die() { printf 'local-site: %s\n' "$*" >&2; exit 1; }
mk() { make -C "${REPO}" --no-print-directory "$@"; }

# containerd_env points nerdctl at the standalone containerd when the shell
# does not already, so status and down see the node setup started.
containerd_env() {
  if [[ -z "${CONTAINERD_ADDRESS:-}" && -S /run/containerd-nerdctl/containerd.sock ]]; then
    export CONTAINERD_ADDRESS=/run/containerd-nerdctl/containerd.sock
  fi
  # The buildkitd and the namespace that go with the standalone containerd,
  # whichever variable the shell already had.
  if [[ "${CONTAINERD_ADDRESS:-}" == /run/containerd-nerdctl/containerd.sock ]]; then
    export BUILDKIT_HOST="${BUILDKIT_HOST:-unix:///run/buildkit-nerdctl/buildkitd.sock}"
    export CONTAINERD_NAMESPACE="${CONTAINERD_NAMESPACE:-default}"
  fi
}

# bridge_name CLI NETWORK prints the Linux bridge of a nerdctl or Docker
# network, and fails when the network does not exist.
bridge_name() {
  local id
  id="$("$1" network inspect --format '{{.Id}}' "$2" 2>/dev/null)" || return 1
  [[ -n "${id}" ]] || return 1
  printf 'br-%s' "${id:0:12}"
}

usage() {
  sed -n '4,/^set -Eeuo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
}

# ---- the host -----------------------------------------------------------------

check_host() {
  case "$(uname -s)" in
    Linux) ;;
    Darwin) die "run this inside the Colima machine: make -C controller host-up, then colima ssh and sudo -i (docs/get-started/local-site.md)" ;;
    *) die "unsupported OS $(uname -s)" ;;
  esac
  [[ "$(id -u)" == 0 ]] || die "run as root: the node containers need iptables rules and a bridge address (sudo -i)"
  [[ -c /dev/vtpmx ]] || die "/dev/vtpmx is missing; load tpm_vtpm_proxy (make -C controller host-up)"
  local missing=()
  # dpkg-scanpackages (dpkg-dev) indexes the OVN packages the node image carries.
  for tool in nerdctl kind kubectl helm clusterctl oras curl jq python3 go gcc git make docker openssl iptables dpkg-scanpackages; do
    command -v "${tool}" >/dev/null 2>&1 || missing+=("${tool}")
  done
  [[ ${#missing[@]} -eq 0 ]] || die "not on PATH: ${missing[*]} (dpkg-scanpackages is in dpkg-dev)"
  containerd_env
  nerdctl info >/dev/null 2>&1 || die "nerdctl cannot reach its containerd; run cli/hack/bootstrap-e2e-host.sh install-runtime and export CONTAINERD_ADDRESS"
  docker info >/dev/null 2>&1 || die "docker cannot reach its daemon; nico-sim runs the tenant nodes on it"
  [[ -n "${GITHUB_TOKEN:-}" ]] || log "warning: GITHUB_TOKEN is not set; clusterctl init reads the GitHub API, which allows anonymous callers 60 requests an hour"
  # The workspace is this script's: a directory it did not write is refused
  # here, as down refuses it. The marker goes in first, so a setup that
  # fails before env.sh is written leaves a workspace down can remove.
  if [[ -d "${WORKSPACE}" && -n "$(ls -A "${WORKSPACE}")" ]] && ! is_workspace; then
    die "${WORKSPACE} is not a workspace this script wrote; set NVKE_LOCAL_SITE_DIR to an empty or new directory"
  fi
  mkdir -p "${WORKSPACE}/releases"
  chmod 700 "${WORKSPACE}"
  : >"${WORKSPACE}/.local-site"
}

# is_workspace reports whether WORKSPACE was written by this script.
is_workspace() {
  [[ -f "${WORKSPACE}/.local-site" ]] || grep -qs '^# Written by hack/local-site.sh setup' "${WORKSPACE}/env.sh"
}

# ---- the images and the binary ------------------------------------------------

build_images() {
  # Always, like `images` below: a binary left from another checkout reads
  # the inputs differently. The Go build cache keeps a no-op rebuild cheap.
  log "building nvkectl (make -C cli build)"
  mk -C cli build >/dev/null
  log "building the bootstrap images (make -C cli images)"
  mk -C cli images CONTROLLER_IMAGE="${BOOTSTRAP_CONTROLLER_IMAGE}" >/dev/null
}

# ---- the node: what the data center hands an operator --------------------------

node_up() {
  # The seed cluster's network, created ahead of kind so its subnet is pinned
  # and its bridge is stable.
  nerdctl network inspect "${TENANT_NETWORK}" >/dev/null 2>&1 ||
    nerdctl network create --driver bridge --subnet "${KIND_NETWORK_SUBNET}" "${TENANT_NETWORK}" >/dev/null
  # A node image from a setup before the CNI packages has none, and the
  # bootstrap would stop at InstallCNI. The node runs an image derived from
  # NODE_IMAGE, cached by a tag that does not follow NODE_IMAGE, so both go.
  local rmi="nerdctl rmi ${NODE_IMAGE} \$(nerdctl images --filter reference=nvkectl-e2e/bootstrap-node --format '{{.Repository}}:{{.Tag}}')"
  if nerdctl inspect "${NODE}" --format '{{.State.Running}}' 2>/dev/null | grep -qx true; then
    log "node ${NODE} is running"
    nerdctl exec "${NODE}" dpkg-query -W "${CNI_CHARTS_PACKAGE}" >/dev/null 2>&1 ||
      die "the running node ${NODE} has no ${CNI_CHARTS_PACKAGE}, so the bootstrap would stop at InstallCNI; run make local-site-down, remove its images (${rmi}) and run setup again to rebuild them with the packages"
  else
    cni_packages
    if nerdctl image inspect "${NODE_IMAGE}" >/dev/null 2>&1 && ! image_has_cni nerdctl; then
      die "${NODE_IMAGE} has no ${CNI_CHARTS_PACKAGE}, so the bootstrap would stop at InstallCNI; remove it and the node image built from it (${rmi}) and run setup again to rebuild them with the packages"
    fi
    log "starting the container node (make -C cli nodes-up; the first run builds the node image)"
    # The build names the image after CAPD_KINDEST_NODE_IMAGE and the node
    # derives its own from NVKECTL_E2E_BASE_IMAGE, so both carry NODE_IMAGE.
    NVKECTL_E2E_NODES=1 NVKECTL_E2E_REUSE=1 CAPD_KINDEST_NODE_IMAGE="${NODE_IMAGE}" NVKECTL_E2E_BASE_IMAGE="${NODE_IMAGE}" mk -C cli nodes-up
  fi
  # nodes-hosts records a SPIRE journal cursor per node under the attestation
  # baseline; right after nodes-up the agent may not have logged yet, so the
  # first reads can find no entries.
  export NVKECTL_E2E_ATTESTATION_BASELINE="${WORKSPACE}/.attestation-baseline.json"
  local attempt hosts err="${WORKSPACE}/.nodes-hosts.err"
  for attempt in 1 2 3 4 5 6; do
    if hosts="$(NVKECTL_E2E_NODES=1 NVKECTL_E2E_HOSTS_CSV="${WORKSPACE}/hosts.csv" mk -C cli nodes-hosts 2>"${err}")"; then break; fi
    [[ "${attempt}" -lt 6 ]] || die "nodes-hosts failed six times: $(tail -n 3 "${err}" | tr '\n' ' ')"
    sleep 5
  done
  rm -f "${err}"
  # Three KEY=value lines; the values are paths and are taken whole.
  local key value
  while IFS='=' read -r key value; do
    [[ -n "${key}" ]] && printf -v "${key}" '%s' "${value}"
  done <<<"${hosts}"
  [[ -n "${NVKECTL_E2E_SPIRE_CA_CERT:-}" && -n "${NVKECTL_E2E_SPIRE_CA_KEY:-}" ]] || die "nodes-hosts named no SPIRE CA: ${hosts}"
  install -m 0644 "${NVKECTL_E2E_SPIRE_CA_CERT}" "${WORKSPACE}/spire-ca.crt"
  install -m 0600 "${NVKECTL_E2E_SPIRE_CA_KEY}" "${WORKSPACE}/spire-ca.key"
  attach_bootstrap_network
}

# cni_packages builds the OVN and Helm packages the node image installs for
# the controller's CNI step, as cli/e2e/bootstrap/run.sh does, and exports
# the paths `make -C cli nodes-up` builds the image with. Without them the
# image carries no CNI charts and the bootstrap stops at InstallCNI. They are
# built once into CNI_BUNDLE_DIR.
cni_packages() {
  local arch="${ARCH}"
  export CAPD_CNI_CHARTS_DEB_PATH="${CNI_BUNDLE_DIR}/${CNI_CHARTS_PACKAGE}_${CNI_STREAM}_${arch}.deb"
  export CAPD_CNI_IMAGES_DEB_PATH="${CNI_BUNDLE_DIR}/nvke-cni-ovn-images-${CNI_STREAM}_${CNI_STREAM}_${arch}.deb"
  export CAPD_HELM_DEB_PATH="${CNI_BUNDLE_DIR}/nke-e2e-helm.deb"
  if [[ -f "${CAPD_CNI_CHARTS_DEB_PATH}" && -f "${CAPD_CNI_IMAGES_DEB_PATH}" && -f "${CAPD_HELM_DEB_PATH}" ]]; then
    return
  fi
  log "building the OVN and Helm packages for the node image (controller/hack/ci/prepare-capd-bundles.sh; minutes, once)"
  mkdir -p "${CNI_BUNDLE_DIR}"
  BUNDLE_ARCH="${arch}" bash "${REPO}/controller/hack/ci/prepare-capd-bundles.sh" "${CNI_BUNDLE_DIR}" >"${WORKSPACE}/.cni-packages.log" 2>&1 ||
    die "building the OVN and Helm packages failed; the log is ${WORKSPACE}/.cni-packages.log"
  [[ -f "${CAPD_CNI_CHARTS_DEB_PATH}" && -f "${CAPD_CNI_IMAGES_DEB_PATH}" && -f "${CAPD_HELM_DEB_PATH}" ]] ||
    die "prepare-capd-bundles.sh left no OVN or Helm package in ${CNI_BUNDLE_DIR}; the log is ${WORKSPACE}/.cni-packages.log"
}

# image_has_cni CLI reports whether NODE_IMAGE, as the nerdctl or docker CLI
# sees it, carries the CNI charts package: an image built before setup
# supplied the packages lacks it, and the build is skipped once the image
# exists.
image_has_cni() {
  "$1" run --rm --network none --entrypoint dpkg-query "${NODE_IMAGE}" -W "${CNI_CHARTS_PACKAGE}" >/dev/null 2>&1
}

# docker_copy_current reports whether Docker's NODE_IMAGE is the image nerdctl
# holds under that name, and not an older build left under the same tag.
# Docker's ID is the manifest digest with the containerd image store and the
# config digest without it; nerdctl reports both.
docker_copy_current() {
  local id
  id="$(docker image inspect --format '{{.Id}}' "${NODE_IMAGE}" 2>/dev/null)" && [[ -n "${id}" ]] || return 1
  nerdctl image inspect --format '{{.Id}} {{.RepoDigests}}' "${NODE_IMAGE}" 2>/dev/null | grep -qF "${id}"
}

# attach_bootstrap_network does what cli/e2e/bootstrap/run.sh does before
# the bootstrap: nerdctl's CNI firewall drops a forward from one bridge out
# another, so the seed cluster on the kind bridge and the node on its bridge
# are allowed to reach each other, both ways, ahead of the isolation chain;
# and the SPIRE server address the node's agent dials goes onto the node
# bridge, since the seed cluster publishes it there as a NodePort. The kind
# bridge may not exist yet: the CNI creates it when the first container
# attaches, which is the bootstrap's seed cluster. iptables takes the name of
# an interface that does not exist yet, so the rules go in now.
attach_bootstrap_network() {
  local node_br kind_br pair
  node_br="$(bridge_name nerdctl "${NODE_NETWORK}")" || die "the ${NODE_NETWORK} network is missing"
  kind_br="$(bridge_name nerdctl "${TENANT_NETWORK}")" || die "the ${TENANT_NETWORK} network is missing"
  ip link show "${node_br}" >/dev/null 2>&1 || die "the ${NODE_NETWORK} bridge ${node_br} is missing; is the node running?"
  for pair in "${kind_br} ${node_br}" "${node_br} ${kind_br}"; do
    # shellcheck disable=SC2086 # two words on purpose
    set -- ${pair}
    iptables -C FORWARD -i "$1" -o "$2" -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -i "$1" -o "$2" -j ACCEPT
  done
  ip address replace "${SPIRE_VIP}/32" dev "${node_br}"
  log "bridged ${TENANT_NETWORK} and ${NODE_NETWORK}; SPIRE server address ${SPIRE_VIP} on ${node_br}"
}

# detach_bootstrap_network undoes attach_bootstrap_network, best effort.
detach_bootstrap_network() {
  local node_br kind_br
  node_br="$(bridge_name nerdctl "${NODE_NETWORK}" 2>/dev/null)" || return 0
  if kind_br="$(bridge_name nerdctl "${TENANT_NETWORK}" 2>/dev/null)"; then
    iptables -D FORWARD -i "${kind_br}" -o "${node_br}" -j ACCEPT 2>/dev/null || true
    iptables -D FORWARD -i "${node_br}" -o "${kind_br}" -j ACCEPT 2>/dev/null || true
  fi
  ip address del "${SPIRE_VIP}/32" dev "${node_br}" 2>/dev/null || true
}

# ---- the images: what a build hands an operator ---------------------------------

# node_ctr runs ctr in the node's containerd, namespace k8s.io, the one the
# kubelet reads. stdin is the import's archive; every other call closes it.
node_ctr() { nerdctl exec -i "${NODE}" ctr -n k8s.io "$@"; }

node_has_image() { node_ctr images ls -q </dev/null 2>/dev/null | grep -qx "$1"; }

# restart_controller rolls the site's controller onto restaged images. The
# stack config names them by the same tag whatever the build, so an install
# renders the same pod template and the old build would keep running until
# the pod next restarted. Nothing to do before the stack is installed. A site
# that does not answer, or a rollout that does not finish, stops setup; the
# caller records the images after this, so the next setup restarts again.
restart_controller() {
  [[ -f "${KUBECONFIG_FILE}" ]] || return 0
  local err answered=false deadline=$((SECONDS + 120))
  # The node may have just started, and its API server takes a minute.
  while ((SECONDS < deadline)); do
    if err="$(kubectl --kubeconfig "${KUBECONFIG_FILE}" --request-timeout=10s -n nvke-controller get deployment nvke-controller -o name 2>&1 >/dev/null)"; then
      answered=true
      break
    fi
    [[ "${err}" == *NotFound* ]] && return 0
    sleep 5
  done
  [[ "${answered}" == true ]] || die "the site did not answer for two minutes, so the controller still runs the old images; run setup again once it does (${err})"
  log "restarting the controller on the restaged images"
  kubectl --kubeconfig "${KUBECONFIG_FILE}" -n nvke-controller rollout restart deployment/nvke-controller >/dev/null
  kubectl --kubeconfig "${KUBECONFIG_FILE}" -n nvke-controller rollout status deployment/nvke-controller --timeout=5m >/dev/null ||
    die "the controller did not come up on the restaged images (kubectl -n nvke-controller get pods); run setup again to restart it"
}

# stage_images stages the two images into the node, and again whenever the
# build changed: the staged ids are recorded in the workspace and compared
# with the local images. A restage restarts an installed controller, so a
# setup after a `git pull` runs the new build.
stage_images() {
  log "tagging the controller image ${CONTROLLER_IMAGE}"
  nerdctl tag "${BOOTSTRAP_CONTROLLER_IMAGE}" "${CONTROLLER_IMAGE}"
  log "building the profile content image ${PROFILES_IMAGE} from controller/test/fixtures"
  nerdctl build --platform "linux/${ARCH}" -t "${PROFILES_IMAGE}" -f "${REPO}/controller/test/fixtures/Dockerfile" "${REPO}/controller/test/fixtures" >/dev/null 2>&1 ||
    nerdctl build --platform "linux/${ARCH}" -t "${PROFILES_IMAGE}" -f "${REPO}/controller/test/fixtures/Dockerfile" "${REPO}/controller/test/fixtures"
  local ids staged="${WORKSPACE}/.staged-images"
  ids="$(nerdctl image inspect --format '{{.Id}}' "${CONTROLLER_IMAGE}" "${PROFILES_IMAGE}")"
  if [[ -f "${staged}" && "$(cat "${staged}")" == "${ids}" ]] && node_has_image "${CONTROLLER_IMAGE}" && node_has_image "${PROFILES_IMAGE}"; then
    log "the controller and profile images are in the node's containerd"
    return
  fi
  local image
  for image in "${CONTROLLER_IMAGE}" "${PROFILES_IMAGE}"; do
    log "staging ${image} into the node's containerd"
    nerdctl save "${image}" | node_ctr images import - >/dev/null
    node_has_image "${image}" || die "${image} is not in the node's containerd after the import"
  done
  # Recorded only after the restart: a setup it stops leaves them unrecorded,
  # and the next one stages and restarts again.
  restart_controller
  printf '%s\n' "${ids}" >"${staged}"
}

# ---- the files the flow reads -------------------------------------------------------

network_cidr() {
  # network_cidr CLI NETWORK prints the IPv4 subnet of a container network.
  "$1" network inspect "$2" --format '{{range .IPAM.Config}}{{.Subnet}} {{end}}' 2>/dev/null |
    awk '{for (i=1;i<=NF;i++) if ($i ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\/[0-9]+$/) {print $i; exit}}'
}

write_inputs() {
  sed -e "s|REPLACE_CONTROLLER_IMAGE|${CONTROLLER_IMAGE}|" -e "s|REPLACE_PROFILES_IMAGE|${PROFILES_IMAGE}|" "${EXAMPLES}/stack-config.json" >"${WORKSPACE}/stack-config.json"
  local kind
  for kind in machineimage clusterrelease nodepoolrelease; do
    cp "${EXAMPLES}/releases/${kind}-${ARCH}.yaml" "${WORKSPACE}/releases/${kind}.yaml"
  done
  cp "${EXAMPLES}/tenant-${ARCH}.yaml" "${WORKSPACE}/tenant.yaml"
  docker network inspect "${TENANT_NETWORK}" >/dev/null 2>&1 || docker network create "${TENANT_NETWORK}" >/dev/null
  local host_cidr tenant_cidr
  host_cidr="$(network_cidr nerdctl "${NODE_NETWORK}")" || die "cannot inspect the ${NODE_NETWORK} network"
  tenant_cidr="$(network_cidr docker "${TENANT_NETWORK}")" || die "cannot inspect the Docker ${TENANT_NETWORK} network"
  [[ -n "${host_cidr}" && -n "${tenant_cidr}" ]] || die "cannot read the ${NODE_NETWORK} or ${TENANT_NETWORK} subnet"
  sed -e "s|REPLACE_HOST_CIDR|${host_cidr}|" -e "s|REPLACE_TENANT_CIDR|${tenant_cidr}|" "${EXAMPLES}/nvke-api-clients.yaml" >"${WORKSPACE}/nvke-api-clients.yaml"
  # The generated credentials persist across setups: every install of the
  # same site needs the same database password. env.sh is rewritten each
  # time, so it follows this script.
  local creds="${WORKSPACE}/credentials.env" node_pw db_pw
  if [[ ! -s "${creds}" ]]; then
    node_pw="$(openssl rand -hex 12)"; db_pw="$(openssl rand -hex 16)"
    [[ -n "${node_pw}" && -n "${db_pw}" ]] || die "openssl generated no password"
    (umask 077 && printf '# shellcheck shell=bash\n# Generated by hack/local-site.sh setup for this site; kept across setups.\nexport NVKECTL_NODE_PASSWORD=%s NVKECTL_DATABASE_PASSWORD=%s\n' "${node_pw}" "${db_pw}" >"${creds}")
  fi
  (umask 077 && cat >"${WORKSPACE}/env.sh" <<ENV
# shellcheck shell=bash
# The local site's environment: source it in the shell you type the flow in.
# Written by hack/local-site.sh setup; the credentials are the lab's.
export NVKE_REPO="${REPO}"
export PATH="${REPO}/cli/bin:\${PATH}"
export CONTAINERD_ADDRESS="${CONTAINERD_ADDRESS:-/run/containerd-nerdctl/containerd.sock}"
export BUILDKIT_HOST="${BUILDKIT_HOST:-unix:///run/buildkit-nerdctl/buildkitd.sock}"
export CONTAINERD_NAMESPACE="${CONTAINERD_NAMESPACE:-default}"
# The node's network and count, as the bootstrap substrate's targets read them.
export NVKECTL_E2E_NETWORK="${NODE_NETWORK}" NVKECTL_E2E_NODES=1 NVKECTL_E2E_ATTESTATION_BASELINE="${WORKSPACE}/.attestation-baseline.json"
# The image the bootstrap's seed cluster side-loads: the one the setup built
# and tagged for the site.
export NVKECTL_NVKE_CONTROLLER_IMAGE="${BOOTSTRAP_CONTROLLER_IMAGE}"
# The SPIRE CA the node's agent trusts: the bootstrap and the install read it.
export NVKECTL_SPIRE_CA_CERT_FILE="${WORKSPACE}/spire-ca.crt" NVKECTL_SPIRE_CA_KEY_FILE="${WORKSPACE}/spire-ca.key"
# The install's credentials. nico-sim accepts its seeded token; the node login
# and the database password are generated for this site, in credentials.env.
export NVKECTL_NICO_TOKEN=nico-simctl-token NVKECTL_NODE_USER=nvke
. "${creds}"
# The inputs deploy/nvke-deploy-local.sh install reads.
export NVKE_SITESPEC="${WORKSPACE}/sitespec.yaml" NVKE_STACK_CONFIG="${WORKSPACE}/stack-config.json" NVKE_KUBECONFIG="${KUBECONFIG_FILE}"
cd "${WORKSPACE}"
ENV
  )
}

# ---- nico-sim: the lab's NICo ----------------------------------------------------

# unbridge_networks undoes bridge_networks, best effort.
unbridge_networks() {
  local node_br tenant_br
  node_br="$(bridge_name nerdctl "${NODE_NETWORK}" 2>/dev/null)" || return 0
  tenant_br="$(bridge_name docker "${TENANT_NETWORK}" 2>/dev/null)" || return 0
  iptables -D FORWARD -i "${node_br}" -o "${tenant_br}" -j ACCEPT 2>/dev/null || true
  iptables -D FORWARD -i "${tenant_br}" -o "${node_br}" -j ACCEPT 2>/dev/null || true
  local tenant_cidr
  tenant_cidr="$(network_cidr docker "${TENANT_NETWORK}")" || return 0
  [[ -z "${tenant_cidr}" ]] || iptables -t raw -D PREROUTING -i "${node_br}" -d "${tenant_cidr}" -j ACCEPT 2>/dev/null || true
}

bridge_networks() {
  # The tenant nodes (Docker's network) and the site node (nerdctl's) must
  # reach each other: the node agent, the SPIRE server and the NVKE API on
  # the site, the agent and the API server on the tenant nodes.
  local node_br tenant_br pair
  node_br="$(bridge_name nerdctl "${NODE_NETWORK}")" || die "the ${NODE_NETWORK} network is missing"
  tenant_br="$(bridge_name docker "${TENANT_NETWORK}")" || die "the Docker ${TENANT_NETWORK} network is missing"
  for pair in "${node_br} ${tenant_br}" "${tenant_br} ${node_br}"; do
    # shellcheck disable=SC2086 # two words on purpose
    set -- ${pair}
    iptables -C FORWARD -i "$1" -o "$2" -j ACCEPT 2>/dev/null || iptables -I FORWARD 1 -i "$1" -o "$2" -j ACCEPT
  done
  # Docker 28 also drops, in the raw table, a packet for a container's address
  # that arrives on any interface but its network's bridge, ahead of FORWARD;
  # the site node's packets arrive on the node bridge.
  local tenant_cidr
  tenant_cidr="$(network_cidr docker "${TENANT_NETWORK}")"
  [[ -n "${tenant_cidr}" ]] || die "cannot read the Docker ${TENANT_NETWORK} subnet"
  iptables -t raw -C PREROUTING -i "${node_br}" -d "${tenant_cidr}" -j ACCEPT 2>/dev/null ||
    iptables -t raw -I PREROUTING 1 -i "${node_br}" -d "${tenant_cidr}" -j ACCEPT
}

# nico_sim_running reports whether the pid file names a live nico-sim, not a
# process that reused the pid: the process's own command must be nico-sim.
nico_sim_running() {
  local pid argv0
  [[ -f "${WORKSPACE}/.nico-sim.pid" ]] || return 1
  pid="$(cat "${WORKSPACE}/.nico-sim.pid")"
  [[ -n "${pid}" ]] || return 1
  [[ -r "/proc/${pid}/cmdline" ]] || return 1
  argv0="$(tr '\0' '\n' <"/proc/${pid}/cmdline" | head -n 1)"
  [[ "${argv0##*/}" == nico-sim ]]
}

nico_sim_up() {
  local sim="${REPO}/controller/nico-sim" pid_file="${WORKSPACE}/.nico-sim.pid" gateway
  gateway="$(nerdctl network inspect "${NODE_NETWORK}" --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | awk '{print $1}')" && [[ -n "${gateway}" ]] || die "cannot read the ${NODE_NETWORK} gateway"
  # Copied again when Docker's copy is an older build than nerdctl's or predates
  # the CNI packages, whether or not nico-sim is running: it starts tenant nodes
  # from this name, and a tenant node without them never gets a CNI.
  if ! docker_copy_current || ! image_has_cni docker; then
    log "copying the node image into Docker for the tenant nodes"
    nerdctl save "${NODE_IMAGE}" | docker load >/dev/null
  fi
  if nico_sim_running; then
    log "nico-sim is running"
  else
    # Built on every setup, like nvkectl; a running nico-sim keeps serving
    # until down, so a rebuilt binary is picked up by the next setup after
    # a down.
    log "building nico-sim (make -C controller/nico-sim build)"
    CGO_ENABLED=1 mk -C controller/nico-sim build >/dev/null
    # nico-sim binds its port last, after its database, seed and Docker
    # checks, so an answer on the port while it starts can be another
    # process's; refuse a port that is already taken.
    if curl -sS -m 1 "http://${gateway}:${NICO_SIM_PORT}/healthz" >/dev/null 2>&1; then
      die "something already answers on ${gateway}:${NICO_SIM_PORT}; stop it (another local site's or Tilt's nico-sim) and run setup again"
    fi
    log "starting nico-sim on ${gateway}:${NICO_SIM_PORT}"
    (
      cd "${sim}"
      export NICO_SIM_LISTEN_ADDR="${gateway}:${NICO_SIM_PORT}" NICO_SIM_DB_PATH="${WORKSPACE}/.nico-sim.db" NICO_SIM_SEED_PATH="${sim}/internal/seed/default-seed.yaml"
      # No console SSH server: it takes any client on every interface, and
      # nothing here reads a console.
      export NICO_SIM_LOG_FORMAT=text NICO_VMS_VM_BACKEND=mock NICO_VMS_BGP_ENABLED=false NICO_VMS_CONSOLE_SSH_PORT=-1
      export NICO_CONTAINERS_ENABLED=true NICO_CONTAINERS_REQUIRE_HEALTHY=true NICO_CONTAINERS_NODE_NETWORK="${TENANT_NETWORK}" NICO_CONTAINERS_NODE_IMAGE="${NODE_IMAGE}"
      nohup ./bin/nico-sim >"${WORKSPACE}/.nico-sim.log" 2>&1 &
      echo $! >"${pid_file}"
    )
  fi
  local _
  for _ in $(seq 1 30); do curl -sS -m 1 "http://${gateway}:${NICO_SIM_PORT}/healthz" >/dev/null 2>&1 && break; sleep 1; done
  curl -sS -m 2 "http://${gateway}:${NICO_SIM_PORT}/healthz" >/dev/null || die "nico-sim did not answer; the log is ${WORKSPACE}/.nico-sim.log"
  # The answer must be this workspace's nico-sim, not another one on the port.
  nico_sim_running || die "nico-sim exited; another process may hold ${gateway}:${NICO_SIM_PORT}; the log is ${WORKSPACE}/.nico-sim.log"
  bridge_networks
}

setup() {
  check_host
  build_images
  node_up
  stage_images
  write_inputs
  nico_sim_up
  # The site's Kubernetes version is the node's: the node image ships one
  # kubelet, and a control plane newer than it leaves features the stack
  # relies on, such as image volumes, off on the node.
  local k8s_version
  k8s_version="$(nerdctl exec "${NODE}" kubelet --version 2>/dev/null | awk '{sub(/^v/, "", $2); print $2}')"
  [[ "${k8s_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "cannot read the node's kubelet version"
  cat <<DONE

The local site is ready for the operator flow. In a root shell:

  source ${WORKSPACE}/env.sh
  nvkectl generate sitespec ${SITE} --nodes hosts.csv --set cluster.k8sVersion=${k8s_version} --set cluster.podCidr=10.244.0.0/16 --set cluster.kubeletConfig.systemReservedMemory=512Mi --set spire.upstreamCA=true > sitespec.yaml
  nvkectl bootstrap site -f sitespec.yaml --timeout 25m

Then, as docs/get-started/local-site.md lists them: clusterctl init,
deploy/nvke-deploy-local.sh install, the NetworkPolicy, nvkectl config set-site
--url http://${NODE_IP}:30080, the releases, a tenant cluster.
The workspace holds:
  hosts.csv, spire-ca.crt, spire-ca.key   the node, as a data center hands it over
  stack-config.json                       nico-sim's NICo API and the two images built from this checkout,
                                          which are in the node's containerd
  nvke-api-clients.yaml                   the networks admitted to the NVKE API
  releases/, tenant.yaml                  the release set and a tenant request for ${ARCH}
  env.sh                                  the environment of the flow: the input paths, the credentials
  credentials.env                         the node login and the database password, kept across setups
DONE
}

status() {
  containerd_env
  local running=false
  nerdctl inspect "${NODE}" --format '{{.State.Running}}' 2>/dev/null | grep -qx true && running=true
  printf 'workspace:   %s\n' "${WORKSPACE}"
  printf 'node:        %s\n' "$([[ "${running}" == true ]] && echo "${NODE} running" || echo "down")"
  printf 'images:      %s\n' "$([[ "${running}" == true ]] && node_has_image "${CONTROLLER_IMAGE}" && node_has_image "${PROFILES_IMAGE}" && echo "staged in the node" || echo "not staged")"
  if [[ -f "${KUBECONFIG_FILE}" ]] && kubectl --kubeconfig "${KUBECONFIG_FILE}" get nodes >/dev/null 2>&1; then
    printf 'management:  up (%s)\n' "${KUBECONFIG_FILE}"
    printf 'stack:       %s\n' "$(kubectl --kubeconfig "${KUBECONFIG_FILE}" -n nvke-controller get deployment nvke-controller -o jsonpath='{.status.readyReplicas}' 2>/dev/null | grep -q '^[1-9]' && echo up || echo "not installed")"
  else
    printf 'management:  down\n'
  fi
  if nico_sim_running; then
    printf 'nico-sim:    running (pid %s)\n' "$(cat "${WORKSPACE}/.nico-sim.pid")"
  else
    printf 'nico-sim:    down\n'
  fi
}

# sim_instance_ids prints the ids of the instances this workspace's nico-sim
# created, read from its database; nothing when there is none. It fails when
# the database cannot be read, so down keeps the only record of those nodes.
sim_instance_ids() {
  local db="${WORKSPACE}/.nico-sim.db"
  [[ -f "${db}" ]] || return 0
  if ! command -v python3 >/dev/null 2>&1; then
    log "python3 is missing, so the tenant nodes in ${db} cannot be found"
    return 1
  fi
  python3 - "${db}" <<'PY' || { log "cannot read the instances in ${db}"; return 1; }
import sqlite3, sys
for (instance_id,) in sqlite3.connect(sys.argv[1]).execute("SELECT id FROM instances"):
    print(instance_id)
PY
}

down() {
  containerd_env
  if [[ -d "${WORKSPACE}" ]] && ! is_workspace; then
    die "${WORKSPACE} is not a workspace this script wrote (no .local-site marker or env.sh written by this script); set NVKE_LOCAL_SITE_DIR to the one it did, or remove it yourself"
  fi
  [[ ! -e "${WORKSPACE}" || -d "${WORKSPACE}" ]] || die "${WORKSPACE} is not a directory"
  [[ ! -L "${WORKSPACE}" ]] || die "${WORKSPACE} is a symlink; remove its target yourself"
  # The tenant clusters, through the site's own entry: never whatever site
  # nvkectl has current.
  if [[ -f "${KUBECONFIG_FILE}" ]] && command -v "${NVKECTL}" >/dev/null 2>&1; then
    local name
    for name in $("${NVKECTL}" --site "${SITE}" get clusters 2>/dev/null | awk 'NR > 1 { print $1 }'); do
      log "deleting tenant cluster ${name}"
      "${NVKECTL}" --site "${SITE}" delete cluster "${name}" >/dev/null 2>&1 || true
    done
    local _
    for _ in $(seq 1 90); do
      [[ -z "$("${NVKECTL}" --site "${SITE}" get clusters 2>/dev/null | awk 'NR > 1')" ]] && break
      sleep 10
    done
  fi
  if nico_sim_running; then
    local pid
    pid="$(cat "${WORKSPACE}/.nico-sim.pid")"
    kill "${pid}" 2>/dev/null || true
    for _ in $(seq 1 20); do kill -0 "${pid}" 2>/dev/null || break; sleep 1; done
    # The workspace goes only once nothing holds its port and its database.
    kill -0 "${pid}" 2>/dev/null && { kill -9 "${pid}" 2>/dev/null || true; sleep 1; }
    kill -0 "${pid}" 2>/dev/null && die "nico-sim (pid ${pid}) did not exit; stop it and run down again"
  fi
  # Only the tenant nodes this workspace's nico-sim created, by the instance
  # ids in its database: every nico-sim on the default seed labels its nodes
  # with the same site, so another one's nodes on the same Docker daemon stay.
  # A failure keeps the workspace, whose database is the only record of them.
  # Without the Docker CLI nico-sim started none.
  if command -v docker >/dev/null 2>&1; then
    local instances instance out ids
    instances="$(sim_instance_ids)" || die "cannot tell which tenant nodes are this workspace's; the workspace stays, so run down again once that is fixed"
    for instance in ${instances}; do
      out="$(docker ps -aq --filter label=nvidia.com/nico-containers.managed=true --filter "label=nvidia.com/nico-containers.instance-id=${instance}")" ||
        die "cannot list the tenant nodes in Docker; the workspace stays, so run down again"
      [[ -z "${out}" ]] || { mapfile -t ids <<<"${out}"; docker rm -f -v "${ids[@]}" >/dev/null; }
    done
  fi
  if command -v nerdctl >/dev/null 2>&1; then
    detach_bootstrap_network
    unbridge_networks
    if nerdctl inspect "${NODE}" >/dev/null 2>&1; then
      log "removing the node (make -C cli nodes-down)"
      NVKECTL_E2E_NODES=1 mk -C cli nodes-down >/dev/null || true
      ! nerdctl inspect "${NODE}" >/dev/null 2>&1 || die "the node ${NODE} is still present; run down again"
    fi
  fi
  rm -rf "${WORKSPACE}"
  log "down"
}

case "${1:-}" in
  setup) setup ;;
  status) status ;;
  down) down ;;
  *) usage ;;
esac
