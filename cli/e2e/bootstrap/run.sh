#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# `nvkectl bootstrap site` e2e entrypoint, invoked by `make test-e2e-bootstrap`
# and the "nvkectl bootstrap e2e" CI workflow.
#
# Stages:
#   1. Fake node containers come up and nvke-agent answers on each.
#   2. `bootstrap site`, run in the background. The moment its ephemeral Kind
#      cluster is ready this suite bridges the two container networks and
#      proves packets cross both ways. The command then deploys Postgres,
#      Temporal, SPIRE and the controller, builds a cluster over the pre-provisioned
#      hosts, writes its kubeconfig and exits 0.
#   3. Every fake node's spire-agent attested to that SPIRE server over TPM.
#      The EK thumbprints were an input to the command, so a bootstrap that
#      deployed SPIRE without the TPM attestor, or dropped the allowlist,
#      leaves every agent locked out.
#   4. Every site spec node reports Ready through the kubeconfig the command
#      wrote -- the cluster an operator gets, not the bootstrap cluster.
#      Check the API endpoint, BGP next-hops and readiness through the VIP.
#      Set NVKECTL_E2E_CONTROL_PLANE_VIP="" only with NVKECTL_E2E_NODES=1 to
#      exercise the supported single-node configuration without kube-vip.
#   5. Traefik serves a whoami Deployment on that cluster: hosts that already
#      existed now serve traffic like any other cluster.
#
# The EXIT trap also scans what the run printed and saved, the bootstrap log and
# the diagnostics bundle, for credentials with gitleaks and gitleaks.toml. It
# runs on success and on failure, so a change that starts printing a secret turns
# the run red. CI sets NVKECTL_E2E_REQUIRE_SECRET_SCAN=1, which makes a missing
# gitleaks a failure; a developer running this locally without it only sees a
# warning.
#
# Everything created is registered for teardown as it is created, and the EXIT
# trap runs those registrations in reverse, whatever the exit. A failing exit
# first collects diagnostics into NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR.
#
# The suite runs entirely on nerdctl/containerd, which `make -C cli
# check-no-docker` keeps true. It needs on the host: nerdctl and a containerd
# for kind to drive (nvkectl sets KIND_EXPERIMENTAL_PROVIDER=nerdctl
# unconditionally; kind states 1.7.0 as its minimum), buildkitd for `nerdctl
# build` to reach, and root or sudo to install iptables FORWARD rules and
# the SPIRE VIP. `cli/hack/bootstrap-e2e-host.sh install-runtime` sets up the
# first two the way CI does.
#
# jq is required for BGP route checks when the control-plane VIP is enabled.
#
# NVKECTL_NVKE_CONTROLLER_IMAGE must already be in containerd's image store: nvkectl saves it
# from there and loads it into the kind cluster rather than pulling from inside
# the cluster, so no registry credentials are needed past that point.

set -Eeuo pipefail

# ── Configuration ──────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_DIR="$(dirname "$(dirname "${SCRIPT_DIR}")")"
NVKECTL="${NVKECTL:-${CLI_DIR}/bin/nvkectl}"
# The site spec is generated per run by `nvkectl generate sitespec`, so this lane
# tests the command an operator uses rather than a fixture kept in step with it
# by hand. Only the cluster name is fixed here.
E2E_CLUSTER_NAME="nke-e2e"
BOOTSTRAP_KIND_CLUSTER="${NVKECTL_BOOTSTRAP_CLUSTER_NAME:-nvkectl-bootstrap}"
BOOTSTRAP_CONTROL_PLANE="${BOOTSTRAP_KIND_CLUSTER}-control-plane"
# The namespace and Deployment `bootstrap site` installs the controller under.
CONTROLLER_NAME=nvke-controller
# The fake-node fixture, as the substrate package names it: N containers on one
# private network, named by prefix and index. The defaults here mirror
# substrate.DefaultConfig; override the same variables to change both.
NODE_COUNT="${NVKECTL_E2E_NODES:-3}"
NODE_PREFIX="${NVKECTL_E2E_NAME_PREFIX:-node}"
NODE_NETWORK="${NVKECTL_E2E_NETWORK:-nvkectl-e2e-nodes}"
NODE1="nvkectl-e2e-${NODE_PREFIX}1"
# The port nvke-agent listens on, as the site spec's nodeAgentPort defaults it.
NODE_AGENT_PORT=15005
# SiteSpec generation records the exact journal boundary and expected result
# for every node here. The later check reads the same file after bootstrap.
export NVKECTL_E2E_ATTESTATION_BASELINE="${CLI_DIR}/.local/attestation-baseline.json"
# Where the node images' spire-agents were built to look for the SPIRE server.
SPIRE_VIP="${NVKECTL_E2E_SPIRE_SERVER_ADDRESS:-172.26.0.2}"
# Use a value that differs from bootstrap's derived
# "<bootstrap-cluster>.nke.local" default. The generated site spec and the
# pre-provisioned agents must receive this same explicit trust domain, or node
# attestation fails.
SPIRE_TRUST_DOMAIN="${NVKECTL_E2E_SPIRE_TRUST_DOMAIN:-nvkectl-e2e.example.test}"
export NVKECTL_E2E_SPIRE_TRUST_DOMAIN="${SPIRE_TRUST_DOMAIN}"
# First CSV node, used for Traefik access. The controller may initialize another node.
INIT_NODE_IP="${NVKECTL_E2E_INIT_NODE_IP:-172.26.0.10}"
# Where Traefik is published on the new cluster, and where stage 5 asks for the
# page. In the NodePort range, and clear of the SPIRE server's 30081.
TRAEFIK_NODE_PORT="${NVKECTL_E2E_TRAEFIK_NODE_PORT:-30082}"
# Where `bootstrap site` writes the finished cluster's kubeconfig, and the key
# that encrypts its etcd data.
TENANT_KUBECONFIG="${SCRIPT_DIR}/.tenant.kubeconfig"
ETCD_KEY_FILE="${SCRIPT_DIR}/.etcd-encryption-key.json"
BOOTSTRAP_LOG="${SCRIPT_DIR}/.bootstrap.log"
# What the Go test that runs the command prints about the run itself. nvkectl's
# own output is not in here: it goes to BOOTSTRAP_LOG, so there is one copy of
# it and the credential scan covers that copy.
BOOTSTRAP_GO_TEST_LOG="${SCRIPT_DIR}/.bootstrap-go-test.log"
GO="${GO:-go}"
# The lane runs `bootstrap site` through the Go tests in bootstraptest. Those
# tests carry two marks: the e2e_bootstrap build tag, which keeps them out of
# `make test` by leaving them unbuilt, and the TestE2E_ name prefix, which is
# what -run selects once the tag is on.

# Keep the VIP outside the node subnet so nodes route through the host's
# BGP-learned /32 instead of ARPing for another node's loopback address.
# An explicit empty value disables the VIP; an unset variable uses the default.
CONTROL_PLANE_VIP="${NVKECTL_E2E_CONTROL_PLANE_VIP-172.26.1.1}"
if [ -z "${CONTROL_PLANE_VIP}" ] && [ "${NODE_COUNT}" -gt 1 ]; then
  printf 'FAIL: NVKECTL_E2E_CONTROL_PLANE_VIP may be empty only when NVKECTL_E2E_NODES=1; multi-node control planes require kube-vip\n' >&2
  exit 1
fi
if [ -n "${CONTROL_PLANE_VIP}" ] && ! command -v jq >/dev/null 2>&1; then
  printf 'FAIL: jq is required for the control-plane VIP checks\n' >&2
  exit 1
fi
# FRR learns the VIP /32 from kube-vip and installs it in the host routing table.
BGP_CONTAINER="nvkectl-e2e-bgp"
FRR_IMAGE="${NVKECTL_E2E_FRR_IMAGE:-quay.io/frrouting/frr:9.1.0}"
REPO_ROOT="$(dirname "${CLI_DIR}")"
BGP_FILES_DIR="${REPO_ROOT}/controller/hack/bgp-containers"
# Scratch dir for the rendered FRR config, bind-mounted into the container.
BGP_RENDER_DIR="${SCRIPT_DIR}/.bgp"
# shellcheck source=bgp-lib.sh
source "${SCRIPT_DIR}/bgp-lib.sh"
# shellcheck source=etcd-lib.sh
source "${SCRIPT_DIR}/etcd-lib.sh"
: "${NVKECTL_NVKE_CONTROLLER_IMAGE:?NVKECTL_NVKE_CONTROLLER_IMAGE must name the controller image to deploy, already present in containerd (see comment above)}"
export NVKECTL_NVKE_CONTROLLER_IMAGE

# The Cluster API infrastructure provider for hosts that already exist, built
# below from this checkout. Without it the CAPI objects the NVKE controller writes have no
# reconciler and the cluster never leaves provisioning. nvkectl finds the
# kustomize base by walking up to go.work, so only the image is named here.
export NVKECTL_CAPI_PROVISIONED_IMAGE="${NVKECTL_CAPI_PROVISIONED_IMAGE:-cluster-api-provider-provisioned:latest}"

# Where a failed bootstrap writes its cluster dump. Set explicitly because
# nvkectl's default is relative to the working directory, which would put the
# bundle where the CI artifact path does not match.
DIAGNOSTICS_DIR="${CLI_DIR}/cluster-diagnostics"
export NVKECTL_BOOTSTRAP_DIAGNOSTICS_DIR="${DIAGNOSTICS_DIR}"

# `kind create cluster` merges into $KUBECONFIG, defaulting to ~/.kube/config:
# use a scratch path so this run never takes ownership of the caller's. Under
# CI's `sudo -E` that file would otherwise end up root-owned, and a later step
# running as the normal user could not update it.
KUBECONFIG_DIR="$(mktemp -d)"
export KUBECONFIG="${KUBECONFIG_DIR}/config"
CNI_BUNDLE_DIR="$(mktemp -d)"

# KUBECONFIG alone is not enough: helm caches API discovery under ~/.kube/cache
# and creates ~/.kube mode 0750 to do it, root-owned under `sudo -E`.
# KUBECACHEDIR moves that cache into the same scratch directory.
export KUBECACHEDIR="${KUBECONFIG_DIR}/cache"

# ── Helpers ────────────────────────────────────────────────────────────────

log() { printf '[nvkectl-bootstrap-e2e] %s\n' "$*"; }

# fail logs one or more lines as a failure and exits 1. The EXIT trap then
# collects diagnostics and tears down whatever was created.
fail() {
  log "FAIL: $1"
  shift
  local line
  for line in "$@"; do log "  ${line}"; done
  exit 1
}

# NETCONFPATH outranks the cni_netconfpath bootstrap-e2e-host.sh writes, and
# this one would put the suite's nerdctl networks where nvkectl's kind commands
# no longer look and its preflight refuses to start.
netconfpath="${NETCONFPATH:-}"
if [ "${netconfpath%/}" = /etc/cni/net.d ]; then
  fail "NETCONFPATH names /etc/cni/net.d, the directory nvkectl keeps nerdctl's networks out of" \
    "unset it, or point it at /etc/containerd-nerdctl/net.d"
fi

# scan_output_for_secrets scans the bootstrap log and the diagnostics bundle for
# credentials, and returns 1 if it finds one or cannot tell. It prints the
# scanner's findings with the secret redacted and never the secret itself: this
# output goes to a public CI log.
#
# It first proves the rules work, because a clean result from a scanner that
# matches nothing means nothing. Each shape the rules target must be flagged when
# it holds a value, and must pass when it holds nvkectl's <redacted> placeholder.
# The sample values are plain on purpose, so the commit that adds this line does
# not trip the Secrets workflow.
#
# --exit-code 2 separates a finding (2) from a scanner failure (1).
GITLEAKS_CONFIG="${SCRIPT_DIR}/gitleaks.toml"
gitleaks_scan() {
  local target="$1"
  shift
  gitleaks dir "${target}" --config "${GITLEAKS_CONFIG}" --redact=100 --no-banner --exit-code 2 "$@"
}
scan_output_for_secrets() {
  if ! command -v gitleaks >/dev/null 2>&1; then
    if [ "${NVKECTL_E2E_REQUIRE_SECRET_SCAN:-}" = 1 ]; then
      log "FAIL: gitleaks is not on PATH, so the bootstrap output was not scanned for credentials"
      return 1
    fi
    log "gitleaks is not on PATH: skipping the scan of the bootstrap output for credentials (CI requires it)"
    return 0
  fi
  if [ ! -f "${BOOTSTRAP_LOG}" ]; then
    log "no bootstrap output to scan: bootstrap site was never started"
    return 0
  fi

  local work rc name shape fingerprint join_token leaked plog trace_hex span_hex
  work="$(mktemp -d)"

  # The rules, proved on a sample of each shape: flagged with a value, clean with
  # the placeholder.
  mkdir -p "${work}/flagged" "${work}/clean"
  for shape in \
    'helm|helm upgrade --install demo chart --set-string auth.password=PLACEHOLDER --wait' \
    'url|{"env":[{"name":"DATABASE_URL","value":"postgres://cloud:PLACEHOLDER@db.example:5432/cloud"}]}' \
    'field|{"auth":{"username":"cloud","password":"PLACEHOLDER"}}' \
    'unquoted|      POSTGRES_PASSWORD:  PLACEHOLDER' \
    'env|{"env":[{"name":"POSTGRES_PASSWORD","value":"PLACEHOLDER"}]}'; do
    name="${shape%%|*}"
    printf '%s\n' "${shape#*|}" | sed 's/PLACEHOLDER/aaaa-bbbb-cccc/' >"${work}/flagged/${name}.txt"
    printf '%s\n' "${shape#*|}" | sed 's/PLACEHOLDER/<redacted>/' >"${work}/clean/${name}.txt"
  done
  # Password fields that hold no credential, which the rules must not flag: a
  # value kubectl describe prints for a Secret, a boolean, and an environment
  # variable fed from a Secret, which has valueFrom and no value.
  printf "      POSTGRES_PASSWORD:  <set to the key 'password' in secret 'pg'>  Optional: false\n" >"${work}/clean/describe-secret.txt"
  printf 'spec:\n  usePassword: true\n  password: null\n' >"${work}/clean/password-state.txt"
  printf '{"env":[{"name":"POSTGRES_PASSWORD","valueFrom":{"secretKeyRef":{"name":"pg","key":"password"}}}]}\n' >"${work}/clean/env-secretref.txt"
  # The one line the rules allow, and the same line under another message, which
  # they must not allow. The hash is computed here, not written in this file: a
  # literal would trip the Secrets workflow just as it trips the default rules.
  fingerprint="$(printf '%s' sample | sha256sum | cut -d' ' -f1)"
  printf 'time=x level=INFO msg="wrote the etcd encryption key" component=x path=/x/.etcd-encryption-key.json sha256=%s trace_id=%032x span_id=%016x\n' \
    "${fingerprint}" 1 1 >"${work}/clean/etcd-fingerprint.txt"
  printf 'time=x level=INFO msg="wrote the file" component=x path=/x/.etcd-encryption-key.json sha256=%s trace_id=%032x span_id=%016x\n' \
    "${fingerprint}" 1 1 >"${work}/flagged/etcd-other.txt"
  # The SPIRE CA line, which holds file paths, and the same line with a field
  # appended after the span ID, which the allowlist does not match. The
  # IDs are derived, and random-looking: with a trivial span ID the default rule
  # would not flag the line even without the allowlist, and the sample would
  # prove nothing.
  trace_hex="$(printf t | sha256sum | cut -c1-32)"
  span_hex="$(printf s | sha256sum | cut -c1-16)"
  printf 'time=x level=INFO msg="SPIRE upstream CA from the environment" component=x cert=/w/cli/.local/spire-ca/ca.crt key=/w/cli/.local/spire-ca/ca.key trace_id=%s span_id=%s\n' \
    "${trace_hex}" "${span_hex}" >"${work}/clean/spire-ca-paths.txt"
  printf 'time=x level=INFO msg="SPIRE upstream CA from the environment" component=x cert=/w/cli/.local/spire-ca/ca.crt key=/w/cli/.local/spire-ca/ca.key trace_id=%s span_id=%s extra=%s\n' \
    "${trace_hex}" "${span_hex}" "${fingerprint}" >"${work}/flagged/spire-ca-extra.txt"
  # The two hashes the rules allow, and a join token and certificate key beside one
  # of them, which they must not. Computed here for the same reason as above.
  printf '{"msg":"update","api_encryption_key_sha256":"%s"}\n' "${fingerprint}" >"${work}/clean/key-fingerprint.txt"
  printf 'kubeadm join x:6443 --discovery-token-ca-cert-hash sha256:%s --control-plane\n' "${fingerprint}" >"${work}/clean/ca-hash.txt"
  join_token="$(printf a | sha256sum | cut -c1-6).$(printf b | sha256sum | cut -c1-16)"
  printf '{"args":["join","--token","%s","--discovery-token-ca-cert-hash","sha256:%s"]}\n' \
    "${join_token}" "${fingerprint}" >"${work}/flagged/join-token.txt"
  # The allowed etcd and SPIRE lines with a credential-shaped field added where
  # a loose allowlist would let it ride along: between the message and the path,
  # and before the message. The value is derived, and long enough for the default
  # rules to flag it under these names.
  leaked="$(printf leak | sha256sum | cut -c1-32)"
  printf 'time=x level=INFO msg="wrote the etcd encryption key" component=x token=%s path=/x/.etcd-encryption-key.json sha256=%s\n' \
    "${leaked}" "${fingerprint}" >"${work}/flagged/etcd-field-before-path.txt"
  printf 'token=%s time=x level=INFO msg="wrote the etcd encryption key" component=x path=/x/.etcd-encryption-key.json sha256=%s\n' \
    "${leaked}" "${fingerprint}" >"${work}/flagged/etcd-field-before-msg.txt"
  printf 'time=x level=INFO msg="SPIRE upstream CA from the environment" component=x api_key=%s cert=/w/ca.crt key=/w/ca.key\n' \
    "${leaked}" >"${work}/flagged/spire-field-before-cert.txt"
  printf 'api_key=%s time=x level=INFO msg="SPIRE upstream CA from the environment" component=x cert=/w/ca.crt key=/w/ca.key\n' \
    "${leaked}" >"${work}/flagged/spire-field-before-msg.txt"
  for name in "${work}"/flagged/*; do
    rc=0
    gitleaks_scan "${name}" >/dev/null 2>&1 || rc=$?
    if [ "${rc}" -ne 2 ]; then
      log "FAIL: the secret scan's rules did not flag the sample ${name##*/} (exit ${rc}), so a clean scan proves nothing"
      rm -rf "${work}"
      return 1
    fi
  done
  rc=0
  gitleaks_scan "${work}/clean" >/dev/null 2>&1 || rc=$?
  if [ "${rc}" -ne 0 ]; then
    log "FAIL: the secret scan flagged the clean samples, the <redacted> placeholder and the fingerprint and hash lines it allows (exit ${rc}), so it would flag a clean run"
    rm -rf "${work}"
    return 1
  fi

  # The scan itself, driven the way it is on the real output: a secret planted
  # in the log, in a file of the diagnostics bundle, and in a pod log inside it
  # (the controller's, in the real bundle) must each fail it, and the same
  # files without it must pass.
  local planted clean_log diag where
  planted="$(sed 's/PLACEHOLDER/aaaa-bbbb-cccc/' <<<'helm upgrade --install demo chart --set-string auth.password=PLACEHOLDER')"
  clean_log="${work}/clean.log"
  printf 'msg="nothing to see" trace_id=%032x\n' 1 >"${clean_log}"
  diag="${work}/diag"
  reset_diag() {
    mkdir -p "${diag}/nvke/controller"
    printf 'no secret here\n' >"${diag}/all-resources.txt"
    printf 'no secret here\n' >"${diag}/nvke/controller/logs.txt"
  }
  reset_diag
  if ! scan_run_output "${clean_log}" "${diag}" >/dev/null 2>&1; then
    log "FAIL: the scan failed a log and diagnostics bundle with no secret in them"
    rm -rf "${work}"
    return 1
  fi
  for where in log diagnostics-file pod-log; do
    reset_diag
    plog="${clean_log}"
    case "${where}" in
      log) printf '%s\n' "${planted}" >"${work}/planted.log"; plog="${work}/planted.log" ;;
      diagnostics-file) printf '%s\n' "${planted}" >"${diag}/all-resources.txt" ;;
      pod-log) printf '%s\n' "${planted}" >"${diag}/nvke/controller/logs.txt" ;;
    esac
    if scan_run_output "${plog}" "${diag}" >/dev/null 2>&1; then
      log "FAIL: the scan passed a secret planted in the ${where}"
      rm -rf "${work}"
      return 1
    fi
  done

  rc=0
  scan_run_output "${BOOTSTRAP_LOG}" "${DIAGNOSTICS_DIR}" || rc=$?
  rm -rf "${work}"
  return "${rc}"
}

# scan_run_output scans a bootstrap log and a diagnostics directory, which may
# not exist, with gitleaks. It returns 1 on a finding or a scanner failure.
scan_run_output() {
  local log_file="$1" diag_dir="$2" out rc
  out="$(mktemp -d)"
  mkdir -p "${out}/output"
  cp "${log_file}" "${out}/output/bootstrap.log"
  if [ -d "${diag_dir}" ]; then
    mkdir -p "${out}/output/diagnostics"
    cp -R "${diag_dir}/." "${out}/output/diagnostics/" || {
      log "FAIL: could not read ${diag_dir}, so it was not scanned for credentials"
      rm -rf "${out}"
      return 1
    }
  fi
  rc=0
  # --verbose names each finding: rule, file and line, with the secret redacted.
  gitleaks_scan "${out}/output" --verbose || rc=$?
  rm -rf "${out}"
  case "${rc}" in
    0) log "PASS: no credential in the bootstrap log or its diagnostics" ;;
    2) log "FAIL: gitleaks found a credential in the bootstrap output; the findings above are redacted" ; return 1 ;;
    *) log "FAIL: gitleaks could not scan the bootstrap output (exit ${rc})" ; return 1 ;;
  esac
}

# defer registers a command for the EXIT trap, which runs registrations in
# reverse order. Register the teardown *before* the operation it undoes: under
# `set -e` an operation can fail partway and exit on the spot, so the teardown
# has to already be there. Each one is best-effort, so a partial state never
# blocks the rest.
DEFERRED=()
defer() { DEFERRED+=("$*"); }

cleanup() {
  local status=$? i
  if [ "${status}" -ne 0 ] && [ "${nodes_started}" = true ]; then
    collect_diagnostics
  fi
  # Before anything is removed: the log and the diagnostics are what is scanned.
  # A leak turns a passing run red; a run that already failed stays failed.
  local scan_failed=0
  scan_output_for_secrets || scan_failed=1
  # CI uploads this directory when the job fails, and a failed scan is a failed
  # job. The bundle is redacted in the scan's output but not on disk, so it is
  # removed rather than published with the credential the scan reported. A
  # scan that could not run counts too: the bundle was not checked.
  if [ "${scan_failed}" -ne 0 ] && [ -d "${DIAGNOSTICS_DIR}" ]; then
    log "removing ${DIAGNOSTICS_DIR}: it may hold a credential, and CI uploads it"
    rm -rf "${DIAGNOSTICS_DIR}"
  fi
  for ((i = ${#DEFERRED[@]} - 1; i >= 0; i--)); do
    eval "${DEFERRED[$i]}" || true
  done

  # Same cause as the KUBECONFIG scratch path above: under `sudo -E` nvkectl
  # writes this bundle as root, and the CI step that uploads it runs as the
  # normal user, which cannot traverse a root-owned 0750 directory. Hand
  # ownership back rather than loosening nvkectl's file modes.
  if [ -n "${SUDO_UID:-}" ] && [ -d "${DIAGNOSTICS_DIR}" ]; then
    log "restoring ownership of ${DIAGNOSTICS_DIR} to uid ${SUDO_UID}"
    chown -R "${SUDO_UID}:${SUDO_GID:-${SUDO_UID}}" "${DIAGNOSTICS_DIR}" || true
  fi

  rm -f "${TENANT_KUBECONFIG}" "${ETCD_KEY_FILE}" "${BOOTSTRAP_LOG}"
  rm -rf "${KUBECONFIG_DIR}" "${CNI_BUNDLE_DIR}" "${BGP_RENDER_DIR}"
  if [ "${scan_failed}" -ne 0 ] && [ "${status}" -eq 0 ]; then
    exit 1
  fi
}
nodes_started=false
trap cleanup EXIT

# wait_until retries CMD every INTERVAL seconds until it succeeds or DEADLINE
# seconds have passed. Returns 0 once CMD succeeds, 1 if the deadline passes.
wait_until() {
  local deadline=$((SECONDS + $1)) interval="$2"
  shift 2
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if "$@"; then return 0; fi
    sleep "${interval}"
  done
  return 1
}

# The teardowns that defer registers, one per thing created.
teardown_nodes() {
  log "tearing down fake node containers"
  make -C "${CLI_DIR}" nodes-down || true
}
# Bare, kind asks Docker, sees none of nerdctl's clusters and answers "no"
# without checking, so ask the provider `bootstrap site` used.
kind_lists_bootstrap_cluster() {
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get clusters 2>/dev/null |
    grep -Fxq "${BOOTSTRAP_KIND_CLUSTER}"
}

# `bootstrap site` is given --keep-bootstrap-cluster, so this suite owns the
# kept cluster's removal; one left behind would trip the next run's stage 2.
# The failure-path net; the happy path deletes at the end, where it can assert.
teardown_bootstrap_cluster() {
  log "tearing down the kept bootstrap kind cluster"
  KIND_EXPERIMENTAL_PROVIDER=nerdctl \
    kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}" || true
}
remove_forward_rules() {
  log "removing the cross-bridge forwarding rules"
  iptables -D FORWARD -i "$1" -o "$2" -j ACCEPT 2>/dev/null || true
  iptables -D FORWARD -i "$2" -o "$1" -j ACCEPT 2>/dev/null || true
}
remove_spire_vip() {
  log "removing ${SPIRE_VIP} from $1"
  ip address del "${SPIRE_VIP}/32" dev "$1" 2>/dev/null || true
}

remove_vip_forward_rule() {
  iptables -D FORWARD -i "$1" -o "$1" -j ACCEPT 2>/dev/null || true
}

# check_api_server accepts a VIP, server URL, and whitespace-separated node IPs.
# Without a VIP, any control-plane node may be the API endpoint.
check_api_server() {
  local vip="$1" server="$2" cp_node_ips="$3" ip
  if [ -n "${vip}" ]; then
    [ "${server}" = "https://${vip}:6443" ]
    return $?
  fi

  for ip in ${cp_node_ips}; do
    if [ "${server}" = "https://${ip}:6443" ]; then
      return 0
    fi
  done
  return 1
}


# Which registry each side's pulls resolve to. A pull that falls through to
# upstream is the difference between a 30 second install and a helm timeout,
# so a slow run that still passes wants this too.
registry_routing() {
  echo "### ${NODE1}: /etc/containerd/certs.d/docker.io/hosts.toml"
  nerdctl exec "${NODE1}" cat /etc/containerd/certs.d/docker.io/hosts.toml
  echo "### ${BOOTSTRAP_CONTROL_PLANE}: /etc/containerd/certs.d/docker.io/hosts.toml"
  nerdctl exec "${BOOTSTRAP_CONTROL_PLANE}" cat /etc/containerd/certs.d/docker.io/hosts.toml
}

# collect_diagnostics writes what a failed run needs once the containers are
# gone: the host's forwarding and network state, each node's journals and
# runtime state, and where each side's image pulls were routed.
collect_diagnostics() {
  local i node output
  mkdir -p "${DIAGNOSTICS_DIR}" || return 0

  log "collecting host forwarding and network state (for the cross-bridge path)"
  {
    echo "### iptables -S FORWARD"; iptables -S FORWARD
    echo "### iptables -S CNI-ISOLATION-STAGE-1"; iptables -S CNI-ISOLATION-STAGE-1
    echo "### iptables -S CNI-ISOLATION-STAGE-2"; iptables -S CNI-ISOLATION-STAGE-2
    echo "### iptables -t nat -S CNI-HOSTPORT-DNAT"; iptables -t nat -S CNI-HOSTPORT-DNAT
    echo "### nerdctl network inspect"; nerdctl network inspect kind "${NODE_NETWORK}"
    registry_routing
    if [ -n "${CONTROL_PLANE_VIP}" ]; then
      echo "### ip route show ${CONTROL_PLANE_VIP}/32"; ip route show "${CONTROL_PLANE_VIP}/32" || true
      echo "### nerdctl logs ${BGP_CONTAINER} (tail 100)"; nerdctl logs "${BGP_CONTAINER}" --tail 100 || true
      echo "### vtysh show bgp summary"; nerdctl exec "${BGP_CONTAINER}" vtysh -c 'show bgp summary' || true
      echo "### node1 lo addresses"; nerdctl exec "${NODE1}" ip -4 addr show lo || true
      echo "### node1 kube-vip manifest"; nerdctl exec "${NODE1}" cat /etc/kubernetes/manifests/kube-vip.yaml || true
      echo "### node1 vip containers"; nerdctl exec "${NODE1}" sh -c 'crictl ps -a | grep -i vip' || true
    fi
  } > "${DIAGNOSTICS_DIR}/host-network.txt" 2>&1 || true

  log "collecting kubelet, containerd, nvke-agent, spire-agent and Open vSwitch diagnostics from ${NODE_COUNT} fake node(s)"
  for ((i = 1; i <= NODE_COUNT; i++)); do
    node="nvkectl-e2e-${NODE_PREFIX}${i}"
    output="${DIAGNOSTICS_DIR}/host-diagnostics-${NODE_PREFIX}${i}.txt"
    {
      printf '### journalctl: kubelet, containerd, nvke-agent, spire-agent, openvswitch-switch\n'
      nerdctl exec "${node}" journalctl -u kubelet -u containerd -u nvke-agent -u spire-agent -u openvswitch-switch \
        --boot --no-pager --output=short-iso-precise --lines=10000 || true

      printf '\n### systemctl status\n'
      nerdctl exec "${node}" systemctl status kubelet containerd openvswitch-switch --no-pager || true

      # The controller's Open vSwitch step fails inside the node when
      # openvswitch-switch does not come up, and its daemons log only here.
      printf '\n### ovs-vsctl show\n'
      nerdctl exec "${node}" ovs-vsctl show || true

      printf '\n### /var/log/openvswitch (tail 200 each)\n'
      nerdctl exec "${node}" sh -c 'tail -n 200 /var/log/openvswitch/*.log' || true

      printf '\n### crictl ps -a\n'
      nerdctl exec "${node}" crictl ps -a || true

      printf '\n### crictl pods\n'
      nerdctl exec "${node}" crictl pods || true

      printf '\n### crictl info\n'
      nerdctl exec "${node}" crictl info || true

      printf '\n### ip addr\n'
      nerdctl exec "${node}" ip addr || true

      printf '\n### ip route\n'
      nerdctl exec "${node}" ip route || true

      printf '\n### CNI files\n'
      nerdctl exec "${node}" ls -la /etc/cni/net.d /opt/cni/bin || true
    } >"${output}" 2>&1
  done

  # nvkectl keeps the bootstrap kind cluster after a failure past site cluster
  # creation, and its controller log names the provisioning step that stalled.
  if kind_lists_bootstrap_cluster; then
    log "collecting the controller logs from the kept bootstrap kind cluster"
    local kubeconfig
    kubeconfig="$(mktemp)"
    if KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name "${BOOTSTRAP_KIND_CLUSTER}" >"${kubeconfig}" 2>/dev/null; then
      kubectl --kubeconfig "${kubeconfig}" -n "${CONTROLLER_NAME}" logs "deployment/${CONTROLLER_NAME}" \
        --all-containers --tail=5000 >"${DIAGNOSTICS_DIR}/controller.log" 2>&1 || true
      kubectl --kubeconfig "${kubeconfig}" -n provisioned-system logs deployment/provisioned-controller-manager \
        --all-containers --tail=2000 >"${DIAGNOSTICS_DIR}/provisioned-controller.log" 2>&1 || true
    fi
    rm -f "${kubeconfig}"
  fi
}

# ── Stage 1: fake nodes ──────────────────────────────────────────────────

log "building the OVN packages for post-init CNI"
BUNDLE_ARCH="$(go env GOARCH)"
export BUNDLE_ARCH
bash "${CLI_DIR}/../controller/hack/ci/prepare-capd-bundles.sh" "${CNI_BUNDLE_DIR}"
CAPD_CNI_CHARTS_DEB_PATH="${CNI_BUNDLE_DIR}/nvke-cni-ovn-charts-2.4.2_2.4.2_${BUNDLE_ARCH}.deb"
CAPD_CNI_IMAGES_DEB_PATH="${CNI_BUNDLE_DIR}/nvke-cni-ovn-images-2.4.2_2.4.2_${BUNDLE_ARCH}.deb"
CAPD_HELM_DEB_PATH="${CNI_BUNDLE_DIR}/nke-e2e-helm.deb"
export CAPD_CNI_CHARTS_DEB_PATH CAPD_CNI_IMAGES_DEB_PATH CAPD_HELM_DEB_PATH
if [ ! -f "${CAPD_CNI_CHARTS_DEB_PATH}" ] || [ ! -f "${CAPD_CNI_IMAGES_DEB_PATH}" ] || [ ! -f "${CAPD_HELM_DEB_PATH}" ]; then
  fail "OVN packages were not built"
fi

# The nodes' openvswitch-switch unit loads the openvswitch module from the
# host tree mounted into them, which works only against the running kernel's
# own build. Without the module the controller's Open vSwitch step fails on
# every node and the site cluster sits in Provisioning until its timeout.
if [ ! -e /sys/module/openvswitch ]; then
  fail "the host has not loaded the openvswitch kernel module; load it before starting the fake nodes"
fi

log "starting fake node containers"
# `nodes-up` can fail after creating some containers; `nodes-down` is safe
# against a partial or nonexistent fixture, and diagnostics are only worth
# collecting once there are nodes to ask.
nodes_started=true
defer teardown_nodes
make -C "${CLI_DIR}" nodes-up

log "PASS: all fake nodes are up with TPM and SPIRE services ready"

# ── Stage 2: bootstrap site ─────────────────────────────────────────────
#
# The fake nodes booted with a software TPM and a spire-agent already pointed
# at SPIRE_VIP:30081, the address `bootstrap site` puts its SPIRE server on,
# and have been retrying since. This stage only records their EK thumbprints in
# the site spec and runs the command; the next one checks the agents got in.

log "ensuring /dev/vtpmx for the fake nodes' swtpm"
# The arm64 runner executes this job as root, and its sudoers file does not
# grant root permission to use sudo. The script itself requires root.
if [ "$(id -u)" -eq 0 ]; then
  bash "${CLI_DIR}/../controller/hack/ensure-vtpm-proxy.sh"
else
  sudo bash "${CLI_DIR}/../controller/hack/ensure-vtpm-proxy.sh"
fi

log "building the provisioned CAPI provider image (${NVKECTL_CAPI_PROVISIONED_IMAGE})"
# Its Docker Hub bases come through NKE_E2E_DOCKERHUB_MIRROR when set, like
# every other image the lanes build on the host.
# shellcheck source=../../../controller/hack/backend-e2e-lib.sh
source "${REPO_ROOT}/controller/hack/backend-e2e-lib.sh"
mirror_build_args="$(dockerhub_build_args "${REPO_ROOT}/cluster-api-provider-provisioned/Dockerfile" | paste -sd ' ' -)"
make -C "${CLI_DIR}" ensure-provisioned-provider-image \
  CAPI_PROVISIONED_IMAGE="${NVKECTL_CAPI_PROVISIONED_IMAGE}" \
  DOCKERHUB_MIRROR_BUILD_ARGS="${mirror_build_args}"

# The completed images are sufficient from this point. On a CI runner,
# retaining their BuildKit intermediates competes with the container storage
# needed by the bootstrap cluster and three site nodes.
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
  log "removing the BuildKit cache before cluster provisioning"
  nerdctl builder prune --all --force || true
fi

# The site spec is generated, not committed: it exercises the command an
# operator runs, and a TPM's endorsement key is created with the container so
# the thumbprints could not be committed anyway.
log "collecting each node's TPM EK thumbprint and the bootstrap CA"
HOSTS_CSV="${SCRIPT_DIR}/.hosts.csv"
# shellcheck disable=SC2046 # the assignments are the documented output contract
export $(NVKECTL_E2E_HOSTS_CSV="${HOSTS_CSV}" \
  make -C "${CLI_DIR}" --no-print-directory nodes-hosts | xargs)

# Both or neither. Without a CA, SPIRE self-signs — a supported configuration,
# so the lane does not require one. The CA reaches bootstrap site through the
# environment.
if [[ -n "${NVKECTL_E2E_SPIRE_CA_CERT:-}" && -n "${NVKECTL_E2E_SPIRE_CA_KEY:-}" ]]; then
  export NVKECTL_SPIRE_CA_CERT_FILE="${NVKECTL_E2E_SPIRE_CA_CERT}"
  export NVKECTL_SPIRE_CA_KEY_FILE="${NVKECTL_E2E_SPIRE_CA_KEY}"
fi

# The spec records that the site uses a CA, so later runs require it.
ca_set=()
if [[ -n "${NVKECTL_SPIRE_CA_CERT_FILE:-}" ]]; then
  ca_set=(--set spire.upstreamCA=true)
fi

# Omit the VIP field when disabled.
vip_set=()
if [ -n "${CONTROL_PLANE_VIP}" ]; then
  vip_set=(--set "cluster.controlPlaneVIP=${CONTROL_PLANE_VIP}")
fi

log "generating the site spec: nvkectl generate sitespec ${E2E_CLUSTER_NAME} --nodes ${NVKECTL_E2E_HOSTS_CSV}"
RENDERED_SITESPEC="${SCRIPT_DIR}/.sitespec.rendered.yaml"
"${NVKECTL}" generate sitespec "${E2E_CLUSTER_NAME}" \
  --nodes "${NVKECTL_E2E_HOSTS_CSV}" \
  --set cluster.k8sVersion=1.34.8 \
  --set cluster.cniName=ovn-kubernetes \
  --set cluster.cniVersion=2.4.2 \
  --set cluster.podCidr=192.168.0.0/16 \
  --set cluster.serviceCidr=10.96.0.0/12 \
  --set cluster.kubeletConfig.systemReservedMemory=512Mi \
  --set "spire.trustDomain=${SPIRE_TRUST_DOMAIN}" \
  "${vip_set[@]}" \
  "${ca_set[@]}" \
  > "${RENDERED_SITESPEC}"

# One thumbprint per node, init node included; stage 4 expects that many nodes.
expected_nodes=$(grep -cE '^[[:space:]]+tpmEkThumbprint:' "${RENDERED_SITESPEC}" || true)
if [ "${expected_nodes}" -eq 0 ]; then
  fail "no TPM EK thumbprints reached ${RENDERED_SITESPEC}"
fi
grep -qE "^[[:space:]]*trustDomain:[[:space:]]*['\"]?${SPIRE_TRUST_DOMAIN}['\"]?[[:space:]]*$" \
  "${RENDERED_SITESPEC}" ||
  fail "SPIRE trust domain ${SPIRE_TRUST_DOMAIN} did not reach ${RENDERED_SITESPEC}"
if [ -n "${CONTROL_PLANE_VIP}" ]; then
  # Quoted or unquoted, either is valid YAML for a string scalar.
  grep -qE "^[[:space:]]*controlPlaneVIP:[[:space:]]*\"?${CONTROL_PLANE_VIP}\"?[[:space:]]*$" "${RENDERED_SITESPEC}" ||
    { log "FAIL: controlPlaneVIP ${CONTROL_PLANE_VIP} did not reach ${RENDERED_SITESPEC}"; exit 1; }
  log "control-plane VIP set to ${CONTROL_PLANE_VIP} in the site spec"
fi
log "allowlisting ${expected_nodes} EK thumbprint(s)"


if [ -n "${CONTROL_PLANE_VIP}" ]; then
  log "starting the FRR BGP speaker for control-plane VIP ${CONTROL_PLANE_VIP}"
  start_bgp_speaker || exit 1
fi

# No chart is named here. What gets installed is pinned in
# cli/internal/bootstrap/controllerdeploy, which is the thing under test: the SPIFFE project's
# public hardened charts, needing no registry credential of their own.
# The command never overwrites a key file and refuses to start over one, so a
# leftover from an interrupted run has to go first.
rm -f "${ETCD_KEY_FILE}"

log "running: nvkectl bootstrap site -f ${RENDERED_SITESPEC} --keep-bootstrap-cluster --timeout 15m" \
  "via the e2e_bootstrap TestE2E_ tests" \
  "(NVKECTL_NVKE_CONTROLLER_IMAGE=${NVKECTL_NVKE_CONTROLLER_IMAGE}," \
  "NVKECTL_CAPI_PROVISIONED_IMAGE=${NVKECTL_CAPI_PROVISIONED_IMAGE})"

# In the background, because the attach below has to happen *during* the run:
# The controller and the spire-agents talk over the node network, which the bootstrap
# cluster is not on, because the nodes came up before it existed.
#
# --keep-bootstrap-cluster: this suite's EXIT trap removes the cluster, and the
# teardown registers here because the command can fail after creating it.
defer teardown_bootstrap_cluster
# The command is run by TestE2E_BootstrapSite rather than invoked here, so that
# starting it, giving it a process group and reaping that group are stated once
# in Go instead of twice in shell. It writes nvkectl's output to BOOTSTRAP_LOG,
# which is what the loop below and every assertion after it read, so this lane
# is unchanged on either side of this line. The test asserts nothing: it reports
# what the command exited with and no more.
#
# setsid makes `go test` a process-group leader so abort_bootstrap can signal
# it. The test binary shares that group, so the signal reaches the test, which
# in turn stops nvkectl and the kind, nerdctl and helm it drives -- those are in
# a group of their own and the lane's signal does not reach them.
#
# -v makes a successful run write its diagnostics bundle too, so the credential
# scan covers the bundle on the passing path and not only after a failure.
# A -run that matches nothing exits 0, so a renamed or mistyped test would look
# like a bootstrap that ran and passed. Assert the selector picks something up
# before relying on it. This compiles the package too, so the run below starts
# without a build in front of it.
#
# Captured rather than piped into grep: under `set -o pipefail`, a `grep -q`
# that exits on the first match leaves `go test` writing to a closed pipe, and
# the pipeline then fails for a reason that has nothing to do with the result.
listed_tests="$("${GO}" test -C "${CLI_DIR}" -tags e2e_bootstrap -list '^TestE2E_' \
  ./e2e/bootstrap/bootstraptest/)" ||
  fail "could not list the bootstrap tests"
case "${listed_tests}" in
  *TestE2E_*) ;;
  *) fail "no TestE2E_ test in the bootstrap test package: the lane would report success without bootstrapping anything" ;;
esac

setsid env \
  NVKECTL="${NVKECTL}" \
  NVKECTL_E2E_SITESPEC="${RENDERED_SITESPEC}" \
  NVKECTL_E2E_BOOTSTRAP_LOG="${BOOTSTRAP_LOG}" \
  NVKECTL_E2E_KUBECONFIG_OUT="${TENANT_KUBECONFIG}" \
  NVKECTL_E2E_ENCRYPTION_KEY_OUT="${ETCD_KEY_FILE}" \
  NVKECTL_E2E_BOOTSTRAP_TIMEOUT=15m \
  NVKECTL_E2E_KEEP_BOOTSTRAP_CLUSTER=1 \
  NVKECTL_E2E_VERBOSE=1 \
  "${GO}" test -C "${CLI_DIR}" -tags e2e_bootstrap -timeout 0 -count=1 -v \
  ./e2e/bootstrap/bootstraptest/ -run '^TestE2E_' \
  >"${BOOTSTRAP_GO_TEST_LOG}" 2>&1 &
bootstrap_pid=$!

# abort_bootstrap stops the background run and its children before this script
# exits. Signalling the group reaches the go test, which stops nvkectl and the
# kind, nerdctl and helm it drives before it goes. Otherwise the EXIT trap would
# delete the cluster the command is still driving.
abort_bootstrap() {
  kill -- "-${bootstrap_pid}" 2>/dev/null || kill "${bootstrap_pid}" 2>/dev/null || true
  wait "${bootstrap_pid}" 2>/dev/null || true
  cat "${BOOTSTRAP_LOG}" || true
  cat "${BOOTSTRAP_GO_TEST_LOG}" || true
  exit 1
}

# cross_bridge_reachable tests forwarding between the two networks, not the
# service on the far side. Refused is the pass: the packet crossed and the far
# side answered that nothing is listening. A timeout is the failure, and is
# what the CNI firewall's cross-bridge DROP looks like. bash's /dev/tcp gives
# that distinction for free, neither image shipping curl.
cross_bridge_reachable() {
  local from="$1" ip="$2" port="$3" what="$4"
  local out rc
  out="$(nerdctl exec "${from}" timeout 5 bash -c "exec 3<>/dev/tcp/${ip}/${port}" 2>&1)" && rc=0 || rc=$?
  case "${rc}" in
    0) log "  ${what}: ${ip}:${port} accepted the connection"; return 0 ;;
    124) log "FAIL: ${what}: ${ip}:${port} timed out -- packets are not crossing the bridges"; return 1 ;;
    *)
      if printf '%s' "${out}" | grep -qi "refused"; then
        log "  ${what}: ${ip}:${port} reachable (refused; nothing listening yet, as expected before SPIRE)"
        return 0
      fi
      log "FAIL: ${what}: ${ip}:${port} failed (exit ${rc}): ${out}"
      return 1
      ;;
  esac
}

# probe_cross_bridge_connectivity checks both directions the moment the cluster
# is attached; unchecked, a break here surfaces fifteen minutes later as "did
# not report provisioned". nvke-agent does not bind its port until the node has
# an SVID and SPIRE is not deployed yet, so kind -> node is expected to be
# refused rather than accepted. node -> kind uses the control plane's kubelet
# port, listening as soon as the cluster is Ready.
probe_cross_bridge_connectivity() {
  local cp_ip ok=true
  # From the network, not the container: the container's own inspect lists its
  # pod bridge alongside the kind network, and ranging over both concatenates
  # the addresses. The network knows exactly one address for it.
  cp_ip="$(nerdctl network inspect kind --format \
    "{{range .Containers}}{{if eq .Name \"${BOOTSTRAP_CONTROL_PLANE}\"}}{{.IPv4Address}}{{end}}{{end}}")" || return 1
  cp_ip="${cp_ip%/*}"
  if [ -z "${cp_ip}" ]; then
    log "FAIL: could not find ${BOOTSTRAP_CONTROL_PLANE} on the kind network"
    return 1
  fi
  cross_bridge_reachable "${BOOTSTRAP_CONTROL_PLANE}" "${INIT_NODE_IP}" "${NODE_AGENT_PORT}" "kind -> node" || ok=false
  cross_bridge_reachable "${NODE1}" "${cp_ip}" 10250 "node -> kind" || ok=false
  if [ "${ok}" != true ]; then
    log "host FORWARD chain:"
    iptables -S FORWARD | sed 's/^/  /'
    return 1
  fi
  log "cross-bridge connectivity confirmed in both directions"
}

# attach_bootstrap_to_node_network bridges the two networks. Both are nerdctl
# bridges, and nerdctl's default CNI config sets the firewall plugin's
# ingressPolicy to "same-bridge", which DROPs any forward from one CNI bridge
# out another. So controller -> nvke-agent and node -> SPIRE VIP are dropped by
# default, the controller's NodePreflight times out, and no node attests. Permit exactly
# this pair of bridges, both directions, at position 1 so it precedes the
# isolation chain; nothing else on the host becomes reachable.
#
# nerdctl cannot connect a running container to another network, so the SPIRE
# VIP goes onto the node network's bridge instead.
attach_bootstrap_to_node_network() {
  local node_id kind_id node_bridge kind_bridge
  node_id="$(nerdctl network inspect --format '{{.Id}}' "${NODE_NETWORK}")" || return 1
  node_bridge="br-${node_id:0:12}"
  ip link show "${node_bridge}" >/dev/null || return 1
  kind_id="$(nerdctl network inspect --format '{{.Id}}' kind)" || return 1
  kind_bridge="br-${kind_id:0:12}"
  ip link show "${kind_bridge}" >/dev/null || return 1

  defer "remove_forward_rules ${kind_bridge} ${node_bridge}"
  iptables -I FORWARD 1 -i "${kind_bridge}" -o "${node_bridge}" -j ACCEPT || return 1
  iptables -I FORWARD 1 -i "${node_bridge}" -o "${kind_bridge}" -j ACCEPT || return 1
  # VIP traffic enters and leaves the same node bridge.
  if [ -n "${CONTROL_PLANE_VIP}" ]; then
    defer "remove_vip_forward_rule ${node_bridge}"
    iptables -I FORWARD 1 -i "${node_bridge}" -o "${node_bridge}" -j ACCEPT || return 1
  fi
  defer "remove_spire_vip ${node_bridge}"
  ip address replace "${SPIRE_VIP}/32" dev "${node_bridge}" || return 1
}

# Attach as soon as Kind reports the cluster ready: after Kind has finished
# with the control-plane container, and before the controller is deployed, so the extra
# network disturbs neither. Bounded separately from the command's own timeout,
# and abandoned if the command exits first.
log "waiting for the bootstrap cluster to become ready, to attach it to the node network at ${SPIRE_VIP}"
attached=false
attach_deadline=$((SECONDS + ${NVKECTL_E2E_ATTACH_TIMEOUT:-1200}))
while kill -0 "${bootstrap_pid}" 2>/dev/null && [ "${SECONDS}" -lt "${attach_deadline}" ]; do
  if grep -q "bootstrap kind cluster ready" "${BOOTSTRAP_LOG}" 2>/dev/null; then
    if attach_bootstrap_to_node_network; then
      log "attached the bootstrap cluster at ${SPIRE_VIP}"
      probe_cross_bridge_connectivity || abort_bootstrap
      attached=true
    else
      log "WARN: could not attach the bootstrap cluster to the node network"
    fi
    break
  fi
  sleep 2
done

if [ "${attached}" != true ]; then
  log "FAIL: the bootstrap cluster was never attached to the node network, so the controller"
  log "  cannot reach the nodes and their agents cannot reach SPIRE. Not waiting"
  log "  for a bootstrap that has no route to the hosts it is building on."
  abort_bootstrap
fi

status=0
wait "${bootstrap_pid}" || status=$?
cat "${BOOTSTRAP_LOG}"
output="$(cat "${BOOTSTRAP_LOG}")"

if [ "${status}" -ne 0 ]; then
  # The go test exit status says only that the run failed. What nvkectl exited
  # with, or that it could not be started at all, is in its log.
  cat "${BOOTSTRAP_GO_TEST_LOG}" || true
  fail "nvkectl bootstrap site failed; see ${BOOTSTRAP_GO_TEST_LOG}"
fi
if ! grep -q "controller deployment healthy" <<<"${output}"; then
  fail "the controller was never confirmed healthy on /readyz"
fi
if ! grep -q "cluster provisioned" <<<"${output}"; then
  fail "the command exited 0 without reporting the cluster provisioned"
fi
if [ ! -s "${TENANT_KUBECONFIG}" ]; then
  fail "no kubeconfig was written to ${TENANT_KUBECONFIG}"
fi
if [[ -n "${NVKECTL_SPIRE_CA_CERT_FILE:-}" ]] && ! grep -q "SPIRE upstream CA from the environment" <<<"${output}"; then
  fail "a bootstrap CA was supplied but bootstrap site did not pick it up"
fi
log "PASS: bootstrap site created the site cluster through the controller API"

# check_trace_id asserts the run's trace ID: printed first and last, the same
# value, and on every log record nvkectl wrote. Records are the slog lines, which
# start with time=; the output of helm, kind and the like is not nvkectl's.
check_trace_id() {
  local ids first last id bare
  ids="$(grep -E '^Trace ID: ' <<<"${output}" || true)"
  [ "$(wc -l <<<"${ids}")" -ge 2 ] ||
    fail "expected a Trace ID line at the start and at the end of the output" "${ids}"
  first="$(head -n1 <<<"${ids}")"
  last="$(tail -n1 <<<"${ids}")"
  [[ "${first}" =~ ^Trace\ ID:\ ([0-9a-f]{32})$ ]] || fail "malformed Trace ID line: ${first}"
  id="${BASH_REMATCH[1]}"
  [ "${first}" = "${last}" ] || fail "the first and last Trace ID lines differ" "${first}" "${last}"
  [ "$(grep -c '^time=' <<<"${output}")" -gt 0 ] || fail "no log records in the output to carry a trace ID"
  bare="$(grep '^time=' <<<"${output}" | grep -vc "trace_id=${id}" || true)"
  [ "${bare}" -eq 0 ] || fail "${bare} log records do not carry trace_id=${id}"
  log "PASS: one trace ID, first and last line, on every nvkectl log record"
}
check_trace_id

# -v makes the run write diagnostics even on success. An empty directory would
# make the credential scan pass over nothing.
[ -n "$(find "${DIAGNOSTICS_DIR}" -type f 2>/dev/null | head -n1)" ] ||
  fail "bootstrap site -v wrote no diagnostics to ${DIAGNOSTICS_DIR}"
log "PASS: a successful run wrote its diagnostics bundle for the credential scan"

# The controller reached this point by answering /readyz through its Service
# and the host through its external NodePort. What that does not show is that
# it runs under the name greenfield sites use, as the ServiceAccount its
# SPIFFE ID names, with SPIRE registered to issue that ID.
bootstrap_kubeconfig="$(mktemp)"
KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get kubeconfig --name "${BOOTSTRAP_KIND_CLUSTER}" \
  > "${bootstrap_kubeconfig}" || fail "could not read the bootstrap cluster's kubeconfig"
bk() { kubectl --kubeconfig "${bootstrap_kubeconfig}" "$@"; }
sa="$(bk -n "${CONTROLLER_NAME}" get deployment "${CONTROLLER_NAME}" \
  -o 'jsonpath={.spec.template.spec.serviceAccountName}')" ||
  fail "no deployment ${CONTROLLER_NAME} in namespace ${CONTROLLER_NAME}"
[ "${sa}" = "${CONTROLLER_NAME}" ] ||
  fail "the controller runs as ServiceAccount ${sa}, not ${CONTROLLER_NAME}"
endpoints="$(bk -n "${CONTROLLER_NAME}" get endpoints "${CONTROLLER_NAME}-external" \
  -o 'jsonpath={.subsets[*].addresses[*].ip}')"
[ -n "${endpoints}" ] || fail "Service ${CONTROLLER_NAME}-external has no endpoints"
spiffe_id="spiffe://${SPIRE_TRUST_DOMAIN}/nvke-service/${CONTROLLER_NAME}"
bk -n spire-server exec spire-server-0 -c spire-server -- \
  /opt/spire/bin/spire-server entry show -spiffeID "${spiffe_id}" | grep -q "Entry ID" ||
  fail "SPIRE has no entry for ${spiffe_id}"
# With a supplied CA, it is the root SPIRE publishes to the agents.
if [[ -n "${NVKECTL_SPIRE_CA_CERT_FILE:-}" ]]; then
  ca_line="$(sed -n 2p "${NVKECTL_SPIRE_CA_CERT_FILE}")"
  bk -n spire-server exec spire-server-0 -c spire-server -- /opt/spire/bin/spire-server bundle show |
    grep -qF "${ca_line}" || fail "SPIRE's bundle does not hold the CA from NVKECTL_SPIRE_CA_CERT_FILE"
fi
rm -f "${bootstrap_kubeconfig}"
log "PASS: the controller runs as ${CONTROLLER_NAME} and SPIRE issues ${spiffe_id}"

# ── Stage 3: every node attested to SPIRE over TPM ──────────────────────

log "waiting for every node's spire-agent to attest"
make -C "${CLI_DIR}" nodes-attested

log "PASS: bootstrap site deployed SPIRE and every node attested to it over TPM"

# all_nodes_ready is the stage 4 condition. `kubectl wait --all` only waits for
# the objects that exist when it starts, so count and readiness are checked
# together: the first Ready node must not pass the stage before CAPI creates
# the rest. Sets total_nodes/ready_nodes for the failure message.
all_nodes_ready() {
  local node_states
  node_states=$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes \
    -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
    2>/dev/null || true)
  total_nodes=$(printf '%s\n' "${node_states}" | awk 'NF {count++} END {print count+0}')
  ready_nodes=$(printf '%s\n' "${node_states}" | awk '$2 == "True" {count++} END {print count+0}')
  [ "${total_nodes}" -eq "${expected_nodes}" ] && [ "${ready_nodes}" -eq "${expected_nodes}" ]
}

# ── Stage 4: every pre-provisioned host is a Ready node ─────────────────
#
# Every site spec node joins as a control-plane node and nvkectl removes the
# control-plane taint once they have all joined, so all must be Ready and
# schedulable. The controller joins them one at a time, ~6 min apart, so the third
# kubelet starts ~13 min after the cluster is created.

log "waiting for ${expected_nodes} nodes to report Ready"
total_nodes=0
ready_nodes=0
if ! wait_until "${NVKECTL_E2E_NODE_READY_TIMEOUT_SECONDS:-1200}" 5 all_nodes_ready; then
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes -o wide || true
  fail "${ready_nodes} of ${expected_nodes} site spec nodes became Ready (${total_nodes} exist)"
fi
log "PASS: all ${ready_nodes} pre-provisioned hosts are Ready nodes"

# ── Stage 4a: control-plane VIP ───────────────────────────
#
# Node IPs are the expected BGP next-hops and valid endpoints without a VIP.
# An unreachable API and a node without an InternalIP are different failures,
# so the lookup and the filter are checked apart. kubectl keeps its stderr to
# name the first; grep exits non-zero on no match, which is the second.
if ! node_addresses="$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes \
  -o 'jsonpath={range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}')"; then
  log "FAIL: could not list nodes for the API-server and BGP checks"
  exit 1
fi
cp_node_ips="$(printf '%s\n' "${node_addresses}" | grep -E '^[0-9]' | sort -u || true)"
if [ -z "${cp_node_ips}" ]; then
  log "FAIL: no node reports an InternalIP for the API-server and BGP checks"
  exit 1
fi

kubeconfig_server="$(grep -E '^[[:space:]]*server:' "${TENANT_KUBECONFIG}" | head -n1 | awk '{print $2}')"
if ! check_api_server "${CONTROL_PLANE_VIP}" "${kubeconfig_server}" "${cp_node_ips}"; then
  log "FAIL: unexpected kubeconfig API server ${kubeconfig_server}"
  log "  VIP: ${CONTROL_PLANE_VIP:-disabled}; control-plane IPs: $(echo "${cp_node_ips}" | tr '\n' ' ')"
  exit 1
fi
log "PASS: the written kubeconfig's API server is ${kubeconfig_server}"

if [ -z "${CONTROL_PLANE_VIP}" ]; then
  log "control-plane VIP disabled; skipping the BGP and VIP-reachability checks"
else
  # Wait for FRR to learn a next-hop for every control-plane node.
  expected_hops="${cp_node_ips}"
  log "waiting for FRR to learn ${CONTROL_PLANE_VIP}/32 with next-hops: $(echo "${expected_hops}" | tr '\n' ' ')"
  hops_deadline=$((SECONDS + ${NVKECTL_E2E_VIP_BGP_TIMEOUT_SECONDS:-180}))
  learned_hops=""
  while [ "${SECONDS}" -lt "${hops_deadline}" ]; do
    learned_hops="$(bgp_vip_nexthops)"
    if [ "${learned_hops}" = "${expected_hops}" ]; then
      break
    fi
    sleep 3
  done
  if [ "${learned_hops}" != "${expected_hops}" ]; then
    log "FAIL: FRR next-hops for ${CONTROL_PLANE_VIP}/32 did not converge to the node set"
    log "  expected: $(echo "${expected_hops}" | tr '\n' ' ')"
    log "  learned:  $(echo "${learned_hops}" | tr '\n' ' ')"
    nerdctl exec "${BGP_CONTAINER}" vtysh -c "show bgp ipv4 unicast ${CONTROL_PLANE_VIP}/32" 2>&1 | sed 's/^/  /' || true
    nerdctl exec "${BGP_CONTAINER}" vtysh -c 'show bgp summary' 2>&1 | sed 's/^/  /' || true
    exit 1
  fi
  log "host kernel route for the VIP:"
  ip route show "${CONTROL_PLANE_VIP}/32" | sed 's/^/  /' || true
  log "PASS: FRR learned ${CONTROL_PLANE_VIP}/32 with one next-hop per control-plane node"

  # Check API readiness through the VIP. Retried: the ECMP route is seconds old
  # here, and a single-shot check has failed with the retry 67ms later passing.
  vip_readyz() {
    kubectl --kubeconfig "${TENANT_KUBECONFIG}" --server "https://${CONTROL_PLANE_VIP}:6443" \
      get --raw /readyz >/dev/null 2>&1
  }
  if wait_until "${NVKECTL_E2E_VIP_READYZ_TIMEOUT_SECONDS:-60}" 3 vip_readyz; then
    log "PASS: the API server answered /readyz through the VIP https://${CONTROL_PLANE_VIP}:6443"
  else
    log "FAIL: the API server did not answer /readyz through the VIP https://${CONTROL_PLANE_VIP}:6443"
    kubectl --kubeconfig "${TENANT_KUBECONFIG}" --server "https://${CONTROL_PLANE_VIP}:6443" get --raw /readyz || true
    exit 1
  fi
fi

# ── Stage 4b: etcd data is encrypted, and the saved key is the nodes' key ──
#
# The checks are in etcd-lib.sh, shared with run-nico.sh.
check_etcd_encrypted_at_rest "${TENANT_KUBECONFIG}" "${ETCD_KEY_FILE}"
control_plane_containers=()
for ((i = 1; i <= NODE_COUNT; i++)); do
  control_plane_containers+=("nvkectl-e2e-${NODE_PREFIX}${i}")
done
check_saved_key_matches_nodes "${ETCD_KEY_FILE}" "${control_plane_containers[@]}"

# hello_world_served is the stage 5 condition. whoami answers with the request
# it received, so "Hostname:" in the body is the pod talking, not Traefik's own
# error page. The request carries the Ingress host name so it has to match a
# rule Traefik read from the Ingress rather than falling through to a default.
hello_world_served() {
  curl -fsS --max-time 5 -H "Host: hello.nke.local" \
    "http://${INIT_NODE_IP}:${TRAEFIK_NODE_PORT}/" | grep -q "Hostname:"
}

# ── Stage 4c: the control-plane taint is gone ───────────────────────────
#
# The site spec declares no worker nodes, so nvkectl must have removed the
# taint. The workloads below carry no toleration for it.

log "checking that no node still carries the control-plane taint"
node_names=$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes -o jsonpath='{.items[*].metadata.name}') \
  || fail "could not list nodes for the control-plane taint check"
[ -n "${node_names}" ] || fail "the cluster lists no nodes"
tainted_nodes=""
for node_name in ${node_names}; do
  taint_keys=$(kubectl --kubeconfig "${TENANT_KUBECONFIG}" get node "${node_name}" -o jsonpath='{.spec.taints[*].key}') \
    || fail "could not read the taints of node ${node_name}"
  if tr ' ' '\n' <<<"${taint_keys}" | grep -Fxq 'node-role.kubernetes.io/control-plane'; then
    tainted_nodes="${tainted_nodes}${node_name} "
  fi
done
if [ -n "${tainted_nodes}" ]; then
  fail "nodes still carry the control-plane taint: ${tainted_nodes}"
fi
log "PASS: every node is schedulable"

# ── Stage 5: hello world ────────────────────────────────────────────────
#
# Traefik is published on a NodePort because that is a route the host has to
# these containers. `--repo` rather than `helm repo add`, so this suite does
# not mutate the caller's global repository list. service.spec.type, not
# service.type: the chart puts the Service spec's fields under `spec`, and a
# value set at the wrong depth is silently ignored.

log "registry routing for this run:"
registry_routing 2>&1 | sed 's/^/  /' || true

log "installing traefik on the new cluster"
helm --kubeconfig "${TENANT_KUBECONFIG}" upgrade --install traefik traefik \
  --repo https://traefik.github.io/charts \
  --version "${NVKECTL_E2E_TRAEFIK_CHART_VERSION:-41.4.0}" \
  --namespace traefik --create-namespace \
  --set service.spec.type=NodePort \
  --set "ports.web.nodePort=${TRAEFIK_NODE_PORT}" \
  --wait --timeout 5m

log "deploying the hello-world workload"
kubectl --kubeconfig "${TENANT_KUBECONFIG}" apply -f "${SCRIPT_DIR}/manifests/hello-world.yaml"
kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n hello-world rollout status deployment/whoami --timeout=5m

log "asking for the page at http://${INIT_NODE_IP}:${TRAEFIK_NODE_PORT}/ (Host: hello.nke.local)"
if ! wait_until 300 5 hello_world_served; then
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n hello-world get all,ingress || true
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" -n traefik logs deployment/traefik --tail=100 || true
  fail "hello world was never served through Traefik"
fi
log "PASS: the cluster built from pre-provisioned hosts serves hello world through Traefik"

# ── This suite's own teardown ───────────────────────────────────────────
#
# Deleted here rather than left to the EXIT trap, which can only swallow a
# failed delete. A cluster left registered makes the next run stop on a name
# clash instead.
#
# Positive control first: kind exits 0 and prints "No kind clusters found."
# even when its provider is unreachable, so absence alone proves nothing.
if ! kind_lists_bootstrap_cluster; then
  fail "${BOOTSTRAP_KIND_CLUSTER} is not registered before teardown" \
    "so this suite cannot see the nerdctl provider and the check below proves nothing"
fi

log "tearing down the bootstrap kind cluster kept for attestation"
KIND_EXPERIMENTAL_PROVIDER=nerdctl \
  kind delete cluster --name "${BOOTSTRAP_KIND_CLUSTER}"

if kind_lists_bootstrap_cluster; then
  KIND_EXPERIMENTAL_PROVIDER=nerdctl kind get clusters || true
  fail "kind cluster ${BOOTSTRAP_KIND_CLUSTER} is still registered after this suite deleted it"
fi
log "PASS: this suite's teardown left no ${BOOTSTRAP_KIND_CLUSTER} cluster behind"
