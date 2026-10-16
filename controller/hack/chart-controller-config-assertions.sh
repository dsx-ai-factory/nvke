#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Renders the chart under value sets that reach each conditional key of
# nke-controller-config, then runs TestRenderedChart over the renders.
# RENDERS_DIR keeps the renders; GO_TEST_FLAGS is passed to go test.

set -euo pipefail

controller="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${CHART:-"${controller}/deploy/charts/controller"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }
# Absolute, since the script later changes into the controller module.
CHART="$(cd "${CHART}" && pwd)"

# Values files live apart from the renders, every one of which the test loads.
values="$(mktemp -d)"
if [[ -n "${RENDERS_DIR:-}" ]]; then
  mkdir -p "${RENDERS_DIR}"
  # Absolute, since go test reads it after the cd into the controller module.
  renders="$(cd "${RENDERS_DIR}" && pwd)"
  trap 'rm -rf "${values}"' EXIT
else
  renders="$(mktemp -d)"
  trap 'rm -rf "${renders}" "${values}"' EXIT
fi

render() {
  local name="$1"
  shift
  helm template cwe "${CHART}" "$@" > "${renders}/${name}.yaml"
}

checksum="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

render defaults
render readiness-custom \
  --set nodeAgent.readiness.workers=4 \
  --set-string nodeAgent.readiness.timeout=10s \
  --set-string nodeAgent.readiness.interval=90s \
  --set nodeAgent.readiness.jitter=0

render legacy-calico \
  --set cni.allowLegacyCalicoClusterCreation=true

# Padded on purpose: the template must trim these.
render dsx-imds \
  --set spiffe.enabled=true \
  --set-string nodeAgent.spireAttestationMethod=dsxImds \
  --set-string 'nodeAgent.dsxImds.operatorTenant= operator-1 ' \
  --set-string 'nodeAgent.dsxImds.endpoint= https://metadata.example.test ' \
  --set-string "nodeAgent.dsxImds.pluginChecksum=${checksum}"

render join-token \
  --set spiffe.enabled=true \
  --set-string nodeAgent.spireAttestationMethod=joinToken

render tls-jwt \
  --set spiffe.enabled=true \
  --set nodeAgent.tlsJwt.enabled=true \
  --set-string nodeAgent.tlsJwt.issuer=https://issuer.example.test \
  --set-string nodeAgent.tlsJwt.jwksURI=https://issuer.example.test/keys \
  --set 'nodeAgent.tlsJwt.algorithms={RS256,ES256}' \
  --set-string nodeAgent.tlsJwt.subjectTrustDomain=example.test \
  --set-string 'nodeAgent.tlsJwt.dsxIdentityExchangeURL= https://exchange.example.test ' \
  --set-string 'nodeAgent.tlsJwt.subjectTenant= operator-1 ' \
  --set-string nodeAgent.tlsJwt.subjectService=nke-node-agent

render external-mtls \
  --set nodeAgent.externalMTLS.enabled=true \
  --set-string nodeAgent.externalMTLS.secret.name=node-agent-client-tls

render tracing-to-endpoint \
  --set tracing.enabled=true \
  --set-string tracing.endpoint=otel-collector:4317 \
  --set tracing.insecure=false \
  --set-string tracing.serviceName=cwe-test

# Neither sets the exporter variables, so both run on Go defaults.
render tracing-without-endpoint --set tracing.enabled=true
render tracing-without-service-name \
  --set tracing.enabled=true \
  --set-string tracing.endpoint=otel-collector:4317 \
  --set-string tracing.serviceName=

render temporal-mtls \
  --set temporal.mtls.enabled=true \
  --set-string temporal.mtls.secretName=temporal-client \
  --set-string temporal.mtls.serverName=temporal.example.test

render teleport \
  --set teleport.enabled=true \
  --set-string teleport.proxyAddress=teleport.example.test:443 \
  --set-string teleport.environment=prod

render profiles-image \
  --set profiles.image.enabled=true \
  --set-string profiles.image.tag=test \
  --set-string profiles.path=/opt/nke/profiles

render infra-providers --set-string 'supportedInfraProviders=nico\, infra'

# Padded on purpose: the template must trim these.
render node-label-allowlist \
  --set 'nodeMetadata.allowlistedLabelKeys={ node.kubernetes.io/custom ,k8s.io/custom}'

render machine-metadata-allowlists \
  --set 'machineMetadata.allowlistedLabelKeys={cluster.x-k8s.io/custom}' \
  --set 'machineMetadata.allowlistedAnnotationKeys={nke.nvidia.com/profile}'

render oidc \
  --set nvkeOIDCAuth.enabled=true \
  --set spiffe.enabled=true \
  --set-string spiffe.trustDomain=example.test \
  --set-string nvkeOIDCAuth.issuerURL=https://oidc.example.test \
  --set-string nvkeOIDCAuth.discoveryURL=https://oidc.example.test/.well-known/openid-configuration \
  --set-string nvkeOIDCAuth.caBundleSecret.namespace=nke-system \
  --set-string nvkeOIDCAuth.caBundleSecret.name=oidc-ca \
  --set-string nvkeOIDCAuth.caBundleSecret.key=ca.crt

render api-auth-permissive \
  --set apiAuth.enabled=true \
  --set apiAuth.permissive=true

# Empty strings fall back to Go defaults, which the template must render.
render empty-strings \
  --set-string identity.fieldManagerName= \
  --set-string identity.managedByLabelValue= \
  --set-string metrics.listenAddress= \
  --set-string supportedInfraProviders= \
  --set-string kubeadm.imageRepository= \
  --set-string scheduledMaintenance.certRenewal.interval= \
  --set-string spiffe.endpointSocket= \
  --set-string spiffe.spire.serverNamespace= \
  --set-string spiffe.spire.serverPort= \
  --set-string spiffe.spire.joinTokenTTL= \
  --set-string temporal.cloudWorkflow.namespace= \
  --set-string teleport.managedByLabelValue= \
  --set-string teleport.tokenNamePrefix=

# Bool and int keys whose Go default is not zero: null and empty fall back to it,
# an explicit false or 0 does not.
render empty-nonzero-defaults \
  --set-string scheduledMaintenance.certRenewal.enabled= \
  --set-string scheduledMaintenance.certRenewal.thresholdDays= \
  --set-string controllers.nico.enabled= \
  --set tracing.enabled=true \
  --set-string tracing.endpoint=otel-collector:4317 \
  --set-string tracing.insecure=
render null-nonzero-defaults \
  --set scheduledMaintenance.certRenewal.enabled=null \
  --set scheduledMaintenance.certRenewal.thresholdDays=null \
  --set controllers.nico.enabled=null \
  --set tracing.enabled=true \
  --set-string tracing.endpoint=otel-collector:4317 \
  --set tracing.insecure=null
render explicit-false-and-zero \
  --set scheduledMaintenance.certRenewal.enabled=false \
  --set scheduledMaintenance.certRenewal.thresholdDays=0 \
  --set controllers.nico.enabled=false

# oidc.providers with only the required fields, with every field, and at the cap.
cat > "${values}/oidc-providers-required.yaml" <<'YAML'
oidc:
  providers:
    - name: corp
      issuerURL: https://login.example.test
      audiences: [nke]
      usernameClaim: sub
      usernamePrefix: ""
      caBundle: |
        -----BEGIN CERTIFICATE-----
        MIIBexample
        -----END CERTIFICATE-----
YAML
cat > "${values}/oidc-providers-full.yaml" <<'YAML'
oidc:
  providers:
    - name: corp
      issuerURL: https://login.example.test
      audiences: [nke, kubectl]
      usernameClaim: email
      usernamePrefix: "corp:"
      groupsClaim: groups
      groupsPrefix: "corp:"
      requiredClaims:
        hd: example.test
        tenant: "1"
      discoveryURL: https://login.internal.example.test/.well-known/openid-configuration
      caBundle: |
        -----BEGIN CERTIFICATE-----
        MIIBexample
        -----END CERTIFICATE-----
        -----BEGIN CERTIFICATE-----
        MIIBintermediate
        -----END CERTIFICATE-----
    - name: partner
      issuerURL: https://idp.partner.example.test/realms/nke
      audiences: [nke]
      usernameClaim: preferred_username
      usernamePrefix: "partner:"
      caBundle: "-----BEGIN CERTIFICATE-----\nMIIBpartner\n-----END CERTIFICATE-----"
YAML
providers() {
  printf 'oidc:\n  providers:\n'
  for ((i = 1; i <= $1; i++)); do
    printf '    - {name: p%d, issuerURL: "https://p%d.example.test", audiences: [nke], usernameClaim: sub, usernamePrefix: "p%d:", caBundle: pem}\n' "${i}" "${i}" "${i}"
  done
}
providers 5 > "${values}/oidc-providers-at-cap.yaml"
providers 6 > "${values}/oidc-providers-over-cap.yaml"
for name in oidc-providers-required oidc-providers-full oidc-providers-at-cap; do
  render "${name}" -f "${values}/${name}.yaml"
done

# The providers reach config.yaml as given, down to the CA bundle's newlines.
for name in oidc-providers-required oidc-providers-full oidc-providers-at-cap; do
  want="$(yq -o=json -I=0 '.oidc.providers | sort_keys(..)' "${values}/${name}.yaml")"
  got="$(yq 'select(.kind == "ConfigMap" and .metadata.name == "nke-controller-config") | .data["config.yaml"]' "${renders}/${name}.yaml" |
    yq -o=json -I=0 '.oidc.providers | sort_keys(..)')"
  [[ "${got}" == "${want}" ]] || { echo "${name}: config.yaml has oidc.providers ${got}, values gave ${want}" >&2; exit 1; }
done
if refusal="$(helm template cwe "${CHART}" -f "${values}/oidc-providers-over-cap.yaml" 2>&1)"; then
  echo "six oidc.providers rendered; the chart must refuse more than five" >&2
  exit 1
fi
[[ "${refusal}" == *"oidc.providers has 6 entries"* ]] || { echo "six oidc.providers failed for another reason: ${refusal}" >&2; exit 1; }
printf '  oidc.providers render as given, and more than five are refused\n'

# A separate hooks directory, with and without the profiles image.
render runbooks-path --set-string profiles.runbooksPath=/opt/nke/hooks
render runbooks-path-with-profiles-image \
  --set profiles.image.enabled=true \
  --set-string profiles.image.tag=test \
  --set-string profiles.path=/opt/nke/profiles \
  --set-string profiles.runbooksPath=/opt/nke/hooks

# env stays open to what nke-controller-config does not carry, such as what
# Tilt and nvkectl bootstrap pass, and refuses what it does.
render env-passthrough \
  --set-string 'env[0].name=LOG_LEVEL' --set-string 'env[0].value=debug' \
  --set-string 'env[1].name=NODE_AGENT_PORT' --set-string 'env[1].value=50051'
if refusal="$(helm template cwe "${CHART}" \
  --set nvkeOIDCAuth.enabled=true --set spiffe.enabled=true --set-string spiffe.trustDomain=example.test \
  --set-string 'env[0].name=NKE_OIDC_AUTH_ISSUER_URL' --set-string 'env[0].value=https://oidc.example.test' 2>&1)"; then
  echo "env set NKE_OIDC_AUTH_ISSUER_URL and rendered; the chart must refuse a variable nke-controller-config carries" >&2
  exit 1
fi
[[ "${refusal}" == *"env sets NKE_OIDC_AUTH_ISSUER_URL, which nke-controller-config carries as oidc.managementOIDCAuth.issuerURL"* ]] ||
  { echo "env override of NKE_OIDC_AUTH_ISSUER_URL failed for another reason: ${refusal}" >&2; exit 1; }
if refusal="$(helm template cwe "${CHART}" --set-string 'env[0].name=CONTROLLER_CONFIG_PATH' --set-string 'env[0].value=' 2>&1)"; then
  echo "env set CONTROLLER_CONFIG_PATH and rendered; the chart must refuse it" >&2
  exit 1
fi
[[ "${refusal}" == *"env sets CONTROLLER_CONFIG_PATH"* ]] ||
  { echo "env override of CONTROLLER_CONFIG_PATH failed for another reason: ${refusal}" >&2; exit 1; }
for key in WORKERS TIMEOUT INTERVAL JITTER; do
  variable="NODE_AGENT_READINESS_${key}"
  if refusal="$(helm template cwe "${CHART}" --set-string "env[0].name=${variable}" --set-string 'env[0].value=1' 2>&1)"; then
    echo "env set ${variable} and rendered; the chart must refuse it" >&2
    exit 1
  fi
  [[ "${refusal}" == *"env sets ${variable}, which nke-controller-config carries as agentReadiness."* ]] ||
    { echo "env override of ${variable} failed for another reason: ${refusal}" >&2; exit 1; }
done
printf '  env overrides of nke-controller-config settings and its path are refused\n'

# A renamed values key must fail the render naming its replacement, not fall
# back to the chart defaults, which is what Helm does with an unknown key.
for pair in nkeOIDCAuth.enabled=true:nvkeOIDCAuth nkeSystemNamespace.create=false:nvkeSystemNamespace; do
  old="${pair%%:*}" new="${pair##*:}"
  if refusal="$(helm template cwe "${CHART}" --set "${old}" 2>&1)"; then
    echo "values set ${old} and rendered; the chart must refuse a renamed key" >&2
    exit 1
  fi
  [[ "${refusal}" == *"was renamed to ${new}"* ]] ||
    { echo "setting ${old} failed for another reason: ${refusal}" >&2; exit 1; }
done
# An explicitly empty old key is false in a Helm condition, so the guard keys
# off presence instead. Assert that, or the rename is bypassable with --set-json.
for new in nvkeOIDCAuth nvkeSystemNamespace; do
  old="nke${new#nvke}"
  if refusal="$(helm template cwe "${CHART}" --set-json "${old}={}" 2>&1)"; then
    echo "values set ${old} to {} and rendered; the chart must refuse a renamed key" >&2
    exit 1
  fi
  [[ "${refusal}" == *"was renamed to ${new}"* ]] ||
    { echo "setting ${old} to {} failed for another reason: ${refusal}" >&2; exit 1; }
done
printf '  renamed values keys are refused, naming their replacement\n'

# The values files the Tilt and nvkectl bootstrap deployments install with.
render tilt -f "${controller}/hack/tilt-controller-chart-values.yaml"
render nvkectl-bootstrap -f "${controller}/../cli/internal/bootstrap/controllerdeploy/manifests/controller-values.yaml"

printf '  rendered %s value sets\n' "$(find "${renders}" -name '*.yaml' | wc -l | tr -d ' ')"
cd "${controller}"

# shellcheck disable=SC2086 # GO_TEST_FLAGS is a list of flags.
NKE_RENDERED_CHART_DIR="${renders}" go test ./internal/controllerconfig/ -run '^TestRenderedChart$' -count=1 ${GO_TEST_FLAGS:-}
