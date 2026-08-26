#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for apiAuth in each state, which `helm lint`
# does not check.
#
# Needs only helm. Resolves the chart relative to this script, so it runs from
# anywhere; CHART overrides that, which is how the workflow points it at
# controller/deploy/charts/cloud-workflow-engine from the repository root.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/cloud-workflow-engine"}"
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

# pair <description> <key> <value> <rendered>
# Asserts value on the line after key. A bare `value: "true"` matches unrelated
# env vars, so checking key and value independently would pass either way.
pair() {
  if grep -A1 "name: $2" <<<"$4" | grep -q "value: \"$3\""; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (expected %s = %s)\n' "$1" "$2" "$3" >&2
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
  configMap:
    create: true
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
# Absent, not empty: NKE reads an empty AUTH_CONFIG_PATH as unset.
absent "AUTH_CONFIG_PATH is not set at all"   'AUTH_CONFIG_PATH'                     "${out}"
absent "PERMISSIVE_MODE is not set"           'PERMISSIVE_MODE'                      "${out}"
absent "no config volume"                     'name: nke-auth-config'                "${out}"
absent "no issuer-discovery binding"          'system:service-account-issuer-discovery' "${out}"

echo "apiAuth enabled, enforcing:"
out="$(render --set apiAuth.enabled=true)"
want   "AUTH_CONFIG_PATH points at the mount" 'value: "/etc/nke/auth/auth.yaml"'     "${out}"
pair   "PERMISSIVE_MODE is false"            'PERMISSIVE_MODE' 'false'              "${out}"
want   "ConfigMap volume"                     'name: nke-auth-config'                "${out}"
want   "mounted where the path says"          'mountPath: "/etc/nke/auth"'           "${out}"
want   "issuer-discovery binding"             'system:service-account-issuer-discovery' "${out}"

echo "apiAuth enabled, permissive:"
out="$(render --set apiAuth.enabled=true --set apiAuth.permissive=true)"
pair   "PERMISSIVE_MODE is true"             'PERMISSIVE_MODE' 'true'               "${out}"

echo "clusterIssuerDiscovery=false:"
out="$(render --set apiAuth.enabled=true --set apiAuth.clusterIssuerDiscovery=false)"
absent "drops the cluster-scoped grant"       'system:service-account-issuer-discovery' "${out}"
want   "but the rest of apiAuth still renders" 'AUTH_CONFIG_PATH'                    "${out}"

echo "the mount path and file name are fixed by the chart:"
out="$(render --set apiAuth.enabled=true)"
want   "mounted at the path AUTH_CONFIG_PATH names" 'mountPath: "/etc/nke/auth"'     "${out}"
want   "and the two agree"                    'value: "/etc/nke/auth/auth.yaml"'     "${out}"

echo "a named ConfigMap is what gets mounted:"
out="$(render --set apiAuth.enabled=true --set apiAuth.configMap.name=my-auth)"
want   "configMap.name is honoured"           'name: my-auth'                        "${out}"

echo "configMap.create=false (the default) mounts a ConfigMap it does not create:"
out="$(render --set apiAuth.enabled=true)"
absent "renders no ConfigMap of its own"      '^kind: ConfigMap'                     "${out}"
want   "still mounts the named one"           'name: nke-auth-config'                "${out}"
# auth.Load runs once at startup, so a configuration edit that does not restart
# the pods does nothing at all. Reloader is what restarts them, here and below.
want   "Reloader watches it"                  'configmap.reloader.stakater.com/reload: "nke-auth-config"' "${out}"

echo "configMap.create=true renders the configuration:"
out="$(render -f "${values}")"
want   "a ConfigMap is rendered"              '^kind: ConfigMap'                     "${out}"
want   "keyed auth.yaml, as the mount expects" '^  auth\.yaml: \|'                    "${out}"
# Supplied by the chart, never by values: the loader requires both and defaults
# neither, and the chart ships in lockstep with the build that accepts them.
want   "apiVersion is injected"                'apiVersion: auth\.nke\.nvidia\.com/v1alpha1' "${out}"
want   "kind is injected"                      'kind: AuthenticationConfiguration'    "${out}"
want   "audiences survive"                     'https://nke\.example\.internal'        "${out}"
# The one that catches a bad nindent: a dropped name widens the rule from one
# service account to every service account in the namespace, and still parses.
want   "the nested admit rule keeps its name" '^        - name: forge-console'       "${out}"
want   "and its namespace"                    '^          namespace: cloud-workflow-engine' "${out}"
# Rendering the ConfigMap does not change how it gets applied: Reloader watches
# it either way, so there is one mechanism to reason about rather than two.
want   "Reloader watches it here too"         'configmap.reloader.stakater.com/reload: "nke-auth-config"' "${out}"

echo "the Reloader annotation names whatever configMap.name says:"
out="$(render --set apiAuth.enabled=true --set apiAuth.configMap.name=my-auth)"
want   "the named ConfigMap is the watched one" 'configmap.reloader.stakater.com/reload: "my-auth"' "${out}"

echo "nothing is watched while authentication is off:"
out="$(render)"
absent "no ConfigMap reload annotation"       'configmap.reloader.stakater.com/reload' "${out}"

echo "an operator override of the reload annotation is left alone:"
out="$(render --set apiAuth.enabled=true \
  --set 'deploymentAnnotations.configmap\.reloader\.stakater\.com/reload=theirs')"
want   "theirs wins"                          'configmap.reloader.stakater.com/reload: theirs' "${out}"
absent "and the chart does not add its own"   'configmap.reloader.stakater.com/reload: "nke-auth-config"' "${out}"

echo "config and configMap.create must agree:"
wantfail "config without create is refused"    'apiAuth\.config is set but would not be rendered' \
  --set apiAuth.enabled=true --set 'apiAuth.config.audiences[0]=https://nke.example.internal'
wantfail "create without config is refused"    'apiAuth\.configMap\.create is true but apiAuth\.config is empty' \
  --set apiAuth.enabled=true --set apiAuth.configMap.create=true
wantfail "config without audiences is refused" 'apiAuth\.config\.audiences is required' \
  --set apiAuth.enabled=true --set apiAuth.configMap.create=true \
  --set 'apiAuth.config.issuers[0].name=local-kubernetes'
wantfail "config without issuers is refused"   'apiAuth\.config\.issuers is required' \
  --set apiAuth.enabled=true --set apiAuth.configMap.create=true \
  --set 'apiAuth.config.audiences[0]=https://nke.example.internal'

echo "the chart owns apiVersion and kind, and says so if you repeat them:"
# Duplicate keys render clean and then fail auth.Load's strict parse, so the
# pod crash-loops on a value the operator most likely pasted from the README.
wantfail "apiVersion in config is refused"     'apiAuth\.config must not carry apiVersion or kind' \
  --set apiAuth.enabled=true --set apiAuth.configMap.create=true \
  --set 'apiAuth.config.audiences[0]=https://nke.example.internal' \
  --set 'apiAuth.config.issuers[0].name=local-kubernetes' \
  --set 'apiAuth.config.apiVersion=auth.nke.nvidia.com/v1alpha1'
wantfail "kind in config is refused"           'apiAuth\.config must not carry apiVersion or kind' \
  --set apiAuth.enabled=true --set apiAuth.configMap.create=true \
  --set 'apiAuth.config.audiences[0]=https://nke.example.internal' \
  --set 'apiAuth.config.issuers[0].name=local-kubernetes' \
  --set 'apiAuth.config.kind=AuthenticationConfiguration'
# The rendered file carries exactly one of each, whatever the values said.
out="$(render -f "${values}")"
if [[ "$(grep -c '^    apiVersion: auth\.nke\.nvidia\.com' <<<"${out}")" -eq 1 ]]; then
  printf '  ok    %s\n' "exactly one apiVersion in the rendered file"
else
  printf '  FAIL  %s\n' "expected exactly one apiVersion line" >&2
  failures=$((failures + 1))
fi

echo "config is inert while authentication is off:"
wantfail "and says so rather than ignoring it" 'apiAuth\.config is set but would not be rendered' \
  --set apiAuth.configMap.create=true --set 'apiAuth.config.audiences[0]=https://nke.example.internal'

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart assertion(s) failed" >&2
  exit 1
fi
echo "all chart auth assertions passed"
