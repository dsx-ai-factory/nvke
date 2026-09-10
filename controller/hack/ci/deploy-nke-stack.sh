#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Deploy the management plane from the nke-stack umbrella chart instead of Tilt.
#
# Runs INSIDE the e2e runner container, invoked by backend-e2e.sh when
# NKE_E2E_STACK_CHART is set. Everything after this point in backend-e2e.sh --
# the rollout checks, the CAPI controller probes, the Ginkgo legs -- is unchanged
# and unaware of which path deployed the stack.
#
# What Tilt does that this reproduces:
#   * the six helm charts, now one release (nke-stack)
#   * deploy/k8s/*.yaml, which includes the OIDC CA chain the chart declares as
#     a prerequisite but deliberately does not own -- the CA is long-lived and
#     re-issuing it invalidates every tenant's embedded trust anchor
#
# The pieces Tilt's provider modules deploy are handled explicitly: the CAPD RBAC
# for cloud-workflow-engine and capd-reboot-controller below, and the BGP
# container in backend-e2e.sh. capi-core-provider deploys nothing of its own.
#
# What it does NOT reproduce, and why the Tilt path stays the default:
#   * capd-reboot-controller, the BGP container wiring, the profiles ConfigMap
#     renderer, and the CWE env overrides Tilt assembles per provider
#   * live_update; this deploys a fixed image and does not watch
#
# Layout inside the runner:
#   /workspace/cloud-workflow-engine   the CONTROLLER directory (tar --transform)
#   /workspace/nke-stack               build/ + generated/nke-stack, copied in
set -euo pipefail

# CHART and VERSION are optional. Given, they name a published artifact to
# install; omitted, nke-stack is built from this working tree.
#
# Build is the default because subcharts are BAKED INTO the package: installing
# a published nke-stack would test the controller chart as it was
# when that artifact was published, so a change to
# controller/deploy/charts/controller/templates would run this suite
# green against the old templates. capd-e2e.yml triggers on controller/**, which
# includes exactly those files.
CWE_IMAGE="${1:?usage: $0 <cwe-image>}"
# Optional, from the workflow inputs of the same name. Set together to install a
# published artifact; leave empty to build.
CHART="${NKE_E2E_STACK_CHART:-}"
# The version of a PUBLISHED chart to install. Deliberately not named VERSION:
# that is the build version backend-e2e.sh passes in for build-local, and
# assigning an empty NKE_E2E_STACK_VERSION over it wiped it.
CHART_VERSION="${NKE_E2E_STACK_VERSION:-}"

# Defaults are the paths inside the e2e runner. Overridable so the script can
# be exercised directly against a local kind cluster, without backend-e2e.sh --
# which is a Linux-host entrypoint and refuses to run elsewhere.
CONTROLLER_DIR="${CONTROLLER_DIR:-/workspace/cloud-workflow-engine}"
# Static application configuration for this suite, mirroring the --set flags
# controller/Tiltfile assembles. Kept as a values file rather than a block of
# --set flags here so it stays reviewable and does not drift silently.
E2E_OVERLAY="${E2E_OVERLAY:-${CONTROLLER_DIR}/deploy/composition/overlays/capd-e2e.yaml}"
STACK_DIR="${STACK_DIR:-/workspace/nke-stack}"
VALUES_OUT="${VALUES_OUT:-/state/nke-stack-cwe-values.yaml}"

log() { printf '\n=== %s\n' "$*" >&2; }

# SPIRE's TPM node attestor verifies the plugin binary against a checksum, and
# refuses to start when it does not match. controller/Tiltfile computes this per
# architecture; on this path nothing does, so it is resolved here from the same
# table. Getting it wrong is not subtle -- spire-server crash-loops with
# "failed to load plugin \"tpm\": checksums did not match" and every component
# that needs an SVID stalls behind it.
#
# Keep these in step with _spire_tpm_attestor_server_checksum() in the Tiltfile.
resolve_tpm_checksum() {
  if [[ -n "${SPIRE_TPM_ATTESTOR_SERVER_CHECKSUM:-}" ]]; then
    printf '%s' "${SPIRE_TPM_ATTESTOR_SERVER_CHECKSUM}"
    return
  fi
  case "$(uname -m)" in
    x86_64)          printf '46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc' ;;
    aarch64 | arm64) printf 'b69d0153c8241383f24da138bc85429a620d9f962a2f10abb314aa0e39046d16' ;;
    *) echo "no TPM attestor checksum for $(uname -m); set SPIRE_TPM_ATTESTOR_SERVER_CHECKSUM" >&2; exit 1 ;;
  esac
}
TPM_CHECKSUM="$(resolve_tpm_checksum)"

# cert-manager may already be present: clusterctl init installs it as a CAPI
# prerequisite, before this script runs. Helm refuses to adopt CRDs it does not
# own, so installing ours on top fails the datastore phase outright with
# "CustomResourceDefinition ... exists and cannot be imported".
#
# controller/Tiltfile solves this with detect_cert_manager(); the same check
# applies here. Presence is tested via the CRD, which is namespace- and
# installer-agnostic; ownership via `helm status`, which is what decides whether
# our release could manage it.
cert_manager_enabled() {
  local owner
  if ! kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
    echo true                       # absent: this composition installs it
    return
  fi
  # Ownership by release name, not `helm status cert-manager`: here cert-manager
  # is a SUBCHART of nke-stack, so no release of its own exists and that check
  # would always say "someone else owns it".
  owner="$(kubectl get crd certificates.cert-manager.io \
    -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)"
  if [[ "${owner}" == "nke-stack" ]]; then
    echo true                       # ours already; an upgrade is fine
  else
    echo false                      # someone else owns it; leave it alone
  fi
}
CERT_MANAGER_ENABLED="$(cert_manager_enabled)"
log "cert-manager.enabled=${CERT_MANAGER_ENABLED}"

# The chart pins image.repository/tag to a published controller image. The e2e
# builds the controller at HEAD and side-loads it into kind, so both are
# repointed at that local tag. This only works because the controller chart is
# a subchart with live values -- a pre-rendered bundle would have baked the
# image string in and ignored the override.
cwe_repository="${CWE_IMAGE%:*}"
cwe_tag="${CWE_IMAGE##*:}"

# The OIDC CA chain, SPIRE nodeports, the TPM pubhashes ConfigMap and the
# nke-system namespace. Applied before the install because the identity phase's
# requires: gate blocks on the CA certificate being Ready.
# Namespaces first: the manifests below target spire-server, which does not
# exist on a clean cluster. Earlier runs only passed because a previous deploy
# had left the namespaces behind.
CHART="${CHART}" bash "${STACK_DIR}/build/nke-stack.sh" namespaces

# capd-reboot-controller: a CAPD test double, deliberately outside the nke-stack
# chart. CAPD "nodes" are containers, so a machine reboot has no analogue and
# this restarts the container when the reboot label appears. The reboot spec
# inherits the `capd` label from its Entry and so runs in this lane; without the
# controller nothing transitions the node and the spec sits until its 45-minute
# timeout. backend-e2e.sh builds and side-loads the image.
# CWE's ServiceAccount needs a ClusterRole over CAPD's own kinds --
# dockerclusters and dockermachines -- to provision a tenant. The capd-provider
# Tilt module applies this; without it CWE accepts the create request, the
# cluster appears in the list, and then ApplyCAPIClusterResources fails with
# "dockerclusters ... is forbidden" until the spec times out ten minutes later.
#
# Like the reboot controller this is CAPD test scaffolding, not management-plane
# software, so it stays out of the nke-stack chart. The chart's own capi.rbac
# covers the provider-independent CAPI kinds; this is the CAPD-specific half.
apply_capd_cwe_rbac() {
  log "applying CAPD RBAC for cloud-workflow-engine"
  kubectl apply -f "${CONTROLLER_DIR}/hack/capd-provider/cwe-rbac.yaml"
}

apply_capd_reboot_controller() {
  log "applying capd-reboot-controller"
  kubectl apply -f "${CONTROLLER_DIR}/hack/capd-reboot-controller/deploy/k8s/capd-reboot-controller.yaml"
  # The ConfigMap is generated rather than checked in, matching the provider
  # module, so the sleep duration stays a knob.
  kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: capd-reboot-controller-config
  namespace: capd-reboot-system
data:
  CAPD_REBOOT_CONTROLLER_SLEEP_DURATION: "${CAPD_REBOOT_SLEEP_DURATION:-10s}"
EOF
}

log "applying cluster prerequisites from deploy/k8s"
# The CRD-free manifests can go first. oidc-upstream-ca.yaml has to, because
# the identity phase's requires: gate blocks on its certificate.
for m in oidc-upstream-ca nke-system-namespace nke-auth-dev; do
  kubectl apply -f "${CONTROLLER_DIR}/deploy/k8s/${m}.yaml"
done

# hack/tilt-cwe-chart-values.yaml is written for the controller chart installed
# standalone, so its keys sit at the top level. Under the umbrella they have to
# be nested one level down. Indent rather than parse: python3 is present but
# PyYAML is not guaranteed, and this file is a single plain document.
apply_capd_reboot_controller
apply_capd_cwe_rbac

log "nesting the CWE values under the subchart key"
{
  printf 'controller:\n'
  sed -e 's/^/  /' -e 's/^  $//' "${CONTROLLER_DIR}/hack/tilt-cwe-chart-values.yaml"
} >"${VALUES_OUT}"

# ClusterSPIFFEID and the spire Services need spire's CRDs and namespace, which
# the datastore/identity phases create -- so these are applied after the install
# rather than before it. The controller blocks on "no identity issued"
# until nke-service exists, which is why the controller phase can need a second
# convergence pass.
# The two NodePort Services select pods by app.kubernetes.io/instance, which
# Helm sets to the RELEASE NAME. Tilt installs spire as its own release named
# "spire", so the manifests hardcode that; under the umbrella every subchart
# inherits the release, and the pods carry instance=nke-stack instead. Applied
# unpatched the selectors match nothing, the Services get zero endpoints, and
# CWE fails to mint a join token with "produced zero addresses" -- the same
# release-derived-identity trap postgresql and temporal needed fullnameOverride
# for. The manifests are shared with the Tilt path, so patch the stream rather
# than the file.
STACK_RELEASE="${STACK_RELEASE:-nke-stack}" # must match build/nke-stack.sh
RELEASE_SCOPED_NODEPORTS=(spire-server-nodeport spire-oidc-provider-nodeport)

apply_spire_dependents() {
  log "applying spire-dependent manifests"
  local m
  for m in spire-server-nodeport spire-oidc-provider-nodeport \
           spire-nke-services-spiffe-id spire-tpm-pubhashes-configmap \
           nke-spire-client; do
    local src="${CONTROLLER_DIR}/deploy/k8s/${m}.yaml"
    case " ${RELEASE_SCOPED_NODEPORTS[*]} " in
      *" ${m} "*)
        sed "s|app.kubernetes.io/instance: spire$|app.kubernetes.io/instance: ${STACK_RELEASE}|" \
          "${src}" | kubectl apply -f -
        ;;
      *)
        kubectl apply -f "${src}"
        ;;
    esac
  done
}

if [[ -z "${CHART}" ]]; then
  log "building nke-stack from the working tree"
  # Not piped: `| tail -1` would mask a build failure behind tail's exit status.
  build_log="$(mktemp)"
  if ! bash "${STACK_DIR}/build/nke-stack.sh" build-local >"${build_log}"; then
    cat "${build_log}" >&2
    exit 1
  fi
  CHART="$(tail -1 "${build_log}")"
  rm -f "${build_log}"
  # A packaged chart is a path, so helm needs no --version.
  CHART_VERSION=""
  log "built ${CHART}"
fi

log "installing nke-stack from ${CHART} (through identity)"
# Two passes, mirroring the order controller/Tiltfile gets from resource_deps:
# spire's CRDs and namespace must exist before ClusterSPIFFEID can be applied,
# and the controller blocks on "no identity issued" until it is. Phases
# are cumulative, so the second pass just carries on.
# Values that genuinely vary per run. MANAGEMENT_VIP and KIND_CLUSTER_NAME come
# from backend-e2e.sh, which computes them; the trust domain follows the cluster
# name exactly as SPIRE_TRUST_DOMAIN does in the Tiltfile, so the identities
# SPIRE issues match the adminID cloud-workflow-engine presents.
: "${MANAGEMENT_VIP:?MANAGEMENT_VIP is required}"
: "${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME is required}"
TRUST_DOMAIN="${KIND_CLUSTER_NAME}.nke.local"

# The env list the controller container needs, assembled the way
# controller/Tiltfile assembles _cwe_chart_extra_env. Four sources:
#   * PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER, set per leg -- the
#     single-runner mode runs cp-controller on, then flips it off
#   * the five credentials the Tiltfile treats as required
#   * the twelve optional names (LOG_LEVEL, FORGE_URL, ...) which nothing on
#     this path sets, so they are absent rather than empty
#
# Passed as JSON for the same reason Tilt uses --set-json: a list of
# name/value objects does not survive --set's index syntax legibly.
cwe_env_json() {
  SPIRE_SERVER_API_ADDRESS="${STACK_RELEASE}-server.spire-server.svc.cluster.local:443" \
  MANAGEMENT_VIP="${MANAGEMENT_VIP}" python3 -c '
import json, os
# SPIRE_SERVER_ADDRESS is deliberately absent: the chart renders it from
# spiffe.spire.serverAddress, which is passed separately. Listing it here too
# produces two entries of the same name, and server-side apply rejects that
# ("duplicate entries for key") rather than taking the last one.
#
# SPIRE_SERVER_API_ADDRESS is the opposite case: the chart never renders it at
# all, so without an entry here CWE falls back to its Go default,
# spire-server.spire-server.svc.cluster.local:8081. That default is wrong on
# both paths -- the service listens on 443, not 8081 -- which is why
# .env.tilt sets it explicitly for the Tilt path. It is also the wrong NAME
# here: the spire chart derives the Service from the release, so Tilt gets
# spire-server while under this umbrella it is <release>-server. Left unset,
# CWE dials a host that does not exist and every join token fails with
# "produced zero addresses", which is what stalled tenant provisioning.
env = [{"name": "SPIRE_SERVER_API_ADDRESS",
        "value": os.environ["SPIRE_SERVER_API_ADDRESS"]}]
flag = os.environ.get("PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER", "")
if flag:
    env.append({"name": "PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER", "value": flag})
for name in ("DGXC_TENANT_USER", "DGXC_TENANT_PASSWORD", "CNPG_SUPERUSER_PASSWORD",
             "CNPG_CLOUD_PASSWORD", "CNPG_TEMPORAL_PASSWORD"):
    value = os.environ.get(name, "")
    if value:
        env.append({"name": name, "value": value})
print(json.dumps(env))
'
}
CWE_ENV_JSON="$(cwe_env_json)"

# The SPIFFE identity cloud-workflow-engine presents. Unchanged by the chart
# rename -- the chart is now `controller`, the identity is not -- and it has to
# match the ClusterSPIFFEID in deploy/k8s/spire-nke-services-spiffe-id.yaml.
SPIRE_CWE_ADMIN_PATH="/nke-service/cloud-workflow-engine"

# The kind bridge gateway, alongside the VIP. Tenants' kube-apiservers fetch the
# OIDC discovery URL by IP, and the provider rejects any request whose Host
# header is not on its accepted list, so both addresses have to be there.
KIND_GATEWAY_IP="$(docker network inspect kind \
  -f '{{range .IPAM.Config}}{{println .Gateway}}{{end}}' 2>/dev/null \
  | grep -Eo '^[0-9]+(\.[0-9]+){3}$' | head -1 || true)"

# Whole lists, not indexed elements. `--set list[3]=x` does NOT append to the
# list the chart already has: helm builds a sparse array and merges it, which
# dropped two static entries and left a null in their place. The provider then
# refused to start with "unable to decode configuration: unexpected token while
# parsing list: NULL". The static entries are repeated here so the list is
# complete by construction.
#
# Keep the static half in step with components/spire.yaml, which is where a
# consumer that is not this suite gets them.
OIDC_DOMAINS="$(MANAGEMENT_VIP="${MANAGEMENT_VIP}" KIND_GATEWAY_IP="${KIND_GATEWAY_IP}" python3 -c '
import json, os
domains = ["nke.site-controller.internal", "localhost", "127.0.0.1", os.environ["MANAGEMENT_VIP"]]
gw = os.environ.get("KIND_GATEWAY_IP", "")
if gw and gw not in domains:
    domains.append(gw)
print(json.dumps(domains))
')"
OIDC_IP_SANS="$(MANAGEMENT_VIP="${MANAGEMENT_VIP}" python3 -c '
import json, os
print(json.dumps([os.environ["MANAGEMENT_VIP"]]))
')"

run_install() {
  CHART="${CHART}" VERSION="${CHART_VERSION}" \
    bash "${STACK_DIR}/build/nke-stack.sh" install \
      --values "${E2E_OVERLAY}" \
      --set-json "controller.env=${CWE_ENV_JSON}" \
      --values "${VALUES_OUT}" \
      --set-string "controller.nkeOIDCAuth.discoveryURL=https://${MANAGEMENT_VIP}:30443/.well-known/openid-configuration" \
      --set-string "controller.spiffe.trustDomain=${TRUST_DOMAIN}" \
      --set-string "controller.spiffe.spire.serverAddress=${MANAGEMENT_VIP}" \
      --set-string "global.spire.trustDomain=${TRUST_DOMAIN}" \
      --set-string "global.spire.clusterName=${KIND_CLUSTER_NAME}" \
      --set-string "spire.spiffe-oidc-discovery-provider.config.jwksUri=https://${MANAGEMENT_VIP}:30443/keys" \
      --set-json "spire.spiffe-oidc-discovery-provider.tls.certManager.certificate.ipAddresses=${OIDC_IP_SANS}" \
      --set-json "spire.spiffe-oidc-discovery-provider.config.additionalDomains=${OIDC_DOMAINS}" \
      --set-string "spire.spire-server.adminIDs[0]=spiffe://${TRUST_DOMAIN}${SPIRE_CWE_ADMIN_PATH}" \
      --set "cert-manager.enabled=${CERT_MANAGER_ENABLED}" \
      --set-string "controller.image.repository=${cwe_repository}" \
      --set-string "controller.image.tag=${cwe_tag}" \
      --set-string postgresql.auth.password="${CNPG_SUPERUSER_PASSWORD:-example-password}" \
      --set-string controller.appDb.postgresPassword="${CNPG_SUPERUSER_PASSWORD:-example-password}" \
      --set-string temporal.server.config.persistence.default.sql.password="${CNPG_SUPERUSER_PASSWORD:-example-password}" \
      --set-string temporal.server.config.persistence.visibility.sql.password="${CNPG_SUPERUSER_PASSWORD:-example-password}" \
      --set-string "spire.spire-server.nodeAttestor.tpmDirect.checksum=${TPM_CHECKSUM}"
}

PHASES_UNTIL=identity run_install
apply_spire_dependents
log "installing nke-stack (controller)"
run_install
