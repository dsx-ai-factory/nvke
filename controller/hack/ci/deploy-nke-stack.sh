#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Deploy the management plane from the generated capd-e2e deploy path instead of
# Tilt.
#
# Runs INSIDE the e2e runner container, invoked by backend-e2e.sh when
# NKE_E2E_USE_STACK is true. Everything after this point in backend-e2e.sh -- the
# rollout checks, the CAPI controller probes, the Ginkgo legs -- is unchanged and
# unaware of which path deployed the stack.
#
# This script no longer applies anything. build/use-nke-sbom.sh walks
# generated/nke-sbom-e2e/capd-e2e/nke-deploy.yaml and installs its items in
# order: the namespaces and OIDC CA chain, nke-auth-dev, the TPM pubhashes
# ConfigMap, the two SPIRE NodePorts, the nke-spire-client probe scaffolding, the
# CAPD RBAC and reboot test double, and finally the nke-stack umbrella, one
# release per phase from the chart's own phases: block.
#
# What remains here is the values that vary per RUN and cannot live in an
# artifact: the per-architecture TPM attestor checksum, whether cert-manager is
# already owned by clusterctl init, the kind bridge gateway, the trust domain
# derived from the kind cluster name, the locally built controller image, and the
# generated database passwords.
#
# Still outside the deploy path, and deliberately so:
#   * clusterctl init -- four providers plus cert-manager, and it runs on the
#     Tilt path too
#   * hack/apply-capi-spiffe-exec-auth.sh -- it PATCHES the Deployments
#     clusterctl init created; a deploy path applies manifests, it does not patch
#     what another installer owns
#   * the BGP container and the image side-loads, both in backend-e2e.sh
#
# Layout inside the runner:
#   /workspace/cloud-workflow-engine   the CONTROLLER directory (tar --transform)
#   /workspace/nke-stack               build/ + generated/, copied in
set -euo pipefail

# CHART is optional. Given, it names a packaged chart to install; omitted,
# nke-stack is built from this working tree.
#
# Build is the default because subcharts are BAKED INTO the package: installing
# a published nke-stack would test the controller chart as it was
# when that artifact was published, so a change to
# controller/deploy/charts/controller/templates would run this suite
# green against the old templates. capd-e2e.yml triggers on controller/**, which
# includes exactly those files.
CWE_IMAGE="${1:?usage: $0 <cwe-image>}"
: "${NODE_AGENT_SPIRE_ATTESTATION_METHOD:?NODE_AGENT_SPIRE_ATTESTATION_METHOD is required}"
# Optional, from the workflow inputs of the same name. Set together to install a
# published artifact; leave empty to build.
CHART="${NKE_E2E_STACK_CHART:-}"
# A published chart's version has nowhere to go now: use-nke-sbom.sh resolves a
# chart from a path, so helm needs no --version, and the SBOM's own release.yaml
# carries the version the artifact was cut with. Refuse rather than accept a
# value and ignore it -- a silently dropped pin is how you end up testing a
# different chart than the one you named.
if [[ -n "${NKE_E2E_STACK_VERSION:-}" ]]; then
  echo "NKE_E2E_STACK_VERSION is set but no longer used: pass a packaged chart path or an OCI ref in NKE_E2E_STACK_CHART." >&2
  exit 1
fi

# Defaults are the paths inside the e2e runner. Overridable so the script can
# be exercised directly against a local kind cluster, without backend-e2e.sh --
# which is a Linux-host entrypoint and refuses to run elsewhere.
STACK_DIR="${STACK_DIR:-/workspace/nke-stack}"
# The generated deploy path this lane installs. The overlay is inside it, copied
# there by composition-gen, so it is no longer named separately here.
SBOM_DIR="${SBOM_DIR:-${STACK_DIR}/generated/nke-sbom-e2e}"
SBOM_TARGET="${SBOM_TARGET:-capd-e2e}"

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

# Everything this script used to apply by hand is now an item of the capd-e2e
# deploy path, applied by build/use-nke-sbom.sh in the order nke-deploy.yaml
# lists: the namespaces, the OIDC CA chain, nke-auth-dev, the TPM pubhashes
# ConfigMap, the two SPIRE NodePorts, the nke-spire-client probe scaffolding, and
# the two CAPD pieces -- the RBAC over dockerclusters/dockermachines without
# which every provisioning write is forbidden, and the reboot test double, since
# a CAPD node is a container and has no reboot of its own.
#
# The two ClusterSPIFFEIDs moved into the spire chart's own values, so they are
# rendered by the release that installs spire rather than applied between
# phases. That is what retired the PHASES_UNTIL two-pass and the sed that
# rewrote app.kubernetes.io/instance on the NodePort Services.
#
# What stays here is only what this RUN knows: the per-architecture TPM
# checksum, cert-manager ownership, the kind gateway address, the trust domain
# derived from the cluster name, the built image, and the generated passwords.

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
  log "built ${CHART}"
fi

# Which release owns spire's Service, derived rather than assumed.
#
# CWE dials spire-server by name, and that name is <release>-server because the
# upstream chart has no fullnameOverride. Under per-phase releases the owning
# release is <prefix>-<the phase spire is in>, so hardcoding "nke-stack" would
# dial a host that does not exist -- and the failure, "produced zero addresses"
# when CWE tries to mint a join token, names neither the release nor the
# Service. Both halves are read from the artifacts that decide them: the prefix
# and release model from release.yaml, the phase membership from the chart.
# The chart's own values, read once. Everything below that needs to know what
# the chart declares -- the phase list, the static OIDC domains -- reads this
# rather than restating it, so the composition stays the single source of truth.
CHART_VALUES="$(mktemp)"
trap 'rm -f "${CHART_VALUES}"' EXIT
helm show values "${CHART}" >"${CHART_VALUES}"

spire_release() {
  local prefix per_phase phase
  IFS=$'\t' read -r _ _ prefix _ per_phase _ \
    < <(python3 "${STACK_DIR}/build/nke-sbom-items.py" release \
          "${SBOM_DIR}/${SBOM_TARGET}/nke-stack/release.yaml")
  if [[ "${per_phase}" != "true" ]]; then
    printf '%s' "${prefix}"
    return
  fi
  phase="$(python3 "${STACK_DIR}/build/nke-sbom-items.py" phases "${CHART_VALUES}" \
    | awk -F'\t' '$2 ~ /(^|,)spire(,|$)/ { print $1; exit }')"
  [[ -n "${phase}" ]] || { echo "no phase in ${CHART} contains spire" >&2; exit 1; }
  printf '%s-%s' "${prefix}" "${phase}"
}
STACK_RELEASE="$(spire_release)"
log "spire is installed by release ${STACK_RELEASE}"

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
# parsing list: NULL".
#
# So the list has to be complete, which used to mean re-typing the chart's
# static entries here -- and then the artifact said three domains while the
# deployment had five. They are read out of the DEPLOY PATH's own values
# instead, so the tree describes the deployment it produces and editing the
# overlay reaches this.
#
# Required, not optional: if the overlay stops declaring them this silently
# drops three hostnames and the provider starts rejecting tenants by Host
# header, which surfaces far from here.
OIDC_OVERLAY="${SBOM_DIR}/${SBOM_TARGET}/nke-stack/values.yaml"
if ! grep -q 'additionalDomains' "${OIDC_OVERLAY}"; then
  echo "the ${SBOM_TARGET} overlay declares no additionalDomains." >&2
  echo "This script appends MANAGEMENT_VIP and the kind gateway to that list; without it" >&2
  echo "the static hostnames would be dropped, because --set-json replaces the whole list." >&2
  exit 1
fi
OIDC_DOMAINS="$(VALUES_FILE="${OIDC_OVERLAY}" MANAGEMENT_VIP="${MANAGEMENT_VIP}" KIND_GATEWAY_IP="${KIND_GATEWAY_IP}" python3 -c '
import json, os, re, sys
raw = open(os.environ["VALUES_FILE"]).read()
blocks = re.findall(r"^[ \t]*additionalDomains:[ \t]*\n((?:[ \t]*-[ \t]*\S.*\n)+)", raw, re.M)
if len(blocks) != 1:
    sys.exit("expected exactly one additionalDomains block in the overlay, found %d" % len(blocks))
domains = [re.sub(r"^[ \t]*-[ \t]*", "", line).strip().strip("\"'"'"'") for line in blocks[0].splitlines() if line.strip()]
for extra in (os.environ["MANAGEMENT_VIP"], os.environ.get("KIND_GATEWAY_IP", "")):
    if extra and extra not in domains:
        domains.append(extra)
print(json.dumps(domains))
')"
OIDC_IP_SANS="$(MANAGEMENT_VIP="${MANAGEMENT_VIP}" python3 -c '
import json, os
print(json.dumps([os.environ["MANAGEMENT_VIP"]]))
')"

# One pass, and one release model. The deploy path decides what is installed and
# in what order; everything below is a value only this run knows.
#
# The overlay is NOT passed here any more: the executor supplies it from
# <target>/nke-stack/values.yaml, which is the same file the generator copied in.
# It lands before this passthrough, so these still win.
log "installing ${SBOM_TARGET} from ${SBOM_DIR} with chart ${CHART}"
bash "${STACK_DIR}/build/use-nke-sbom.sh" install \
  --sbom "${SBOM_DIR}" \
  --target "${SBOM_TARGET}" \
  --chart "${CHART}" \
  -- \
      --set-json "controller.env=${CWE_ENV_JSON}" \
      --set-string "controller.nkeOIDCAuth.discoveryURL=https://${MANAGEMENT_VIP}:30443/.well-known/openid-configuration" \
      --set-string "controller.spiffe.trustDomain=${TRUST_DOMAIN}" \
      --set-string "controller.spiffe.spire.serverAddress=${MANAGEMENT_VIP}" \
      --set-string "controller.nodeAgent.spireAttestationMethod=${NODE_AGENT_SPIRE_ATTESTATION_METHOD}" \
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
