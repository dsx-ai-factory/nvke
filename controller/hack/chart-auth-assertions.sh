#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for apiAuth and apiAuthz in each state, which
# `helm lint` does not check.
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
# Asserts one key of the rendered nke-controller-config, where the controller
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
# Absent, not empty: NVKE reads an empty AUTH_CONFIG_PATH as unset.
absent "AUTH_CONFIG_PATH is not set at all"   'AUTH_CONFIG_PATH'                     "${out}"
setting "permissiveMode is false"            permissiveMode false                   "${out}"
absent "no config volume"                     'name: nvke-auth-config'                "${out}"
absent "no issuer-discovery binding"          'system:service-account-issuer-discovery' "${out}"

echo "apiAuth enabled, enforcing:"
out="$(render --set apiAuth.enabled=true)"
want   "AUTH_CONFIG_PATH points at the mount" 'value: "/etc/nke/auth/auth.yaml"'     "${out}"
setting "permissiveMode is false"           permissiveMode false                   "${out}"
want   "ConfigMap volume"                     'name: nvke-auth-config'                "${out}"
want   "mounted where the path says"          'mountPath: "/etc/nke/auth"'           "${out}"
want   "issuer-discovery binding"             'system:service-account-issuer-discovery' "${out}"

echo "apiAuth enabled, permissive:"
out="$(render --set apiAuth.enabled=true --set apiAuth.permissive=true)"
setting "permissiveMode is true"            permissiveMode true                    "${out}"

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
# By source template, not kind: the chart renders other ConfigMaps regardless.
absent "renders no ConfigMap of its own"      '^# Source: .*/templates/configmap-auth\.yaml$' "${out}"
want   "still mounts the named one"           'name: nvke-auth-config'                "${out}"
# authn.Load runs once at startup, so a configuration edit that does not restart
# the pods does nothing at all. Reloader is what restarts them, here and below.
want   "Reloader watches it"                  'configmap.reloader.stakater.com/reload: "nvke-auth-config,nvke-controller-config"' "${out}"

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
want   "Reloader watches it here too"         'configmap.reloader.stakater.com/reload: "nvke-auth-config,nvke-controller-config"' "${out}"

echo "an install that predates the rename pins the old name and keeps it:"
out="$(render --set apiAuth.enabled=true --set apiAuth.configMap.name=nke-auth-config)"
want   "the pinned ConfigMap is mounted"      'name: nke-auth-config'                "${out}"
want   "and it is the watched one"            'configmap.reloader.stakater.com/reload: "nke-auth-config,nvke-controller-config"' "${out}"

echo "the Reloader annotation names whatever configMap.name says:"
out="$(render --set apiAuth.enabled=true --set apiAuth.configMap.name=my-auth)"
want   "the named ConfigMap is the watched one" 'configmap.reloader.stakater.com/reload: "my-auth,nvke-controller-config"' "${out}"

echo "only nvke-controller-config is watched while authentication is off:"
out="$(render)"
want   "no auth ConfigMap in the reload annotation" 'configmap.reloader.stakater.com/reload: "nvke-controller-config"' "${out}"

echo "an operator override of the reload annotation is left alone:"
out="$(render --set apiAuth.enabled=true \
  --set 'deploymentAnnotations.configmap\.reloader\.stakater\.com/reload=theirs')"
want   "theirs wins"                          'configmap.reloader.stakater.com/reload: theirs' "${out}"
absent "and the chart does not add its own"   'configmap.reloader.stakater.com/reload: ".*nke-' "${out}"

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
# Duplicate keys render clean and then fail authn.Load's strict parse, so the
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

# An issuer behind a private CA needs a bundle in the same directory as the
# config. The mount has no items selector, so any extra key is a file there.
echo "extraFiles put a CA bundle beside the config:"
ca_values="$(dirname "${values}")/auth-ca-values.yaml"
cat > "${ca_values}" <<'YAML'
apiAuth:
  enabled: true
  configMap:
    create: true
    extraFiles:
      spire-ca.crt: |
        -----BEGIN CERTIFICATE-----
        notarealcertificate
        -----END CERTIFICATE-----
  config:
    audiences: ["https://nke.example.internal"]
    issuers:
      - name: spire
        issuer: "https://spire.example.internal"
        jwks:
          uri: "https://spire.example.internal:30443/keys"
          caBundlePath: /etc/nke/auth/spire-ca.crt
        algorithms: [RS256]
        admit:
          spiffe:
            trustDomain: example.internal
            pathPrefixes: ["/nke-service/"]
YAML
out="$(render -f "${ca_values}")"
want   "the bundle is a key in the ConfigMap"  '^  spire-ca\.crt: \|'     "${out}"
want   "its content is carried through"        'notarealcertificate'      "${out}"
want   "the config file is still there"        '^  auth\.yaml: \|'        "${out}"

# The whole point: caBundlePath names a file that has to exist in the mount, and
# a missing one is a CrashLoopBackOff on a path rather than a render error.
echo "a caBundlePath nothing supplies is refused:"
wantfail "unlisted file in the mount is refused" 'nothing puts spire-ca\.crt there' \
  --set apiAuth.enabled=true --set apiAuth.configMap.create=true \
  --set 'apiAuth.config.audiences[0]=https://nke.example.internal' \
  --set 'apiAuth.config.issuers[0].name=spire' \
  --set 'apiAuth.config.issuers[0].jwks.caBundlePath=/etc/nke/auth/spire-ca.crt'
# A path outside the mount is the operator's own volume, and none of the chart's business.
out="$(render -f "${values}")"
want   "a CA outside the mount is left alone"  'caBundlePath: /var/run/secrets' "${out}"

echo "extraFiles cannot shadow the config file:"
wantfail "auth.yaml in extraFiles is refused" 'must not contain "auth\.yaml"' \
  --set apiAuth.enabled=true --set apiAuth.configMap.create=true \
  --set 'apiAuth.config.audiences[0]=https://nke.example.internal' \
  --set 'apiAuth.config.issuers[0].name=local-kubernetes' \
  --set 'apiAuth.configMap.extraFiles.auth\.yaml=whatever'

echo "extraFiles is inert without create:"
wantfail "and says so rather than ignoring it" 'apiAuth\.configMap\.extraFiles is set but would not be rendered' \
  --set apiAuth.enabled=true --set 'apiAuth.configMap.extraFiles.spire-ca\.crt=x'

echo "apiAuthz disabled (the default):"
out="$(render)"
# Absent, not empty, for the same reason as AUTH_CONFIG_PATH above: an empty
# AUTHZ_BUNDLE_PATH reads as unset, so the deployment would look configured and
# authorize nobody.
absent "AUTHZ_BUNDLE_PATH is not set at all" 'AUTHZ_BUNDLE_PATH'                    "${out}"
setting "authzPermissive is false"          authzPermissive false                  "${out}"
absent "no policy volume"                    'name: nke-authz-policy'               "${out}"
absent "nothing mounted at the bundle path"  'mountPath: "/etc/nke/authz"'          "${out}"

echo "apiAuthz enabled, enforcing:"
out="$(render --set apiAuth.enabled=true --set apiAuthz.enabled=true)"
want   "AUTHZ_BUNDLE_PATH points at the mount" 'value: "/etc/nke/authz"'            "${out}"
setting "authzPermissive is false"          authzPermissive false                  "${out}"
want   "policy volume"                       'name: nke-authz-policy'               "${out}"
want   "mounted where the bundle path says"  'mountPath: "/etc/nke/authz"'          "${out}"
# The engine resolves the bundle's file names under AUTHZ_BUNDLE_PATH, so the
# mount and the variable have to name the same directory or it loads nothing.
want   "and the two agree"                   'value: "/etc/nke/authz"'              "${out}"

echo "apiAuthz enabled, permissive:"
out="$(render --set apiAuth.enabled=true --set apiAuthz.enabled=true --set apiAuthz.permissive=true)"
setting "authzPermissive is true"           authzPermissive true                   "${out}"

echo "the named ConfigMap is what holds the bundle:"
# A name of its own, distinct from the volume's, so the assertion cannot pass on
# the volume name alone.
out="$(render --set apiAuth.enabled=true --set apiAuthz.enabled=true --set apiAuthz.existingConfigMap=site-authz)"
want   "existingConfigMap is honoured"       'name: "site-authz"'                   "${out}"

echo "the guards that refuse to render:"
wantfail     "apiAuthz without apiAuth is refused" 'requires apiAuth.enabled' \
  --set apiAuthz.enabled=true
# A values file that sets apiAuthz.configMap or apiAuthz.bundle fails, naming
# apiAuthz.existingConfigMap. Rendering as if those keys meant nothing would
# mount a bundle other than the one the operator wrote down.
wantfail     "apiAuthz.configMap is refused, naming existingConfigMap" 'set apiAuthz\.existingConfigMap' \
  --set apiAuth.enabled=true --set apiAuthz.enabled=true --set apiAuthz.configMap.name=x
wantfail     "apiAuthz.bundle is refused, naming existingConfigMap" 'set apiAuthz\.existingConfigMap' \
  --set apiAuth.enabled=true --set apiAuthz.enabled=true --set apiAuthz.bundle.policies=x

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart assertion(s) failed" >&2
  exit 1
fi
echo "all chart auth assertions passed"
