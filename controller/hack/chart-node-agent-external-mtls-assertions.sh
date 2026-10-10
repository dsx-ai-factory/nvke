#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

chart="${CHART:-controller/deploy/charts/controller}"
failures=0

want() {
  local label="$1" pattern="$2" rendered="$3"
  if grep -Fq -- "${pattern}" <<<"${rendered}"; then
    printf '  ok    %s\n' "${label}"
  else
    printf '  FAIL  %s\n' "${label}" >&2
    failures=$((failures + 1))
  fi
}

absent() {
  local label="$1" pattern="$2" rendered="$3"
  if grep -Fq -- "${pattern}" <<<"${rendered}"; then
    printf '  FAIL  %s\n' "${label}" >&2
    failures=$((failures + 1))
  else
    printf '  ok    %s\n' "${label}"
  fi
}

disabled="$(helm template cwe "${chart}")"
want "disabled flag" 'name: NODE_AGENT_EXTERNAL_MTLS_ENABLED' "${disabled}"
want "disabled flag value" 'value: "false"' "${disabled}"
absent "disabled certificate mount" 'name: node-agent-client-tls' "${disabled}"

enabled="$(helm template cwe "${chart}" \
  --set nodeAgent.externalMTLS.enabled=true \
  --set nodeAgent.externalMTLS.secret.name=dsx-controller-client \
  --set nodeAgent.externalMTLS.secret.certificateKey=client.pem \
  --set nodeAgent.externalMTLS.secret.privateKeyKey=client-key.pem \
  --set nodeAgent.externalMTLS.secret.caBundleKey=trust.pem)"
want "enabled flag" 'name: NODE_AGENT_EXTERNAL_MTLS_ENABLED' "${enabled}"
absent "controller-owned client Certificate" 'name: nke-controller-agent-client' "${enabled}"
want "client identity Secret" 'secretName: dsx-controller-client' "${enabled}"
want "certificate Secret key" 'key: client.pem' "${enabled}"
want "private-key Secret key" 'key: client-key.pem' "${enabled}"
want "CA bundle Secret key" 'key: trust.pem' "${enabled}"
want "certificate target path" 'path: tls.crt' "${enabled}"
want "private-key target path" 'path: tls.key' "${enabled}"
want "CA bundle target path" 'path: ca.crt' "${enabled}"
want "certificate mount" 'mountPath: /etc/nke/node-agent-client-tls' "${enabled}"
want "certificate mount read-only" 'readOnly: true' "${enabled}"
want "certificate reload annotation" 'secret.reloader.stakater.com/reload: "dsx-controller-client"' "${enabled}"
absent "authentication ConfigMap" 'name: nke-node-agent-auth' "${enabled}"

if ((failures > 0)); then
  exit 1
fi
