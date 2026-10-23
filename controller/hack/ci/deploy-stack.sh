#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Deploy the management plane from the capd-e2e deploy path instead of Tilt.
#
# Runs INSIDE the e2e runner container, invoked by backend-e2e.sh when
# NKE_E2E_USE_STACK is true. Everything after this point in backend-e2e.sh -- the
# rollout checks, the CAPI controller probes, the Ginkgo legs -- is unchanged and
# unaware of which path deployed the stack.
#
# This script no longer applies anything. deploy/nvke-deploy.sh walks
# controller/deploy/capd-e2e/nvke-deploy.yaml and installs its items in
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
#   /workspace/nke-stack               deploy/ and controller/deploy/, copied in
set -euo pipefail

#
# Build is the default because subcharts are BAKED INTO the package: installing
# a published nke-stack would test the controller chart as it was
# when that artifact was published, so a change to
# controller/deploy/charts/controller/templates would run this suite
# green against the old templates. capd-e2e.yml triggers on controller/**, which
# includes exactly those files.
NVKE_CONTROLLER_IMAGE="${1:?usage: $0 <controller-image>}"
: "${NODE_AGENT_SPIRE_ATTESTATION_METHOD:?NODE_AGENT_SPIRE_ATTESTATION_METHOD is required}"
# Optional, from the workflow inputs of the same name. Set together to install a
# published artifact; leave empty to build.
if [[ -n "${NKE_E2E_STACK_CHART:-}" ]]; then
  echo "NKE_E2E_STACK_CHART is set but no longer used: there is no umbrella chart." >&2
  echo "Each component is its own release, named in controller/deploy/${DEPLOY_TARGET:-capd-e2e}." >&2
  exit 1
fi
# A published chart's version has nowhere to go now: nvke-deploy.sh resolves
# a path chart from the checkout and a release's chart by digest; neither
# takes a --version. Refuse rather than accept a value and ignore it -- a
# silently dropped pin is how you end up testing a different chart than the
# one you named.
if [[ -n "${NKE_E2E_STACK_VERSION:-}" ]]; then
  echo "NKE_E2E_STACK_VERSION is set but no longer used: component versions come from the checkout or the release, not from this variable." >&2
  exit 1
fi

# Defaults are the paths inside the e2e runner. Overridable so the script can
# be exercised directly against a local kind cluster, without backend-e2e.sh --
# which is a Linux-host entrypoint and refuses to run elsewhere.
STACK_DIR="${STACK_DIR:-/workspace/nke-stack}"
# The generated deploy path this lane installs. The overlay is inside it, copied
# beside each item, so it is no longer named separately here.
DEPLOY_PATHS_DIR="${DEPLOY_PATHS_DIR:-${STACK_DIR}/controller/deploy}"
DEPLOY_TARGET="${DEPLOY_TARGET:-capd-e2e}"

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

# cert-manager is not installed here at all. clusterctl init brings it up before
# this script runs, and the deploy path has no cert-manager item -- so there is
# nothing to detect and nothing to switch off. That detection existed only
# because the umbrella bundled cert-manager as a subchart.

# The chart pins image.repository/tag to a published controller image. The e2e
# builds the controller at HEAD and side-loads it into kind, so both are
# repointed at that local tag. This only works because the controller chart is
# a subchart with live values -- a pre-rendered bundle would have baked the
# image string in and ignored the override.
cwe_repository="${NVKE_CONTROLLER_IMAGE%:*}"
cwe_tag="${NVKE_CONTROLLER_IMAGE##*:}"

# Everything this script used to apply by hand is now an item of the capd-e2e
# deploy path, applied by deploy/nvke-deploy.sh in the order nvke-deploy.yaml
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

# Values that genuinely vary per run. MANAGEMENT_VIP and KIND_CLUSTER_NAME come
# from backend-e2e.sh, which computes them; the trust domain follows the cluster
# name exactly as SPIRE_TRUST_DOMAIN does in the Tiltfile, so the identities
# SPIRE issues match the adminID the controller presents.
: "${MANAGEMENT_VIP:?MANAGEMENT_VIP is required}"
: "${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME is required}"
TRUST_DOMAIN="${KIND_CLUSTER_NAME}.nvke.local"

# The env list the controller container needs, assembled the way
# controller/Tiltfile assembles _cwe_chart_extra_env. Three sources:
#   * the five credentials the Tiltfile treats as required
#   * NODE_REBOOT_SOFT_REBOOT_ENABLED, which backend-e2e.yml does set
#   * the remaining optional names (LOG_LEVEL, NODE_PROVISIONING_TIMEOUT, ...) which nothing on
#     this path sets, so they are absent rather than empty
#
# Passed as JSON for the same reason Tilt uses --set-json: a list of
# name/value objects does not survive --set's index syntax legibly.
controller_env_json() {
  SPIRE_SERVER_API_ADDRESS="spire-server.spire-server.svc.cluster.local:443" \
  MANAGEMENT_VIP="${MANAGEMENT_VIP}" python3 -c '
import json, os
# SPIRE_SERVER_ADDRESS is deliberately absent: the chart renders it from
# spiffe.spire.serverAddress, which is passed separately. Listing it here too
# produces two entries of the same name, and server-side apply rejects that
# ("duplicate entries for key") rather than taking the last one.
#
# SPIRE_SERVER_API_ADDRESS is the opposite case: the chart never renders it at
# all, so without an entry here the controller falls back to its Go default,
# spire-server.spire-server.svc.cluster.local:8081. That default is wrong on
# both paths -- the service listens on 443, not 8081 -- which is why
# .env.tilt sets it explicitly for the Tilt path. It is also the wrong NAME
# here: the spire chart derives the Service from the release, so Tilt gets
# spire-server while under this umbrella it is <release>-server. Left unset,
# the controller dials a host that does not exist and every join token fails with
# "produced zero addresses", which is what stalled tenant provisioning.
env = [{"name": "SPIRE_SERVER_API_ADDRESS",
        "value": os.environ["SPIRE_SERVER_API_ADDRESS"]}]
# backend-e2e.yml sets this and only Tilt was forwarding it. Left out, the controller takes
# its "true" default and reboots a container node in-guest, which never restarts.
soft_reboot = os.environ.get("NODE_REBOOT_SOFT_REBOOT_ENABLED", "")
if soft_reboot:
    env.append({"name": "NODE_REBOOT_SOFT_REBOOT_ENABLED", "value": soft_reboot})
for name in ("DGXC_TENANT_USER", "DGXC_TENANT_PASSWORD", "CNPG_SUPERUSER_PASSWORD",
             "CNPG_CLOUD_PASSWORD", "CNPG_TEMPORAL_PASSWORD"):
    value = os.environ.get(name, "")
    if value:
        env.append({"name": name, "value": value})
print(json.dumps(env))
'
}
NVKE_CONTROLLER_ENV_JSON="$(controller_env_json)"

# The SPIFFE identity the controller presents: its ServiceAccount, nvke-controller
# in every deploy path, under the nvke-service ClusterSPIFFEID in the deploy
# path's spire values.
SPIRE_CONTROLLER_ADMIN_PATH="/nvke-service/nvke-controller"

# The kind bridge gateway, alongside the VIP. Tenants' kube-apiservers fetch the
# OIDC discovery URL by IP, and the provider rejects any request whose Host
# header is not on its accepted list, so both addresses have to be there.
KIND_GATEWAY_IP="$(docker network inspect kind \
  -f '{{range .IPAM.Config}}{{println .Gateway}}{{end}}' 2>/dev/null \
  | grep -Eo '^[0-9]+(\.[0-9]+){3}$' | head -1 || true)"

# A values file replaces this key outright, so the list it carries has to be
# complete. Re-typing the static half here is what made the artifact say three
# domains while the deployment had five, so they are read out of the deploy
# path's own spire values instead and only the two per-run addresses are added.
#
# Required, not optional: if the overlay stops declaring them this silently
# drops three hostnames and the provider starts rejecting tenants by Host
# header, which surfaces far from here.
OIDC_OVERLAY="${DEPLOY_PATHS_DIR}/${DEPLOY_TARGET}/spire/overlay.yaml"
if ! grep -q 'additionalDomains' "${OIDC_OVERLAY}"; then
  echo "the ${DEPLOY_TARGET} overlay declares no additionalDomains." >&2
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
# Per-run values, one file per release.
#
# These were --set flags when one umbrella release held every component, so each
# key carried its subchart as a prefix. With one release per chart the prefixes
# are gone and a value has to be aimed at a single item: controller.spiffe.X
# becomes spiffe.X on the controller release and reaches nothing else.
VALUES_DIR="$(mktemp -d)"
trap 'rm -rf "${VALUES_DIR}"' EXIT

cat >"${VALUES_DIR}/controller.yaml" <<EOF
image:
  repository: "${cwe_repository}"
  tag: "${cwe_tag}"
appDb:
  postgresPassword: "${CNPG_SUPERUSER_PASSWORD:-example-password}"
nvkeOIDCAuth:
  discoveryURL: "https://${MANAGEMENT_VIP}:30443/.well-known/openid-configuration"
spiffe:
  trustDomain: "${TRUST_DOMAIN}"
  spire:
    serverAddress: "${MANAGEMENT_VIP}"
nodeAgent:
  spireAttestationMethod: "${NODE_AGENT_SPIRE_ATTESTATION_METHOD}"
env: ${NVKE_CONTROLLER_ENV_JSON}
EOF

cat >"${VALUES_DIR}/spire.yaml" <<EOF
# global reaches spire's own subcharts -- spire-server, spire-agent, the OIDC
# provider -- exactly as it did through the umbrella, because spire is itself a
# chart of subcharts.
global:
  spire:
    trustDomain: "${TRUST_DOMAIN}"
    clusterName: "${KIND_CLUSTER_NAME}"
spire-server:
  adminIDs:
    - "spiffe://${TRUST_DOMAIN}${SPIRE_CONTROLLER_ADMIN_PATH}"
  nodeAttestor:
    tpmDirect:
      checksum: "${TPM_CHECKSUM}"
spiffe-oidc-discovery-provider:
  config:
    jwksUri: "https://${MANAGEMENT_VIP}:30443/keys"
    additionalDomains: ${OIDC_DOMAINS}
  tls:
    certManager:
      certificate:
        ipAddresses: ${OIDC_IP_SANS}
EOF

cat >"${VALUES_DIR}/postgresql.yaml" <<EOF
auth:
  password: "${CNPG_SUPERUSER_PASSWORD:-example-password}"
EOF

cat >"${VALUES_DIR}/temporal.yaml" <<EOF
server:
  config:
    persistence:
      default:
        sql:
          password: "${CNPG_SUPERUSER_PASSWORD:-example-password}"
      visibility:
        sql:
          password: "${CNPG_SUPERUSER_PASSWORD:-example-password}"
EOF

EXTRA=()

# capnico-e2e renders the Secret CAPNICo reads. CAPD needs none of this:
# clusterctl init brings that provider, and the controller reaches it with no credentials.
#
# The provider itself is not an item -- install-capnico.sh installs it before
# this runs -- so nothing here names a chart for it.
if [[ "${DEPLOY_TARGET}" == capnico-e2e ]]; then
  # Where CAPNICo reaches the NICo API. Empty NICO_ENDPOINT means the simulator,
  # matching controller/hack/nico-provider/Tiltfile: the host defaults to the
  # kind host address and the port is the simulator's. The other three are the
  # simulator's own constants -- nothing here is secret, which is why this
  # target renders the same Secret shape a real site fills with NICo
  # credentials.
  nico_endpoint="${NICO_ENDPOINT:-http://${NICO_SIM_ENDPOINT_HOST:-${KIND_GATEWAY_IP}}:18080}"
  : "${nico_endpoint:?NICO_ENDPOINT or NICO_SIM_ENDPOINT_HOST is required: the kind gateway could not be read}"

  # The BootstrapConfig the template item renders against. JSON because
  # deploy/lib/render-template.py takes JSON -- PyYAML is not guaranteed here.
  BOOTSTRAP_CONFIG="${VALUES_DIR}/bootstrap-config.json"
  NICO_ENDPOINT="${nico_endpoint}" \
  NICO_ORG_ID="${NICO_ORG_ID:-novxwn552ssc}" \
  NICO_TOKEN="${NICO_TOKEN:-nico-simctl-token}" \
  NICO_API_NAME="${NICO_API_NAME:-forge}" python3 -c '
import json, os
print(json.dumps({"Nico": {
    "Endpoint": os.environ["NICO_ENDPOINT"],
    "OrgID":    os.environ["NICO_ORG_ID"],
    "Token":    os.environ["NICO_TOKEN"],
    "APIName":  os.environ["NICO_API_NAME"],
}}, indent=2))
' >"${BOOTSTRAP_CONFIG}"

  EXTRA+=(--config "${BOOTSTRAP_CONFIG}")
fi

# Applies the releases the controller resolves every OS image from. The controller chart
# installs their CRDs, so this waits for the CRDs before applying.
# NKE_CONTROLLER_RELEASES_DIR is a deprecated alias for NVKE_CONTROLLER_RELEASES_DIR; #1399 removes it.
NVKE_CONTROLLER_RELEASES_DIR="${NVKE_CONTROLLER_RELEASES_DIR:-${NKE_CONTROLLER_RELEASES_DIR:-/workspace/cloud-workflow-engine/test/fixtures/releases}}"
[[ -d "${NVKE_CONTROLLER_RELEASES_DIR}" ]] || { echo "releases not found at ${NVKE_CONTROLLER_RELEASES_DIR}; set NVKE_CONTROLLER_RELEASES_DIR" >&2; exit 1; }

apply_releases() {
  local crd
  for crd in machineimages clusterreleases nodepoolreleases; do
    local name="${crd}.internal.nvke.nvidia.com" _
    for _ in $(seq 1 450); do
      kubectl get crd "${name}" >/dev/null 2>&1 && break
      sleep 2
    done
    kubectl wait --for=condition=Established --timeout=120s "crd/${name}"
  done
  kubectl apply --server-side --field-manager=deploy-stack-releases -f "${NVKE_CONTROLLER_RELEASES_DIR}"
}

log "applying the releases from ${NVKE_CONTROLLER_RELEASES_DIR} once the CRDs exist"
apply_releases &
RELEASES_PID=$!
trap 'rm -rf "${VALUES_DIR}"; kill "${RELEASES_PID}" 2>/dev/null || true' EXIT

log "installing ${DEPLOY_TARGET} from ${DEPLOY_PATHS_DIR}"
bash "${STACK_DIR}/deploy/nvke-deploy.sh" install \
  --deploy-path "${DEPLOY_PATHS_DIR}/${DEPLOY_TARGET}" \
  --values-for "controller=${VALUES_DIR}/controller.yaml" \
  --values-for "spire=${VALUES_DIR}/spire.yaml" \
  --values-for "postgresql=${VALUES_DIR}/postgresql.yaml" \
  --values-for "temporal=${VALUES_DIR}/temporal.yaml" \
  ${EXTRA[@]+"${EXTRA[@]}"}

# The install returned, so the CRDs existed. Surface a failure to apply.
wait "${RELEASES_PID}"
