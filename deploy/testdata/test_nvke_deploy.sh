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
#
# Three deploy paths are installed in dry-run: the committed one, one rendered
# for a site, and one rendered against a release from a local NVKR Run.
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

hex_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# A deploy path rendered against a release from a local NVKR Run: the Run's
# packages/ holds one OCI layout per artifact, and the release description
# names them with digests that match. The refs' host is only a name.
# fake_layout DIR NAME MANIFEST_JSON [BLOB]: writes an OCI layout holding the
# manifest (and the blob it names) and prints the manifest digest.
fake_layout() {
  local dir="$1/$2" json="$3" blob="${4:-}" m
  mkdir -p "${dir}/blobs/sha256"
  printf '{"imageLayoutVersion":"1.0.0"}' >"${dir}/oci-layout"
  printf '%s' "${json}" >"${dir}/manifest.json"
  m="$(hex_of "${dir}/manifest.json")"
  mv "${dir}/manifest.json" "${dir}/blobs/sha256/${m}"
  [[ -z "${blob}" ]] || cp "${blob}" "${dir}/blobs/sha256/$(hex_of "${blob}")"
  printf '{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%s}]}' "${m}" "$(wc -c <"${dir}/blobs/sha256/${m}" | tr -d ' ')" >"${dir}/index.json"
  printf 'sha256:%s' "${m}"
}
run_dir="${WORK}/run"
packages="${run_dir}/packages"
printf 'not really a chart\n' >"${WORK}/chart.tgz"
layer="$(hex_of "${WORK}/chart.tgz")"
d_cwe="$(fake_layout "${packages}" cwe '{"schemaVersion":2,"config":{"digest":"sha256:0"},"layers":[]}')"
d_capnico="$(fake_layout "${packages}" capnico-controller-arm64 '{"schemaVersion":2,"config":{"digest":"sha256:1"},"layers":[]}')"
d_profiles="$(fake_layout "${packages}" controller-profiles '{"schemaVersion":2,"config":{"digest":"sha256:2"},"layers":[]}')"
d_temporal="$(fake_layout "${packages}" temporal "{\"schemaVersion\":2,\"layers\":[{\"mediaType\":\"application/vnd.cncf.helm.chart.content.v1.tar+gzip\",\"digest\":\"sha256:${layer}\",\"size\":$(wc -c <"${WORK}/chart.tgz" | tr -d ' ')}]}" "${WORK}/chart.tgz")"
art() { printf '{"name":"%s","kind":"%s","digest":"%s","reference":"nvkr.local/nvke/%s@%s"%s}' "$1" "$2" "$3" "$1" "$3" "${4:-}"; }
zeros="$(printf '0%.0s' $(seq 1 64))"
cat >"${WORK}/local.json" <<JSON
{"schemaVersion":"nvkr.dev/release/v4","subject":"nvkr.local/nvke/releases@sha256:${zeros}","lane":"unqualified-local-execution","scope":"none",
 "artifacts":[$(art cwe oci "${d_cwe}"),$(art capnico-controller-arm64 oci "${d_capnico}" ',"arch":"linux/arm64"'),$(art controller-profiles oci "${d_profiles}"),$(art temporal helm-oci "${d_temporal}")],
 "contentSubject":"sha256:${zeros}","policyDigest":"sha256:${zeros}"}
JSON
rendered="${WORK}/rendered-release"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --release "${WORK}/local.json" --nvkr-run-dir "${run_dir}" --arch linux/arm64 --out "${rendered}" 2>&1)" || fail "render exited $?: ${out}"
expect "render reports the images" "^images: nvkr.local/nvke/capnico-controller-arm64@${d_capnico}, nvkr.local/nvke/cwe@${d_cwe}, nvkr.local/nvke/controller-profiles@${d_profiles}$" "${out}"
expect "render says where a missing chart comes from" '^nktp: nktp is not in the release; the chart comes from the deploy path or its repository$' "${out}"
[[ -f "${rendered}/charts/temporal/temporal-${d_temporal#sha256:}.tgz" ]] && pass "the temporal archive is in the rendered tree" || fail "temporal archive missing"
[[ "$(cat "${rendered}/charts/temporal/temporal-${d_temporal#sha256:}.tgz")" == "not really a chart" ]] && pass "the archive is the layout's blob" || fail "archive content"
grep -q '^  path: charts/temporal/temporal-' "${rendered}/temporal/release.yaml" && pass "temporal release.yaml points at the archive" || fail "temporal release.yaml"
grep -q '^  repository: https://go.temporal.io' "${NICO}/temporal/release.yaml" && pass "the committed release.yaml is untouched" || fail "committed temporal release.yaml changed"
[[ ! -e "${NICO}/charts" && ! -e "${NICO}/images.txt" ]] && pass "nothing was written into the committed deploy path" || fail "committed deploy path written"
grep -q "^nvkr.local/nvke/cwe@${d_cwe}	${packages}/cwe$" "${rendered}/images.txt" && pass "images.txt names the cwe layout" || fail "images.txt: $(cat "${rendered}/images.txt")"
[[ -f "${rendered}/nvkr-release.json" ]] && pass "the release description travels with the render" || fail "nvkr-release.json missing"

# The executor installs the rendered tree with no release knowledge: charts
# from the archives, images by digest, imports before anything is applied.
out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --config "${CONFIG}" --dry-run 2>&1)" || fail "rendered dry run exited $?"
expect "temporal installed from the archive" "helm upgrade --install temporal ${rendered}/charts/temporal/temporal-${d_temporal#sha256:}\\.tgz " "${out}"
reject "no dependency build for an archive" 'helm dependency build .*temporal' "${out}"
expect "postgresql still comes from its public repository" 'helm upgrade --install postgres oci://registry-1.docker.io/bitnamicharts/postgresql --version 18.5.15 ' "${out}"
expect "nktp still comes from the checkout" 'helm upgrade --install nktp .*/nktp/deploy/charts/nktp ' "${out}"
expect "the controller takes its images from release-images.yaml" "helm upgrade --install nke-controller .*/controller/deploy/charts/controller .*--values ${rendered}/controller/release-images.yaml" "${out}"
grep -q "repository: \"nvkr.local/nvke/cwe\"" "${rendered}/controller/release-images.yaml" && pass "release-images.yaml carries the cwe repository" || fail "release-images.yaml"
grep -q "digest: \"${d_profiles}\"" "${rendered}/controller/release-images.yaml" && pass "and the profiles digest" || fail "profiles digest"
expect "capnico is applied through a kustomization" "^kubectl kustomize ${rendered}/capnico" "${out}"
expect "the kustomization names the local image" "^=== kustomization: nvkr.local/nvke/capnico-controller-arm64@${d_capnico}$" "${out}"
grep -q '^  - capnico.yaml$' "${rendered}/capnico/kustomization.yaml" && pass "the kustomization lists the committed manifest" || fail "kustomization resources"
reject "release.yaml is not a kustomization resource" 'release.yaml' "$(cat "${rendered}/capnico/kustomization.yaml")"
for image in cwe capnico-controller-arm64 controller-profiles; do
  expect "${image} is imported from its layout" "^tar -c -C ${packages}/${image} \\. ?$" "${out}"
  expect "${image} is named for the kubelet" "^ctr -n k8s.io images import --local --digests --base-name nvkr.local/nvke/${image} - ?$" "${out}"
  expect "${image} is checked after the import" "^ctr -n k8s.io images ls -q name==nvkr.local/nvke/${image}@sha256:[0-9a-f]{64} ?$" "${out}"
done
expect "images are imported before the first item" '^(tar|ctr) .*' "$(sed -n '1,/^=== item nvke-base-config/p' <<<"${out}")"
# CTR may be a command line, as it is for a kind node.
out="$(CTR="docker exec -i kind-control-plane ctr" bash "${EXECUTOR}" install --deploy-path "${rendered}" --config "${CONFIG}" --dry-run 2>&1)" || fail "kind CTR dry run exited $?"
expect "images are imported into the kind node" '^docker exec -i kind-control-plane ctr -n k8s.io images import --local --digests --base-name nvkr.local/nvke/cwe - ?$' "${out}"
# A layout that went missing after the render stops the install.
mv "${packages}/cwe" "${packages}/cwe.gone"
out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --config "${CONFIG}" --dry-run 2>&1)" && fail "missing layout exited 0"
expect "the missing layout is named" "image nvkr.local/nvke/cwe@${d_cwe}: its OCI layout ${packages}/cwe is missing" "${out}"
reject "nothing is applied when a layout is missing" '^(kubectl|helm) ' "${out}"
mv "${packages}/cwe.gone" "${packages}/cwe"

# Render refuses, naming every problem at once, and leaves nothing behind.
cat >"${WORK}/no-cwe.json" <<JSON
{"schemaVersion":"nvkr.dev/release/v4","subject":"nvkr.local/nvke/releases@sha256:${zeros}","lane":"unqualified-local-execution","scope":"none",
 "artifacts":[$(art capnico-controller-arm64 oci "${d_capnico}" ',"arch":"linux/arm64"'),$(art controller-profiles oci "${d_profiles}")],
 "contentSubject":"sha256:${zeros}","policyDigest":"sha256:${zeros}"}
JSON
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --release "${WORK}/no-cwe.json" --nvkr-run-dir "${run_dir}" --arch linux/amd64 --out "${WORK}/refused" 2>&1)" && fail "render without cwe exited 0"
expect "the missing image artifact is named" 'controller: image cwe is not in the release' "${out}"
expect "the wrong architecture is named" 'capnico: image: capnico-controller is published only for other architectures' "${out}"
[[ ! -e "${WORK}/refused" ]] && pass "a refused render leaves no output" || fail "refused output left behind"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --release "${HERE}/nvkr-release/bad-schema.json" --out "${WORK}/bad" 2>&1)" && fail "bad description exited 0"
expect "a bad description is named" 'bad-schema.json' "${out}"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --release "${WORK}/local.json" --nvkr-run-dir "${run_dir}" --arch linux/arm64 --out "${rendered}" 2>&1)" && fail "existing --out exited 0"
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
