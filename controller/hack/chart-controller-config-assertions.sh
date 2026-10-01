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

if [[ -n "${RENDERS_DIR:-}" ]]; then
  mkdir -p "${RENDERS_DIR}"
  # Absolute, since go test reads it after the cd into the controller module.
  renders="$(cd "${RENDERS_DIR}" && pwd)"
else
  renders="$(mktemp -d)"
  trap 'rm -rf "${renders}"' EXIT
fi

render() {
  local name="$1"
  shift
  helm template cwe "${CHART}" "$@" > "${renders}/${name}.yaml"
}

checksum="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

render defaults

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

render oidc \
  --set nkeOIDCAuth.enabled=true \
  --set spiffe.enabled=true \
  --set-string spiffe.trustDomain=example.test \
  --set-string nkeOIDCAuth.issuerURL=https://oidc.example.test \
  --set-string nkeOIDCAuth.discoveryURL=https://oidc.example.test/.well-known/openid-configuration \
  --set-string nkeOIDCAuth.caBundleSecret.namespace=nke-system \
  --set-string nkeOIDCAuth.caBundleSecret.name=oidc-ca \
  --set-string nkeOIDCAuth.caBundleSecret.key=ca.crt

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

printf '  rendered %s value sets\n' "$(find "${renders}" -name '*.yaml' | wc -l | tr -d ' ')"
cd "${controller}"
# shellcheck disable=SC2086 # GO_TEST_FLAGS is a list of flags.
NKE_RENDERED_CHART_DIR="${renders}" go test ./internal/controllerconfig/ -run '^TestRenderedChart$' -count=1 ${GO_TEST_FLAGS:-}
