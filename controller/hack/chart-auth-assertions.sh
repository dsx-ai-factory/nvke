#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for apiAuth in each state, which `helm lint`
# does not check.
#
# Needs helm and yq. Resolves the chart relative to this script, so it runs from
# anywhere; CHART overrides that, which is how the workflow points it at
# controller/deploy/charts/controller from the repository root.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/controller"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }

failures=0

render() { helm template cwe "${CHART}" "$@"; }

# want <description> <pattern> <rendered>
want() {
  if grep -qE "$2" <<<"$3"; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected to find: %s)\n' "$1" "$2" >&2
    failures=$((failures + 1))
  fi
}

# absent <description> <pattern> <rendered>
absent() {
  if grep -qE "$2" <<<"$3"; then
    printf '  FAIL  %s (should not have rendered: %s)\n' "$1" "$2" >&2
    failures=$((failures + 1))
  else
    printf '  ok    %s\n' "$1"
  fi
}

# setting <description> <config.yaml key> <value> <rendered>
# Asserts one key of the rendered nvke-controller-config, where the controller
# reads its settings. A list compares as its items joined with commas.
setting() {
  local got
  got="$(yq 'select(.kind == "ConfigMap" and .metadata.name == "nvke-controller-config") | .data["config.yaml"]' <<<"$4" |
    yq ".$2 | (select(tag == \"!!seq\") | join(\",\")) // .")"
  if [[ "${got}" == "$3" ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s = %s, got %s)\n' "$1" "$2" "$3" "${got}" >&2
    failures=$((failures + 1))
  fi
}

# wantfail <description> <pattern> <helm args...>
# Asserts the render fails and says why. A render that succeeds is the failure:
# these values are the ones that would otherwise be silently ignored.
wantfail() {
  local desc="$1" pattern="$2"; shift 2
  local out
  if out="$(helm template cwe "${CHART}" "$@" 2>&1)"; then
    printf '  FAIL  %s (render succeeded; expected it to fail)\n' "${desc}" >&2
    failures=$((failures + 1))
  elif grep -qE "${pattern}" <<<"${out}"; then
    printf '  ok    %s\n' "${desc}"
  else
    printf '  FAIL  %s (failed, but not with: %s)\n' "${desc}" "${pattern}" >&2
    failures=$((failures + 1))
  fi
}

# A configuration the chart will render, kept whole rather than assembled from
# --set flags so the nested admit rule is exercised as an operator would write it.
values="$(mktemp -d)/auth-values.yaml"
trap 'rm -rf "$(dirname "${values}")"' EXIT HUP INT TERM
cat > "${values}" <<'YAML'
apiAuth:
  enabled: true
  config:
    audiences: ["https://nke.example.internal"]
    issuers:
      - name: local-kubernetes
        issuer: "https://kubernetes.default.svc.cluster.local"
        jwks:
          uri: "https://kubernetes.default.svc/openid/v1/jwks"
          caBundlePath: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
          bearerTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token
        algorithms: [RS256, ES256]
        admit:
          serviceAccounts:
            - namespace: cloud-workflow-engine
              name: forge-console
YAML

echo "apiAuth disabled (the default):"
out="$(render)"
setting "apiAuth is off in nvke-controller-config" apiAuth.enabled false             "${out}"
setting "apiAuth.permissive is false"        apiAuth.permissive false               "${out}"
absent "no issuer-discovery binding"          'system:service-account-issuer-discovery' "${out}"
want   "only nvke-controller-config is watched" 'configmap.reloader.stakater.com/reload: "nvke-controller-config"' "${out}"

echo "apiAuth enabled renders the configuration into nvke-controller-config:"
out="$(render -f "${values}")"
setting "apiAuth is on"                       apiAuth.enabled true                   "${out}"
setting "apiAuth.permissive is false"        apiAuth.permissive false               "${out}"
setting "audiences survive"                   apiAuth.audiences https://nke.example.internal "${out}"
# The one that catches a bad nindent: a dropped name widens the rule from one
# service account to every service account in the namespace, and still parses.
setting "the nested admit rule keeps its name" 'apiAuth.issuers[0].admit.serviceAccounts[0].name' forge-console "${out}"
setting "and its namespace"                   'apiAuth.issuers[0].admit.serviceAccounts[0].namespace' cloud-workflow-engine "${out}"
want   "issuer-discovery binding"             'system:service-account-issuer-discovery' "${out}"
# The authentication configuration is read once at startup, so an edit that
# does not restart the pods does nothing. Reloader is what restarts them.
want   "Reloader watches nvke-controller-config" 'configmap.reloader.stakater.com/reload: "nvke-controller-config"' "${out}"
# The external-ConfigMap mode is gone: nothing else is rendered, mounted or
# pointed at. By source template, not kind: the chart renders other ConfigMaps.
absent "no authentication ConfigMap"         'nvke-auth-config'                     "${out}"
absent "nothing mounted at the old path"     '/etc/nke/auth'                        "${out}"
absent "AUTH_CONFIG_PATH is not set"         'AUTH_CONFIG_PATH'                     "${out}"
absent "no apiVersion or kind is injected"   'AuthenticationConfiguration'          "${out}"

echo "apiAuth enabled, permissive:"
out="$(render -f "${values}" --set apiAuth.permissive=true)"
setting "apiAuth.permissive is true"        apiAuth.permissive true                "${out}"
absent "the top-level key is gone"           '^    permissiveMode:'                  "${out}"

echo "clusterIssuerDiscovery=false:"
out="$(render -f "${values}" --set apiAuth.clusterIssuerDiscovery=false)"
absent "drops the cluster-scoped grant"       'system:service-account-issuer-discovery' "${out}"
setting "but apiAuth still renders"           apiAuth.enabled true                   "${out}"

echo "an operator override of the reload annotation is left alone:"
out="$(render -f "${values}" \
  --set 'deploymentAnnotations.configmap\.reloader\.stakater\.com/reload=theirs')"
want   "theirs wins"                          'configmap.reloader.stakater.com/reload: theirs' "${out}"
absent "and the chart does not add its own"   'configmap.reloader.stakater.com/reload: ".*nvke-' "${out}"

echo "enabled and config must agree:"
wantfail "enabled without config is refused"   'apiAuth\.enabled is true but apiAuth\.config is empty' \
  --set apiAuth.enabled=true
wantfail "config without enabled is refused"   'apiAuth\.config is set but apiAuth\.enabled is false' \
  --set 'apiAuth.config.audiences[0]=https://nke.example.internal'
wantfail "config without audiences is refused" 'apiAuth\.config\.audiences is required' \
  --set apiAuth.enabled=true \
  --set 'apiAuth.config.issuers[0].name=local-kubernetes'
wantfail "config without issuers is refused"   'apiAuth\.config\.issuers is required' \
  --set apiAuth.enabled=true \
  --set 'apiAuth.config.audiences[0]=https://nke.example.internal'

echo "permissive needs authentication on:"
wantfail "permissive without enabled is refused" 'apiAuth\.permissive is true but apiAuth\.enabled is false' \
  --set apiAuth.permissive=true

echo "apiAuth.config carries audiences and issuers only:"
# Most likely pasted from the retired auth.yaml; the strict loader would refuse
# them as unknown fields and the pod would crash-loop.
wantfail "apiVersion in config is refused"     'apiAuth\.config must not carry apiVersion or kind' \
  -f "${values}" --set 'apiAuth.config.apiVersion=auth.nke.nvidia.com/v1alpha1'
wantfail "kind in config is refused"           'apiAuth\.config must not carry apiVersion or kind' \
  -f "${values}" --set 'apiAuth.config.kind=AuthenticationConfiguration'
# apiAuth.config goes into nvke-controller-config's apiAuth section whole, next
# to the chart's own enabled and permissive, so a copy would be a duplicate key.
wantfail "enabled in config is refused"        'apiAuth\.config must not carry enabled' \
  -f "${values}" --set 'apiAuth.config.enabled=true'
wantfail "permissive in config is refused"     'apiAuth\.config must not carry permissive' \
  -f "${values}" --set 'apiAuth.config.permissive=true'

echo "the retired external-ConfigMap mode is refused by name:"
wantfail "env AUTH_CONFIG_PATH is refused"     'env sets AUTH_CONFIG_PATH, which the controller no longer reads' \
  -f "${values}" --set-string 'env[0].name=AUTH_CONFIG_PATH' --set-string 'env[0].value=/x'
wantfail "even with authentication off"        'env sets AUTH_CONFIG_PATH, which the controller no longer reads' \
  --set-string 'env[0].name=AUTH_CONFIG_PATH' --set-string 'env[0].value=/x'
wantfail "configMap.create is refused"         'apiAuth\.configMap was removed' \
  -f "${values}" --set apiAuth.configMap.create=true
wantfail "configMap.name is refused"           'apiAuth\.configMap was removed' \
  --set apiAuth.configMap.name=nke-auth-config
wantfail "configMap.extraFiles is refused"     'apiAuth\.configMap was removed' \
  -f "${values}" --set 'apiAuth.configMap.extraFiles.spire-ca\.crt=x'

# An issuer behind a private CA carries its bundle inline.
echo "an inline caBundle travels with the configuration:"
inline_ca_values="$(dirname "${values}")/auth-inline-ca-values.yaml"
cat > "${inline_ca_values}" <<'YAML'
apiAuth:
  enabled: true
  config:
    audiences: ["https://nke.example.internal"]
    issuers:
      - name: spire
        issuer: "https://spire.example.internal"
        jwks:
          uri: "https://spire.example.internal:30443/keys"
          caBundle: |
            -----BEGIN CERTIFICATE-----
            notarealcertificate
            -----END CERTIFICATE-----
        algorithms: [RS256]
        admit:
          spiffe:
            trustDomain: example.internal
            pathPrefixes: ["/nke-service/"]
YAML
out="$(render -f "${inline_ca_values}")"
want   "the bundle is carried inline"          'notarealcertificate'      "${out}"
setting "and reaches nvke-controller-config"  'apiAuth.issuers[0].name' spire "${out}"

# /etc/nke/auth was where the retired ConfigMap and its extraFiles were mounted.
# A path into it would crash-loop on a missing file, so it fails the render.
echo "a caBundlePath into the retired mount is refused:"
wantfail "and names the inline alternative"    'Put the PEM inline as jwks\.caBundle' \
  --set apiAuth.enabled=true \
  --set 'apiAuth.config.audiences[0]=https://nke.example.internal' \
  --set 'apiAuth.config.issuers[0].name=spire' \
  --set 'apiAuth.config.issuers[0].jwks.caBundlePath=/etc/nke/auth/spire-ca.crt'
# A path elsewhere is a file the pod already has, or the operator's own volume.
out="$(render -f "${values}")"
setting "a CA outside it is left alone"       'apiAuth.issuers[0].jwks.caBundlePath' /var/run/secrets/kubernetes.io/serviceaccount/ca.crt "${out}"

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart assertion(s) failed" >&2
  exit 1
fi
echo "all chart auth assertions passed"
