#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Tests for nvke-deploy.sh against deploy/nico, committed and rendered for a
# site, in --dry-run: the item order, where each chart comes from, and the
# refusals. Needs python3 and nvkectl: NVKECTL names the binary, else the
# one on PATH; make -C deploy test builds it.
#
# Usage: ./test_nvke_deploy.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="$(cd "${HERE}/.." && pwd)"
REPO="$(cd "${DEPLOY_DIR}/.." && pwd)"
EXECUTOR="${DEPLOY_DIR}/nvke-deploy.sh"
NICO="${DEPLOY_DIR}/nico"
CONFIG="${HERE}/bootstrap-config.json"
NVKECTL="${NVKECTL:-nvkectl}"
command -v "${NVKECTL}" >/dev/null 2>&1 || { echo "nvkectl not found: set NVKECTL or run make -C deploy test" >&2; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/nvke-deploy-test.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
WORK="$(cd "${WORK}" && pwd)"
# The render refuses an --out under a temp directory; this suite renders
# into one on purpose. Test-only allowance.
export NVKECTL_RENDER_ALLOW_TEMP_OUT=1

failures=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}
expect() {
  local what="$1" pattern="$2" text="$3"
  if grep -qE -- "${pattern}" <<<"${text}"; then pass "${what}"; else fail "${what}: no line matches ${pattern}"; fi
}
reject() {
  local what="$1" pattern="$2" text="$3"
  if grep -qE -- "${pattern}" <<<"${text}"; then fail "${what}: a line matches ${pattern}"; else pass "${what}"; fi
}

# The committed deploy path: every chart comes from the checkout or its
# repository, in list order; template items need --config.
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --config "${CONFIG}" --dry-run 2>&1)" || fail "plain dry run exited $?"
order="$(grep -oE '^=== item [a-z-]+ \(' <<<"${out}" | sed -E 's/^=== item ([a-z-]+) \(/\1/' | tr '\n' ' ' || true)"
if [[ "${order}" == "nvke-base-config spire-tpm-pubhashes spire-server-nodeport spire-oidc-provider-nodeport capnico nktp postgresql spire-crds temporal spire controller " ]]; then
  pass "items apply in nvke-deploy.yaml order"
else
  fail "item order: ${order}"
fi
expect "nktp from the checkout" 'helm upgrade --install nktp .*/nktp/deploy/charts/nktp ' "${out}"
expect "controller from the checkout" 'helm upgrade --install nke-controller .*/controller/deploy/charts/controller ' "${out}"
expect "controller in its release.yaml namespace" 'helm upgrade --install nke-controller .* --namespace nke-controller ' "${out}"
expect "postgresql from its OCI repository" 'helm upgrade --install postgres oci://registry-1.docker.io/bitnamicharts/postgresql --version 18.5.15 ' "${out}"
expect "spire from its index repository" 'helm upgrade --install spire spire --repo https://spiffe.github.io/helm-charts-hardened/ --version 0.30.1 ' "${out}"
expect "capnico.yaml applied as committed" "kubectl apply --server-side -f ${NICO}/capnico/capnico.yaml" "${out}"
expect "a template item is rendered by the executor when --config is given" "kubectl apply --server-side -f .*/20-nico-credentials.yaml" "${out}"

# A deploy path rendered for a site: nvkectl render deploy-path writes the
# site values and renders the template items, and the executor installs the
# result with no --config and no knowledge of the site.
mkdir -p "${WORK}/site"
for f in nico-token registry-token tenant-password; do printf 'secret-%s\n' "$f" >"${WORK}/site/$f"; done
sed -e 's|REPLACE_ARCH|arm64|; s|REPLACE_NICO_ENDPOINT|https://nico.example.com|; s|REPLACE_NICO_ORG_ID|org-1|; s|REPLACE_NICO_API_NAME|forge|' \
    -e "s|REPLACE_NICO_TOKEN_PATH|${WORK}/site/nico-token|; s|REPLACE_REGISTRY_SERVER|ghcr.io|; s|REPLACE_REGISTRY_USERNAME|robot|" \
    -e "s|REPLACE_REGISTRY_TOKEN_PATH|${WORK}/site/registry-token|; s|REPLACE_TENANT_USER|nvke|; s|REPLACE_TENANT_PASSWORD_PATH|${WORK}/site/tenant-password|" \
    -e "s|REPLACE_DB_PASSWORD_PATH|${WORK}/site/db-password|" \
    "${REPO}/cli/examples/sitespec.yaml" >"${WORK}/site/sitespec.yaml"
# deploy/nico enables profiles.image without a pin, so the spec names the image.
sed -i.bak 's|^  # images:$|  images:|; s|^  #   profiles: ""$|    profiles: ghcr.io/example/controller-profiles:v1|' "${WORK}/site/sitespec.yaml" && rm -f "${WORK}/site/sitespec.yaml.bak"
printf -- '-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n' >"${WORK}/site/ca.crt"
printf -- '-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n' >"${WORK}/site/ca.key"
printf 'spire:\n  caCertFile: %s\n  caKeyFile: %s\n' "${WORK}/site/ca.crt" "${WORK}/site/ca.key" >>"${WORK}/site/sitespec.yaml"
rendered="${WORK}/rendered"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${WORK}/site/sitespec.yaml" --out "${rendered}" 2>&1)" || fail "render exited $?: ${out}"
expect "render reports the site values" '^controller: site values written$' "${out}"
expect "render reports the templates" '^nvke-base-config: 4 templates rendered; applies as a manifest item$' "${out}"
expect "a missing database password file is created" "^database password generated and written to ${WORK}/site/db-password$" "${out}"
[[ -s "${WORK}/site/db-password" && ! -e "${rendered}/db-password" ]] && pass "the password lives in the operator's file, not the tree" || fail "db password placement"
find "${rendered}" -maxdepth 0 -perm 0700 | grep -q . && pass "the rendered tree is private" || fail "rendered tree mode"
[[ ! -e "${NICO}/site.env" && ! -e "${NICO}/controller/site-values.yaml" ]] && pass "nothing was written into the committed deploy path" || fail "committed deploy path written"
grep -q '^  - site-values.yaml$' "${rendered}/controller/release.yaml" && pass "the controller's values list ends with site-values.yaml" || fail "controller release.yaml: $(cat "${rendered}/controller/release.yaml")"
grep -q 'token: "secret-nico-token"' "${rendered}/nvke-base-config/20-nico-credentials.yaml" && pass "the NICo Secret is rendered from the token file" || fail "nico credentials"
[[ -z "$(ls "${rendered}/nvke-base-config/"*.tmpl 2>/dev/null)" ]] && pass "no template is left in the rendered item" || fail "templates left"
grep -q 'name: nvke-base-config, type: manifest' "${rendered}/nvke-deploy.yaml" && pass "the template item became a manifest item" || fail "item list: $(cat "${rendered}/nvke-deploy.yaml")"
grep -q '^SITE_NAME=nvke-site$' "${rendered}/site.env" && pass "site.env names the site" || fail "site.env"
expect "the operator is told which files hold credentials" "^${rendered} holds this site's credentials: controller/site-values.yaml, postgresql/site-values.yaml, temporal/site-values.yaml, nvke-base-config/20-nico-credentials.yaml, .*\. Delete it after the install\.$" "${out}"
[[ "$(cat "${rendered}/.gitignore")" == "*" ]] && pass "the rendered tree ignores itself for git" || fail ".gitignore"

out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --dry-run 2>&1)" || fail "rendered dry run exited $?"
expect "the controller takes its site values" "helm upgrade --install nke-controller .*--values ${rendered}/controller/values.yaml --values ${rendered}/controller/overlay.yaml --values ${rendered}/controller/site-values.yaml" "${out}"
expect "spire takes its site values" "helm upgrade --install spire .*--values ${rendered}/spire/site-values.yaml" "${out}"
expect "spire reads the CA from the operator's files" "helm upgrade --install spire .*--set-file spire-server.upstreamAuthority.disk.secret.data.certificate=${WORK}/site/ca.crt --set-file spire-server.upstreamAuthority.disk.secret.data.key=${WORK}/site/ca.key --set-file spire-server.upstreamAuthority.disk.secret.data.bundle=${WORK}/site/ca.crt" "${out}"
! grep -q 'BEGIN' "${rendered}/spire/site-values.yaml" && pass "the CA is not copied into the spire values" || fail "spire site values hold PEM"
expect "the rendered Secret is applied as a manifest" "^kubectl apply --server-side -f ${rendered}/nvke-base-config/20-nico-credentials.yaml" "${out}"
expect "the namespaces manifest is applied too" "^kubectl apply --server-side -f ${rendered}/nvke-base-config/00-namespaces.yaml" "${out}"
reject "no python template render for a rendered path" 'render-template' "${out}"
expect "postgresql still comes from its public repository" 'helm upgrade --install postgres oci://registry-1.docker.io/bitnamicharts/postgresql --version 18.5.15 ' "${out}"
expect "a committed chart path is absolute in the rendered tree, so it resolves from anywhere" "helm upgrade --install nktp ${REPO}/nktp/deploy/charts/nktp " "${out}"
expect "the controller chart too" "helm upgrade --install nke-controller ${REPO}/controller/deploy/charts/controller " "${out}"
# A secret that cannot be written into a quoted YAML value is refused.
mkdir -p "${WORK}/bad-site"
cp "${WORK}/site/"* "${WORK}/bad-site/"
printf 'tok"en\n' >"${WORK}/bad-site/nico-token"
sed "s|${WORK}/site/|${WORK}/bad-site/|g" "${WORK}/site/sitespec.yaml" >"${WORK}/bad-site/sitespec.yaml"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${WORK}/bad-site/sitespec.yaml" --out "${WORK}/bad-rendered" 2>&1)" && fail "a quote in a token exited 0"
expect "the unescapable token is refused by field" 'controllerStack.nico.tokenFile: the value holds a character the templates cannot carry' "${out}"

# Without the allowance, an --out under a temp directory is refused.
out="$(env -u NVKECTL_RENDER_ALLOW_TEMP_OUT "${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${WORK}/site/sitespec.yaml" --out "${WORK}/refused-temp" 2>&1)" && fail "temp --out exited 0"
expect "a temp --out is refused" 'is under a shared temp directory' "${out}"
[[ ! -e "${WORK}/refused-temp" ]] && pass "a refused temp --out is not created" || fail "temp out created"

# Render refuses a site spec with placeholders, naming them, and leaves nothing.
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${REPO}/cli/examples/sitespec.yaml" --out "${WORK}/refused" 2>&1)" && fail "render with placeholders exited 0"
expect "placeholders are named" 'controllerStack.arch is still a REPLACE_\* placeholder' "${out}"
[[ ! -e "${WORK}/refused" ]] && pass "a refused render leaves no output" || fail "refused output left behind"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${WORK}/site/sitespec.yaml" --out "${rendered}" 2>&1)" && fail "existing --out exited 0"
expect "an existing --out is refused" 'exists; render writes a new directory' "${out}"

# A parser failure stops the install before anything is applied.
mkdir -p "${WORK}/broken-list"
cp -R "${NICO}/." "${WORK}/broken-list/"
printf 'items:\n  - {name: nvke-base-config type: template}\n' >"${WORK}/broken-list/nvke-deploy.yaml"
out="$(bash "${EXECUTOR}" install --deploy-path "${WORK}/broken-list" --config "${CONFIG}" --dry-run 2>&1)" && fail "malformed nvke-deploy.yaml exited 0"
expect "malformed nvke-deploy.yaml is named" "cannot parse ${WORK}/broken-list/nvke-deploy.yaml" "${out}"
reject "nothing is applied after a malformed nvke-deploy.yaml" '^(kubectl|helm) ' "${out}"
mkdir -p "${WORK}/broken-release"
cp -R "${NICO}/." "${WORK}/broken-release/"
printf 'chart:\n  name: [\n' >"${WORK}/broken-release/postgresql/release.yaml"
out="$(bash "${EXECUTOR}" install --deploy-path "${WORK}/broken-release" --config "${CONFIG}" --dry-run 2>&1)" && fail "malformed release.yaml exited 0"
expect "malformed release.yaml is named" "item postgresql: cannot parse ${WORK}/broken-release/postgresql/release.yaml" "${out}"
reject "no helm call for the item with the malformed release.yaml" 'helm upgrade --install postgres ' "${out}"

# Errors name their cause.
out="$(bash "${EXECUTOR}" install --config "${CONFIG}" --dry-run 2>&1)" && fail "no --deploy-path exited 0"
expect "missing --deploy-path is named" 'deploy-path is required' "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --dry-run 2>&1)" && fail "template item without --config exited 0"
expect "template item needs --config" 'needs --config' "${out}"

if [[ "${failures}" -eq 0 ]]; then
  echo "all nvke-deploy tests passed"
else
  echo "${failures} nvke-deploy test(s) failed"
  exit 1
fi
