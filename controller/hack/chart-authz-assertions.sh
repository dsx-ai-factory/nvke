#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts what the chart renders for apiAuthz.configMap.create/apiAuthz.bundle/
# apiAuthz.policyMode (design-authz-chart-validation.md), which `helm lint`
# does not check. apiAuthz.enabled's own basic wiring (env vars, mount path,
# the enabled-requires-apiAuth guard) is already covered by
# chart-auth-assertions.sh; this script covers only what §4/§5 of that design
# added on top: rendering the bundle from values instead of assuming an
# externally-managed ConfigMap, the named presets, and the required-bundle-part
# checks in controller.apiAuthz.validate.
#
# Needs only helm. Resolves the chart relative to this script, so it runs from
# anywhere; CHART overrides that, which is how the workflow points it at
# controller/deploy/charts/controller from the repository root.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/controller"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }

failures=0

render() { helm template cwe "${CHART}" --set apiAuth.enabled=true "$@"; }

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

# wantfail <description> <pattern> <helm args...>
# Asserts the render fails and says why. A render that succeeds is the failure:
# these values are the ones that would otherwise be silently ignored, or would
# mount a bundle NewLocal refuses at pod startup.
wantfail() {
  local desc="$1" pattern="$2"; shift 2
  local out
  if out="$(render "$@" 2>&1)"; then
    printf '  FAIL  %s (render succeeded; expected it to fail)\n' "${desc}" >&2
    failures=$((failures + 1))
  elif grep -qE "${pattern}" <<<"${out}"; then
    printf '  ok    %s\n' "${desc}"
  else
    printf '  FAIL  %s (failed, but not with: %s)\n' "${desc}" "${pattern}" >&2
    failures=$((failures + 1))
  fi
}

echo "configMap.create=false (the default) renders no ConfigMap of its own:"
out="$(render --set apiAuthz.enabled=true)"
absent "no ConfigMap of its own"              '^kind: ConfigMap'                     "${out}"
want   "still mounts the named one"           'name: nke-authz-policy'               "${out}"
absent "no validate-authz-bundle hook Pod"    'hook.: post-install,post-upgrade'     "${out}"

echo "policyMode=standard renders a ConfigMap matching the shipped bundle's shape:"
out="$(render --set apiAuthz.enabled=true --set apiAuthz.configMap.create=true --set apiAuthz.policyMode=standard)"
want   "a ConfigMap is rendered"              '^kind: ConfigMap'                     "${out}"
want   "keyed policies.cedar"                 '^  policies\.cedar: \|'                "${out}"
want   "keyed roles.json"                     '^  roles\.json: \|'                    "${out}"
want   "keyed actions.json"                   '^  actions\.json: \|'                  "${out}"
absent "not keyed schema.json (build-time only, not mounted)" '^  schema\.json: \|'    "${out}"
want   "the reader role's permit rule"        'readers-may-read'                      "${out}"
want   "the operator role's permit rule"      'operators-may-write'                   "${out}"
want   "operator is a declared role"          '"id":"operator","type":"NKE::Role"'    "${out}"
absent "no validate-authz-bundle hook Pod when validationHook.enabled is left at its default (false)" \
                                               'validate-authz-bundle'                 "${out}"

echo "apiAuthz.validationHook.enabled=true is required, on top of configMap.create, to render the hook Pod:"
echo "  (it MUST stay false by default: controller has no \"authz validate\" CLI subcommand yet, and a"
echo "  configMap.create=true install/upgrade with the hook live would run a command that does not exist)"
out="$(render --set apiAuthz.enabled=true --set apiAuthz.configMap.create=true --set apiAuthz.policyMode=standard --set apiAuthz.validationHook.enabled=true)"
want   "the validate-authz-bundle hook Pod is rendered" 'validate-authz-bundle'        "${out}"
want   "as a post-install,post-upgrade hook, not helm test" 'helm.sh/hook.: post-install,post-upgrade' "${out}"

echo "policyMode=readOnly grants only the reader role's permissions:"
out="$(render --set apiAuthz.enabled=true --set apiAuthz.configMap.create=true --set apiAuthz.policyMode=readOnly)"
want   "the reader role's permit rule"        'readers-may-read'                      "${out}"
absent "no operator permit rule"              'operators-may-write'                   "${out}"
absent "no operator role declared"            '"id":"operator","type":"NKE::Role"'    "${out}"

echo "an unrecognized policyMode is refused, naming the packaged set:"
wantfail "bogus policyMode is refused" 'apiAuthz\.policyMode "bogus" is not a packaged preset. NKE ships: readOnly, standard' \
  --set apiAuthz.enabled=true --set apiAuthz.configMap.create=true --set apiAuthz.policyMode=bogus

echo "configMap.create=true with neither bundle.policies nor policyMode is refused:"
wantfail "no policies and no policyMode is refused" 'apiAuthz\.bundle\.policies is empty and apiAuthz\.policyMode is unset' \
  --set apiAuthz.enabled=true --set apiAuthz.configMap.create=true

# Values with commas/semicolons in them do not survive --set cleanly, so these
# three cases go through -f against a small values file each, the same reason
# chart-auth-assertions.sh does this for its nested admit rule.
tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT HUP INT TERM

echo "an explicit bundle.policies with no roles and no policyMode is refused separately:"
roles_values="${tmpdir}/authz-roles-values.yaml"
cat > "${roles_values}" <<'YAML'
apiAuthz:
  enabled: true
  configMap:
    create: true
  bundle:
    policies: "permit(principal, action, resource);"
YAML
wantfail "roles empty and no policyMode is refused" 'apiAuthz\.bundle\.roles is empty and apiAuthz\.policyMode is unset' \
  -f "${roles_values}"

echo "a policies document with no permit/forbid rule is refused:"
empty_values="${tmpdir}/authz-empty-policy-values.yaml"
cat > "${empty_values}" <<'YAML'
apiAuthz:
  enabled: true
  configMap:
    create: true
  policyMode: standard
  bundle:
    policies: "// no rules here"
YAML
wantfail "an empty-looking policy document is refused" 'does not appear to contain any permit/forbid rule' \
  -f "${empty_values}"

echo "bundle.policies wins over policyMode when both are set:"
override_values="${tmpdir}/authz-override-values.yaml"
cat > "${override_values}" <<'YAML'
apiAuthz:
  enabled: true
  configMap:
    create: true
  policyMode: standard
  bundle:
    policies: "permit(principal, action, resource);"
YAML
out="$(render -f "${override_values}")"
want   "the custom policy is what renders"    'permit\(principal, action, resource\);' "${out}"
absent "not the preset's own rule"            'readers-may-read'                       "${out}"
want   "bundle.roles still falls back to the preset" 'operator'                        "${out}"

echo "apiAuthz.bundle set without configMap.create is refused rather than silently ignored:"
wantfail "bundle without create is refused" 'apiAuthz\.bundle / apiAuthz\.policyMode is set but would not be rendered' \
  --set apiAuthz.enabled=true --set apiAuthz.policyMode=standard

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart authz assertion(s) failed" >&2
  exit 1
fi
echo "all chart authz assertions passed"
