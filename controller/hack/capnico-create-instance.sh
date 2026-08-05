#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# Boot a bare VM on the local forge-vms backend through CAPNICo (the replacement
# for `self-bootstrap create`).
#
# Applies the six resources CAPNICo needs -- Namespace, bootstrap Secret,
# NicoCluster, Cluster, NicoMachine, Machine -- then waits for the NicoMachine to
# report ready and prints the forge-vms instance JSON.
#
# Notes:
#   - The iPXE script comes from the CWE-side OS profile
#     (profiles/osProfiles/<OS_PROFILE>/ipxeScript); forge-vms and CAPNICo only
#     ever see the rendered text. `{{ .DiskSelectionStrategy }}` is the only
#     template variable today and is rendered with sed; any surviving `{{` is a
#     hard failure rather than a VM that silently fails to boot.
#   - The userData is a deliberately minimal bare boot: no teleport, SPIRE,
#     nke-agent or kubeadm. Readiness is a port-22 probe (forge-vms injects its
#     own forge-diag user), so cloud-init never gates Ready.
#   - CAPNICo v0.0.28 CRDs strict-decode: the capital-`ID` spelling (siteID,
#     vpcID, instanceTypeID, subnetID, sshKeyGroupIDs) is load-bearing. Lowercase
#     `id` fields are rejected with a hard BadRequest, not pruned.
#   - Applying a changed spec does NOT re-image a provisioned VM, so spec drift
#     is refused unless RECREATE=true (which deletes the instance first).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAMESPACE="${NAMESPACE:-forge-test}"
CLUSTER_NAME="${CLUSTER_NAME:-forge-vms-bare}"
INSTANCE_NAME="${INSTANCE_NAME:-capnico-vm-1}"
BOOTSTRAP_SECRET_NAME="${BOOTSTRAP_SECRET_NAME:-${INSTANCE_NAME}-bootstrap}"

OS_PROFILE="${OS_PROFILE:-nke-260611-24.04-amd64-fde}"
DISK_SELECTION_STRATEGY="${DISK_SELECTION_STRATEGY:-smallest}"

# forge-vms seed resources (forge-vms/internal/seed/default-seed.yaml).
ORG_ID="${ORG_ID:-novxwn552ssc}"
SITE_ID="${SITE_ID:-a1b2c3d4-0001-4000-8000-000000000001}"
VPC_ID="${VPC_ID:-a1b2c3d4-0002-4000-8000-000000000001}"
SUBNET_ID="${SUBNET_ID:-a1b2c3d4-0003-4000-8000-000000000001}"
SSH_KEY_GROUP_ID="${SSH_KEY_GROUP_ID:-a1b2c3d4-0006-4000-8000-000000000001}"
# cpu2-mem4gb-disk50gb. The instance type's disk_gib overrides
# FORGE_VMS_VM_DISK_GIB, and 50 GiB is ample for a bare boot. Use
# a1b2c3d4-0005-4000-8000-000000000004 (cpu6-mem32gb-disk60gb) for real NKE-node
# work later.
INSTANCE_TYPE_ID="${INSTANCE_TYPE_ID:-a1b2c3d4-0005-4000-8000-000000000005}"

FORGE_VMS_URL="${FORGE_VMS_URL:-http://localhost:18080}"
FORGE_VMS_TOKEN="${FORGE_VMS_TOKEN:-forge-vmsctl-token}"

# cluster.x-k8s.io/v1beta1 is still served on the pinned CAPI v1.12.3 and
# matches CWE's own nico_machine_builders output, but the CRDs mark it
# deprecated (applying it prints a deprecation warning) and v1beta2 is the
# storage version. v1beta2 is the current-generation form: it references infra
# via apiGroup, not apiVersion.
CAPI_API_VERSION="${CAPI_API_VERSION:-v1beta1}"
NICO_API_VERSION="infrastructure.cluster.x-k8s.io/v1alpha1"

# A cold CloudFront qcow pull can take 10-25 minutes; a warm image cache boots in
# 1-3. The default has to cover the cold case or the first run of a new profile
# fails while the VM is still downloading.
WAIT_TIMEOUT="${WAIT_TIMEOUT:-30m}"
RECREATE="${RECREATE:-false}"

# NicoMachine.status.ready=true and the Machine.spec.providerID patch come from
# two different reconciles, so the providerID routinely lands a beat after the VM
# reports ready. Poll for it instead of reading once.
PROVIDER_ID_TIMEOUT_SECONDS="${PROVIDER_ID_TIMEOUT_SECONDS:-120}"
PROVIDER_ID_POLL_INTERVAL_SECONDS="${PROVIDER_ID_POLL_INTERVAL_SECONDS:-2}"

# Login for the bare-boot VM. Required from the environment -- credentials are
# never committed, so there is no default and an unset value fails fast here
# rather than baking a well-known password into every VM.
VM_USER="${VM_USER:?export VM_USER (bare-boot VM login user)}"
VM_PASSWORD="${VM_PASSWORD:?export VM_PASSWORD (bare-boot VM login password)}"

command -v jq >/dev/null || {
  echo "jq is required (it escapes the VM login into the cloud-config and prints the instance JSON)" >&2
  exit 1
}
command -v curl >/dev/null || {
  echo "curl is required (it queries the forge-vms instance after the VM is ready)" >&2
  exit 1
}

# These end up in Kubernetes object names and in a sed replacement, so reject
# bad values here rather than after half the resources have been applied.
for _name_var in INSTANCE_NAME CLUSTER_NAME NAMESPACE BOOTSTRAP_SECRET_NAME; do
  case "${!_name_var}" in
    *[!a-z0-9-]* | -* | *- | "")
      echo "${_name_var}='${!_name_var}' is not an RFC 1123 name (lowercase alphanumerics and '-', starting and ending alphanumeric)" >&2
      exit 1
      ;;
  esac
done

# Both feed $(( )) and sleep, so reject junk here rather than after the VM is
# already booting.
for _int_var in PROVIDER_ID_TIMEOUT_SECONDS PROVIDER_ID_POLL_INTERVAL_SECONDS; do
  case "${!_int_var}" in
    "" | *[!0-9]*)
      echo "${_int_var}='${!_int_var}' must be a non-negative integer (seconds)" >&2
      exit 1
      ;;
  esac
done
[ "${PROVIDER_ID_POLL_INTERVAL_SECONDS}" -gt 0 ] || {
  echo "PROVIDER_ID_POLL_INTERVAL_SECONDS must be at least 1 (0 would spin)" >&2
  exit 1
}

# Spliced into a sed replacement whose delimiter is '|' (see IPXE_SCRIPT below),
# so anything outside the known strategy names is refused.
case "${DISK_SELECTION_STRATEGY}" in
  smallest | largest) ;;
  *)
    echo "Unsupported DISK_SELECTION_STRATEGY '${DISK_SELECTION_STRATEGY}' (expected smallest or largest)" >&2
    exit 1
    ;;
esac

case "${CAPI_API_VERSION}" in
  v1beta1) INFRA_REF_API_LINE="    apiVersion: ${NICO_API_VERSION}" ;;
  v1beta2) INFRA_REF_API_LINE="    apiGroup: infrastructure.cluster.x-k8s.io" ;;
  *)
    echo "Unsupported CAPI_API_VERSION '${CAPI_API_VERSION}' (expected v1beta1 or v1beta2)" >&2
    exit 1
    ;;
esac

# ---------------------------------------------------------------------------
# Render the OS profile's iPXE script.
# ---------------------------------------------------------------------------

IPXE_SOURCE="${REPO_ROOT}/profiles/osProfiles/${OS_PROFILE}/ipxeScript"
if [ ! -f "${IPXE_SOURCE}" ]; then
  echo "OS profile '${OS_PROFILE}' has no ipxeScript at ${IPXE_SOURCE}" >&2
  exit 1
fi

IPXE_SCRIPT="$(sed "s|{{ \.DiskSelectionStrategy }}|${DISK_SELECTION_STRATEGY}|g" "${IPXE_SOURCE}")"
if printf '%s\n' "${IPXE_SCRIPT}" | grep -q '{{'; then
  echo "Unrendered template variables remain in the ${OS_PROFILE} ipxeScript:" >&2
  printf '%s\n' "${IPXE_SCRIPT}" | grep -n '{{' >&2
  exit 1
fi
IPXE_SCRIPT_INDENTED="$(printf '%s\n' "${IPXE_SCRIPT}" | sed 's/^/    /')"

# ---------------------------------------------------------------------------
# Minimal bare-boot userData.
#
# cloudInitInjectHostname is intentionally left unset on the NicoMachine: it
# would prepend a second hostname directive to this cloud-config.
#
# The login is emitted as a JSON string -- a valid YAML double-quoted scalar --
# rather than interpolated raw. A password containing ': ', ' #', a quote or a
# newline otherwise changes what the chpasswd entry means, and the failure is
# silent: readiness is a port-22 probe (forge-vms injects its own forge-diag
# user), so a mis-parsed cloud-config still reports Ready. jq reads the values
# from the environment so they never appear in a command line (`ps`).
# ---------------------------------------------------------------------------

VM_USER_YAML="$(jq -rn 'env.VM_USER | tojson')"
VM_LOGIN_YAML="$(jq -rn '(env.VM_USER + ":" + env.VM_PASSWORD) | tojson')"

USER_DATA="$(cat <<EOF
#cloud-config
hostname: ${INSTANCE_NAME}
fqdn: ${INSTANCE_NAME}.local
preserve_hostname: false
manage_etc_hosts: true
ssh_pwauth: true
package_update: false
package_upgrade: false

users:
  - name: ${VM_USER_YAML}
    gecos: capnico bare boot dev user
    groups: [adm, sudo]
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    lock_passwd: false

# Throwaway credential for a local-only VM on the forge-vms bridge; the VM is
# not reachable off-host and holds nothing of value.
chpasswd:
  expire: false
  list:
    - ${VM_LOGIN_YAML}

write_files:
  - path: /etc/capnico-bare-boot-ok
    permissions: "0644"
    content: |
      capnico bare boot marker

runcmd:
  - [sh, -c, "date -Is >> /etc/capnico-bare-boot-ok"]
  - [sh, -c, "echo 'capnico bare boot complete' > /dev/console"]

final_message: "capnico bare boot complete"
EOF
)"
USER_DATA_INDENTED="$(printf '%s\n' "${USER_DATA}" | sed 's/^/    /')"

# ---------------------------------------------------------------------------
# Idempotency: an existing NicoMachine is only re-created on explicit request.
# ---------------------------------------------------------------------------

if kubectl get nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" >/dev/null 2>&1; then
  if [ "${RECREATE}" = "true" ]; then
    echo "RECREATE=true -- deleting the existing ${INSTANCE_NAME} instance first" >&2
    # DELETE_CLUSTER=never explicitly: the NicoCluster and Cluster are re-applied
    # unchanged a few lines below, so tearing them down here is pure latency --
    # and it must not become destructive if the delete script's default changes.
    NAMESPACE="${NAMESPACE}" CLUSTER_NAME="${CLUSTER_NAME}" INSTANCE_NAME="${INSTANCE_NAME}" \
      BOOTSTRAP_SECRET_NAME="${BOOTSTRAP_SECRET_NAME}" DELETE_NAMESPACE=false DELETE_CLUSTER=never \
      "${REPO_ROOT}/hack/capnico-delete-instance.sh"
  else
    DRIFT=""
    [ "$(kubectl get nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.vpcID}')" = "${VPC_ID}" ] || DRIFT="${DRIFT} vpcID"
    [ "$(kubectl get nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.instanceTypeID}')" = "${INSTANCE_TYPE_ID}" ] || DRIFT="${DRIFT} instanceTypeID"
    [ "$(kubectl get nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.interfaces[0].subnetID}')" = "${SUBNET_ID}" ] || DRIFT="${DRIFT} subnetID"
    [ "$(kubectl get nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.ipxeScript}')" = "${IPXE_SCRIPT}" ] || DRIFT="${DRIFT} ipxeScript"

    if [ -n "${DRIFT}" ]; then
      echo "NicoMachine '${INSTANCE_NAME}' exists with a different spec (${DRIFT# })." >&2
      echo "Applying a new spec does not re-image a provisioned VM. Re-run with RECREATE=true to delete and re-create it." >&2
      exit 1
    fi

    if [ "$(kubectl get nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.ready}')" = "true" ]; then
      echo "NicoMachine '${INSTANCE_NAME}' already exists and is ready -- nothing to do."
      echo "(Only vpcID, instanceTypeID, subnetID and ipxeScript are compared. The bootstrap" >&2
      echo " Secret's userData -- VM_USER/VM_PASSWORD -- sshKeyGroupIDs and siteID are not:" >&2
      echo " changing those needs RECREATE=true.)" >&2
      exit 0
    fi
    echo "NicoMachine '${INSTANCE_NAME}' already exists and is not ready yet -- re-applying and waiting."
  fi
fi

# ---------------------------------------------------------------------------
# Apply the resources. Order matters: the Cluster/Machine reference objects that
# must already exist, and every apply is idempotent.
# ---------------------------------------------------------------------------

echo "Creating instance '${INSTANCE_NAME}' (cluster ${CLUSTER_NAME}, profile ${OS_PROFILE}) in namespace ${NAMESPACE}" >&2

# A namespace left over from a delete run may still be Terminating. `apply` is a
# no-op PATCH on it and succeeds, but the Secret apply below is then rejected by
# the namespace lifecycle admission plugin -- so wait it out first.
for _ in $(seq 1 60); do
  NS_PHASE="$(kubectl get namespace "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [ "${NS_PHASE}" = "Terminating" ] || break
  echo "Namespace ${NAMESPACE} is still Terminating -- waiting..." >&2
  sleep 2
done
if [ "${NS_PHASE}" = "Terminating" ]; then
  echo "Namespace ${NAMESPACE} is still Terminating after 120s -- giving up." >&2
  exit 1
fi

kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${BOOTSTRAP_SECRET_NAME}
  namespace: ${NAMESPACE}
type: cluster.x-k8s.io/secret
stringData:
  value: |
${USER_DATA_INDENTED}
EOF

kubectl apply -f - <<EOF
apiVersion: ${NICO_API_VERSION}
kind: NicoCluster
metadata:
  name: ${CLUSTER_NAME}
  namespace: ${NAMESPACE}
spec:
  siteID: "${SITE_ID}"
EOF

kubectl apply -f - <<EOF
apiVersion: cluster.x-k8s.io/${CAPI_API_VERSION}
kind: Cluster
metadata:
  name: ${CLUSTER_NAME}
  namespace: ${NAMESPACE}
spec:
  infrastructureRef:
${INFRA_REF_API_LINE}
    kind: NicoCluster
    name: ${CLUSTER_NAME}
  controlPlaneEndpoint:
    host: "0.0.0.0"
    port: 6443
EOF

kubectl apply -f - <<EOF
apiVersion: ${NICO_API_VERSION}
kind: NicoMachine
metadata:
  name: ${INSTANCE_NAME}
  namespace: ${NAMESPACE}
spec:
  vpcID: "${VPC_ID}"
  instanceTypeID: "${INSTANCE_TYPE_ID}"
  sshKeyGroupIDs:
    - "${SSH_KEY_GROUP_ID}"
  ipxeScript: |
${IPXE_SCRIPT_INDENTED}
  interfaces:
    - subnetID: "${SUBNET_ID}"
      physical: true
EOF

kubectl apply -f - <<EOF
apiVersion: cluster.x-k8s.io/${CAPI_API_VERSION}
kind: Machine
metadata:
  name: ${INSTANCE_NAME}
  namespace: ${NAMESPACE}
  labels:
    cluster.x-k8s.io/cluster-name: ${CLUSTER_NAME}
spec:
  clusterName: ${CLUSTER_NAME}
  infrastructureRef:
${INFRA_REF_API_LINE}
    kind: NicoMachine
    name: ${INSTANCE_NAME}
  bootstrap:
    dataSecretName: ${BOOTSTRAP_SECRET_NAME}
EOF

# ---------------------------------------------------------------------------
# Wait for the VM. The Machine stays READY=Unknown in the bare-VM flow (no node
# ever joins the cluster), so readiness is asserted by the NicoMachine.
# ---------------------------------------------------------------------------

echo "Waiting up to ${WAIT_TIMEOUT} for nicomachine/${INSTANCE_NAME} to become ready..." >&2
if ! kubectl wait --for=jsonpath='{.status.ready}'=true \
  "nicomachine/${INSTANCE_NAME}" -n "${NAMESPACE}" --timeout="${WAIT_TIMEOUT}"; then
  echo "--- nicomachine/${INSTANCE_NAME} status ---" >&2
  kubectl get nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status}' >&2 || true
  echo >&2
  echo "--- CAPNICo controller logs (last 100 lines) ---" >&2
  kubectl logs -n capnico-system deployment/controller-manager --tail=100 >&2 || true
  exit 1
fi

# NicoMachine readiness and the providerID patch are separate reconciles, so a
# single read races the controller. The first check is immediate -- when the
# providerID is already there (the common case) this costs nothing.
echo "Waiting up to ${PROVIDER_ID_TIMEOUT_SECONDS}s for machine/${INSTANCE_NAME} to report spec.providerID..." >&2
INSTANCE_ID=""
PROVIDER_ID_DEADLINE=$((SECONDS + PROVIDER_ID_TIMEOUT_SECONDS))
while :; do
  # `|| true`: a transient API error must not abort the loop under `set -e`.
  PROVIDER_ID="$(kubectl get machine "${INSTANCE_NAME}" -n "${NAMESPACE}" \
    -o jsonpath='{.spec.providerID}' 2>/dev/null || true)"
  INSTANCE_ID="${PROVIDER_ID#nico://}"
  if [ -n "${INSTANCE_ID}" ]; then
    break
  fi
  if [ "${SECONDS}" -ge "${PROVIDER_ID_DEADLINE}" ]; then
    break
  fi
  sleep "${PROVIDER_ID_POLL_INTERVAL_SECONDS}"
done

if [ -z "${INSTANCE_ID}" ]; then
  echo "nicomachine/${INSTANCE_NAME} is ready but Machine.spec.providerID was still empty after ${PROVIDER_ID_TIMEOUT_SECONDS}s." >&2
  echo "CAPNICo sets it in a separate reconcile -- check the CAPNICo controller logs." >&2
  # Status only. The Machine/NicoMachine specs reference the bootstrap Secret and
  # carry the ipxeScript, so nothing here dumps a whole object (same reason as the
  # deliberate jq projection at the end of this script).
  echo "--- machine/${INSTANCE_NAME} phase ---" >&2
  kubectl get machine "${INSTANCE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.phase}' >&2 || true
  echo >&2
  echo "--- machine/${INSTANCE_NAME} conditions ---" >&2
  kubectl get machine "${INSTANCE_NAME}" -n "${NAMESPACE}" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}) {.message}{"\n"}{end}' >&2 || true
  echo "--- nicomachine/${INSTANCE_NAME} conditions ---" >&2
  kubectl get nicomachine "${INSTANCE_NAME}" -n "${NAMESPACE}" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}) {.message}{"\n"}{end}' >&2 || true
  exit 1
fi

echo "Instance ${INSTANCE_NAME} is ready (forge-vms instance ${INSTANCE_ID})" >&2
# The fields are projected deliberately: the instance JSON carries `userData`
# verbatim, and that is the cloud-config holding the VM login password. Dumping
# the bare object would print the credential into the Tilt resource log (and any
# tilt snapshot or CI log). Do not replace this with a plain `| jq`.
curl -fsS -H "Authorization: Bearer ${FORGE_VMS_TOKEN}" \
  "${FORGE_VMS_URL}/v2/org/${ORG_ID}/forge/instance/${INSTANCE_ID}" |
  jq '{id, name, status, instanceTypeId, vpcId, siteId, interfaces}'
