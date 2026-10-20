#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Sourced only by nico-vms-e2e.sh. NVKR owns publication authentication before
# handing off these private files. Local SHA256 checks establish exact bytes,
# not signer authority. This branch never builds product components or resolves tags.
#
# Required NKE_E2E_CANDIDATE_ inputs:
# RELEASE (release-description/v1 JSON exported after parent verification),
# LAYOUT (complete published artifacts), NVKECTL,
# SIMULATOR, SEED, QCOW2 (absolute files, each with an exact _SHA256 blob pin),
# SITESPEC and STACK_CONFIG (separate public configuration), NKTP_VALUES, RELEASES (owner-generated profile CRs),
# OS_PROFILE, KUBERNETES_VERSION, CONTROLLER_VERSION, SIMULATOR_ADDRESS,
# SIMULATOR_ENDPOINT (exactly http://<SIMULATOR_ADDRESS>:18089),
# MIN_AVAILABLE_MIB, MIN_DISK_MIB and RUNTIME_LOCK (precreated owned file).
# IMAGE_INPUTS is the owning consumer's JSON array of {name, reference,
# manifestDigest, archive, archiveDigest}; VALUES holds spire.json,
# postgresql.json and temporal.json derived from the same verified artifacts.
# The parent holds the same lock on FD9 through setup and passes it to this
# process. NKE_CI_HOST_EXTERNAL_DHCP=true leaves DHCP ownership to this attempt.
# Optional NETWORK_OWNER=nvkr-network-guard instead uses the parent's owned
# DHCP_HOSTS_DIR, DHCP_LEASE_FILE and DHCP_PID (all three required in that mode).
# The parent serves QCOW2 at http://172.30.0.1:18080/nvke-node-image-arm64.qcow2;
# this adapter checks the complete served bytes and observed guest backing files.
# The supplied nvkectl must support inspect nvkr-release and render deploy-path
# --nvkr-run-dir for the current release-description/v1 contract. Its inspection
# admits metadata and platform selection only; it establishes no authentication.
# STATE_DIR must be fresh and outside shared /tmp, as the renderer requires.
# Native Ginkgo/JUnit and KVM observations are diagnostic runtime evidence;
# this adapter neither signs observations nor authorizes a Release.

candidate_network_inputs() {
  local argument hosts=false lease=false
  case "${NKE_E2E_CANDIDATE_NETWORK_OWNER:-}" in
    "")
      [[ "${NKE_CI_HOST_EXTERNAL_DHCP:-}" == true ]] || fail "host setup must leave DHCP ownership to this attempt"
      ;;
    nvkr-network-guard)
      for variable in NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR NKE_E2E_CANDIDATE_DHCP_LEASE_FILE NKE_E2E_CANDIDATE_DHCP_PID; do candidate_require "${variable}"; done
      [[ "${NICO_VMS_BRIDGE_NAME:-}" == nkdvm0 ]] || fail "the admitted network guard requires its owned nkdvm0 bridge"
      [[ "${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR}" == /* && -d "${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR}" && ! -L "${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR}" && -O "${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR}" ]] || fail "guard DHCP hosts directory is not private and owned"
      [[ "${NKE_E2E_CANDIDATE_DHCP_LEASE_FILE}" == /* && -f "${NKE_E2E_CANDIDATE_DHCP_LEASE_FILE}" && ! -L "${NKE_E2E_CANDIDATE_DHCP_LEASE_FILE}" && -O "${NKE_E2E_CANDIDATE_DHCP_LEASE_FILE}" ]] || fail "guard DHCP lease file is not regular and owned"
      [[ "${NKE_E2E_CANDIDATE_DHCP_PID}" != *[!0-9]* ]] || fail "guard DHCP process identifier is invalid"
      kill -0 "${NKE_E2E_CANDIDATE_DHCP_PID}" || fail "guard DHCP process is not running"
      [[ "${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR##*/}" == dhcp-hosts && "${NKE_E2E_CANDIDATE_DHCP_LEASE_FILE}" == "${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR%/dhcp-hosts}/leases" ]] || fail "guard DHCP paths are inconsistent"
      sudo -n test "/proc/${NKE_E2E_CANDIDATE_DHCP_PID}/exe" -ef "${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR%/dhcp-hosts}/dnsmasq" || fail "guard DHCP process does not execute the owned tool"
      # /proc supplies actual NUL-framed argv, so whitespace in paths never
      # becomes ambiguous. These are owner process checks, not a receipt.
      while IFS= read -r -d '' argument; do
        [[ "${argument}" != "--dhcp-hostsdir=${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR}" ]] || hosts=true
        [[ "${argument}" != "--dhcp-leasefile=${NKE_E2E_CANDIDATE_DHCP_LEASE_FILE}" ]] || lease=true
      done < <(sudo -n cat "/proc/${NKE_E2E_CANDIDATE_DHCP_PID}/cmdline")
      [[ "${hosts}" == true && "${lease}" == true ]] || fail "running DHCP process does not own the exact guard paths"
      ;;
    *) fail "unknown candidate network owner" ;;
  esac
}

candidate_require() {
  [[ -n "${!1:-}" ]] || fail "candidate mode requires $1"
}

candidate_pin() {
  local file="$1" pin="$2" digest
  [[ "${file}" == /* && -f "${file}" && ! -L "${file}" ]] || fail "candidate input must be an absolute regular non-symlink file"
  digest="$(sha256sum <"${file}")" || return 1
  [[ "sha256:${digest%% *}" == "${pin}" ]] || fail "candidate input differs from its exact byte pin"
}

candidate_kubectl() { kubectl --kubeconfig "${E2E_KUBECONFIG}" "$@"; }

candidate_release_inputs() {
  local role selected
  [[ -f "${NKE_E2E_CANDIDATE_RELEASE}" && ! -L "${NKE_E2E_CANDIDATE_RELEASE}" ]] || fail "candidate release description is not a regular file"
  # The product owner validates its current v1 format and full inventory.
  # Do not translate lifecycle verification JSON or infer signer authority.
  timeout 30 "${NKE_E2E_CANDIDATE_NVKECTL}" inspect nvkr-release "${NKE_E2E_CANDIDATE_RELEASE}" --artifacts >/dev/null || fail "supplied CLI cannot read the current complete release description"
  for role in controller-chart nktp postgresql spire-crds spire temporal cwe controller-profiles capnico-controller; do
    selected="$(timeout 30 "${NKE_E2E_CANDIDATE_NVKECTL}" inspect nvkr-release "${NKE_E2E_CANDIDATE_RELEASE}" --select "${role}" --arch linux/arm64)" || fail "candidate role ${role} is not selectable for Linux ARM64"
    # The inspect owner deliberately returns empty success for an absent role.
    [[ -n "${selected}" ]] || fail "candidate release description omits required role ${role}"
  done
}

candidate_image_inputs() {
  local row archive pin role
  [[ -f "${NKE_E2E_CANDIDATE_IMAGE_INPUTS}" && ! -L "${NKE_E2E_CANDIDATE_IMAGE_INPUTS}" ]] || fail "candidate native image inputs are missing"
  jq -e 'type == "array" and length > 0 and all(.[];
    (.name | type == "string" and length > 0) and
    (.manifestDigest | type == "string" and startswith("sha256:")) and
    (.reference | type == "string" and length > 0) and
    (.archive | type == "string" and startswith("/")) and
    (.archiveDigest | type == "string" and startswith("sha256:"))) and
    (group_by(.name) | all(.[]; length == 1))' "${NKE_E2E_CANDIDATE_IMAGE_INPUTS}" >/dev/null || fail "candidate native image input descriptions are invalid"
  while IFS= read -r row; do
    jq -e '.reference | endswith("@" + $digest)' --arg digest "$(jq -r .manifestDigest <<<"${row}")" <<<"${row}" >/dev/null || fail "candidate native image reference is not exact"
    archive="$(jq -r .archive <<<"${row}")"
    pin="$(jq -r .archiveDigest <<<"${row}")"
    candidate_pin "${archive}" "${pin}" || return 1
  done < <(jq -c '.[]' "${NKE_E2E_CANDIDATE_IMAGE_INPUTS}")
  for role in spire postgresql temporal; do
    [[ -f "${NKE_E2E_CANDIDATE_VALUES}/${role}.json" && ! -L "${NKE_E2E_CANDIDATE_VALUES}/${role}.json" ]] || fail "candidate ${role} image values are missing"
    jq -e 'type == "object" and length > 0' "${NKE_E2E_CANDIDATE_VALUES}/${role}.json" >/dev/null || fail "candidate ${role} values are invalid"
  done
}

candidate_import_images() {
  local row archive ref repository names
  while IFS= read -r row; do
    archive="$(jq -r .archive <<<"${row}")"
    ref="$(jq -r .reference <<<"${row}")"
    repository="${ref%@*}"
    timeout 180 docker exec -i "${KIND_CLUSTER_NAME}-control-plane" ctr -n k8s.io images import --local --digests \
      --platform linux/arm64 --base-name "${repository}" - <"${archive}" || fail "cannot import selected native candidate image"
    names="$(timeout 30 docker exec -i "${KIND_CLUSTER_NAME}-control-plane" ctr -n k8s.io images ls -q "name==${ref}" </dev/null)" || fail "cannot read back native candidate image"
    [[ "${names}" == "${ref}" ]] || fail "containerd did not resolve the exact selected native digest"
    printf '%s\n' "${row}" >>"${STATE_DIR}/imported-images.jsonl"
  done < <(jq -c '.[]' "${NKE_E2E_CANDIDATE_IMAGE_INPUTS}")
}

candidate_api() {
  curl --fail --silent --show-error --connect-timeout 5 --max-time 15 \
    -H "Authorization: Bearer ${NICO_VMS_TOKEN}" "$@"
}

candidate_verify_disk_service() {
  local digest
  digest="$(set -o pipefail; curl --fail --silent --show-error --connect-timeout 5 --max-time 300 \
    http://172.30.0.1:18080/nvke-node-image-arm64.qcow2 | timeout --foreground --kill-after=5s 300 sha256sum)" || return 1
  [[ "sha256:${digest%% *}" == "${NKE_E2E_CANDIDATE_QCOW2_SHA256}" ]]
}

candidate_domain_gone() {
  local domains
  domains="$(timeout --foreground --kill-after=5s 15 virsh -c "${LIBVIRT_URI}" list --all --name)" || return 1
  ! grep -Fxq "$1" <<<"${domains}"
}

candidate_verify_guest_backing() {
  local id="$1" disk chain base digest
  disk="${STATE_DIR}/sim/instances/${id}/disk.qcow2"
  # Foreground timeout leaves timed tools inside the monitor's owned process
  # group, so cancelling the monitor also stops and joins an in-flight hash.
  chain="$(timeout --foreground --kill-after=5s 30 qemu-img info --force-share --backing-chain --output=json "${disk}")" || return 1
  # The native simulator owner creates one qcow2 overlay over its read-only
  # cached base. qemu-img owns interpretation of that chain, not an XML parser.
  base="$(jq -er --arg disk "${disk}" 'select(type == "array" and length == 2 and
    .[0].filename == $disk and all(.[]; .format == "qcow2")) | .[1].filename' <<<"${chain}")" || return 1
  [[ "${base}" == "${NKE_E2E_CANDIDATE_QCOW2}" || "${base}" == "${STATE_DIR}/sim/cache/"*.qcow2 ]] || return 1
  [[ -f "${base}" && ! -L "${base}" ]] || return 1
  digest="$(timeout --foreground --kill-after=5s 300 sha256sum <"${base}")" || return 1
  # A completed hash proving different content is terminal. Retrying a
  # transient observation must never discard a known wrong boot image.
  [[ "sha256:${digest%% *}" == "${NKE_E2E_CANDIDATE_QCOW2_SHA256}" ]] || return 2
}

candidate_record_instances() {
  local rows id domain observed status
  rows="$(candidate_api "${candidate_endpoint}/v2/org/${NICO_VMS_ORG_ID}/forge/instance")" || return 1
  jq -e 'type == "array" and all(.[]; (.id | type == "string" and length > 0))' <<<"${rows}" >/dev/null || return 1
  while IFS= read -r id; do
    [[ "${id}" != *[!a-zA-Z0-9-]* ]] || return 1
    printf '%s\n' "${id}" >>"${STATE_DIR}/owned-instance-ids"
    domain="nico-vm-${id}"
    observed="${STATE_DIR}/observed-domain-${id}.xml"
    [[ ! -s "${observed}" ]] || continue
    if ! timeout --foreground --kill-after=5s 15 virsh -c "${LIBVIRT_URI}" dominfo "${domain}" >/dev/null 2>&1; then
      candidate_domain_gone "${domain}" && continue
      return 1
    fi
    if ! timeout --foreground --kill-after=5s 15 virsh -c "${LIBVIRT_URI}" dumpxml "${domain}" >"${observed}.partial"; then
      candidate_domain_gone "${domain}" && continue
      return 1
    fi
    grep -q "<domain type='kvm'" "${observed}.partial" || return 1
    if candidate_verify_guest_backing "${id}"; then
      :
    else
      status=$?
      ((status != 2)) || return 2
      candidate_domain_gone "${domain}" && continue
      return 1
    fi
    mv "${observed}.partial" "${observed}" || return 1
  done < <(jq -r '.[].id' <<<"${rows}")
  sort -u "${STATE_DIR}/owned-instance-ids" -o "${STATE_DIR}/owned-instance-ids"
}

candidate_monitor_instances() {
  local failures=0 status
  while true; do
    if candidate_record_instances; then
      failures=0
    else
      status=$?
      ((status != 2)) || return 2
      failures=$((failures + 1))
      printf 'ownership observation failed (%s/3 consecutive attempts)\n' "${failures}" >&2
      ((failures < 3)) || return 1
    fi
    sleep 2
  done
}

candidate_start_controller_forward() {
  local inventory
  inventory="$(ss -H -lnt "sport = :${NVKECTL_SMOKE_CONTROLLER_PORT}")" || return 1
  [[ -z "${inventory}" ]] || fail "candidate controller smoke port is already owned"
  setsid kubectl --kubeconfig "${E2E_KUBECONFIG}" -n "${NVKECTL_SMOKE_CONTROLLER_NAMESPACE}" \
    port-forward --address 127.0.0.1 "svc/${NVKECTL_SMOKE_CONTROLLER_SERVICE}" \
    "${NVKECTL_SMOKE_CONTROLLER_PORT}:${NVKECTL_SMOKE_CONTROLLER_PORT}" >"${STATE_DIR}/controller-forward.log" 2>&1 &
  candidate_controller_pid=$!
  for _ in $(seq 1 30); do
    kill -0 "${candidate_controller_pid}" || return 1
    if curl --fail --silent --show-error --connect-timeout 2 --max-time 5 \
      "http://127.0.0.1:${NVKECTL_SMOKE_CONTROLLER_PORT}/healthz" >/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}

candidate_stop_group() {
  local pid="$1" privileged="${2:-false}" gone=false
  [[ -n "${pid}" ]] || return 0
  local -a command=()
  [[ "${privileged}" != true ]] || command=(sudo -n)
  "${command[@]}" kill -TERM -- "-${pid}" 2>/dev/null || true
  for _ in $(seq 1 20); do
    if ! "${command[@]}" kill -0 -- "-${pid}" 2>/dev/null; then gone=true; break; fi
    sleep 1
  done
  if [[ "${gone}" != true ]]; then
    "${command[@]}" kill -KILL -- "-${pid}" 2>/dev/null || return 1
  fi
  wait "${pid}" 2>/dev/null || true
  ! "${command[@]}" kill -0 -- "-${pid}" 2>/dev/null
}

candidate_verify_reports() {
  [[ -s "${STATE_DIR}/junit.xml" ]] || fail "candidate suite omitted JUnit"
  jq -e 'length == 1 and .[0].SuiteSucceeded == true and .[0].PreRunStats.SpecsThatWillRun > 0 and ([.[0].SpecReports[] | select(.LeafNodeType == "It" and .State == "passed")] | length) == .[0].PreRunStats.SpecsThatWillRun' \
    "${STATE_DIR}/ginkgo.json" >/dev/null || fail "candidate suite was empty, skipped required cases or incomplete"
}

candidate_cleanup() {
  local rc=$? cleanup=0 id domain remaining pid rows safe=false domains_clear=false
  trap - EXIT INT TERM
  set +e
  if [[ "${candidate_state_owned}" == true ]]; then
    # Stop test creation and the owned management cluster before draining
    # simulator records, so its controllers cannot create another instance.
    candidate_stop_group "${candidate_suite_pid}" || cleanup=1
    candidate_stop_group "${candidate_monitor_pid}" || cleanup=1
    candidate_stop_group "${candidate_controller_pid}" || cleanup=1
    if [[ "${KIND_CLUSTER_OWNED}" == true ]]; then
      timeout 30 kubectl --kubeconfig "${E2E_KUBECONFIG}" get clusters,machines -A -o json >"${STATE_DIR}/final-capi.json" 2>"${STATE_DIR}/final-capi.err"
      if timeout 120 kind delete cluster --name "${KIND_CLUSTER_NAME}" >"${STATE_DIR}/kind-delete.log" 2>&1; then
        if remaining="$(timeout 30 kind get clusters 2>"${STATE_DIR}/kind-inventory.err")" && ! grep -Fxq "${KIND_CLUSTER_NAME}" <<<"${remaining}"; then
          KIND_CLUSTER_OWNED=false
        else
          cleanup=1
        fi
      else
        cleanup=1
      fi
    fi
    if [[ -n "${candidate_sim_pid}" ]]; then
      candidate_record_instances || cleanup=1
      # Historical IDs remain in the ledger for exact domain recovery. Delete
      # only currently live instances; successful suite deletions need no retry.
      rows="$(candidate_api "${candidate_endpoint}/v2/org/${NICO_VMS_ORG_ID}/forge/instance")" || cleanup=1
      if jq -e 'type == "array" and all(.[]; .id | type == "string")' <<<"${rows}" >/dev/null; then
        while IFS= read -r id; do
          [[ -n "${id}" && "${id}" != *[!a-zA-Z0-9-]* ]] || { cleanup=1; continue; }
          grep -Fxq "${id}" "${STATE_DIR}/owned-instance-ids" || { cleanup=1; continue; }
          candidate_api -X DELETE "${candidate_endpoint}/v2/org/${NICO_VMS_ORG_ID}/forge/instance/${id}" >/dev/null || cleanup=1
        done < <(jq -r '.[].id' <<<"${rows}")
        for _ in $(seq 1 60); do
          remaining="$(candidate_api "${candidate_endpoint}/v2/org/${NICO_VMS_ORG_ID}/forge/instance")" || { cleanup=1; break; }
          if jq -e 'type == "array" and length == 0' <<<"${remaining}" >/dev/null; then break; fi
          sleep 2
        done
        jq -e 'type == "array" and length == 0' <<<"${remaining:-null}" >/dev/null || cleanup=1
      else
        cleanup=1
      fi
      # The private API issued this exact ledger; never delete by name prefix.
      while IFS= read -r id; do
        [[ -n "${id}" && "${id}" != *[!a-zA-Z0-9-]* ]] || { cleanup=1; continue; }
        domain="nico-vm-${id}"
        if timeout 15 virsh -c "${LIBVIRT_URI}" dominfo "${domain}" >/dev/null 2>&1; then
          cleanup=1
          timeout 15 virsh -c "${LIBVIRT_URI}" dumpxml "${domain}" >"${STATE_DIR}/domain-${id}.xml" 2>/dev/null
          timeout 15 virsh -c "${LIBVIRT_URI}" destroy "${domain}" >/dev/null 2>&1
          timeout 15 virsh -c "${LIBVIRT_URI}" undefine "${domain}" --nvram >/dev/null 2>&1 || \
            timeout 15 virsh -c "${LIBVIRT_URI}" undefine "${domain}" >/dev/null 2>&1
        fi
      done <"${STATE_DIR}/owned-instance-ids"
    fi
    for pid in "${candidate_spire_pid}" "${candidate_oidc_pid}" "${candidate_sim_pid}"; do
      candidate_stop_group "${pid}" || cleanup=1
    done
    candidate_stop_group "${candidate_dhcp_pid}" true || cleanup=1
    if [[ "${KIND_CLUSTER_OWNED}" == true ]]; then
      timeout 120 kind delete cluster --name "${KIND_CLUSTER_NAME}" >"${STATE_DIR}/kind-delete.log" 2>&1 || cleanup=1
      remaining="$(timeout 30 kind get clusters 2>"${STATE_DIR}/kind-inventory.err")" || cleanup=1
      if grep -Fxq "${KIND_CLUSTER_NAME}" <<<"${remaining}"; then cleanup=1; fi
    fi
    if remaining="$(timeout 15 virsh -c "${LIBVIRT_URI}" list --all --name)" && [[ -z "${remaining}" ]]; then
      domains_clear=true
    else
      cleanup=1
    fi
    if [[ -f "${STATE_DIR}/sim/nico-sim" ]]; then
      sudo -n setcap -r "${STATE_DIR}/sim/nico-sim" || cleanup=1
    fi
    # Caller inputs remain outside this newly created tree. Raw credentials,
    # guest disks and executables are never uploaded as diagnostics.
    if [[ "${domains_clear}" == true ]]; then rm -rf -- "${STATE_DIR}/sim"; fi
    rm -rf -- "${STATE_DIR}/rendered" "${STATE_DIR}/nvkectl-home" || cleanup=1
    rm -f -- "${E2E_BIN}" "${E2E_CONFIG}" "${E2E_KUBECONFIG}" || cleanup=1
    if [[ ! -e "${STATE_DIR}/sim" ]] && redact_diagnostics; then safe=true; else cleanup=1; fi
    jq -n --argjson test_exit "${candidate_test_exit}" --argjson cleanup_exit "${cleanup}" --argjson entry_exit "${rc}" \
      --arg profile "${NKE_E2E_CANDIDATE_OS_PROFILE}" --arg kube "${NKE_E2E_CANDIDATE_KUBERNETES_VERSION}" \
      '{test_exit:$test_exit, cleanup_exit:$cleanup_exit, entry_exit:$entry_exit, profile:$profile, kubernetes:$kube, qualification_authority:"none"}' \
      >"${STATE_DIR}/candidate-outcome.json" || cleanup=1
    # The marker authorizes only successfully redacted diagnostics. Cleanup
    # failures remain uploadable and force a failed job.
    if [[ "${safe}" == true ]]; then touch "${DIAGNOSTICS_MARKER}" || cleanup=1; fi
  fi
  [[ "${rc}" != 0 || "${cleanup}" == 0 ]] || rc=1
  exit "${rc}"
}

candidate_render_stack() {
  "${NVKECTL_BIN}" render deploy-path --deploy-path "${WORKSPACE_ROOT}/deploy/nico" --sitespec "${NKE_E2E_CANDIDATE_SITESPEC}" \
    --stack-config "${NKE_E2E_CANDIDATE_STACK_CONFIG}" \
    --release "${NKE_E2E_CANDIDATE_RELEASE}" --nvkr-run-dir "${NKE_E2E_CANDIDATE_LAYOUT}" --arch linux/arm64 --out "${STATE_DIR}/rendered"
}

candidate_admit() {
  [[ "$(uname -s)" == Linux && "$(uname -m)" == aarch64 ]] || fail "the initial candidate VM profile requires native Linux ARM64"
  [[ -r /dev/kvm && -w /dev/kvm ]] || fail "candidate VM execution requires usable KVM"
  (exec 8<>/dev/kvm) || fail "cannot open KVM"
  [[ "${NKE_E2E_LABEL_FILTER:-}" == "(nico-vms && ovn) || health" ]] || fail "first candidate mode requires the explicit OVN and health suite contract"
  [[ -z "${KEEP}" ]] || fail "candidate mode requires unconditional owned cleanup"
  for tool in curl docker go helm jq kind kubectl clusterctl sha256sum setsid virsh flock timeout dnsmasq sudo ss ip make qemu-img cloud-localds setcap getcap awk df sort seq; do require_command "${tool}"; done
  for variable in NKE_E2E_CANDIDATE_RELEASE NKE_E2E_CANDIDATE_LAYOUT NKE_E2E_CANDIDATE_NVKECTL NKE_E2E_CANDIDATE_NVKECTL_SHA256 \
    NKE_E2E_CANDIDATE_SIMULATOR NKE_E2E_CANDIDATE_SIMULATOR_SHA256 NKE_E2E_CANDIDATE_SEED NKE_E2E_CANDIDATE_SEED_SHA256 \
    NKE_E2E_CANDIDATE_QCOW2 NKE_E2E_CANDIDATE_QCOW2_SHA256 \
    NKE_E2E_CANDIDATE_SITESPEC NKE_E2E_CANDIDATE_STACK_CONFIG NKE_E2E_CANDIDATE_NKTP_VALUES NKE_E2E_CANDIDATE_RELEASES NKE_E2E_CANDIDATE_OS_PROFILE NKE_E2E_CANDIDATE_KUBERNETES_VERSION \
    NKE_E2E_CANDIDATE_CONTROLLER_VERSION NKE_E2E_CANDIDATE_SIMULATOR_ENDPOINT NKE_E2E_CANDIDATE_SIMULATOR_ADDRESS NKE_E2E_CANDIDATE_MIN_AVAILABLE_MIB NKE_E2E_CANDIDATE_MIN_DISK_MIB \
    NKE_E2E_CANDIDATE_IMAGE_INPUTS NKE_E2E_CANDIDATE_VALUES; do candidate_require "${variable}"; done
  [[ "${NKE_E2E_CANDIDATE_SIMULATOR_ENDPOINT}" == "http://${NKE_E2E_CANDIDATE_SIMULATOR_ADDRESS}:18089" ]] || fail "candidate simulator API must use its exact admitted listener18089"
  [[ "${NKE_E2E_CANDIDATE_OS_PROFILE}" == nvke-local-26-04-arm64-plain ]] || fail "candidate mode requires the explicit plain Ubuntu26.04 ARM64 profile"
  for pair in NVKECTL SIMULATOR SEED QCOW2; do
    local file_variable="NKE_E2E_CANDIDATE_${pair}" pin_variable="NKE_E2E_CANDIDATE_${pair}_SHA256"
    candidate_pin "${!file_variable}" "${!pin_variable}" || return 1
  done
  [[ -x "${NKE_E2E_CANDIDATE_NVKECTL}" && -x "${NKE_E2E_CANDIDATE_SIMULATOR}" ]] || fail "candidate tools are not executable"
  [[ -f "${NKE_E2E_CANDIDATE_SITESPEC}" && ! -L "${NKE_E2E_CANDIDATE_SITESPEC}" && -d "${NKE_E2E_CANDIDATE_LAYOUT}" && -d "${NKE_E2E_CANDIDATE_RELEASES}" ]] || fail "candidate handoff omits site, release resources or layout"
  for variable in NKE_E2E_CANDIDATE_STACK_CONFIG NKE_E2E_CANDIDATE_NKTP_VALUES; do
    [[ "${!variable}" == /* && -f "${!variable}" && ! -L "${!variable}" ]] || fail "candidate stack inputs must be absolute regular files"
  done
  for variable in NVKECTL_NICO_TOKEN NVKECTL_NODE_USER NVKECTL_NODE_PASSWORD NVKECTL_DATABASE_PASSWORD NVKECTL_SPIRE_CA_CERT_FILE NVKECTL_SPIRE_CA_KEY_FILE; do candidate_require "${variable}"; done
  candidate_release_inputs
  "${NKE_E2E_CANDIDATE_NVKECTL}" validate sitespec -f "${NKE_E2E_CANDIDATE_SITESPEC}" >/dev/null
  candidate_image_inputs
  [[ ! -e "${STATE_DIR}" && ! -L "${STATE_DIR}" ]] || fail "candidate mode requires a fresh private state directory"
  [[ "${LIBVIRT_URI}" == qemu:///session ]] || fail "candidate mode requires its dedicated libvirt session"
  candidate_network_inputs
  [[ "${NKE_E2E_CANDIDATE_MIN_AVAILABLE_MIB}" != *[!0-9]* && "${NKE_E2E_CANDIDATE_MIN_DISK_MIB}" != *[!0-9]* ]] || fail "candidate resource budgets must be positive integer MiB"
  [[ ${#NKE_E2E_CANDIDATE_MIN_AVAILABLE_MIB} -le 9 && ${#NKE_E2E_CANDIDATE_MIN_DISK_MIB} -le 9 ]] || fail "candidate budgets exceed supported MiB bounds"
  ((10#${NKE_E2E_CANDIDATE_MIN_AVAILABLE_MIB} > 0 && 10#${NKE_E2E_CANDIDATE_MIN_DISK_MIB} > 0)) || fail "candidate resource budgets must be positive"
}

run_candidate_e2e() {
  candidate_admit
  candidate_state_owned=false candidate_test_exit=null candidate_endpoint="${NKE_E2E_CANDIDATE_SIMULATOR_ENDPOINT}"
  candidate_sim_pid="" candidate_dhcp_pid="" candidate_monitor_pid="" candidate_spire_pid="" candidate_oidc_pid="" candidate_suite_pid="" candidate_controller_pid=""
  local bridge="${NICO_VMS_BRIDGE_NAME:-cwebr0}" gateway=172.30.0.1 available minimum disk inventory
  local simulator_executable="${NKE_E2E_CANDIDATE_SIMULATOR}"
  local vm_memory_mib=6144
  local dhcp_hosts="${STATE_DIR}/sim/dhcp-hosts" dhcp_leases="${STATE_DIR}/sim/leases"
  umask 077
  local lock="${NKE_E2E_CANDIDATE_RUNTIME_LOCK:?parent must provide its admitted runtime lock}"
  [[ -f "${lock}" && ! -L "${lock}" && -O "${lock}" ]] || fail "runtime lock must already be an owned regular file"
  if [[ ! "/proc/$$/fd/9" -ef "${lock}" ]]; then exec 9<>"${lock}"; fi
  flock -n 9 || fail "another local VM attempt owns the runtime"
  inventory="$(timeout 15 virsh -c "${LIBVIRT_URI}" list --all --name)" || fail "cannot inspect libvirt session"
  [[ -z "${inventory}" ]] || fail "refusing a libvirt session with foreign domains"
  inventory="$(ss -H -lnt 'sport = :18089')" || fail "cannot inspect simulator listener"
  [[ -z "${inventory}" ]] || fail "the simulator listener is already owned"
  inventory="$(ss -H -lnu 'sport = :67')" || fail "cannot inspect DHCP listener"
  if [[ "${NKE_E2E_CANDIDATE_NETWORK_OWNER:-}" == nvkr-network-guard ]]; then
    [[ -n "${inventory}" ]] || fail "guard DHCP listener is unavailable"
    inventory="$(ss -H -lnu 'sport = :53')" || fail "cannot inspect guard DNS listener"
    [[ -n "${inventory}" ]] || fail "guard DNS listener is unavailable"
    dhcp_hosts="${NKE_E2E_CANDIDATE_DHCP_HOSTS_DIR}"
    dhcp_leases="${NKE_E2E_CANDIDATE_DHCP_LEASE_FILE}"
    vm_memory_mib=8192
  else
    [[ -z "${inventory}" ]] || fail "a DHCP server already owns this runtime"
  fi
  candidate_verify_disk_service || fail "served candidate disk differs from its exact byte pin or is unavailable"
  ip link show "${bridge}" >/dev/null || fail "host setup has not prepared the dedicated VM bridge"
  sudo -n true || fail "candidate DHCP and simulator networking require admitted passwordless sudo"
  available="$(awk '/MemAvailable:/{print int($2/1024)}' /proc/meminfo)"
  minimum="$((10#${NKE_E2E_CANDIDATE_MIN_AVAILABLE_MIB}))"
  ((available >= minimum)) || fail "candidate memory budget is unavailable"
  disk="$(df -Pm "${WORKSPACE_ROOT}" | awk 'NR==2{print $4}')"
  ((disk >= 10#${NKE_E2E_CANDIDATE_MIN_DISK_MIB})) || fail "candidate disk budget is unavailable"
  inventory="$(timeout 30 kind get clusters)" || fail "cannot inspect kind clusters"
  if grep -Fxq "${KIND_CLUSTER_NAME}" <<<"${inventory}"; then fail "candidate kind cluster already exists"; fi
  mkdir -p "$(dirname "${STATE_DIR}")"
  mkdir -m 0700 "${STATE_DIR}"
  candidate_state_owned=true
  trap candidate_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  mkdir "${STATE_DIR}/sim" "${STATE_DIR}/sim/files" "${STATE_DIR}/sim/dhcp-hosts"
  touch "${STATE_DIR}/owned-instance-ids" "${STATE_DIR}/sim/leases"
  jq -n --arg cli "${NKE_E2E_CANDIDATE_NVKECTL_SHA256}" --arg simulator "${NKE_E2E_CANDIDATE_SIMULATOR_SHA256}" \
    --arg disk "${NKE_E2E_CANDIDATE_QCOW2_SHA256}" --arg seed "${NKE_E2E_CANDIDATE_SEED_SHA256}" \
    '{cli:$cli, simulator:$simulator, qcow2:$disk, seed:$seed, byte_integrity_only:true}' >"${STATE_DIR}/candidate-inputs.json"
  export KUBECONFIG="${E2E_KUBECONFIG}"
  NVKECTL_SMOKE_PREBUILT="${NKE_E2E_CANDIDATE_NVKECTL}"
  NVKECTL_SMOKE_SHA256="${NKE_E2E_CANDIDATE_NVKECTL_SHA256}"
  nvkectl_smoke_build arm64
  if [[ "${NKE_E2E_CANDIDATE_NETWORK_OWNER:-}" == nvkr-network-guard ]]; then
    # The guard owns this exact copy and its journaled capability lifecycle.
    sudo -n setcap -v cap_net_admin=ep "${simulator_executable}"
  else
    # The capability belongs to an attempt copy, never the caller's artifact.
    cp "${NKE_E2E_CANDIDATE_SIMULATOR}" "${STATE_DIR}/sim/nico-sim"
    chmod 0700 "${STATE_DIR}/sim/nico-sim"
    candidate_pin "${STATE_DIR}/sim/nico-sim" "${NKE_E2E_CANDIDATE_SIMULATOR_SHA256}"
    sudo -n setcap cap_net_admin=ep "${STATE_DIR}/sim/nico-sim"
    sudo -n setcap -v cap_net_admin=ep "${STATE_DIR}/sim/nico-sim"
    simulator_executable="${STATE_DIR}/sim/nico-sim"
  fi
  # Foreground DHCP is supervised here, never through run-dnsmasq.sh's shared pkill.
  if [[ "${NKE_E2E_CANDIDATE_NETWORK_OWNER:-}" != nvkr-network-guard ]]; then
  setsid sudo -n dnsmasq --user="$(id -un)" --group="$(id -gn)" --keep-in-foreground --conf-file=/dev/null --interface="${bridge}" --bind-interfaces \
    --listen-address="${gateway}" --dhcp-range="172.30.0.10,172.30.0.239,255.255.255.0,30m" \
    --dhcp-hostsdir="${STATE_DIR}/sim/dhcp-hosts" --dhcp-leasefile="${STATE_DIR}/sim/leases" --pid-file="${STATE_DIR}/sim/dhcp.pid" \
    --dhcp-option=option:router,"${gateway}" --dhcp-option=option:dns-server,"${gateway}" \
    --address=/carbide-pxe.forge/"${gateway}" --log-facility=- >"${STATE_DIR}/dhcp.log" 2>&1 &
  candidate_dhcp_pid=$!
  fi
  # Restrict child environment to the runtime and the selected candidate.
  setsid env -i PATH="${PATH}" HOME="${HOME}" XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}" \
    NICO_SIM_LISTEN_ADDR="${NKE_E2E_CANDIDATE_SIMULATOR_ADDRESS}:18089" NICO_SIM_DB_PATH="${STATE_DIR}/sim/nico-sim.db" NICO_SIM_SEED_PATH="${NKE_E2E_CANDIDATE_SEED}" \
    NICO_SIM_FILES_DIR="${STATE_DIR}/sim/files" NICO_VMS_VM_BACKEND=libvirt NICO_VMS_LIBVIRT_URI="${LIBVIRT_URI}" \
    NICO_VMS_BASE_IMAGE="${NKE_E2E_CANDIDATE_QCOW2}" NICO_VMS_BRIDGE_NAME="${bridge}" NICO_VMS_BRIDGE_CIDR=172.30.0.0/24 \
    NICO_VMS_IMAGE_CACHE_DIR="${STATE_DIR}/sim/cache" NICO_VMS_DHCP_HOSTS_DIR="${dhcp_hosts}" NICO_VMS_DHCP_LEASE_FILE="${dhcp_leases}" \
    NICO_VMS_CLEANUP_ORPHANED_VMS=false NICO_CONTAINERS_ENABLED=false NICO_VMS_BGP_ENABLED=true NICO_VMS_BGP_LISTEN_PORT=10179 \
    NICO_VMS_BGP_VIP_CIDRS=172.31.0.0/24 NICO_VMS_CONSOLE_SSH_PORT=-1 NICO_VMS_VM_VCPUS=4 NICO_VMS_VM_MEMORY_MIB="${vm_memory_mib}" NICO_VMS_VM_DISK_GIB=60 NICO_VMS_READINESS_TIMEOUT=30m NICO_VMS_DIAG_SSH_ENABLED=false \
    "${simulator_executable}" >"${STATE_DIR}/simulator.log" 2>&1 &
  candidate_sim_pid=$!
  for _ in $(seq 1 60); do
    kill -0 "${candidate_sim_pid}" || fail "owned simulator exited"
    if [[ "${NKE_E2E_CANDIDATE_NETWORK_OWNER:-}" == nvkr-network-guard ]]; then
      kill -0 "${NKE_E2E_CANDIDATE_DHCP_PID}" || fail "guard DHCP exited"
    else
      sudo -n kill -0 "${candidate_dhcp_pid}" || fail "owned DHCP exited"
    fi
    if candidate_api "${candidate_endpoint}/healthz" >/dev/null; then break; fi
    sleep 2
  done
  candidate_api "${candidate_endpoint}/healthz" >/dev/null
  candidate_record_instances
  export -f candidate_record_instances candidate_api candidate_domain_gone candidate_verify_guest_backing candidate_monitor_instances
  export candidate_endpoint NICO_VMS_ORG_ID NICO_VMS_TOKEN STATE_DIR LIBVIRT_URI NKE_E2E_CANDIDATE_QCOW2 NKE_E2E_CANDIDATE_QCOW2_SHA256
  setsid bash -ec 'candidate_monitor_instances' >"${STATE_DIR}/ownership-monitor.log" 2>&1 &
  candidate_monitor_pid=$!
  KIND_CLUSTER_OWNED=true
  kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${E2E_KUBECONFIG}" --config "${CONTROLLER_ROOT}/hack/controller-capi-kind.yaml" --wait 5m
  candidate_import_images
  candidate_kubectl get nodes -o json >"${STATE_DIR}/kind-nodes.json"
  if jq -e 'any(.items[].spec.taints[]?; .key == "node-role.kubernetes.io/control-plane")' "${STATE_DIR}/kind-nodes.json" >/dev/null; then
    candidate_kubectl taint nodes --all node-role.kubernetes.io/control-plane- >/dev/null
  fi
  clusterctl init --core "cluster-api:$(make -C "${WORKSPACE_ROOT}" --no-print-directory print-CAPI_VERSION)" \
    --bootstrap "kubeadm:$(make -C "${WORKSPACE_ROOT}" --no-print-directory print-CAPI_VERSION)" \
    --control-plane "kubeadm:$(make -C "${WORKSPACE_ROOT}" --no-print-directory print-CAPI_VERSION)"
  candidate_render_stack
  # Matching MachineImage, ClusterRelease and NodePoolRelease resources are
  # supplied by the caller for the current internal.nvke.nvidia.com/v1 API.
  jq -n '{tenantAccess:{enabled:true},nvkeOIDCAuth:{enabled:true},apiAuth:{enabled:true,configMap:{name:"nke-auth-config"}}}' >"${STATE_DIR}/controller-values.json"
  candidate_kubectl apply --server-side -f "${CONTROLLER_ROOT}/deploy/k8s/nke-auth-dev.yaml"
  CTR="docker exec -i ${KIND_CLUSTER_NAME}-control-plane ctr" bash "${WORKSPACE_ROOT}/deploy/nvke-deploy.sh" install \
    --deploy-path "${STATE_DIR}/rendered" --kubeconfig "${E2E_KUBECONFIG}" --values-for "controller=${STATE_DIR}/controller-values.json" \
    --values-for "spire=${NKE_E2E_CANDIDATE_VALUES}/spire.json" --values-for "postgresql=${NKE_E2E_CANDIDATE_VALUES}/postgresql.json" \
    --values-for "temporal=${NKE_E2E_CANDIDATE_VALUES}/temporal.json" \
    --values-for "nktp=${NKE_E2E_CANDIDATE_NKTP_VALUES}"
  candidate_kubectl apply --server-side -f "${NKE_E2E_CANDIDATE_RELEASES}"
  bash "${CONTROLLER_ROOT}/hack/apply-capi-spiffe-exec-auth.sh" --kubeconfig "${E2E_KUBECONFIG}"
  for service in spire-server-np spire-oidc-provider-np; do
    local port=30440
    [[ "${service}" != spire-oidc-provider-np ]] || port=30443
    setsid kubectl --kubeconfig "${E2E_KUBECONFIG}" -n spire-server port-forward --address "${gateway}" "svc/${service}" "${port}:443" >"${STATE_DIR}/${service}.log" 2>&1 &
    if [[ "${port}" == 30440 ]]; then candidate_spire_pid=$!; else candidate_oidc_pid=$!; fi
  done
  candidate_kubectl -n nke-controller rollout status deployment/nke-controller --timeout=5m
  candidate_kubectl -n capnico-system rollout status deployment/controller-manager --timeout=5m
  jq -n --arg kubeconfig "${E2E_KUBECONFIG}" --arg context "kind-${KIND_CLUSTER_NAME}" \
    --arg profile "${NKE_E2E_CANDIDATE_OS_PROFILE}" --arg kube "${NKE_E2E_CANDIDATE_KUBERNETES_VERSION}" \
    --arg binary "${NVKECTL_BIN}" '{kube:{kubeconfig:$kubeconfig,context:$context},cluster:{nicoVMSOSProfile:$profile,kubernetesVersion:$kube},nvkectl:{binary:$binary},lifecycle:{skipDelete:false,preserveOnFailure:false}}' >"${E2E_CONFIG}"
  (cd "${CONTROLLER_ROOT}/test/e2e" && GOENV=off CGO_ENABLED=0 go test -c -o "${E2E_BIN}" .)
  set +e
  NKE_CONTROLLER_RELEASES_DIR="${NKE_E2E_CANDIDATE_RELEASES}" NKE_CONTROLLER_EXPECTED_VERSION="${NKE_E2E_CANDIDATE_CONTROLLER_VERSION}" \
    setsid "${E2E_BIN}" -test.run='^TestE2E$' -test.v -test.timeout=285m -e2e.config="${E2E_CONFIG}" -ginkgo.v -ginkgo.no-color -ginkgo.fail-on-empty \
    -ginkgo.fail-fast -ginkgo.timeout=270m "-ginkgo.label-filter=${LABEL_FILTER}" "-ginkgo.junit-report=${STATE_DIR}/junit.xml" \
    "-ginkgo.json-report=${STATE_DIR}/ginkgo.json" >"${STATE_DIR}/e2e.log" 2>&1 &
  candidate_suite_pid=$!
  wait "${candidate_suite_pid}"
  candidate_test_exit=$?
  # A joined leader can leave descendants in its owned process group. Keep
  # the group identity until EXIT cleanup verifies the entire group is gone.
  set -e
  cat "${STATE_DIR}/e2e.log"
  kill -0 "${candidate_monitor_pid}" || fail "ownership monitor failed"
  ((candidate_test_exit == 0)) || fail "candidate tenant suite failed"
  candidate_verify_reports
  compgen -G "${STATE_DIR}/observed-domain-*.xml" >/dev/null || fail "candidate suite never exercised a native KVM guest"
  candidate_verify_disk_service || fail "served candidate disk changed during qualification"
  NVKECTL_SMOKE_KUBERNETES_VERSION="${NKE_E2E_CANDIDATE_KUBERNETES_VERSION}"
  candidate_start_controller_forward || fail "owned candidate controller forwarding did not become ready"
  nvkectl_smoke
  e2e_wait_for_zero_tenant_clusters candidate_kubectl log || fail "candidate suite left tenant clusters"
}
