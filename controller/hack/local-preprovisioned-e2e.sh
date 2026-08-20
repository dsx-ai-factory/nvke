#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Bring up a local environment for the preprovisioned E2E specs and run them.
#
# The specs need four things, in order:
#
#   1. a kind management cluster, which also creates the `kind` docker network
#      the host containers attach to
#   2. CAPI core installed in it. The Cluster API machine controller is what
#      sets the ownerRef on an infrastructure machine and copies its
#      spec.providerID and status.addresses up to the Machine, which is what
#      both CWE and the provider-ID controller read. The CAPD *infrastructure*
#      provider is deliberately not installed: nothing on this path creates
#      DockerMachines, and its absence proves it.
#   3. the preprovisioned provider CRDs, which CWE's controllers watch
#   4. CWE deployed by Tilt with the provider turned on
#
# The substrate containers themselves are owned by the suite, not by this
# script. See test/e2e/internal/substrate.
#
# Usage:
#   hack/local-preprovisioned-e2e.sh
#   LOCAL_PREPROV_E2E_LABEL_FILTER=substrate hack/local-preprovisioned-e2e.sh

set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
state_dir="${LOCAL_PREPROV_E2E_STATE_DIR:-${repo_root}/.local/preprovisioned-e2e}"
tmp_dir="$(mktemp -d)"
tilt_config="${repo_root}/tilt_config.json"
tilt_config_backup="${tmp_dir}/tilt_config.json"
# Keep `preprovisioned &&` in any override. The other labels are shared with the
# rest of the suite: `happy-path` alone also selects the CAPD and forge-vms
# entries in tenant_test.go, which cannot pass here because this script installs
# CAPI core without an infrastructure provider.
label_filter="${LOCAL_PREPROV_E2E_LABEL_FILTER:-preprovisioned}"
had_tilt_config=false
tilt_config_staged=false
port_forward_pid=""
bgp_container_name="${LOCAL_PREPROV_E2E_BGP_CONTAINER_NAME:-bgp-containers}"
bgp_started=false

log() {
  printf '[local-preprovisioned-e2e] %s\n' "$*"
}

fail() {
  log "$*" >&2
  exit 1
}

cleanup() {
  local rc=$?
  set +e

  if [[ -n "${port_forward_pid}" ]]; then
    kill "${port_forward_pid}" 2>/dev/null
  fi

  if [[ "${bgp_started}" == true ]]; then
    CONTAINER_NAME="${bgp_container_name}" "${repo_root}/hack/bgp-containers/down.sh"
  fi

  if [[ "${rc}" -ne 0 ]]; then
    mkdir -p "${state_dir}"
    log "collecting diagnostics into ${state_dir}"
    kubectl get pods -A -o wide >"${state_dir}/management-pods.txt" 2>&1
    kubectl -n cloud-workflow-engine logs deployment/cloud-workflow-engine \
      --all-containers --tail=-1 >"${state_dir}/cwe.log" 2>&1
    kubectl get preprovisionedclusters,preprovisionedmachines -A -o yaml \
      >"${state_dir}/preprovisioned-objects.yaml" 2>&1
    docker ps -a >"${state_dir}/docker-ps.txt" 2>&1
  fi

  # Restore whatever tilt_config.json the developer had before this run.
  if [[ "${tilt_config_staged}" == true ]]; then
    if [[ "${had_tilt_config}" == true ]]; then
      cp "${tilt_config_backup}" "${tilt_config}"
    else
      rm -f "${tilt_config}"
    fi
  fi

  rm -rf "${tmp_dir}"
  exit "${rc}"
}
trap cleanup EXIT

for command in docker go helm kind kubectl tilt; do
  command -v "${command}" >/dev/null 2>&1 || fail "${command} is required in PATH"
done

cd "${repo_root}"

# Fail on a missing prerequisite now rather than after ten minutes of cluster
# bring-up. The Tiltfile checks both, but only once it loads, which is the last
# step of the sequence below.
if [[ ! -f .env.tilt ]]; then
  missing=()
  for credential in DGXC_TENANT_USER DGXC_TENANT_PASSWORD \
    CNPG_SUPERUSER_PASSWORD CNPG_CLOUD_PASSWORD CNPG_TEMPORAL_PASSWORD; do
    [[ -n "${!credential:-}" ]] || missing+=("${credential}")
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    fail "missing local-dev credential(s): ${missing[*]}
  Tilt has no defaults for these. Copy .env.tilt.example to .env.tilt and fill
  them in, or export them before running this script. Any value works: they
  belong to a throwaway kind cluster."
  fi
fi

# The fleet OS profiles live in nke-site-deploy, which most checkouts do not
# have as a sibling. Nothing on the preprovisioned path reads them: a CAPI
# cluster has no osProfile field, and the machine template carries the image.
# So default to the in-repo fixture tree rather than require that checkout.
export CWE_PROFILES_DIR="${CWE_PROFILES_DIR:-test/fixtures/profiles}"
log "using CWE_PROFILES_DIR=${CWE_PROFILES_DIR}"

log "ensuring the kind management cluster"
make -f Makefile.dev kind-up

# CAPI core must be installed before Tilt runs: the Tiltfile's
# check-management-cluster preflight asserts capi-controller-manager exists and
# fails the whole run otherwise.
log "ensuring CAPI core is installed"
make -f Makefile.dev capi-core-up

# The shared configure-kube-vip runbook uses BGP. Start the repository's FRR
# peer so kube-vip can advertise the API server VIP.
log "starting the FRR peer for the shared kube-vip runbook"
CONTAINER_NAME="${bgp_container_name}" NETWORK_NAME="kind" ./hack/bgp-containers/up.sh
bgp_started=true

# up.sh runs FRR on the host network, so the peer is already the substrate's
# default gateway (the kind bridge gateway) and no override is needed. Only
# point the hosts at it explicitly if it ever lands on the bridge instead,
# where its own address is what they must route through.
bgp_bridge_address="$(docker inspect "${bgp_container_name}" \
  -f '{{range $network, $config := .NetworkSettings.Networks}}{{if eq $network "kind"}}{{$config.IPAddress}}{{end}}{{end}}' \
  2>/dev/null || true)"
if [[ -n "${bgp_bridge_address}" ]]; then
  log "FRR peer is on the kind bridge at ${bgp_bridge_address}; using it as the host gateway"
  export CWE_E2E_PREPROV_GATEWAY="${bgp_bridge_address}"
else
  log "FRR peer is on the host network; leaving the hosts' default gateway alone"
fi

# The substrate's node image derives from the CAPD node image, which is where
# nke-agent, spire-agent and the vTPM actually come from.
log "ensuring the CAPD node image (base for the preprovisioned node image)"
make -f Makefile.dev capd-image

# The provider controllers watch these kinds, and a watch on a kind the API
# server does not know stops the manager from starting. They ship with the
# provider rather than with the CWE chart, so apply them here.
log "installing the preprovisioned provider CRDs"
kubectl apply -f preprovisioned/config/crd/

# An interrupted `tilt ci` leaves the CWE helm release wedged in
# pending-install/pending-upgrade, and every later run then fails with
# "another operation (install/upgrade/rollback) is in progress". There is no
# earlier revision to roll back to on a first install, so clear it outright.
release_status="$(helm status cwe -n cloud-workflow-engine -o json 2>/dev/null |
  sed -n 's/.*"status":"\([a-z-]*\)".*/\1/p' | head -1 || true)"
case "${release_status}" in
  pending-*)
    log "clearing wedged helm release cwe (status=${release_status})"
    helm uninstall cwe -n cloud-workflow-engine --wait --timeout 5m || true
    ;;
esac

log "staging tilt_config.json with providers=[capi_core]"
if [[ -f "${tilt_config}" ]]; then
  cp "${tilt_config}" "${tilt_config_backup}"
  had_tilt_config=true
fi
printf '%s\n' '{"providers": ["capi_core"]}' >"${tilt_config}"
tilt_config_staged=true

log "deploying CWE with tilt ci"
TILT_DISABLE_ANALYTICS=1 tilt ci --timeout "${LOCAL_PREPROV_E2E_TILT_TIMEOUT:-25m}"

# Three flags, for three different reasons:
#
#   ENABLE_CAPI                          registers the CAPI controllers at all,
#                                        including the provider-ID controller
#                                        that gives each tenant Node its
#                                        providerID
#   ENABLE_PREPROVISIONED_PROVIDER       registers the two provider reconcilers
#   LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP
#                                        the substrate's "hosts" are containers,
#                                        so kubeadm needs the container-mode
#                                        contract and preflight needs the
#                                        bootstrap-success sentinel rather than
#                                        a cloud-init status. Real preprovisioned
#                                        hardware needs neither; this is a
#                                        property of the local substrate, not of
#                                        the path under test, which is why it is
#                                        set here rather than inferred from the
#                                        machine template.
log "enabling the preprovisioned provider on the CWE deployment"
kubectl -n cloud-workflow-engine set env deployment/cloud-workflow-engine \
  ENABLE_CAPI=true \
  ENABLE_PREPROVISIONED_PROVIDER=true \
  LOCAL_DEV_FORCE_DOCKER_MACHINE_BOOTSTRAP=true
kubectl -n cloud-workflow-engine rollout status deploy/cloud-workflow-engine --timeout=5m

# CWE rejects a cluster whose siteID is not a registered site, and the nil-UUID
# site that marks a cluster CAPI-backed is registered by a manual button in the
# Tilt UI, which `tilt ci` never presses. Headless runs have to do it themselves
# or every createCluster fails with "Site ID ... is not registered".
log "registering the default (nil UUID) site"
kubectl -n cloud-workflow-engine port-forward svc/cloud-workflow-engine 18080:8080 >/dev/null 2>&1 &
port_forward_pid=$!
until curl -fsS http://localhost:18080/api/sites >/dev/null 2>&1; do sleep 1; done
API_URL="http://localhost:18080/api" ./hack/register-default-site.sh
kill "${port_forward_pid}" 2>/dev/null || true
port_forward_pid=""

log "running specs matching label filter '${label_filter}'"
cd "${repo_root}/test/e2e"
go test . -count=1 -timeout "${LOCAL_PREPROV_E2E_TIMEOUT:-90m}" -args \
  -ginkgo.v \
  -ginkgo.label-filter="${label_filter}"

log "done"
