#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Shared body for the two backend E2E entrypoints, ci/backend-e2e.sh and
# local-backend-e2e.sh. Source it; do not run it.
#
# The two entrypoints differ in how they get a Kind cluster with the controller on it: CI
# builds every image and runs the tools from an inner container, while the local
# one uses the Makefile targets and the host's tools. What they must NOT differ
# in is everything after that, and they had: the local one never set
# `site_controller_vip`, so CAPD tenants were handed the Kind bridge gateway,
# where nothing is bound on native Docker. Nothing compared the two.
#
# So the parts below live here once. Each takes an exec function, because that
# is the one thing that legitimately differs: CI runs kubectl inside its runner,
# local runs it on the host.

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  echo "error: source backend-e2e-lib.sh from another script" >&2
  exit 1
fi

# The Kind control-plane container's address on the `kind` network. CAPD tenants
# are sibling containers there, so this is what reaches the management cluster's
# NodePorts from a tenant node. The Tiltfile falls back to the bridge gateway
# when this is unset, which works on Colima and not on native Docker.
e2e_management_vip() {
  local cluster="$1" vip
  vip="$(docker inspect \
    --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}' \
    "${cluster}-control-plane" 2>/dev/null || true)"
  [[ -n "${vip}" ]] || return 1
  printf '%s' "${vip}"
}

# SPIRE node attestation. swtpm on the CAPD node needs /dev/vtpmx from whichever
# kernel runs Docker: the Colima VM on macOS, where colima-start.sh guarantees
# it, and the host kernel on Linux, where the GitHub runners lack the module.
e2e_attestation_method() {
  if [[ "$(uname -s)" == Darwin || -e /sys/class/misc/vtpmx ]]; then
    printf 'tpm'
  else
    printf 'joinToken'
  fi
}

# Prefixes an official Docker Hub image with NKE_E2E_DOCKERHUB_MIRROR when that
# is set, so a host `docker run` or `docker create` of it is not an anonymous
# Hub pull from the runner's address. Bare names only: `alpine:3.22`, not
# `org/image`.
dockerhub_image() {
  local image="$1" mirror="${NKE_E2E_DOCKERHUB_MIRROR:-}"
  printf '%s' "${mirror:+${mirror}/library/}${image}"
}

# The bundle the node agent installs CNI from. hack/ci/prepare-capd-bundles.sh
# builds it from checksum-verified public sources, and the node image carries no
# bundle server of its own, so this is the only source a run has. Serving it from
# a volume rather than a host path keeps this working where the Docker daemon
# does not share the host filesystem, as on Colima.
e2e_start_bundle_mirror() {
  local name="$1" volume="$2" dir="$3" loader="$1-loader" ip
  docker volume create "${volume}" >/dev/null || return 1
  docker create --name "${loader}" -v "${volume}:/bundle" "$(dockerhub_image alpine:3.22)" true >/dev/null || return 1
  docker cp "${dir}/." "${loader}:/bundle/" >/dev/null || return 1
  docker rm "${loader}" >/dev/null || return 1
  docker run -d --name "${name}" --network kind \
    -v "${volume}:/usr/share/nginx/html:ro" "$(dockerhub_image nginx:alpine)" >/dev/null || return 1
  # An address, not the container name: tenant nodes are plain containers on the
  # kind network, where name resolution depends on the host resolver.
  ip="$(docker inspect "${name}" \
    --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}' 2>/dev/null)"
  [[ -n "${ip}" ]] || return 1
  printf '%s' "${ip}"
}

e2e_stop_bundle_mirror() {
  local name="$1" volume="$2"
  docker rm -f "${name}" "${name}-loader" >/dev/null 2>&1 || true
  docker volume rm "${volume}" >/dev/null 2>&1 || true
}

# tilt_config.json, written the same way for both entrypoints.
e2e_write_tilt_config() {
  local out="$1" providers="$2" cwe_image="$3" vip="$4" bgp_container="$5"
  local dsx_identity_exchange_image="${6:-}"
  jq -n \
    --arg cwe_image "${cwe_image}" \
    --arg site_controller_vip "${vip}" \
    --arg bgp_container_name "${bgp_container}" \
    --arg dsx_identity_exchange_image "${dsx_identity_exchange_image}" \
    --argjson providers "${providers}" \
    '{
      providers: $providers,
      cwe_image: $cwe_image,
      site_controller_vip: $site_controller_vip,
      bgp_containers_container_name: $bgp_container_name,
      dsx_identity_exchange_image: $dsx_identity_exchange_image
    }' >"${out}"
}

# The Ginkgo run. `exec_fn` runs a command against the E2E runner, which is the
# one thing that legitimately differs: CI execs into its inner runner, local
# into a container on the Kind network. Anything after `--` is appended to the
# suite's argv, which is how CI adds its extra assertions without either side
# carrying the other's concerns.
#
# The flags below are the ones both must share. `-ginkgo.fail-on-empty` is in
# that set deliberately: without it a label filter that matches no spec passes
# silently, which is the failure mode a shared filter constant exists to stop.
e2e_run_ginkgo() {
  local exec_fn="$1" log_fn="$2" state_dir="$3" label_filter="$4"
  local suite_minutes="${E2E_SUITE_TIMEOUT_MINUTES:-100}"
  shift 4
  [[ "${1:-}" == "--" ]] && shift

  "${log_fn}" "running E2E specs matching ${label_filter}"

  set +e
  "${exec_fn}" \
    /state/cwe-e2e.test \
    -test.v \
    -test.timeout="$((suite_minutes + 5))m" \
    -e2e.config=/state/e2e-config.json \
    -ginkgo.v \
    -ginkgo.no-color \
    -ginkgo.fail-on-empty \
    -ginkgo.timeout="${suite_minutes}m" \
    "-ginkgo.label-filter=${label_filter}" \
    "-ginkgo.junit-report=/state/junit.xml" \
    "$@" \
    2>&1 | tee "${state_dir}/e2e.log"
  local rc=${PIPESTATUS[0]}
  set -e

  if ((rc == 0)); then
    e2e_assert_no_new_skips "${log_fn}" "${state_dir}/e2e.log" || rc=1
  fi
  return "${rc}"
}

# Specs that call Skip() at run time for a reason we accept. Keyed by file, so
# editing a spec does not churn this list, and adding or deleting specs never
# touches it: a label-filter exclusion prints nothing, only a spec that started
# and skipped does.
E2E_EXPECTED_RUNTIME_SKIPS=(
  pod_cidr_test.go # multi-CIDR rollover, encoded only by the nico-vms specs
  oidc_test.go     # OIDC is off in the deployments this runs against
)

# Fails a leg on any runtime skip not listed above. Ginkgo reports these only
# inside a "127 Skipped" total that is 126 label-filter exclusions, so a spec
# that quietly stops running is indistinguishable from one that was never
# selected.
e2e_assert_no_new_skips() {
  local log_fn="$1" log_file="$2" file unexpected=()
  while read -r file; do
    [[ -n "${file}" ]] || continue
    # shellcheck disable=SC2076  # literal match is what we want here
    [[ " ${E2E_EXPECTED_RUNTIME_SKIPS[*]} " =~ " ${file} " ]] || unexpected+=("${file}")
  done < <(grep -oE '\[SKIPPED\] in \[It\] - [^ ]+' "${log_file}" |
    sed -E 's#.*/##; s#:[0-9]+$##' | sort -u)

  if ((${#unexpected[@]} > 0)); then
    "${log_fn}" "specs skipped at run time that were expected to run: ${unexpected[*]}" >&2
    return 1
  fi
}

# After the run the suite's delete specs should have removed every tenant
# cluster. Count CAPI Cluster objects rather than asking the controller API, which
# enforces auth: zero objects is an auth-free "the cluster is clean". Returns
# non-zero on timeout rather than exiting, because the two entrypoints want
# different things from that: CI dumps the surviving clusters, local stops
# outright.
e2e_wait_for_zero_tenant_clusters() {
  local kubectl_fn="$1" log_fn="$2"
  local deadline=$((SECONDS + 300)) list count
  while true; do
    if list="$("${kubectl_fn}" get clusters.cluster.x-k8s.io -A --no-headers 2>/dev/null)"; then
      count="$(printf '%s' "${list}" | grep -c . || true)"
      if [[ "${count}" == "0" ]]; then
        "${log_fn}" "cleanup gate: no tenant clusters remain"
        return 0
      fi
    fi
    if ((SECONDS >= deadline)); then
      "${log_fn}" "cleanup gate: ${count:-unknown} tenant cluster(s) still present after 5m" >&2
      return 1
    fi
    sleep 10
  done
}

# Runs node image $1's ovs-vswitchd check on the Docker kernel. ARC's runner
# cannot load modules itself, and its installed module files are not in DinD's
# filesystem. If the daemon cannot load the module, stream the runner's matching
# module tree into a privileged helper instead. Never invoke ovs-ctl: its fallback
# removes the bridge module and destroys the Docker networks (#1515).
e2e_host_loads_openvswitch() (
  set -o pipefail
  local image="$1" kernel module_root="${NKE_E2E_HOST_MODULES_DIR:-/lib/modules}"
  if docker run --rm --privileged -v /lib/modules:/lib/modules:ro --entrypoint /bin/sh "${image}" \
    -c '
      set -eu
      if test -e /sys/module/openvswitch; then
        echo "openvswitch was already loaded before the Docker probe"
      else
        modprobe --verbose openvswitch
        test -e /sys/module/openvswitch
        echo "openvswitch is available after the Docker module insertion attempt"
      fi
    '; then
    return 0
  fi

  kernel="$(uname -r)" || return 1
  if [[ "$(uname -s)" != Linux || ! -d "${module_root}/${kernel}" ]]; then
    echo "openvswitch is unavailable to Docker and no matching local module tree exists" >&2
    return 1
  fi
  echo "loading openvswitch through privileged Docker with the runner's ${kernel} module files" >&2
  tar -C "${module_root}" -cf - "${kernel}" |
    docker run --rm --privileged -i --entrypoint /bin/sh "${image}" -c '
      set -eu
      mkdir -p /lib/modules
      tar -C /lib/modules -xf -
      if test -e /sys/module/openvswitch; then
        echo "openvswitch was already loaded before the streamed-module probe"
      elif modprobe --verbose openvswitch; then
        echo "openvswitch is available after the streamed-module insertion attempt"
      else
        echo "openvswitch insertion failed; matching kernel messages:" >&2
        dmesg | tail -n 100 | grep -E "openvswitch|nsh|nf_conn|nf_nat|libcrc32c|module_layout|BTF|version.*symbol" >&2 || true
        exit 1
      fi
      test -e /sys/module/openvswitch
    '
)

# Per-node evidence: console, unit status, and the journals of the units a
# CAPD node depends on. Without this a node whose agent never binds its port is
# undiagnosable, and the entrypoints disagreed here too: CI collected it, local
# did not.
e2e_collect_node_diagnostics() {
  local out_dir="$1" node_image="$2" node_name
  mkdir -p "${out_dir}"
  while IFS= read -r node_name; do
    [[ -n "${node_name}" ]] || continue
    docker logs "${node_name}" >"${out_dir}/${node_name}-console.log" 2>&1
    docker exec "${node_name}" systemctl status \
      nvke-agent spire-agent swtpm-local containerd kubelet \
      ovsdb-server ovs-vswitchd nke-container-network nke-container-cni-images --no-pager \
      >"${out_dir}/${node_name}-systemd.txt" 2>&1
    docker exec "${node_name}" journalctl --no-pager \
      -u nvke-agent -u spire-agent -u swtpm-local -u containerd -u kubelet \
      -u ovsdb-server -u ovs-vswitchd -u nke-container-network -u nke-container-cni-images \
      >"${out_dir}/${node_name}-journal.txt" 2>&1
  done < <(docker ps -a --filter "ancestor=${node_image}" --format '{{.Names}}')
}

# The Tilt providers a CAPD run needs. This belongs beside the filters because
# the two are one contract: the suite filter selects reboot_test.go, which
# reboots a worker through the nke.nvidia.com/reboot label, and nothing acts on
# that label unless capd-reboot-controller is deployed.
# shellcheck disable=SC2034  # consumed by the scripts that source this file
E2E_CAPD_PROVIDERS='["capi_core", "bgp-containers", "capd", "capd-reboot-controller"]'

# The label filter for the full suite, built from the backend's ginkgo label. It
# runs every entry for the backend, cp-controller entries included, plus the
# `health` probes against the deployed controller and the authentication specs.
# authn-spire stays out because it needs SPIRE pod attestation that neither
# container-backed backend wires up. The CI workflows pass the same expression
# as `label-filter` for their full runs; keep the three in step.
e2e_suite_filter() {
  printf '(%s || health) || (authn && !authn-spire) || authn-permissive' "$1"
}

# Kept for callers that still name CAPD directly.
# shellcheck disable=SC2034  # consumed by the scripts that source this file
E2E_CAPD_SUITE_FILTER="$(e2e_suite_filter capd)"

# Build-args that point a Dockerfile's Docker Hub bases at the mirror in
# NKE_E2E_DOCKERHUB_MIRROR, one token per line for `mapfile -t`. Handles
# `ARG BASE_REGISTRY=docker.io` and `ARG <NAME>_IMAGE=docker.io/<image>`, and
# prints nothing when the mirror is unset. The dind daemon on the ARC runners
# has no registry mirror of its own, and the arm64 pool shares one egress
# address, so anonymous docker.io pulls return 429 whenever several lanes
# build at once (failure 26 in the 2026-10-02 CI notes).
dockerhub_build_args() {
  local dockerfile="$1" mirror="${NKE_E2E_DOCKERHUB_MIRROR:-}" name value
  [[ -n "${mirror}" ]] || return 0
  while IFS='=' read -r name value; do
    case "${value}" in
      docker.io) printf -- '--build-arg\n%s=%s/library\n' "${name}" "${mirror}" ;;
      docker.io/*/*) printf -- '--build-arg\n%s=%s/%s\n' "${name}" "${mirror}" "${value#docker.io/}" ;;
      docker.io/*) printf -- '--build-arg\n%s=%s/library/%s\n' "${name}" "${mirror}" "${value#docker.io/}" ;;
    esac
  done < <(sed -n 's/^ARG \([A-Z_]*\)=\(docker\.io[^ ]*\)$/\1=\2/p' "${dockerfile}")
}

# Copies a kind config to $2, pointing its `image: kindest/node:...` lines at
# NKE_E2E_DOCKERHUB_MIRROR when that is set. kind pulls the node image through
# the host daemon, which on the ARC runners has no registry mirror, so this was
# the last anonymous docker.io pull every CAPI lane made. The digest is the same
# on the mirror, and the containerd mirror written into the nodes afterwards is
# unaffected.
kind_config_for_mirror() {
  local src="$1" dst="$2" mirror="${NKE_E2E_DOCKERHUB_MIRROR:-}"
  if [[ -n "${mirror}" ]]; then
    sed -E "s#^(\s*image: )kindest/node:#\1${mirror}/kindest/node:#" "${src}" >"${dst}"
  else
    cp "${src}" "${dst}"
  fi
}

# Points containerd in the kind node of cluster $1 at NKE_E2E_DOCKERHUB_MIRROR
# for Docker Hub pulls, when that is set. Both Hub host names, since charts and
# manifests name either. Without this every image the management cluster runs
# from Docker Hub is an anonymous pull from the runner's address.
configure_kind_dockerhub_mirror() {
  local cluster_name="$1" mirror="${NKE_E2E_DOCKERHUB_MIRROR:-}" node_name registry_host
  [[ -n "${mirror}" ]] || return 0
  node_name="${cluster_name}-control-plane"
  log "routing management-cluster Docker Hub pulls through ${mirror}"
  for registry_host in docker.io registry-1.docker.io; do
    docker exec "${node_name}" mkdir -p "/etc/containerd/certs.d/${registry_host}"
    printf '%s\n' \
      "server = \"https://${mirror}\"" \
      '' \
      "[host.\"https://${mirror}\"]" \
      '  capabilities = ["pull", "resolve"]' |
      docker exec -i "${node_name}" cp /dev/stdin "/etc/containerd/certs.d/${registry_host}/hosts.toml"
  done
}
