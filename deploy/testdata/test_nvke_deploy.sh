#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Tests for nvke-deploy.sh against deploy/nico, committed and rendered for a
# site, in --dry-run: the item order, where each chart comes from, and the
# refusals, and check-site.sh against stub tools. Needs python3 and nvkectl:
# NVKECTL names the binary, else the one on PATH; make -C deploy test builds it.
#
# Usage: ./test_nvke_deploy.sh
#
# Three deploy paths are installed in dry-run: the committed one, one rendered
# for a site against stack artifacts with local layouts, and one rendered
# against the artifacts alone. One wrapped install runs against the stub tools
# for real, for the install record.
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
# The site spec's environment overrides would change every spec this suite
# loads; an operator's shell may export them, under either prefix.
for name in INFRA_PROVIDER SPIRE_EXTERNAL_ADDRESS CLUSTER_NODES SPIFFE_TRUST_DOMAIN; do
  unset "NVKECTL_${name}" "NKECTL_${name}"
done

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

# Stub tools. The kubectl stub answers the exact reads check-site.sh and the
# executor make; STUB_* switch the answers. Every executor run that may read
# the cluster goes through it, so the suite never reaches a real one.
CHECK="${DEPLOY_DIR}/check-site.sh"
stubs="${WORK}/stubs"
mkdir -p "${stubs}"
cat >"${stubs}/kubectl" <<'STUB'
#!/usr/bin/env bash
# Every call is logged; a mutating verb fails, so "changes nothing" is a
# fact about the calls made, not about the output. STUB_ALLOW_MUTATE lets
# an install through, for the record it writes.
[[ -z "${STUB_LOG:-}" ]] || printf '%s\n' "$*" >>"${STUB_LOG}"
if [[ -z "${STUB_ALLOW_MUTATE:-}" ]]; then
  case " $* " in
    *" apply "*|*" create "*|*" delete "*|*" patch "*|*" taint "*|*" annotate "*|*" label "*|*" replace "*|*" edit "*) echo "stub kubectl: mutating verb refused: $*" >&2; exit 97 ;;
  esac
fi
case " $* " in *" kustomize "*) cat >/dev/null 2>&1 || true; exit 0 ;; esac
case " $* " in *" apply "*) cat >/dev/null 2>&1 || true; exit 0 ;; esac
if [[ -n "${STUB_API_DOWN:-}" ]]; then
  echo 'The connection to the server 10.0.0.100:6443 was refused' >&2; exit 1
fi
if [[ -n "${STUB_DENY:-}" && "$*" == *" get "* ]]; then
  echo 'Error from server (Forbidden): nodes is forbidden: User "stub" cannot list resource "nodes"' >&2
  exit 1
fi
case "$*" in
  *"get nodes"*)
    if [[ -n "${STUB_NO_NODES:-}" ]]; then exit 0; fi
    if [[ -n "${STUB_NO_ARCH_LABEL:-}" ]]; then printf 'node-1\t\t%s\n' "${STUB_TAINT:-}"; exit 0; fi
    printf 'node-1\t%s\t%s\n' "${STUB_ARCH:-arm64}" "${STUB_TAINT:-}"
    [[ -z "${STUB_SECOND_NODE:-}" ]] || printf 'node-2\t%s\t\n' "${STUB_ARCH:-arm64}"
    [[ -z "${STUB_MIXED_ARCH:-}" ]] || printf 'node-2\tamd64\t\n' ;;
  *"get storageclass"*)
    [[ -n "${STUB_NO_SC:-}" ]] || printf 'local-path\ttrue\n'
    [[ -z "${STUB_SECOND_SC:-}" ]] || printf 'nvme\ttrue\n' ;;
  *"capi-controller-manager"*)
    if [[ -n "${STUB_CAPI_IMAGE:-}" ]]; then printf '%s' "${STUB_CAPI_IMAGE}"; else
      echo 'Error from server (NotFound): deployments.apps "capi-controller-manager" not found' >&2; exit 1; fi ;;
  *"capi-kubeadm-"*)
    if [[ -n "${STUB_DENY_KUBEADM:-}" ]]; then echo 'Error from server (Forbidden): deployments.apps is forbidden' >&2; exit 1; fi
    if [[ -n "${STUB_NO_KUBEADM:-}" ]]; then echo 'Error from server (NotFound): deployments.apps not found' >&2; exit 1; else printf 'deployment.apps/x'; fi ;;
esac
exit 0
STUB
printf '#!/usr/bin/env bash\ncase "$1" in version) printf "v4.2.4" ;; esac\nexit 0\n' >"${stubs}/helm"
printf '#!/usr/bin/env bash\nexit 0\n' >"${stubs}/clusterctl"
printf '#!/usr/bin/env bash\n[[ -z "${CURL_LOG:-}" ]] || printf "%%s\\n" "$*" >>"${CURL_LOG}"\nexit 0\n' >"${stubs}/curl"
# ctr answers the one query the import makes after it: the image is there.
cat >"${stubs}/ctr" <<'STUB'
#!/usr/bin/env bash
cat >/dev/null 2>&1 || true
all="$*"
case "${all}" in *"images ls -q name=="*) printf '%s\n' "${all##*name==}" ;; esac
exit 0
STUB
# nvkectl-log records every nvkectl invocation and runs the real one, so the
# suite can see the arguments the executor passes to the render.
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" >>"${NVKECTL_LOG}"\nexec "${REAL_NVKECTL}" "$@"\n' >"${stubs}/nvkectl-log"
chmod +x "${stubs}"/*
: >"${WORK}/stub.kubeconfig"
: >"${WORK}/stub.log"

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
# result with no --config and no knowledge of the site. The two inputs sit in
# different directories, so a value from the wrong one is seen: the site spec
# under site/, the stack config under stack/. The credentials come from the
# environment.
mkdir -p "${WORK}/site" "${WORK}/stack"
export NVKECTL_NICO_TOKEN=secret-nico-token NVKECTL_NODE_USER=nvke NVKECTL_NODE_PASSWORD=secret-tenant-password NVKECTL_DATABASE_PASSWORD=secret-db-password
cp "${REPO}/cli/examples/sitespec.yaml" "${WORK}/site/sitespec.yaml"
printf -- '-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n' >"${WORK}/site/ca.crt"
printf -- '-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n' >"${WORK}/site/ca.key"
export NVKECTL_SPIRE_CA_CERT_FILE="${WORK}/site/ca.crt" NVKECTL_SPIRE_CA_KEY_FILE="${WORK}/site/ca.key"
printf 'spire:\n  trustDomain: nvke-site.nke.local\n  upstreamCA: true\n' >>"${WORK}/site/sitespec.yaml"
SITESPEC="${WORK}/site/sitespec.yaml"
# The example, filled in.
sed -e 's|REPLACE_NICO_ENDPOINT|https://nico.example.com|; s|REPLACE_NICO_ORG_ID|org-1|; s|REPLACE_NICO_API_NAME|forge|' \
    "${REPO}/deploy/examples/stack-config.json" >"${WORK}/stack/stack-config.json"
STACK="${WORK}/stack/stack-config.json"
# A stack config that still carries the image or mirror keys is refused.
sed -e 's|^}$|,"images":{"controller":"","profiles":""},"mirror":""}|' "${STACK}" | tr -d '\n' | sed 's|}\s*,|},|' >"${WORK}/stack/old-stack-config.json"
out="$("${NVKECTL}" inspect stack-config "${WORK}/stack/old-stack-config.json" 2>&1)" && fail "a stack config with images and mirror loaded"
expect "images is an unknown key of the stack config" '^images: unknown field$' "${out}"
expect "mirror is an unknown key of the stack config" '^mirror: unknown field$' "${out}"

hex_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# The stack artifacts with local layouts: one OCI layout per artifact under
# layouts/, and the file names them by relative path, as make
# local-stack-artifacts writes it. The refs' host is only a name.
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
layouts="${WORK}/layouts"
printf 'not really a chart\n' >"${WORK}/chart.tgz"
layer="$(hex_of "${WORK}/chart.tgz")"
d_cwe="$(fake_layout "${layouts}" cwe '{"schemaVersion":2,"config":{"digest":"sha256:0"},"layers":[]}')"
d_capnico="$(fake_layout "${layouts}" capnico-controller-arm64 '{"schemaVersion":2,"config":{"digest":"sha256:1"},"layers":[]}')"
d_capnico_amd="$(fake_layout "${layouts}" capnico-controller '{"schemaVersion":2,"config":{"digest":"sha256:3"},"layers":[]}')"
d_profiles="$(fake_layout "${layouts}" controller-profiles '{"schemaVersion":2,"config":{"digest":"sha256:2"},"layers":[]}')"
d_temporal="$(fake_layout "${layouts}" temporal "{\"schemaVersion\":2,\"layers\":[{\"mediaType\":\"application/vnd.cncf.helm.chart.content.v1.tar+gzip\",\"digest\":\"sha256:${layer}\",\"size\":$(wc -c <"${WORK}/chart.tgz" | tr -d ' ')}]}" "${WORK}/chart.tgz")"
# art NAME KIND DIGEST [EXTRA]: one artifact with a layout under layouts/NAME.
art() { printf '{"name":"%s","kind":"%s","digest":"%s","reference":"nvke.local/%s@%s","layout":"layouts/%s"%s}' "$1" "$2" "$3" "$1" "$3" "$1" "${4:-}"; }
cat >"${WORK}/local-stack-artifacts.json" <<JSON
{"schemaVersion":"nvke.nvidia.com/stack-artifacts/v1",
 "artifacts":[$(art cwe oci "${d_cwe}"),$(art capnico-controller-arm64 oci "${d_capnico}" ',"os":"linux","arch":"arm64"'),$(art capnico-controller oci "${d_capnico_amd}" ',"os":"linux","arch":"amd64"'),$(art controller-profiles oci "${d_profiles}"),$(art temporal helm-oci "${d_temporal}" ',"chartVersion":"0.74.0"')]}
JSON
LOCAL="${WORK}/local-stack-artifacts.json"
out="$("${NVKECTL}" inspect stack-artifacts "${LOCAL}" --meta 2>&1)" || fail "the local artifacts do not load: ${out}"
expect "inspect stack-artifacts prints the file" "^STACK_ARTIFACTS='${LOCAL}'$" "${out}"
expect "and counts the layouts" "^LAYOUTS='5'$" "${out}"
expect "every layout is there" "^LAYOUTS_MISSING=''$" "${out}"
expect "a layout's host is not pulled from" "^PULL_HOSTS=''$" "${out}"
out="$("${NVKECTL}" inspect stack-config "${STACK}" 2>&1)" || fail "the filled example does not load: ${out}"
expect "inspect stack-config prints the file" "^STACK_CONFIG='${STACK}'$" "${out}"
expect "inspect stack-config prints the NICo endpoint" "^STACK_NICO_ENDPOINT='https://nico.example.com'$" "${out}"
expect "and the org" "^STACK_NICO_ORG_ID='org-1'$" "${out}"
out="$("${NVKECTL}" inspect stack-config "${REPO}/deploy/examples/stack-config.json" 2>&1 >"${WORK}/inspect.stdout")" && fail "the example with its placeholders loaded"
[[ ! -s "${WORK}/inspect.stdout" ]] && pass "a bad stack config prints nothing on stdout" || fail "inspect stack-config wrote to stdout: $(cat "${WORK}/inspect.stdout")"
expect "the first problem is on stderr" '^nico.endpoint: still a REPLACE_\* placeholder; fill it in$' "${out}"
out="$(NVKECTL_NODE_PASSWORD='' "${NVKECTL}" inspect stack-config "${STACK}" 2>&1 >"${WORK}/inspect.stdout")" && fail "inspect stack-config without a credential exited 0"
[[ ! -s "${WORK}/inspect.stdout" ]] && pass "a missing credential prints nothing on stdout" || fail "inspect stack-config wrote to stdout without a credential"
expect "the missing credential is named as the environment's" '^environment: NVKECTL_NODE_PASSWORD is not set$' "${out}"
rendered="${WORK}/rendered"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --out "${WORK}/site-alone" 2>&1)" && fail "a site render without the artifacts exited 0"
expect "a site alone is refused: the artifacts name the profiles image" 'the deploy path enables profiles.image and nothing names the image; pass --stack-artifacts with a controller-profiles artifact' "${out}"
[[ ! -e "${WORK}/site-alone" ]] && pass "a refused render leaves no output" || fail "refused output left behind"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --out "${rendered}" 2>&1)" || fail "render exited $?: ${out}"
expect "render reports the site values" '^controller: site values written$' "${out}"
expect "render reports the artifacts" '^stack-artifacts: no source block$' "${out}"
expect "render reports the templates" '^nvke-base-config: 3 templates rendered; applies as a manifest item$' "${out}"
reject "no host-default warning when --arch is given" 'warning: --arch not given' "${out}"
grep -q 'dbSecretName: nvke-database' "${rendered}/controller/site-values.yaml" && pass "the controller reads its connection string from the nvke-database Secret" || fail "controller site values: $(cat "${rendered}/controller/site-values.yaml")"
grep -q 'existingSecret: nvke-database' "${rendered}/postgresql/site-values.yaml" && grep -c 'existingSecret: nvke-database' "${rendered}/temporal/site-values.yaml" | grep -qx 2 && pass "postgresql and both temporal datastores read the same Secret" || fail "postgresql/temporal site values"
! grep -rq 'secret-db-password\|BEGIN' "${rendered}"/*/site-values.yaml && pass "no values file holds a credential" || fail "a values file holds a credential"
grep -q 'name: site-credentials, type: manifest' "${rendered}/nvke-deploy.yaml" && pass "the render adds the site-credentials item" || fail "item list: $(cat "${rendered}/nvke-deploy.yaml")"
sed -n '/^items:/,$p' "${rendered}/nvke-deploy.yaml" | sed -n 3p | grep -q 'name: site-credentials' && pass "the site-credentials item follows the first item" || fail "item order: $(cat "${rendered}/nvke-deploy.yaml")"
grep -q "database-connection-string: $(printf 'postgresql://postgres:secret-db-password@postgres-postgresql.postgres.svc.cluster.local:5432/nke-controller?sslmode=disable' | base64 | tr -d '\n')$" "${rendered}/site-credentials/secrets.yaml" && pass "the Secrets item holds the controller's connection string, base64 in data" || fail "secrets.yaml: $(cat "${rendered}/site-credentials/secrets.yaml")"
grep -q "tls.key: $(base64 <"${WORK}/site/ca.key" | tr -d '\n')$" "${rendered}/site-credentials/secrets.yaml" && grep -q 'namespace: spire-server' "${rendered}/site-credentials/secrets.yaml" && pass "and the SPIRE CA Secret" || fail "secrets.yaml lacks the CA"
reject "the Secrets use data, not stringData" 'stringData' "$(cat "${rendered}/site-credentials/secrets.yaml")"
grep -q 'bundle: bundle.crt' "${rendered}/spire/site-values.yaml" && pass "the spire values keep the bundle path on" || fail "spire bundle marker missing"
find "${rendered}" -maxdepth 0 -perm 0700 | grep -q . && pass "the rendered tree is private" || fail "rendered tree mode"
[[ ! -e "${NICO}/site.env" && ! -e "${NICO}/controller/site-values.yaml" ]] && pass "nothing was written into the committed deploy path" || fail "committed deploy path written"
grep -q '^  - site-values.yaml$' "${rendered}/controller/release.yaml" && pass "the controller's values list ends with site-values.yaml" || fail "controller release.yaml: $(cat "${rendered}/controller/release.yaml")"
grep -q 'token: "secret-nico-token"' "${rendered}/nvke-base-config/20-nico-credentials.yaml" && pass "the NICo Secret is rendered from NVKECTL_NICO_TOKEN" || fail "nico credentials"
[[ -z "$(ls "${rendered}/nvke-base-config/"*.tmpl 2>/dev/null)" ]] && pass "no template is left in the rendered item" || fail "templates left"
grep -q 'name: nvke-base-config, type: manifest' "${rendered}/nvke-deploy.yaml" && pass "the template item became a manifest item" || fail "item list: $(cat "${rendered}/nvke-deploy.yaml")"
grep -q '^SITE_NAME=nvke-site$' "${rendered}/site.env" && grep -q '^SITE_TRUST_DOMAIN=nvke-site.nke.local$' "${rendered}/site.env" && pass "site.env names the site and its trust domain" || fail "site.env"
expect "the operator is told which files hold credentials" "^${rendered} holds this site's credentials: site-credentials/secrets.yaml, nvke-base-config/20-nico-credentials.yaml, .*\. Delete it after the install\.$" "${out}"
[[ "$(cat "${rendered}/.gitignore")" == "*" ]] && pass "the rendered tree ignores itself for git" || fail ".gitignore"

# External credentials: no credential or CA variable read, no secret written,
# the Secrets listed.
out="$(env -u NVKECTL_NICO_TOKEN -u NVKECTL_NODE_USER -u NVKECTL_NODE_PASSWORD -u NVKECTL_DATABASE_PASSWORD -u NVKECTL_SPIRE_CA_CERT_FILE -u NVKECTL_SPIRE_CA_KEY_FILE "${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --credentials external --out "${WORK}/external" 2>&1)" || fail "external render exited $?: ${out}"
expect "the external render holds no credential" "^${WORK}/external holds no credential\.$" "${out}"
expect "the external render names the database Secret" '^expected in the cluster once its namespace exists and before the item that reads it: Secret postgres/nvke-database with keys password, postgres-password$' "${out}"
expect "and the NICo Secret" '^expected in the cluster once its namespace exists and before the item that reads it: Secret capnico-system/nico-credentials with keys ' "${out}"
expect "and the CA Secret" '^expected in the cluster once its namespace exists and before the item that reads it: Secret spire-server/spire-upstream-ca with keys bundle.crt, tls.crt, tls.key$' "${out}"
expect "the external render shows the connection string shape" '^expected in the cluster once its namespace exists and before the item that reads it: Secret nke-controller/nvke-database with keys database-connection-string, the URL postgresql://postgres:<password>@postgres-postgresql.postgres.svc.cluster.local:5432/nke-controller\?sslmode=disable with the password URL-escaped$' "${out}"
[[ ! -e "${WORK}/external/site-credentials" && ! -e "${WORK}/external/nvke-base-config/20-nico-credentials.yaml" && -e "${WORK}/external/nvke-base-config/45-nktp-cluster.yaml" && ! -e "${WORK}/external/.gitignore" ]] && pass "the external tree carries no Secret and no .gitignore" || fail "external tree: $(ls -a "${WORK}/external" "${WORK}/external/nvke-base-config")"
! grep -rqF -e "${DEPLOY_DIR}" -e "${WORK}" "${WORK}/external" && pass "the external tree names no path of this machine" || fail "the external tree names a local path: $(grep -rnF -e "${DEPLOY_DIR}" -e "${WORK}" "${WORK}/external" | head -3)"
grep -q 'path: ../../controller/deploy/charts/controller' "${WORK}/external/controller/release.yaml" && pass "the external tree keeps the committed chart path" || fail "external controller release.yaml: $(cat "${WORK}/external/controller/release.yaml")"
! grep -q '"layout"' "${WORK}/external/stack-artifacts.json" && pass "the external tree's artifacts copy names no layout" || fail "external stack-artifacts.json names a layout: $(grep -n layout "${WORK}/external/stack-artifacts.json")"

out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --dry-run 2>&1)" || fail "rendered dry run exited $?"
expect "the controller takes its site values" "helm upgrade --install nke-controller .*--values ${rendered}/controller/values.yaml --values ${rendered}/controller/overlay.yaml --values ${rendered}/controller/release-images.yaml --values ${rendered}/controller/site-values.yaml" "${out}"
expect "spire takes its site values" "helm upgrade --install spire .*--values ${rendered}/spire/site-values.yaml" "${out}"
expect "the Secrets item is applied before the charts" "^kubectl apply --server-side -f ${rendered}/site-credentials/secrets.yaml" "${out}"
reject "spire takes no --set-file" 'helm upgrade --install spire .*--set-file' "${out}"

# The controller reads its connection string from the Secret the render built
# from the deploy path's appDb values, so a --values-for file that sets appDb
# would be ignored: the executor refuses it before it applies anything.
printf 'appDb:\n  url: external-pg.example.test:6432\n  dbName: customdb\n' >"${WORK}/appdb-override.yaml"
printf '{"appDb": {"url": "external-pg.example.test:6432"}}\n' >"${WORK}/appdb-override.json"
# Spellings a line-by-line reader misses: a flow mapping after a comment, and a
# top-level mapping indented as a whole.
printf '# Database override\n{appDb: {url: external-pg.example.test:6432}}\n' >"${WORK}/appdb-override-flow.yaml"
printf '  appDb:\n    url: external-pg.example.test:6432\n  image:\n    tag: v-test\n' >"${WORK}/appdb-override-indented.yaml"
printf 'image:\n  tag: v-test\nsecrets:\n  appDb: not-the-top-level\n' >"${WORK}/image-override.yaml"
for f in appdb-override.yaml appdb-override.json appdb-override-flow.yaml appdb-override-indented.yaml; do
  appdb_out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --dry-run --values-for "controller=${WORK}/${f}" 2>&1)" && fail "a controller --values-for ${f} that sets appDb exited 0"
  expect "a ${f} that sets appDb is refused" "^nvke-deploy: --values-for controller: ${WORK}/${f} sets appDb, but controller reads its database connection string from the Secret" "${appdb_out}"
  reject "nothing is applied before the appDb refusal (${f})" 'kubectl .*apply|helm upgrade' "${appdb_out}"
done
expect "the refusal says where appDb is changed" 'change appDb in the deploy path \(for controller, its overlay.yaml\) and run again$' "${appdb_out}"
appdb_out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --dry-run --values-for "controller=${WORK}/image-override.yaml" 2>&1)" || fail "a controller --values-for without appDb exited $?: ${appdb_out}"
expect "a --values-for file without appDb is still applied" "helm upgrade --install nke-controller .*--values ${WORK}/image-override.yaml" "${appdb_out}"
# A deploy path the render did not write holds no Secret: the chart builds the
# connection string from the merged values, so an appDb override is applied.
mkdir -p "${WORK}/handpath/ctl"
printf 'items:\n  - {name: ctl, type: helm}\n' >"${WORK}/handpath/nvke-deploy.yaml"
printf 'chart:\n  name: nke-controller\n  version: "1.0.0"\n  repository: oci://registry.example.test/charts\nrelease:\n  name: nke-controller\n  namespace: nke-controller\n' >"${WORK}/handpath/ctl/release.yaml"
appdb_out="$(bash "${EXECUTOR}" install --deploy-path "${WORK}/handpath" --dry-run --values-for "ctl=${WORK}/appdb-override.yaml" 2>&1)" || fail "an appDb override on a deploy path with no Secret exited $?: ${appdb_out}"
expect "an appDb override is applied where the chart reads appDb" "helm upgrade --install nke-controller .*--values ${WORK}/appdb-override.yaml" "${appdb_out}"
! grep -q 'BEGIN' "${rendered}/spire/site-values.yaml" && pass "the CA is not copied into the spire values" || fail "spire site values hold PEM"
expect "the rendered Secret is applied as a manifest" "^kubectl apply --server-side -f ${rendered}/nvke-base-config/20-nico-credentials.yaml" "${out}"
expect "the namespaces manifest is applied too" "^kubectl apply --server-side -f ${rendered}/nvke-base-config/00-namespaces.yaml" "${out}"
reject "no python template render for a rendered path" 'render-template' "${out}"
expect "postgresql still comes from its public repository" 'helm upgrade --install postgres oci://registry-1.docker.io/bitnamicharts/postgresql --version 18.5.15 ' "${out}"
expect "a committed chart path is absolute in the rendered tree, so it resolves from anywhere" "helm upgrade --install nktp ${REPO}/nktp/deploy/charts/nktp " "${out}"
expect "the controller chart too" "helm upgrade --install nke-controller ${REPO}/controller/deploy/charts/controller " "${out}"
# A secret that cannot be written into a quoted YAML value is refused.
out="$(NVKECTL_NICO_TOKEN='tok"en' "${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --out "${WORK}/bad-rendered" 2>&1)" && fail "a quote in a token exited 0"
expect "the unescapable token is refused by variable" 'NVKECTL_NICO_TOKEN: the value holds a character the templates cannot carry' "${out}"

# Without the allowance, an --out under a temp directory is refused.
out="$(env -u NVKECTL_RENDER_ALLOW_TEMP_OUT "${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --out "${WORK}/refused-temp" 2>&1)" && fail "temp --out exited 0"
expect "a temp --out is refused" 'is under a shared temp directory' "${out}"
[[ ! -e "${WORK}/refused-temp" ]] && pass "a refused temp --out is not created" || fail "temp out created"

# Render refuses a stack config with placeholders, naming the file and every
# placeholder, and leaves nothing.
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${REPO}/deploy/examples/stack-config.json" --stack-artifacts "${LOCAL}" --arch linux/arm64 --out "${WORK}/refused" 2>&1)" && fail "render with placeholders exited 0"
expect "the stack config is named" "^Error: stack config ${REPO}/deploy/examples/stack-config.json:$" "${out}"
expect "placeholders are named" '^nico.endpoint: still a REPLACE_\* placeholder; fill it in$' "${out}"
expect "every placeholder is named" '^nico.apiName: still a REPLACE_\* placeholder; fill it in$' "${out}"
[[ ! -e "${WORK}/refused" ]] && pass "a refused render leaves no output" || fail "refused output left behind"
# The two inputs go together.
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --out "${WORK}/refused" 2>&1)" && fail "render with --sitespec alone exited 0"
expect "the render refuses --sitespec without --stack-config" "^Error: --sitespec needs --stack-config: the stack's input is the stack config file$" "${out}"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --stack-config "${STACK}" --out "${WORK}/refused" 2>&1)" && fail "render with --stack-config alone exited 0"
expect "the render refuses --stack-config without --sitespec" '^Error: --stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec$' "${out}"
[[ ! -e "${WORK}/refused" ]] && pass "a refused render leaves no output" || fail "refused output left behind"
# Without --arch the render uses this machine's architecture and says so.
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --out "${WORK}/host-arch" 2>&1)"
expect "the host-default warning fires without --arch" "^warning: --arch not given; rendering for the host's linux/" "${out}"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --out "${rendered}" 2>&1)" && fail "existing --out exited 0"
expect "an existing --out is refused" 'exists; render writes a new directory' "${out}"
# The hand-written example is the operator's starting point: it loads once
# its placeholders are filled, and names the five artifacts deploy/nico binds.
out="$("${NVKECTL}" inspect stack-artifacts "${REPO}/deploy/examples/stack-artifacts.json" --meta 2>&1)" && fail "the example with its placeholders loaded"
expect "the example's placeholders are refused by name" 'artifact cwe: reference is still a REPLACE_\* placeholder; fill it in' "${out}"
sed -e 's|REPLACE_REGISTRY|registry.example.com|g; s|REPLACE_CHART_VERSION|1.2.3|g' -e "s|REPLACE_DIGEST|$(printf 'a%.0s' $(seq 1 64))|g" "${REPO}/deploy/examples/stack-artifacts.json" >"${WORK}/example-filled.json"
out="$("${NVKECTL}" inspect stack-artifacts "${WORK}/example-filled.json" --bindings --deploy-path "${NICO}" --arch linux/arm64 2>&1)" || fail "the filled example does not load: ${out}"
expect "the filled example binds the controller image" $'^controller\timage\tcwe\tcwe\tbound$' "${out}"
expect "the filled example binds the nktp chart" $'^nktp\tchart\tnktp\tnktp\tbound$' "${out}"
reject "the filled example has no unused artifact" '^unused' "${out}"

# A deploy path rendered against the artifacts alone: the charts the file
# carries come from their layouts, the images are pinned, and the executor
# imports the layouts before anything is applied.
rendered="${WORK}/rendered-artifacts"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --out "${rendered}" 2>&1)" || fail "render exited $?: ${out}"
expect "render reports the images" "^images: nvke.local/capnico-controller-arm64@${d_capnico}, nvke.local/cwe@${d_cwe}, nvke.local/controller-profiles@${d_profiles}$" "${out}"
expect "render says where a missing chart comes from" '^nktp: nktp is not in the stack artifacts; the chart comes from the deploy path or its repository$' "${out}"
[[ -f "${rendered}/charts/temporal/temporal-${d_temporal#sha256:}.tgz" ]] && pass "the temporal archive is in the rendered tree" || fail "temporal archive missing"
[[ "$(cat "${rendered}/charts/temporal/temporal-${d_temporal#sha256:}.tgz")" == "not really a chart" ]] && pass "the archive is the layout's blob" || fail "archive content"
grep -q '^  path: charts/temporal/temporal-' "${rendered}/temporal/release.yaml" && pass "temporal release.yaml points at the archive" || fail "temporal release.yaml"
grep -q '^  repository: https://go.temporal.io' "${NICO}/temporal/release.yaml" && pass "the committed release.yaml is untouched" || fail "committed temporal release.yaml changed"
[[ ! -e "${NICO}/charts" && ! -e "${NICO}/images.txt" ]] && pass "nothing was written into the committed deploy path" || fail "committed deploy path written"
grep -q "^nvke.local/cwe@${d_cwe}	${layouts}/cwe$" "${rendered}/images.txt" && pass "images.txt names the cwe layout, absolute" || fail "images.txt: $(cat "${rendered}/images.txt")"
[[ -f "${rendered}/stack-artifacts.json" ]] && pass "the stack artifacts travel with the render" || fail "stack-artifacts.json missing"
out="$("${NVKECTL}" inspect stack-artifacts "${rendered}/stack-artifacts.json" --artifacts 2>&1)" || fail "the rendered copy does not load: ${out}"
expect "the rendered copy carries absolute layouts" $'^cwe\toci\t.*\t'"${layouts}"'/cwe$' "${out}"
[[ ! -e "${rendered}/nvkr-release.json" ]] && pass "no nvkr-release.json" || fail "nvkr-release.json written"

# The executor installs the rendered tree with no artifact knowledge: charts
# from the archives, images by digest, imports before anything is applied.
out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --config "${CONFIG}" --dry-run 2>&1)" || fail "rendered dry run exited $?"
expect "temporal installed from the archive" "helm upgrade --install temporal ${rendered}/charts/temporal/temporal-${d_temporal#sha256:}\\.tgz " "${out}"
reject "no dependency build for an archive" 'helm dependency build .*temporal' "${out}"
expect "postgresql still comes from its public repository" 'helm upgrade --install postgres oci://registry-1.docker.io/bitnamicharts/postgresql --version 18.5.15 ' "${out}"
expect "nktp still comes from the checkout" 'helm upgrade --install nktp .*/nktp/deploy/charts/nktp ' "${out}"
expect "the controller takes its images from release-images.yaml" "helm upgrade --install nke-controller .*/controller/deploy/charts/controller .*--values ${rendered}/controller/release-images.yaml" "${out}"
grep -q "repository: \"nvke.local/cwe\"" "${rendered}/controller/release-images.yaml" && pass "release-images.yaml carries the cwe repository" || fail "release-images.yaml"
grep -q "digest: \"${d_profiles}\"" "${rendered}/controller/release-images.yaml" && pass "and the profiles digest" || fail "profiles digest"
expect "capnico is applied through a kustomization" "^kubectl kustomize ${rendered}/capnico" "${out}"
expect "the kustomization names the local image" "^=== kustomization: nvke.local/capnico-controller-arm64@${d_capnico}$" "${out}"
grep -q '^  - capnico.yaml$' "${rendered}/capnico/kustomization.yaml" && pass "the kustomization lists the committed manifest" || fail "kustomization resources"
reject "release.yaml is not a kustomization resource" 'release.yaml' "$(cat "${rendered}/capnico/kustomization.yaml")"
for image in cwe capnico-controller-arm64 controller-profiles; do
  expect "${image} is imported from its layout" "^tar -c -C ${layouts}/${image} \\. ?$" "${out}"
  expect "${image} is named for the kubelet" "^ctr -n k8s.io images import --local --digests --base-name nvke.local/${image} - ?$" "${out}"
  expect "${image} is checked after the import" "^ctr -n k8s.io images ls -q name==nvke.local/${image}@sha256:[0-9a-f]{64} ?$" "${out}"
done
expect "images are imported before the first item" '^(tar|ctr) .*' "$(sed -n '1,/^=== item nvke-base-config/p' <<<"${out}")"
# CTR may be a command line, as it is for a container node.
out="$(CTR="nerdctl exec -i nvkectl-e2e-node1 ctr" bash "${EXECUTOR}" install --deploy-path "${rendered}" --config "${CONFIG}" --dry-run 2>&1)" || fail "container-node CTR dry run exited $?"
expect "images are imported into the node container" '^nerdctl exec -i nvkectl-e2e-node1 ctr -n k8s.io images import --local --digests --base-name nvke.local/cwe - ?$' "${out}"
# A layout that went missing after the render stops the install.
mv "${layouts}/cwe" "${layouts}/cwe.gone"
out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --config "${CONFIG}" --dry-run 2>&1)" && fail "missing layout exited 0"
expect "the missing layout is named" "image nvke.local/cwe@${d_cwe}: its OCI layout ${layouts}/cwe is missing" "${out}"
reject "nothing is applied when a layout is missing" '^(kubectl|helm) ' "${out}"
# And the render refuses it up front, naming the artifact.
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --out "${WORK}/refused" 2>&1)" && fail "render with a missing layout exited 0"
expect "the render names the artifact whose layout is missing" "artifact cwe: layout ${layouts}/cwe:" "${out}"
[[ ! -e "${WORK}/refused" ]] && pass "a refused render leaves no output" || fail "refused output left behind"
mv "${layouts}/cwe.gone" "${layouts}/cwe"

# Render refuses, naming every problem at once, and leaves nothing behind.
cat >"${WORK}/no-cwe.json" <<JSON
{"schemaVersion":"nvke.nvidia.com/stack-artifacts/v1",
 "artifacts":[$(art capnico-controller-arm64 oci "${d_capnico}" ',"os":"linux","arch":"arm64"'),$(art controller-profiles oci "${d_profiles}")]}
JSON
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --stack-artifacts "${WORK}/no-cwe.json" --arch linux/amd64 --out "${WORK}/refused" 2>&1)" && fail "render without cwe exited 0"
expect "the missing image artifact is named" 'controller: image cwe is not in the stack artifacts' "${out}"
expect "the wrong architecture is named" 'capnico: image: capnico-controller is published only for other architectures' "${out}"
[[ ! -e "${WORK}/refused" ]] && pass "a refused render leaves no output" || fail "refused output left behind"
# The artifacts decide the version of a chart they carry: the difference from
# the pin is a warning, and the rendered copy records the artifact's version.
cat >"${WORK}/other-chart.json" <<JSON
{"schemaVersion":"nvke.nvidia.com/stack-artifacts/v1",
 "artifacts":[$(art cwe oci "${d_cwe}"),$(art capnico-controller-arm64 oci "${d_capnico}" ',"os":"linux","arch":"arm64"'),$(art controller-profiles oci "${d_profiles}"),$(art temporal helm-oci "${d_temporal}" ',"chartVersion":"9.9.9"'),{"name":"spare","kind":"oci","digest":"${d_cwe}","reference":"nvke.local/spare@${d_cwe}","layout":"layouts/cwe"}]}
JSON
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --stack-artifacts "${WORK}/other-chart.json" --arch linux/arm64 --out "${WORK}/other-chart" 2>&1)" || fail "render with another chart version exited $?: ${out}"
expect "the chart version difference is a warning" '^warning: temporal: the stack artifacts carry temporal 9.9.9, release.yaml pins 0.74.0' "${out}"
grep -q '^  version: "9.9.9"$' "${WORK}/other-chart/temporal/release.yaml" && pass "the rendered copy records the artifact's chart version" || fail "temporal version not recorded"
expect "an artifact no item binds is a note" "^stack-artifacts: spare is not used by ${NICO}$" "${out}"
reject "a bound artifact is not noted as unused" 'stack-artifacts: (cwe|temporal|capnico-controller-arm64|controller-profiles) is not used' "${out}"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --stack-artifacts "${HERE}/stack-artifacts/bad-schema.json" --out "${WORK}/bad" 2>&1)" && fail "bad file exited 0"
expect "a bad file is named" 'bad-schema.json' "${out}"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --stack-artifacts "${REPO}/hack/testdata/nvkr-release/valid.json" --out "${WORK}/bad" 2>&1)" && fail "an NVKR export exited 0"
expect "an NVKR export is refused with the converter" 'this is an NVKR release description; convert it with hack/nvkr-run-to-stack-artifacts.py' "${out}"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --out "${rendered}" 2>&1)" && fail "existing --out exited 0"
expect "an existing --out is refused" 'exists; render writes a new directory' "${out}"

# The executor renders by itself when given the three inputs, into a private
# scratch directory that is gone when it exits. --arch stands in for the
# node read here: there is no cluster.
state="${WORK}/state"
renders="${state}/renders"
installs="${state}/installs"
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --dry-run 2>&1)" || fail "wrapped render dry run exited $?: ${out}"
expect "the executor logs the inputs before it renders" "^=== inputs: site spec ${SITESPEC}, site nvke-site, stack config ${STACK}, nico https://nico.example.com org org-1, stack artifacts ${LOCAL}, CTR ctr$" "${out}"
expect "the executor renders before installing" "^=== rendering ${NICO}$" "${out}"
expect "and installs the rendered tree" "helm upgrade --install nke-controller .*--values ${renders}/install\.[A-Za-z0-9]+/rendered/controller/site-values.yaml" "${out}"
expect "the render's notes reach the operator" '^stack-artifacts: no source block$' "${out}"
[[ -z "$(ls -A "${renders}" 2>/dev/null)" ]] && pass "the scratch render is removed when the install ends" || fail "scratch left: $(ls -A "${renders}")"
[[ ! -e "${installs}" || -z "$(ls -A "${installs}")" ]] && pass "a dry run writes no install record" || fail "a dry run wrote a record: $(ls -A "${installs}")"
find "${state}" -maxdepth 0 -perm 0700 | grep -q . && pass "the state directory is private" || fail "state directory mode"
# A real install against the stub tools: the record is written on success,
# with the four files, and the scratch is gone.
out="$(PATH="${stubs}:${PATH}" STUB_ALLOW_MUTATE=1 NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --kubeconfig "${WORK}/stub.kubeconfig" 2>&1)" || fail "wrapped install against the stubs exited $?: ${out}"
expect "the install says where its record is" "^=== installed; record in ${installs}/[0-9]{8}T[0-9]{6}Z\.[A-Za-z0-9]+$" "${out}"
record="$(ls -1d "${installs}"/*/ 2>/dev/null | head -n 1)"
[[ -n "${record}" ]] && pass "one install record" || fail "no install record under ${installs}"
[[ "$(ls -A "${record}" | sort | tr '\n' ' ')" == "images.txt render.log stack-artifacts.json status " ]] && pass "the record holds exactly images.txt, render.log, stack-artifacts.json and status" || fail "record files: $(ls -A "${record}")"
grep -q '^status=ok$' "${record}/status" && grep -q "^ctr=ctr$" "${record}/status" && grep -q '^arch=linux/arm64$' "${record}/status" && grep -q "^kubeconfig=${WORK}/stub.kubeconfig$" "${record}/status" && grep -q '^argv=install ' "${record}/status" && pass "status records the outcome, CTR, the architecture, the kubeconfig and the argv" || fail "status: $(cat "${record}/status")"
grep -q '^stack-artifacts: no source block$' "${record}/render.log" && pass "render.log holds the render's notes" || fail "render.log: $(cat "${record}/render.log")"
"${NVKECTL}" inspect stack-artifacts "${record}/stack-artifacts.json" --meta >/dev/null 2>&1 && pass "the recorded stack-artifacts.json loads" || fail "the recorded file does not load"
grep -q "^nvke.local/cwe@${d_cwe}	${layouts}/cwe$" "${record}/images.txt" && pass "the recorded images.txt names the layouts" || fail "recorded images.txt"
find "${record}" -type f ! -perm 0600 | grep -q . && fail "a record file is not 0600" || pass "the record's files are private"
[[ -z "$(ls -A "${renders}" 2>/dev/null)" ]] && pass "the scratch is removed after a real install" || fail "scratch left: $(ls -A "${renders}")"
# A second install within the same second gets its own record.
out="$(PATH="${stubs}:${PATH}" STUB_ALLOW_MUTATE=1 NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 2>&1)" || fail "second wrapped install exited $?: ${out}"
[[ "$(ls -1d "${installs}"/*/ | wc -l | tr -d ' ')" == 2 ]] && pass "two installs, two records" || fail "records: $(ls "${installs}")"
# A failed install writes no record and leaves no scratch.
rm -rf "${installs}"
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 2>&1)" && fail "an install whose kubectl refuses exited 0"
[[ ! -e "${installs}" || -z "$(ls -A "${installs}")" ]] && pass "a failed install writes no record" || fail "a failed install wrote a record"
[[ -z "$(ls -A "${renders}" 2>/dev/null)" ]] && pass "a failed install leaves no scratch behind" || fail "scratch left: $(ls -A "${renders}")"
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${HERE}/stack-artifacts/bad-schema.json" --arch linux/arm64 --dry-run 2>&1)" && fail "failed render exited 0"
expect "the failed render's error reaches the operator" 'bad-schema.json: schemaVersion is' "${out}"
[[ -z "$(ls -A "${renders}" 2>/dev/null)" ]] && pass "a failed render leaves no scratch behind" || fail "scratch left after a failed render: $(ls -A "${renders}")"
[[ ! -e "${installs}" || -z "$(ls -A "${installs}")" ]] && pass "a failed render writes no record" || fail "a failed render wrote a record"
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${REPO}/hack/testdata/nvkr-release/valid.json" --arch linux/arm64 --dry-run 2>&1)" && fail "an NVKR export exited 0"
expect "the executor's render refuses an NVKR export with the converter" 'convert it with hack/nvkr-run-to-stack-artifacts.py' "${out}"
out="$(PATH="${stubs}:${PATH}" NVKECTL=/nonexistent/nvkectl bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch linux/arm64 --dry-run 2>&1)" && fail "render without nvkectl exited 0"
expect "a render without nvkectl is named" 'nvkectl is needed to render' "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --stack-artifacts "${LOCAL}" --dry-run 2>&1)" && fail "artifacts without sitespec exited 0"
expect "the artifacts without the two inputs are refused" '^nvke-deploy: --stack-artifacts needs --sitespec and --stack-config$' "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --dry-run 2>&1)" && fail "a site without the artifacts exited 0"
expect "a site install names its artifacts" '^nvke-deploy: --sitespec needs --stack-artifacts: a site install names its artifacts; make local-stack-artifacts writes the file$' "${out}"
for old in "--release ${LOCAL}" "--nvkr-run-dir ${layouts}"; do
  out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" ${old} --dry-run 2>&1)" && fail "${old%% *} exited 0"
  expect "${old%% *} is an unknown argument" "^nvke-deploy: unknown argument: ${old%% *}$" "${out}"
done
expect "usage names the converter" 'hack/nvkr-run-to-stack-artifacts.py' "$(bash "${EXECUTOR}" --help 2>&1 || true)"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --dry-run 2>&1)" && fail "--sitespec alone exited 0"
expect "the executor refuses --sitespec without --stack-config" "^nvke-deploy: --sitespec needs --stack-config: the stack's input is the stack config file$" "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --stack-config "${STACK}" --dry-run 2>&1)" && fail "--stack-config alone exited 0"
expect "the executor refuses --stack-config without --sitespec" '^nvke-deploy: --stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec$' "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --arch arm64 --dry-run 2>&1)" && fail "a bare --arch exited 0"
expect "the executor refuses an --arch outside linux/amd64 and linux/arm64" '^nvke-deploy: --arch arm64 is not linux/amd64 or linux/arm64$' "${out}"
# A stack config with placeholders stops the render; nothing is applied.
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${REPO}/deploy/examples/stack-config.json" --stack-artifacts "${LOCAL}" --arch linux/arm64 --dry-run 2>&1)" && fail "executor with placeholders exited 0"
expect "the executor's render names the placeholders" '^nico.endpoint: still a REPLACE_\* placeholder; fill it in$' "${out}"
reject "nothing is applied after a refused render" '^(kubectl|helm) ' "${out}"

# The node architecture: the executor reads it from the cluster on every run,
# and on a dry run too when the API answers, and passes it to the render as
# --arch. The stub kubectl is the cluster; the nvkectl-log wrapper records
# what the render was given.
nvkectl_log="${WORK}/nvkectl.log"
real_nvkectl="${NVKECTL}"
exec_arch() {
  : >"${nvkectl_log}"
  PATH="${stubs}:${PATH}" NVKECTL="${stubs}/nvkectl-log" REAL_NVKECTL="${real_nvkectl}" NVKECTL_LOG="${nvkectl_log}" NVKE_STATE_DIR="${state}" \
    bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --kubeconfig "${WORK}/stub.kubeconfig" "$@" 2>&1
}
out="$(STUB_ARCH=amd64 exec_arch --dry-run)" || fail "dry run with the stub cluster exited $?: ${out}"
expect "the nodes' architecture reaches the render" 'render deploy-path .* --arch linux/amd64( |$)' "$(cat "${nvkectl_log}")"
reject "no host-default warning when the nodes were read" 'warning: --arch not given' "${out}"
out="$(STUB_ARCH=amd64 exec_arch --arch linux/arm64 --dry-run)" || fail "dry run with --arch exited $?: ${out}"
expect "--arch on the executor overrides the nodes" 'render deploy-path .* --arch linux/arm64( |$)' "$(cat "${nvkectl_log}")"
out="$(STUB_MIXED_ARCH=1 exec_arch --dry-run)" && fail "mixed architectures exited 0"
expect "mixed architectures stop the install" '^nvke-deploy: cluster: nodes report amd64 arm64; the stack installs for one architecture$' "${out}"
reject "nothing is rendered on mixed architectures" 'render deploy-path' "$(cat "${nvkectl_log}")"
out="$(STUB_NO_ARCH_LABEL=1 exec_arch --dry-run)" && fail "an unlabelled node exited 0"
expect "an unlabelled node stops the install" '^nvke-deploy: cluster: node node-1 has no kubernetes.io/arch label$' "${out}"
out="$(STUB_NO_NODES=1 exec_arch --dry-run)" && fail "no nodes exited 0"
expect "no nodes stops the install" '^nvke-deploy: cluster: no nodes$' "${out}"
out="$(STUB_API_DOWN=1 exec_arch --dry-run)" || fail "dry run with the API down exited $?: ${out}"
expect "a dry run with the API down says it renders for this machine" "^=== cannot list nodes; the dry run renders for this machine's architecture$" "${out}"
reject "and passes no --arch" ' --arch ' "$(cat "${nvkectl_log}")"
expect "so the render warns" "^warning: --arch not given; rendering for the host's linux/" "${out}"
out="$(STUB_API_DOWN=1 exec_arch)" && fail "an install with the API down exited 0"
expect "an install with the API down stops at the node read" '^nvke-deploy: cluster: cannot list nodes: The connection to the server' "${out}"
reject "nothing is applied when the API is down" '^(kubectl|helm) ' "${out}"

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

# check-site.sh against the stub tools: every finding passes on a ready
# cluster and names the fix on one that is not.
check() { PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${WORK}/check-state" bash "${CHECK}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${ARTIFACTS:-${LOCAL}}" --kubeconfig "${WORK}/stub.kubeconfig" 2>&1; }
: >"${WORK}/curl.log"
out="$(STUB_LOG="${WORK}/stub.log" CURL_LOG="${WORK}/curl.log" check)" || fail "check-site on a ready cluster exited $?: ${out}"
grep -qE ' (apply|create|delete|patch|taint|annotate) ' "${WORK}/stub.log" && fail "check-site made a mutating kubectl call: $(grep -E ' (apply|create|delete|patch|taint|annotate) ' "${WORK}/stub.log")" || pass "check-site made no mutating kubectl call ($(wc -l <"${WORK}/stub.log" | tr -d ' ') reads)"
reject "no trace id line between findings" '^Trace ID: ' "${out}"
expect "the inputs are said first, with their absolute paths and facts" "^INFO  inputs: site spec ${SITESPEC}, site nvke-site, stack config ${STACK}, nico https://nico.example.com org org-1, stack artifacts ${LOCAL} \(5 artifacts, 5 layouts\)$" "${out}"
[[ "$(grep -n '^INFO  inputs:' <<<"${out}" | cut -d: -f1)" -lt "$(grep -n '^PASS  site spec:' <<<"${out}" | cut -d: -f1)" ]] && pass "the inputs line comes before the input findings" || fail "inputs line order"
expect "the site spec passes" "^PASS  site spec: ${SITESPEC} is valid$" "${out}"
expect "the stack config passes" "^PASS  stack config: ${STACK}$" "${out}"
expect "the stack artifacts pass, without a source block" "^PASS  stack artifacts: ${LOCAL} \(5 artifacts, no source block\)$" "${out}"
reject "no qualification warning without a source block" '^WARN  stack artifacts: the source' "${out}"
expect "the bindings are checked for the nodes' architecture" "^PASS  stack artifacts: bindings of ${NICO} checked for linux/arm64$" "${out}"
reject "a layout's host is not probed" 'nvke.local' "$(cat "${WORK}/curl.log")"
reject "no multi-node layout warning on one node" '^WARN  stack artifacts: .*layout' "${out}"
expect "the nodes' architecture is reported" '^PASS  cluster: nodes are arm64$' "${out}"
expect "no taint" '^PASS  cluster: no node carries the control-plane NoSchedule taint$' "${out}"
expect "one default StorageClass" '^PASS  cluster: default StorageClass local-path$' "${out}"
expect "Cluster API is not installed yet and the init command is printed" '^PASS  Cluster API: not installed yet; run: clusterctl init .*--core cluster-api:v[0-9.]+ --bootstrap kubeadm:v[0-9.]+ --control-plane kubeadm:v[0-9.]+ --wait-providers$' "${out}"
expect "the check ends ready with the init command" '^  clusterctl init --kubeconfig .* --wait-providers$' "${out}"
expect "the check ends ready with the executor command, by its resolved path, with CTR for the layouts" "^  CTR=ctr ${DEPLOY_DIR}/nvke-deploy.sh install --deploy-path ${NICO} --sitespec ${SITESPEC} --stack-config ${STACK} --stack-artifacts ${LOCAL} --kubeconfig ${WORK}/stub.kubeconfig$" "${out}"
# A file with a source block: the verify hint and the qualification warning;
# its registry host is probed; no CTR line without layouts.
valid="${HERE}/stack-artifacts/valid.json"
: >"${WORK}/curl.log"
out="$(ARTIFACTS="${valid}" CURL_LOG="${WORK}/curl.log" check)" || fail "check-site with a sourced file exited $?: ${out}"
expect "the source is named with its verify command" "^PASS  stack artifacts: ${valid} \(15 artifacts; source ghcr.io/example/recipes/releases@sha256:0+, declared pipeline-attested\); verify it with: nvkr release verify --reference ghcr.io/example/recipes/releases@sha256:0+$" "${out}"
expect "the declared qualification is a warning" "^WARN  stack artifacts: the source's qualification is declared by its publisher, not verified here$" "${out}"
expect "an unused artifact is said" '^INFO  stack artifacts: nvkectl is not used by ' "${out}"
expect "the pull host is probed" '^PASS  egress: https://ghcr.io/v2/$' "${out}"
expect "the executor command without CTR when nothing is a layout" "^  ${DEPLOY_DIR}/nvke-deploy.sh install --deploy-path ${NICO} --sitespec ${SITESPEC} --stack-config ${STACK} --stack-artifacts ${valid} --kubeconfig ${WORK}/stub.kubeconfig$" "${out}"
# A missing layout, a missing image, the wrong architecture, several nodes.
out="$(ARTIFACTS="${HERE}/stack-artifacts/layout-missing.json" check)" && fail "check-site with a missing layout exited 0"
expect "a missing layout is a finding" '^FAIL  stack artifacts: layout for cwe is missing or does not list its digest; run: ' "${out}"
expect "a missing required image is a finding" '^FAIL  stack artifacts: controller profiles-image controller-profiles is not in the file$' "${out}"
out="$(ARTIFACTS="${WORK}/no-cwe.json" STUB_ARCH=amd64 check)" && fail "check-site with amd64 nodes and arm64 artifacts exited 0"
expect "an artifact for another architecture is a finding naming both" '^FAIL  stack artifacts: capnico image capnico-controller is published only for linux/arm64; the nodes are linux/amd64$' "${out}"
out="$(STUB_SECOND_NODE=1 check)" && fail "check-site on two nodes exited 0"
expect "layouts on several nodes are a warning naming CTR" '^WARN  stack artifacts: 5 layout\(s\) and 2 nodes; a layout reaches one node.s containerd, the one CTR talks to; CTR is ctr$' "${out}"
out="$(ARTIFACTS="${REPO}/hack/testdata/nvkr-release/valid.json" check)" && fail "check-site with an NVKR export exited 0"
expect "an NVKR export is a finding with the converter" "^FAIL  stack artifacts: ${REPO}/hack/testdata/nvkr-release/valid.json does not parse: .*convert it with hack/nvkr-run-to-stack-artifacts.py; run: " "${out}"
reject "no bindings check without the file" 'bindings of' "${out}"
out="$(STUB_CAPI_IMAGE="registry.k8s.io/cluster-api/cluster-api-controller:$(sed -n 's/^CAPI_VERSION[[:space:]]*?=[[:space:]]*//p' "${DEPLOY_DIR}/../make/tool-versions.mk")" check)" || fail "check-site with Cluster API installed exited $?: ${out}"
expect "Cluster API at the pinned version passes" '^PASS  Cluster API: v[0-9.]+ already installed$' "${out}"
reject "no init command is printed when Cluster API is installed" '^  clusterctl init' "${out}"
out="$(STUB_DENY=1 check)" && fail "check-site with denied reads exited 0"
expect "a denied node read is a finding, not a pass" '^FAIL  cluster: cannot list nodes: Error from server \(Forbidden\)' "${out}"
expect "a denied StorageClass read is a finding" '^FAIL  cluster: cannot list StorageClasses: ' "${out}"
expect "a denied Cluster API read is a finding, not an init hint" '^FAIL  cluster: cannot read deployment capi-system/capi-controller-manager: ' "${out}"
reject "no init hint on a denied read" 'not installed yet' "${out}"
reject "the check changes nothing" '^(PASS|WARN|SKIP|FAIL)  .*(taint nodes |apply |helm upgrade)' "${out}"
out="$(STUB_TAINT=NoSchedule STUB_NO_SC=1 STUB_CAPI_IMAGE=registry.k8s.io/cluster-api/cluster-api-controller:v0.0.1 check)" && fail "check-site on an unready cluster exited 0"
expect "the taint is a finding with the fix" '^FAIL  cluster: node-1 still carries the control-plane NoSchedule taint, which the bootstrap removes; run: kubectl taint nodes' "${out}"
expect "the missing StorageClass is a finding with the fix" '^FAIL  cluster: no default StorageClass; Postgres and SPIRE need persistent volumes' "${out}"
expect "another Cluster API version is a finding with the upgrade command" '^FAIL  Cluster API: v0.0.1 is installed; the stack is tested with v[0-9.]+, and clusterctl init does not upgrade\. Run: clusterctl upgrade apply ' "${out}"
expect "the failures are counted" '3 finding\(s\) to fix before the stack installs' "${out}"
out="$(STUB_MIXED_ARCH=1 STUB_SECOND_SC=1 check)" && fail "check-site on a mixed cluster exited 0"
expect "an even node count is a finding" '^FAIL  cluster: 2 node\(s\); expected an odd number, at least 1$' "${out}"
expect "mixed architectures are a finding" '^FAIL  cluster: nodes report amd64 arm64; the stack installs for one architecture$' "${out}"
expect "two default StorageClasses are a finding" '^FAIL  cluster: 2 default StorageClasses; the stack needs exactly one$' "${out}"
out="$(STUB_NO_ARCH_LABEL=1 check)" && fail "check-site with an unlabelled node exited 0"
expect "an unlabelled node is a finding" '^FAIL  cluster: node node-1 has no kubernetes.io/arch label$' "${out}"
out="$(STUB_NO_NODES=1 check)" && fail "check-site with no nodes exited 0"
expect "no nodes is a finding" '^FAIL  cluster: no nodes$' "${out}"
# A bad stack config is a finding naming the first problem; the executor is not run.
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${WORK}/check-state" bash "${CHECK}" --sitespec "${SITESPEC}" --stack-config "${REPO}/deploy/examples/stack-config.json" --stack-artifacts "${LOCAL}" --kubeconfig "${WORK}/stub.kubeconfig" 2>&1)" && fail "check-site with a bad stack config exited 0"
expect "a bad stack config is a finding with its first problem" "^FAIL  stack config: ${REPO}/deploy/examples/stack-config.json is not ready: nico.endpoint: still a REPLACE_\* placeholder; fill it in; run: $(printf '%q' "${NVKECTL}") inspect stack-config ${REPO}/deploy/examples/stack-config.json$" "${out}"
expect "the inputs line leaves out what the bad file could not give" "^INFO  inputs: site spec ${SITESPEC}, site nvke-site, stack config ${REPO}/deploy/examples/stack-config.json, stack artifacts ${LOCAL} \(5 artifacts, 5 layouts\)$" "${out}"
reject "the executor command is not printed on a FAIL" 'nvke-deploy.sh install' "${out}"
# A missing credential is a stack config finding too.
out="$(NVKECTL_NODE_PASSWORD='' check)" && fail "check-site without a credential exited 0"
expect "a missing credential is a finding" "^FAIL  stack config: ${STACK} is not ready: environment: NVKECTL_NODE_PASSWORD is not set; run: " "${out}"
# An inspect that fails without a word is a finding, not an aborted check.
printf '#!/usr/bin/env bash\nif [[ "$1 $2" == "inspect stack-config" ]]; then exit 1; fi\nexec "${REAL_NVKECTL}" "$@"\n' >"${stubs}/nvkectl-silent"
chmod +x "${stubs}/nvkectl-silent"
out="$(NVKECTL="${stubs}/nvkectl-silent" REAL_NVKECTL="${real_nvkectl}" check)" && fail "check-site with a silent inspect failure exited 0"
expect "a silent inspect failure is a finding" "^FAIL  stack config: ${STACK} is not ready: nvkectl inspect stack-config exited non-zero without a reason; run: " "${out}"
expect "and the check runs to its verdict" '^check-site.sh: [0-9]+ finding\(s\) to fix' "${out}"
out="$(bash "${CHECK}" --sitespec "${SITESPEC}" --kubeconfig "${WORK}/stub.kubeconfig" 2>&1)" && fail "check-site with --sitespec alone exited 0"
expect "the check refuses --sitespec without --stack-config" "^check-site.sh: --sitespec needs --stack-config: the stack's input is the stack config file$" "${out}"
out="$(bash "${CHECK}" --stack-config "${STACK}" --kubeconfig "${WORK}/stub.kubeconfig" 2>&1)" && fail "check-site with --stack-config alone exited 0"
expect "the check refuses --stack-config without --sitespec" '^check-site.sh: --stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec$' "${out}"
out="$(bash "${CHECK}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --kubeconfig "${WORK}/stub.kubeconfig" 2>&1)" && fail "check-site without the artifacts exited 0"
expect "the check refuses a site without its artifacts" '^check-site.sh: --sitespec needs --stack-artifacts: a site install names its artifacts; make local-stack-artifacts writes the file$' "${out}"
out="$(bash "${CHECK}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --stack-artifacts "${LOCAL}" --release x.json --kubeconfig "${WORK}/stub.kubeconfig" 2>&1)" && fail "check-site with --release exited 0"
expect "--release is an unknown argument of the check" '^check-site.sh: unknown argument: --release$' "${out}"
out="$(STUB_API_DOWN=1 check)" && fail "check-site with the API down exited 0"
expect "an unreachable API is a finding" "^FAIL  cluster: API not reachable with ${WORK}/stub.kubeconfig$" "${out}"
reject "no node findings when the API is down" '^(PASS|FAIL)  cluster: .*node' "${out}"
out="$(STUB_CAPI_IMAGE="registry.k8s.io/cluster-api/cluster-api-controller:$(sed -n 's/^CAPI_VERSION[[:space:]]*?=[[:space:]]*//p' "${DEPLOY_DIR}/../make/tool-versions.mk")@sha256:0000000000000000000000000000000000000000000000000000000000000000" STUB_NO_KUBEADM=1 check)" && fail "check-site with a core-only Cluster API exited 0"
expect "a digest-pinned image is compared by its tag, and a core without the kubeadm providers is a finding" '^FAIL  Cluster API: v[0-9.]+ core is installed without capi-kubeadm-bootstrap-system/capi-kubeadm-bootstrap-controller-manager capi-kubeadm-control-plane-system/capi-kubeadm-control-plane-controller-manager; run: clusterctl init' "${out}"
PIN="$(sed -n 's/^CAPI_VERSION[[:space:]]*?=[[:space:]]*//p' "${DEPLOY_DIR}/../make/tool-versions.mk")"
out="$(STUB_CAPI_IMAGE="registry.k8s.io/cluster-api/cluster-api-controller:${PIN}" STUB_DENY_KUBEADM=1 check)" && fail "check-site with a denied kubeadm read exited 0"
expect "a denied kubeadm-provider read is a finding" '^FAIL  cluster: cannot read deployment capi-kubeadm-bootstrap-system/' "${out}"
reject "no 'already installed' pass after a denied provider read" 'already installed' "${out}"
out="$(STUB_CAPI_IMAGE="registry.k8s.io/cluster-api/cluster-api-controller@sha256:0000000000000000000000000000000000000000000000000000000000000000" check)" && fail "check-site with a digest-only Cluster API image exited 0"
expect "a digest-only image is reported as such, not by its repository" '^FAIL  Cluster API: registry.k8s.io/cluster-api/cluster-api-controller@sha256:0+ is installed, pinned by digest; its version cannot be read' "${out}"
out="$(NVKECTL=/nonexistent/nvkectl check)" && fail "check-site without nvkectl exited 0"
expect "a missing nvkectl is a finding, and the input checks are skipped" '^FAIL  tool nvkectl: /nonexistent/nvkectl not found' "${out}"
expect "the site spec is skipped without nvkectl" "^SKIP  site spec: ${SITESPEC} \(no nvkectl\)$" "${out}"
expect "the stack config is skipped without nvkectl" "^SKIP  stack config: ${STACK} \(no nvkectl\)$" "${out}"
expect "the stack artifacts are skipped without nvkectl" "^SKIP  stack artifacts: ${LOCAL} \(no nvkectl\)$" "${out}"
expect "the inputs line is printed without nvkectl, paths only" "^INFO  inputs: site spec ${SITESPEC}, stack config ${STACK}, stack artifacts ${LOCAL}$" "${out}"
expect "the verdict carries the check's own name" '^check-site.sh: [0-9]+ finding\(s\) to fix' "${out}"
# The executor refuses an unknown item type before it applies anything.
mkdir -p "${WORK}/badpath/x"; printf 'items:\n  - {name: x, type: bogus}\n' >"${WORK}/badpath/nvke-deploy.yaml"; printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n' >"${WORK}/badpath/x/a.yaml"
out="$(bash "${EXECUTOR}" install --deploy-path "${WORK}/badpath" --dry-run 2>&1)" && fail "an unknown item type exited 0"
expect "an unknown item type is refused by the parser" 'unrecognised:.*type: bogus' "${out}"
reject "nothing is applied before the refusal" 'kubectl .*apply' "${out}"
out="$(bash "${CHECK}" --kubeconfig "${WORK}/stub.kubeconfig" 2>&1)" && fail "check-site without the inputs exited 0"
expect "the inputs are required" '^usage: check-site.sh' "${out}"
PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${WORK}/check-state" bash "${CHECK}" --kubeconfig "${WORK}/stub.kubeconfig" --diagnostics >/dev/null 2>&1 || fail "check-site --diagnostics exited $?"
[[ -f "${WORK}/check-state/diagnostics/nodes.txt" ]] && pass "diagnostics are written under the state directory" || fail "no diagnostics: $(ls -R "${WORK}/check-state" 2>/dev/null)"
# --diagnostics copies the last successful install's record when there is one.
mkdir -p "${WORK}/check-state/installs/20260101T000000Z.aaaa" "${WORK}/check-state/installs/20260102T000000Z.bbbb"
printf 'status=ok\n' >"${WORK}/check-state/installs/20260102T000000Z.bbbb/status"
PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${WORK}/check-state" bash "${CHECK}" --kubeconfig "${WORK}/stub.kubeconfig" --diagnostics >/dev/null 2>&1 || fail "check-site --diagnostics with a record exited $?"
[[ -f "${WORK}/check-state/diagnostics/last-successful-install/status" ]] && pass "the last successful install's record is in the diagnostics" || fail "no install record in the diagnostics"


# build/stack-artifacts.sh against stub tools: docker, oras and helm write
# layout-shaped directories; python3 and nvkectl are real. The stubs read
# STUB_* to fail at a chosen point or to change what they write.
BUILD="${REPO}/build/stack-artifacts.sh"
bstubs="${WORK}/build-stubs"
mkdir -p "${bstubs}"
# fake_image_layout DIR DIGEST [EXTRA_MANIFEST]: an OCI layout whose index
# lists DIGEST, plus a second manifest when asked.
cat >"${bstubs}/fake-layout.sh" <<'STUB'
#!/usr/bin/env bash
dir="$1" digest="$2" extra="${3:-}"
mkdir -p "${dir}/blobs/sha256"
printf '{"imageLayoutVersion":"1.0.0"}' >"${dir}/oci-layout"
m="{\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"digest\":\"${digest}\",\"size\":2}"
[[ -z "${extra}" ]] || m="${m},{\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"digest\":\"${extra}\",\"size\":2}"
printf '{"schemaVersion":2,"manifests":[%s]}' "${m}" >"${dir}/index.json"
STUB
# digest_for NAME: a stable fake digest from the name and STUB_DIGEST_SEED.
cat >"${bstubs}/digest-for.sh" <<'STUB'
#!/usr/bin/env bash
if [[ -n "${STUB_BAD_DIGEST:-}" && "$1" == "${STUB_BAD_DIGEST}" ]]; then printf 'sha256:not-a-digest'; exit 0; fi
if command -v sha256sum >/dev/null 2>&1; then h="$(printf '%s%s' "$1" "${STUB_DIGEST_SEED:-}" | sha256sum | cut -d' ' -f1)"; else h="$(printf '%s%s' "$1" "${STUB_DIGEST_SEED:-}" | shasum -a 256 | cut -d' ' -f1)"; fi
printf 'sha256:%s' "${h}"
STUB
cat >"${bstubs}/docker" <<'STUB'
#!/usr/bin/env bash
# buildx version: ok. buildx build ... --output type=oci,dest=TAR: a tar of a
# fake layout named after the Dockerfile's directory. Anything else: ok.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[[ "$1 $2" != "buildx build" ]] && exit 0
dest=""; file=""
while [[ $# -gt 0 ]]; do
  case "$1" in --output) dest="${2#type=oci,dest=}"; shift 2 ;; --file) file="$2"; shift 2 ;; *) shift ;; esac
done
name="$(basename "$(dirname "${file}")")"; [[ "${name}" == controller ]] && name=cwe; [[ "${name}" == fixtures ]] && name=controller-profiles
[[ "${STUB_FAIL:-}" == "build:${name}" ]] && { echo "stub docker: build of ${name} failed" >&2; exit 1; }
tmp="$(mktemp -d "${TMPDIR:-/tmp}/stub-docker.XXXXXX")"
bash "${here}/fake-layout.sh" "${tmp}" "$(bash "${here}/digest-for.sh" "${name}")" $([[ "${STUB_TWO_MANIFESTS:-}" == "${name}" ]] && bash "${here}/digest-for.sh" "${name}-second")
tar -cf "${dest}" -C "${tmp}" .
rm -rf "${tmp}"
STUB
cat >"${bstubs}/nerdctl" <<'STUB'
#!/usr/bin/env bash
# The same as the docker stub, without the buildx verb.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[[ "$1" != build ]] && exit 0
printf 'nerdctl\n' >>"${STUB_CLI_LOG:-/dev/null}"
exec bash "${here}/docker" buildx "$@"
STUB
cat >"${bstubs}/oras" <<'STUB'
#!/usr/bin/env bash
# cp --to-oci-layout IMAGE DIR:TAG and push --oci-layout DIR:TAG ...: a fake
# layout at DIR named after its last path element.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
verb="$1"; shift
dir=""
for arg in "$@"; do case "${arg}" in /*:*) dir="${arg%:*}" ;; esac; done
[[ -n "${dir}" ]] || { echo "stub oras: no layout argument" >&2; exit 1; }
name="$(basename "${dir}")"
[[ "${STUB_FAIL:-}" == "${verb}:${name}" ]] && { echo "stub oras: ${verb} of ${name} failed" >&2; exit 1; }
bash "${here}/fake-layout.sh" "${dir}" "$(bash "${here}/digest-for.sh" "${name}")" $([[ "${STUB_TWO_MANIFESTS:-}" == "${name}" ]] && bash "${here}/digest-for.sh" "${name}-second")
STUB
cat >"${bstubs}/helm" <<'STUB'
#!/usr/bin/env bash
# package DIR --destination D writes <name>-<version>.tgz from Chart.yaml;
# repo add and dependency build do nothing.
case "$1" in
  package) chart="$2"; dest="$4"; name="$(awk '$1 == "name:" { print $2; exit }' "${chart}/Chart.yaml")"; ver="$(awk '$1 == "version:" { gsub(/"/, "", $2); print $2; exit }' "${chart}/Chart.yaml")"
    printf 'not really a chart\n' >"${dest}/${name}-${ver}.tgz" ;;
esac
exit 0
STUB
chmod +x "${bstubs}"/*
bout="${WORK}/dist/local-site"
build() { PATH="${bstubs}:${PATH}" NVKECTL="${NVKECTL}" bash "${BUILD}" --profiles-src "${REPO}/controller/test/fixtures" --out "${bout}" --arch arm64 2>&1; }
out="$(build)" || fail "stack-artifacts.sh exited $?: ${out}"
[[ -f "${bout}/stack-artifacts.json" ]] && pass "the build writes stack-artifacts.json" || fail "no stack-artifacts.json: ${out}"
listing="$("${NVKECTL}" inspect stack-artifacts "${bout}/stack-artifacts.json" --artifacts 2>&1)" || fail "the built file does not load: ${listing}"
[[ "$(wc -l <<<"${listing}" | tr -d ' ')" == 5 ]] && pass "five artifacts" || fail "artifacts: ${listing}"
expect "cwe is a local image with a layout" $'^cwe\toci\tnvke.local/cwe@sha256:[0-9a-f]{64}\tsha256:[0-9a-f]{64}\tlinux/arm64\t\t'"${bout}"'/images/cwe$' "${listing}"
expect "controller-profiles is a local image" $'^controller-profiles\toci\tnvke.local/controller-profiles@' "${listing}"
expect "capnico keeps its upstream repository" $'^capnico-controller\toci\tghcr.io/dsx-ai-factory/cluster-api-provider-nico/controller@sha256:[0-9a-f]{64}\t.*\tlinux/arm64\t\t'"${bout}"'/images/capnico-controller$' "${listing}"
expect "the controller chart is a helm-oci artifact with its version" $'^controller-chart\thelm-oci\tnvke.local/controller-chart@.*\t0.0.0\t'"${bout}"'/images/controller-chart$' "${listing}"
expect "nktp too" $'^nktp\thelm-oci\tnvke.local/nktp@.*\t0.0.0\t' "${listing}"
expect "the build prints every artifact" $'^capnico-controller\toci\tghcr.io/' "${out}"
for layout in cwe controller-profiles capnico-controller controller-chart nktp; do
  [[ "$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["manifests"]))' "${bout}/images/${layout}/index.json")" == 1 ]] || fail "layout ${layout} lists more than one manifest"
done
pass "one manifest per layout"
bindings="$("${NVKECTL}" inspect stack-artifacts "${bout}/stack-artifacts.json" --bindings --deploy-path "${NICO}" --arch linux/arm64 2>&1)" || fail "bindings: ${bindings}"
reject "every artifact deploy/nico needs is bound; none is unused" $'\t(missing|other-arch)|^unused' "${bindings}"
expect "the controller chart binds" $'^controller\tchart\tcontroller-chart\tcontroller-chart\tbound$' "${bindings}"
[[ ! -e "${bout}.tmp" ]] && pass "no tmp tree is left" || fail "tmp tree left"
# Running twice replaces the tree: a changed input changes the digest.
before="$(hex_of "${bout}/stack-artifacts.json")"
out="$(STUB_DIGEST_SEED=second build)" || fail "second build exited $?: ${out}"
[[ "$(hex_of "${bout}/stack-artifacts.json")" != "${before}" ]] && pass "a second build replaces the file" || fail "second build left the file as it was"
[[ "$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["manifests"]))' "${bout}/images/cwe/index.json")" == 1 ]] && pass "a rebuilt layout holds one manifest" || fail "manifests accumulated"
# A producer failing mid-list leaves the previous OUT byte-identical and no tmp.
before="$(find "${bout}" -type f -exec cat {} + | hex_of /dev/stdin)"
out="$(STUB_FAIL=push:nktp build)" && fail "a failing producer exited 0"
expect "the failing producer is named" 'stub oras: push of nktp failed' "${out}"
[[ "$(find "${bout}" -type f -exec cat {} + | hex_of /dev/stdin)" == "${before}" ]] && pass "a failed build leaves the previous output as it was" || fail "a failed build changed the output"
[[ ! -e "${bout}.tmp" ]] && pass "a failed build leaves no tmp tree" || fail "tmp tree left after a failure"
# A stale tmp tree is removed first.
mkdir -p "${bout}.tmp/images/stale"; : >"${bout}.tmp/images/stale/index.json"
out="$(build)" || fail "build over a stale tmp exited $?: ${out}"
[[ ! -e "${bout}/images/stale" && ! -e "${bout}.tmp" ]] && pass "a stale tmp tree is removed" || fail "stale tmp content survived"
# Refusals: a two-manifest index, a malformed digest, a placeholder in the
# profiles source, a missing tool, an unknown flag.
out="$(STUB_TWO_MANIFESTS=cwe build)" && fail "a two-manifest index exited 0"
expect "a two-manifest index is refused naming the layout" 'images/cwe: index.json lists 2 manifests; a layout holds one artifact' "${out}"
out="$(STUB_BAD_DIGEST=nktp build)" && fail "a malformed digest exited 0"
expect "a malformed digest is refused" 'not-a-digest' "${out}"
mkdir -p "${WORK}/bad-profiles/profiles"; printf 'token: REPLACE_ME\n' >"${WORK}/bad-profiles/profiles/x.yaml"
out="$(PATH="${bstubs}:${PATH}" bash "${BUILD}" --profiles-src "${WORK}/bad-profiles" --out "${WORK}/dist/other" 2>&1)" && fail "REPLACE_ in the profiles exited 0"
expect "a placeholder in the profiles source is refused" 'still holds REPLACE_ placeholders' "${out}"
out="$(PATH="${bstubs}:${PATH}" bash "${BUILD}" --profiles-src "${REPO}/controller/test/fixtures" --out "${WORK}/dist/other" --container-cli nerdctl --flag 2>&1)" && fail "an unknown flag exited 0"
expect "an unknown flag is named and usage printed" '^stack-artifacts: unknown argument: --flag$' "${out}"
expect "usage" '^usage: build/stack-artifacts.sh' "${out}"
out="$(PATH="${bstubs}:${PATH}" bash "${BUILD}" --out "${WORK}/dist/other" 2>&1)" && fail "no --profiles-src exited 0"
expect "the two required flags" '^usage: build/stack-artifacts.sh' "${out}"
# Without docker on PATH the CLI is nerdctl. The PATH is a directory of
# links to the tools the script and python need, so a docker installed on
# this machine is not found.
nodocker="${WORK}/build-stubs-nerdctl"; mkdir -p "${nodocker}"; for f in nerdctl oras helm fake-layout.sh digest-for.sh docker; do cp "${bstubs}/${f}" "${nodocker}/"; done
# The docker stub stays for the nerdctl stub to exec, under another name.
mv "${nodocker}/docker" "${nodocker}/docker-impl"; sed -i.bak 's|"${here}/docker"|"${here}/docker-impl"|' "${nodocker}/nerdctl"; rm -f "${nodocker}/nerdctl.bak"
for tool in bash sh env python3 tar git awk sed grep sort head find basename dirname mktemp mkdir rm mv cp cat uname tr wc ls cut chmod date readlink sha256sum shasum openssl; do
  real="$(command -v "${tool}" 2>/dev/null)" && ln -sf "${real}" "${nodocker}/${tool}"
done
: >"${WORK}/cli.log"
out="$(PATH="${nodocker}" STUB_CLI_LOG="${WORK}/cli.log" NVKECTL="${NVKECTL}" bash "${BUILD}" --profiles-src "${REPO}/controller/test/fixtures" --out "${WORK}/dist/nerdctl" --arch amd64 2>&1)" || fail "build without docker exited $?: ${out}"
grep -q nerdctl "${WORK}/cli.log" && pass "without docker the images build with nerdctl" || fail "nerdctl was not used: $(cat "${WORK}/cli.log")"
out="$(PATH="${nodocker}" bash "${BUILD}" --profiles-src "${REPO}/controller/test/fixtures" --out "${WORK}/dist/other" --container-cli docker 2>&1)" && fail "--container-cli docker without docker exited 0"
expect "a missing tool is named" '^stack-artifacts: docker is not on PATH$' "${out}"

if [[ "${failures}" -eq 0 ]]; then
  echo "all nvke-deploy tests passed"
else
  echo "${failures} nvke-deploy test(s) failed"
  exit 1
fi
