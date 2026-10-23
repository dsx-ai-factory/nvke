#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Asserts where the chart's policy bundle comes from, which `helm lint` does
# not check. apiAuthz.enabled's own wiring (env vars, mount path, the
# enabled-requires-apiAuth guard, the refused legacy keys) is covered by
# chart-auth-assertions.sh; this script covers the two paths:
#
#   default   apiAuthz.existingConfigMap empty. The chart renders
#             <fullname>-authz-default from its packaged files and mounts all
#             three keys from it.
#   override  apiAuthz.existingConfigMap set. The chart still renders its own
#             ConfigMap, and mounts policies.cedar and roles.json from the
#             named one and actions.json from its own.
#
# On both, roleMappings.json comes from the operator's mappings ConfigMap,
# nke-authz-role-mappings or apiAuthz.roleMappingsConfigMap, through an
# optional source.
#
# Needs only helm. Resolves the chart relative to this script, so it runs from
# anywhere; CHART overrides that, which is how the workflow points it at
# controller/deploy/charts/controller from the repository root.

set -euo pipefail

CHART="${CHART:-"$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/deploy/charts/controller"}"
[[ -d "${CHART}" ]] || { echo "chart not found at ${CHART}" >&2; exit 2; }

failures=0

# The release is named cwe, so controller.fullname is cwe-nvke-controller.
default_cm="cwe-nvke-controller-authz-default"
mappings_cm="nke-authz-role-mappings"

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

# equal <description> <want> <got>
equal() {
  if [[ "$2" == "$3" ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s (want %s, got %s)\n' "$1" "$2" "$3" >&2
    failures=$((failures + 1))
  fi
}

# wantfail <description> <pattern> <helm args...>
# Asserts the render fails and says why. A render that succeeds is the failure.
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

# configmap <name> <rendered>
# Prints the one rendered ConfigMap named <name>.
configmap() {
  awk -v name="$1" '
    function flush() { if (doc ~ /\nkind: ConfigMap\n/ && index(doc, "\n  name: " name "\n")) printf "%s", doc }
    /^---/ { flush(); doc = "\n"; next }
    { doc = doc $0 "\n" }
    END { flush() }
  ' <<<"$2"
}

# data_keys <configmap>
# Prints the ConfigMap's data keys, space-separated, in rendered order.
data_keys() {
  awk '
    /^data:/ { indata = 1; next }
    indata && /^[^ ]/ { indata = 0 }
    indata && /^  [^ ]+: / { sub(/^  /, ""); sub(/:.*/, ""); printf "%s%s", sep, $0; sep = " " }
  ' <<<"$1"
}

# volume_sources <rendered>
# Prints the nke-authz-policy volume's projected sources as
# "<configmap>=<key>:<path>,<key>:<path>;<configmap>?=<key>:<path>", in order,
# so one comparison covers the source count, each source's name, whether it is
# optional (the "?"), and each source's items and the file each lands at. A
# quoted name is printed without its quotes.
volume_sources() {
  awk '
    function flush() {
      if (name != "") { printf "%s%s%s=%s", ssep, name, opt, keys; ssep = ";" }
      name = ""; opt = ""; keys = ""; ksep = ""
    }
    /^        - name: nke-authz-policy$/ { invol = 1; next }
    invol && (/^        - / || /^      [^ ]/ || /^---/) { invol = 0 }
    invol && /^              - configMap:$/ { flush(); pending = 1; next }
    invol && pending && /^                  name: / { sub(/^ *name: /, ""); gsub(/"/, ""); name = $0; pending = 0; next }
    invol && /^                  optional: true$/ { opt = "?"; next }
    invol && /^                    - key: / { sub(/^ *- key: /, ""); keys = keys ksep $0; ksep = "," }
    invol && /^                      path: / { sub(/^ *path: /, ""); keys = keys ":" $0 }
    END { flush() }
  ' <<<"$1"
}

# default_cm_name <rendered>
# Prints the name of the rendered ConfigMap ending -authz-default.
default_cm_name() {
  awk '/^kind: ConfigMap$/ { cm = 1 } /^---/ { cm = 0 } cm && /^  name: .*-authz-default$/ { print $2; exit }' <<<"$1"
}

# name_fits <description> <rendered>
# Asserts the chart ConfigMap's name is at most 63 characters, keeps its
# suffix whole, and is the name the volume mounts.
name_fits() {
  local name
  name="$(default_cm_name "$2")"
  if [[ -n "${name}" && "${#name}" -le 63 && "${name}" == *[^-]-authz-default ]]; then
    printf '  ok    %s renders %s (%d characters)\n' "$1" "${name}" "${#name}"
  else
    printf '  FAIL  %s renders "%s" (%d characters)\n' "$1" "${name}" "${#name}" >&2
    failures=$((failures + 1))
  fi
  equal "  and the volume mounts that name" "${name}" "$(volume_sources "$2" | sed 's/=.*//')"
}

echo "apiAuthz disabled renders no bundle:"
out="$(helm template cwe "${CHART}")"
absent "no chart ConfigMap"                    'authz-default'                         "${out}"

echo "the default path, apiAuthz.enabled=true alone:"
out="$(render --set apiAuthz.enabled=true)"
cm="$(configmap "${default_cm}" "${out}")"
want   "the chart renders <fullname>-authz-default" '^kind: ConfigMap'                "${cm}"
equal  "with the three files the chart ships" \
  "actions.json policies.cedar roles.json" "$(data_keys "${cm}")"
absent "and not roleMappings.json, which the operator supplies" '^  roleMappings\.json:' "${cm}"
absent "and not schema.json, which nothing reads at runtime" '^  schema\.json:'        "${cm}"
want   "annotated with how to customize it" \
  'nke\.nvidia\.com/customize: "Managed by Helm\. To customize, set apiAuthz\.existingConfigMap\."' "${cm}"
want   "carrying the packaged policies"        'readers-may-read'                      "${cm}"
want   "and the packaged roles"                '"id": "operator"'                      "${cm}"
equal  "mounted as one source with all three items, then the optional mappings" \
  "${default_cm}=actions.json:actions.json,policies.cedar:policies.cedar,roles.json:roles.json;${mappings_cm}?=roleMappings.json:roleMappings.json" "$(volume_sources "${out}")"
absent "and no ConfigMap rendered for the mappings" '^kind: ConfigMap' "$(configmap "${mappings_cm}" "${out}")"

echo "the override path, apiAuthz.existingConfigMap=site-authz:"
out="$(render --set apiAuthz.enabled=true --set apiAuthz.existingConfigMap=site-authz)"
cm="$(configmap "${default_cm}" "${out}")"
want   "the chart still renders its own ConfigMap" '^kind: ConfigMap'                "${cm}"
equal  "with the same three files" \
  "actions.json policies.cedar roles.json" "$(data_keys "${cm}")"
# The operator's two files come from their ConfigMap. actions.json, which is
# generated for this image, comes only from the chart's.
equal  "mounted as the operator's two files, the chart's actions.json, then the optional mappings" \
  "site-authz=policies.cedar:policies.cedar,roles.json:roles.json;${default_cm}=actions.json:actions.json;${mappings_cm}?=roleMappings.json:roleMappings.json" "$(volume_sources "${out}")"

echo "the mappings ConfigMap, apiAuthz.roleMappingsConfigMap=site-mappings:"
out="$(render --set apiAuthz.enabled=true --set apiAuthz.roleMappingsConfigMap=site-mappings)"
equal  "mounts roleMappings.json from the named ConfigMap" \
  "${default_cm}=actions.json:actions.json,policies.cedar:policies.cedar,roles.json:roles.json;site-mappings?=roleMappings.json:roleMappings.json" "$(volume_sources "${out}")"
out="$(render --set apiAuthz.enabled=true --set apiAuthz.existingConfigMap=site-authz --set apiAuthz.roleMappingsConfigMap=site-mappings)"
equal  "and does so beside existingConfigMap" \
  "site-authz=policies.cedar:policies.cedar,roles.json:roles.json;${default_cm}=actions.json:actions.json;site-mappings?=roleMappings.json:roleMappings.json" "$(volume_sources "${out}")"
equal  "with whitespace trimmed" \
  "${default_cm}=actions.json:actions.json,policies.cedar:policies.cedar,roles.json:roles.json;site-mappings?=roleMappings.json:roleMappings.json" \
  "$(volume_sources "$(render --set apiAuthz.enabled=true --set-string 'apiAuthz.roleMappingsConfigMap= site-mappings ')")"
equal  "and whitespace alone uses the default" \
  "${default_cm}=actions.json:actions.json,policies.cedar:policies.cedar,roles.json:roles.json;${mappings_cm}?=roleMappings.json:roleMappings.json" \
  "$(volume_sources "$(render --set apiAuthz.enabled=true --set-string 'apiAuthz.roleMappingsConfigMap= ')")"
want   "a name YAML reads as a boolean is quoted" '^                  name: "yes"$' \
  "$(render --set apiAuthz.enabled=true --set-string apiAuthz.roleMappingsConfigMap=yes)"

echo "existingConfigMap is rendered as a string, whatever YAML would make of it:"
# Unquoted, "yes" is a boolean to a YAML 1.1 reader and 123 an integer, and the
# API server refuses either as a ConfigMap name.
want   "a name YAML reads as a boolean is quoted" '^                  name: "yes"$' \
  "$(render --set apiAuthz.enabled=true --set apiAuthz.existingConfigMap=yes)"
want   "a name YAML reads as a number is quoted" '^                  name: "123"$' \
  "$(render --set apiAuthz.enabled=true --set-string apiAuthz.existingConfigMap=123)"

echo "existingConfigMap that is not a string is refused:"
wantfail "a boolean"  'apiAuthz\.existingConfigMap must be a ConfigMap name' \
  --set apiAuthz.enabled=true --set apiAuthz.existingConfigMap=false
wantfail "a number"   'apiAuthz\.existingConfigMap must be a ConfigMap name' \
  --set apiAuthz.enabled=true --set apiAuthz.existingConfigMap=0
wantfail "an object"  'apiAuthz\.existingConfigMap must be a ConfigMap name' \
  --set apiAuthz.enabled=true --set-json 'apiAuthz.existingConfigMap={"name":"site-authz"}'
wantfail "a list"     'apiAuthz\.existingConfigMap must be a ConfigMap name' \
  --set apiAuthz.enabled=true --set-json 'apiAuthz.existingConfigMap=["site-authz"]'

echo "roleMappingsConfigMap that is not a string is refused:"
wantfail "a boolean"  'apiAuthz\.roleMappingsConfigMap must be a ConfigMap name' \
  --set apiAuthz.enabled=true --set apiAuthz.roleMappingsConfigMap=false
wantfail "a number"   'apiAuthz\.roleMappingsConfigMap must be a ConfigMap name' \
  --set apiAuthz.enabled=true --set apiAuthz.roleMappingsConfigMap=0
wantfail "an object"  'apiAuthz\.roleMappingsConfigMap must be a ConfigMap name' \
  --set apiAuthz.enabled=true --set-json 'apiAuthz.roleMappingsConfigMap={"name":"site-mappings"}'
wantfail "a list"     'apiAuthz\.roleMappingsConfigMap must be a ConfigMap name' \
  --set apiAuthz.enabled=true --set-json 'apiAuthz.roleMappingsConfigMap=["site-mappings"]'

echo "existingConfigMap naming the chart's own ConfigMap:"
wantfail "is refused" "apiAuthz\\.existingConfigMap names ${default_cm}, the ConfigMap this chart renders" \
  --set apiAuthz.enabled=true --set "apiAuthz.existingConfigMap=${default_cm}"
wantfail "is refused with surrounding whitespace" "apiAuthz\\.existingConfigMap names ${default_cm}, the ConfigMap this chart renders" \
  --set apiAuthz.enabled=true --set-string "apiAuthz.existingConfigMap= ${default_cm} "

echo "roleMappingsConfigMap naming the chart's own ConfigMap:"
wantfail "is refused" "apiAuthz\\.roleMappingsConfigMap names ${default_cm}, the ConfigMap this chart renders" \
  --set apiAuthz.enabled=true --set "apiAuthz.roleMappingsConfigMap=${default_cm}"

echo "a chart packaged without one of its bundle files:"
# No values reach this check, so it runs against a copy of the chart with the
# default roles.json removed.
broken="$(mktemp -d)"
trap 'rm -rf "${broken}"' EXIT
cp -R "${CHART}/." "${broken}/"
rm "${broken}/files/authz/default/roles.json"
out=""
if out="$(helm template cwe "${broken}" --set apiAuth.enabled=true --set apiAuthz.enabled=true 2>&1)"; then
  printf '  FAIL  %s (render succeeded; expected it to fail)\n' "is refused" >&2
  failures=$((failures + 1))
else
  want "is refused, naming the missing file" \
    "packaged roles\.json \(files/authz/default/roles\.json\) is missing or empty" "${out}"
fi

echo "the chart ConfigMap's name stays within 63 characters:"
long_name="$(printf 'f%.0s' {1..63})"
name_fits "a 63-character fullnameOverride" \
  "$(render --set apiAuthz.enabled=true --set "fullnameOverride=${long_name}")"
# Cut at 49, this one ends in a hyphen, which the helper drops rather than
# rendering "--authz-default".
name_fits "a fullnameOverride with a hyphen at character 49" \
  "$(render --set apiAuthz.enabled=true --set "fullnameOverride=${long_name:0:48}-${long_name:0:14}")"
# 53 is the longest release name Helm accepts; with the chart name appended,
# controller.fullname is cut at 63 before this helper cuts it again.
long_release="$(printf 'r%.0s' {1..53})"
name_fits "a 53-character release name" \
  "$(helm template "${long_release}" "${CHART}" --set apiAuth.enabled=true --set apiAuthz.enabled=true)"

if [[ "${failures}" -ne 0 ]]; then
  echo "${failures} chart authz assertion(s) failed" >&2
  exit 1
fi
echo "all chart authz assertions passed"
