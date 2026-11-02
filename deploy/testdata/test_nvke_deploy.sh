#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Tests for nvke-deploy.sh against deploy/nico, committed and rendered for a
# site, in --dry-run: the item order, where each chart comes from, and the
# refusals, and check-site.sh against stub tools. Needs python3 and nvkectl: NVKECTL names the binary, else the
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
# The site spec's environment overrides would change every spec this suite
# loads; an operator's shell may export them, under either prefix.
for name in INFRA_PROVIDER SPIRE_EXTERNAL_ADDRESS CLUSTER_NODES SPIFFE_TRUST_DOMAIN; do
  unset "NVKECTL_${name}" "NKECTL_${name}"
done
# The local site's env.sh exports the wrapper's inputs; a shell that sourced
# it would point the wrapper tests at a real site.
unset NVKE_SITESPEC NVKE_STACK_CONFIG NVKE_KUBECONFIG NVKE_LOCAL_SITE_DIR

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
# fact about the calls made, not about the output.
[[ -z "${STUB_LOG:-}" ]] || printf '%s\n' "$*" >>"${STUB_LOG}"
case " $* " in
  *" apply "*|*" create "*|*" delete "*|*" patch "*|*" taint "*|*" annotate "*|*" label "*|*" replace "*|*" edit "*) echo "stub kubectl: mutating verb refused: $*" >&2; exit 97 ;;
esac
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
  *"get secret oidc-provider-upstream-ca"*)
    # The CA Secret the OIDC upstream CA Certificate is gated on. Absent is a
    # site with no CA yet, so the Certificate applies; STUB_CA_SECRET is a
    # site that already has one.
    if [[ -z "${STUB_CA_SECRET:-}" ]]; then
      echo 'Error from server (NotFound): secrets "oidc-provider-upstream-ca" not found' >&2; exit 1
    fi
    printf 'oidc-provider-upstream-ca\n' ;;
  *"capi-kubeadm-"*)
    if [[ -n "${STUB_DENY_KUBEADM:-}" ]]; then echo 'Error from server (Forbidden): deployments.apps is forbidden' >&2; exit 1; fi
    if [[ -n "${STUB_NO_KUBEADM:-}" ]]; then echo 'Error from server (NotFound): deployments.apps not found' >&2; exit 1; else printf 'deployment.apps/x'; fi ;;
esac
exit 0
STUB
printf '#!/usr/bin/env bash\ncase "$1" in version) printf "v4.2.4" ;; esac\nexit 0\n' >"${stubs}/helm"
printf '#!/usr/bin/env bash\nexit 0\n' >"${stubs}/clusterctl"
printf '#!/usr/bin/env bash\nexit 0\n' >"${stubs}/curl"
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
expect "controller from the checkout" 'helm upgrade --install nvke-controller .*/controller/deploy/charts/controller ' "${out}"
expect "controller in its release.yaml namespace" 'helm upgrade --install nvke-controller .* --namespace nvke-controller ' "${out}"
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
# The example, filled in. deploy/nico enables profiles.image without a pin,
# so the config names the image.
sed -e 's|REPLACE_NICO_ENDPOINT|https://nico.example.com|; s|REPLACE_NICO_ORG_ID|org-1|; s|REPLACE_NICO_API_NAME|forge|' \
    -e 's|"profiles": ""|"profiles": "ghcr.io/example/controller-profiles:v1"|' \
    "${REPO}/deploy/examples/stack-config.json" >"${WORK}/stack/stack-config.json"
STACK="${WORK}/stack/stack-config.json"
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
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --out "${rendered}" 2>&1)" || fail "render exited $?: ${out}"
expect "render reports the site values" '^controller: site values written$' "${out}"
expect "render reports the templates" '^nvke-base-config: 3 templates rendered; applies as a manifest item$' "${out}"
reject "no host-default warning when --arch is given" 'warning: --arch not given' "${out}"
grep -q 'dbSecretName: nvke-database' "${rendered}/controller/site-values.yaml" && pass "the controller reads its connection string from the nvke-database Secret" || fail "controller site values: $(cat "${rendered}/controller/site-values.yaml")"
grep -q 'existingSecret: nvke-database' "${rendered}/postgresql/site-values.yaml" && grep -c 'existingSecret: nvke-database' "${rendered}/temporal/site-values.yaml" | grep -qx 2 && pass "postgresql and both temporal datastores read the same Secret" || fail "postgresql/temporal site values"
! grep -rq 'secret-db-password\|BEGIN' "${rendered}"/*/site-values.yaml && pass "no values file holds a credential" || fail "a values file holds a credential"
grep -q 'name: site-credentials, type: manifest' "${rendered}/nvke-deploy.yaml" && pass "the render adds the site-credentials item" || fail "item list: $(cat "${rendered}/nvke-deploy.yaml")"
sed -n '/^items:/,$p' "${rendered}/nvke-deploy.yaml" | sed -n 3p | grep -q 'name: site-credentials' && pass "the site-credentials item follows the first item" || fail "item order: $(cat "${rendered}/nvke-deploy.yaml")"
grep -q "database-connection-string: $(printf 'postgresql://postgres:secret-db-password@postgres-postgresql.postgres.svc.cluster.local:5432/nvke-controller?sslmode=disable' | base64 | tr -d '\n')$" "${rendered}/site-credentials/secrets.yaml" && pass "the Secrets item holds the controller's connection string, base64 in data" || fail "secrets.yaml: $(cat "${rendered}/site-credentials/secrets.yaml")"
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
out="$(env -u NVKECTL_NICO_TOKEN -u NVKECTL_NODE_USER -u NVKECTL_NODE_PASSWORD -u NVKECTL_DATABASE_PASSWORD -u NVKECTL_SPIRE_CA_CERT_FILE -u NVKECTL_SPIRE_CA_KEY_FILE "${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --credentials external --out "${WORK}/external" 2>&1)" || fail "external render exited $?: ${out}"
expect "the external render holds no credential" "^${WORK}/external holds no credential\.$" "${out}"
expect "the external render names the database Secret" '^expected in the cluster once its namespace exists and before the item that reads it: Secret postgres/nvke-database with keys password, postgres-password$' "${out}"
expect "and the NICo Secret" '^expected in the cluster once its namespace exists and before the item that reads it: Secret capnico-system/nico-credentials with keys ' "${out}"
expect "and the CA Secret" '^expected in the cluster once its namespace exists and before the item that reads it: Secret spire-server/spire-upstream-ca with keys bundle.crt, tls.crt, tls.key$' "${out}"
expect "the external render shows the connection string shape" '^expected in the cluster once its namespace exists and before the item that reads it: Secret nvke-controller/nvke-database with keys database-connection-string, the URL postgresql://postgres:<password>@postgres-postgresql.postgres.svc.cluster.local:5432/nvke-controller\?sslmode=disable with the password URL-escaped$' "${out}"
[[ ! -e "${WORK}/external/site-credentials" && ! -e "${WORK}/external/nvke-base-config/20-nico-credentials.yaml" && -e "${WORK}/external/nvke-base-config/45-nktp-cluster.yaml" && ! -e "${WORK}/external/.gitignore" ]] && pass "the external tree carries no Secret and no .gitignore" || fail "external tree: $(ls -a "${WORK}/external" "${WORK}/external/nvke-base-config")"
! grep -rqF -e "${DEPLOY_DIR}" -e "${WORK}" "${WORK}/external" && pass "the external tree names no path of this machine" || fail "the external tree names a local path: $(grep -rnF -e "${DEPLOY_DIR}" -e "${WORK}" "${WORK}/external" | head -3)"
grep -q 'path: ../../controller/deploy/charts/controller' "${WORK}/external/controller/release.yaml" && pass "the external tree keeps the committed chart path" || fail "external controller release.yaml: $(cat "${WORK}/external/controller/release.yaml")"

# create-if-absent: the OIDC upstream CA Certificate is applied on a first
# install and skipped once the CA Secret exists, so re-running the install on
# a site that has a CA cannot re-issue it under the new NVKE subject. The two
# Issuers beside it carry no key material and are applied either way.
ca_cert="${rendered}/nvke-base-config/41-oidc-upstream-ca-certificate.yaml"
ca_out="$(PATH="${stubs}:${PATH}" bash "${EXECUTOR}" install --deploy-path "${rendered}" --dry-run 2>&1)" || fail "first-install dry run exited $?: ${ca_out}"
expect "a site with no CA applies the CA Certificate" "^kubectl apply --server-side -f ${ca_cert}" "${ca_out}"
expect "and its Issuers" "^kubectl apply --server-side -f ${rendered}/nvke-base-config/40-oidc-upstream-ca.yaml" "${ca_out}"
ca_out="$(STUB_CA_SECRET=1 PATH="${stubs}:${PATH}" bash "${EXECUTOR}" install --deploy-path "${rendered}" --dry-run 2>&1)" || fail "re-install dry run exited $?: ${ca_out}"
reject "a site that has a CA does not re-apply the Certificate" "apply --server-side -f ${ca_cert}" "${ca_out}"
expect "and says why it was skipped" 'skipping 41-oidc-upstream-ca-certificate\.yaml: secret/oidc-provider-upstream-ca already exists in spire-server' "${ca_out}"
expect "the Issuers are applied on a re-install too" "^kubectl apply --server-side -f ${rendered}/nvke-base-config/40-oidc-upstream-ca.yaml" "${ca_out}"
# A probe that fails for a reason other than NotFound is not "absent": in a
# dry run it is reported and the file counted as absent, and a real install
# stops rather than re-issue the CA on a cluster it could not read.
ca_out="$(STUB_API_DOWN=1 PATH="${stubs}:${PATH}" bash "${EXECUTOR}" install --deploy-path "${rendered}" --dry-run 2>&1)" || fail "unreadable-cluster dry run exited $?: ${ca_out}"
expect "a dry run reports a probe it could not make" 'cannot probe secret/oidc-provider-upstream-ca in spire-server' "${ca_out}"

out="$(bash "${EXECUTOR}" install --deploy-path "${rendered}" --dry-run 2>&1)" || fail "rendered dry run exited $?"
expect "the controller takes its site values" "helm upgrade --install nvke-controller .*--values ${rendered}/controller/values.yaml --values ${rendered}/controller/overlay.yaml --values ${rendered}/controller/site-values.yaml" "${out}"
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
expect "a --values-for file without appDb is still applied" "helm upgrade --install nvke-controller .*--values ${WORK}/image-override.yaml" "${appdb_out}"
# A deploy path the render did not write holds no Secret: the chart builds the
# connection string from the merged values, so an appDb override is applied.
mkdir -p "${WORK}/handpath/ctl"
printf 'items:\n  - {name: ctl, type: helm}\n' >"${WORK}/handpath/nvke-deploy.yaml"
printf 'chart:\n  name: nvke-controller\n  version: "1.0.0"\n  repository: oci://registry.example.test/charts\nrelease:\n  name: nvke-controller\n  namespace: nvke-controller\n' >"${WORK}/handpath/ctl/release.yaml"
appdb_out="$(bash "${EXECUTOR}" install --deploy-path "${WORK}/handpath" --dry-run --values-for "ctl=${WORK}/appdb-override.yaml" 2>&1)" || fail "an appDb override on a deploy path with no Secret exited $?: ${appdb_out}"
expect "an appDb override is applied where the chart reads appDb" "helm upgrade --install nvke-controller .*--values ${WORK}/appdb-override.yaml" "${appdb_out}"
! grep -q 'BEGIN' "${rendered}/spire/site-values.yaml" && pass "the CA is not copied into the spire values" || fail "spire site values hold PEM"
expect "the rendered Secret is applied as a manifest" "^kubectl apply --server-side -f ${rendered}/nvke-base-config/20-nico-credentials.yaml" "${out}"
expect "the namespaces manifest is applied too" "^kubectl apply --server-side -f ${rendered}/nvke-base-config/00-namespaces.yaml" "${out}"
reject "no python template render for a rendered path" 'render-template' "${out}"
expect "postgresql still comes from its public repository" 'helm upgrade --install postgres oci://registry-1.docker.io/bitnamicharts/postgresql --version 18.5.15 ' "${out}"
expect "a committed chart path is absolute in the rendered tree, so it resolves from anywhere" "helm upgrade --install nktp ${REPO}/nktp/deploy/charts/nktp " "${out}"
expect "the controller chart too" "helm upgrade --install nvke-controller ${REPO}/controller/deploy/charts/controller " "${out}"
# A secret that cannot be written into a quoted YAML value is refused.
out="$(NVKECTL_NICO_TOKEN='tok"en' "${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --out "${WORK}/bad-rendered" 2>&1)" && fail "a quote in a token exited 0"
expect "the unescapable token is refused by variable" 'NVKECTL_NICO_TOKEN: the value holds a character the templates cannot carry' "${out}"

# Without the allowance, an --out under a temp directory is refused.
out="$(env -u NVKECTL_RENDER_ALLOW_TEMP_OUT "${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --out "${WORK}/refused-temp" 2>&1)" && fail "temp --out exited 0"
expect "a temp --out is refused" 'is under a shared temp directory' "${out}"
[[ ! -e "${WORK}/refused-temp" ]] && pass "a refused temp --out is not created" || fail "temp out created"

# Render refuses a stack config with placeholders, naming the file and every
# placeholder, and leaves nothing.
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${REPO}/deploy/examples/stack-config.json" --arch linux/arm64 --out "${WORK}/refused" 2>&1)" && fail "render with placeholders exited 0"
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
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --out "${WORK}/host-arch" 2>&1)" || fail "render without --arch exited $?: ${out}"
expect "the host-default warning fires without --arch" "^warning: --arch not given; rendering for the host's linux/" "${out}"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --out "${rendered}" 2>&1)" && fail "existing --out exited 0"
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
# The shape nvkr release export writes, with the fields the render reads.
art() { printf '{"name":"%s","kind":"%s","digest":"%s","reference":"nvkr.local/nvke/%s@%s"%s}' "$1" "$2" "$3" "$1" "$3" "${4:-}"; }
zeros="$(printf '0%.0s' $(seq 1 64))"
cat >"${WORK}/local.json" <<JSON
{"schemaVersion":"nvkr.dev/release-description/v1","reference":"nvkr.local/nvke/releases@sha256:${zeros}","rootDigest":"sha256:${zeros}",
 "declaredQualification":"unqualified-local-execution","metadata":{"contentSubject":"sha256:${zeros}"},
 "outputs":[$(art cwe oci "${d_cwe}"),$(art capnico-controller-arm64 oci "${d_capnico}" ',"os":"linux","arch":"arm64"'),$(art controller-profiles oci "${d_profiles}"),$(art temporal helm-oci "${d_temporal}" ',"chartVersion":"0.74.0"')]}
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
expect "the controller takes its images from release-images.yaml" "helm upgrade --install nvke-controller .*/controller/deploy/charts/controller .*--values ${rendered}/controller/release-images.yaml" "${out}"
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
{"schemaVersion":"nvkr.dev/release-description/v1","reference":"nvkr.local/nvke/releases@sha256:${zeros}","rootDigest":"sha256:${zeros}",
 "declaredQualification":"unqualified-local-execution","metadata":{"contentSubject":"sha256:${zeros}"},
 "outputs":[$(art capnico-controller-arm64 oci "${d_capnico}" ',"os":"linux","arch":"arm64"'),$(art controller-profiles oci "${d_profiles}")]}
JSON
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --release "${WORK}/no-cwe.json" --nvkr-run-dir "${run_dir}" --arch linux/amd64 --out "${WORK}/refused" 2>&1)" && fail "render without cwe exited 0"
expect "the missing image artifact is named" 'controller: image cwe is not in the release' "${out}"
expect "the wrong architecture is named" 'capnico: image: capnico-controller is published only for other architectures' "${out}"
[[ ! -e "${WORK}/refused" ]] && pass "a refused render leaves no output" || fail "refused output left behind"
# The release decides the version of a chart it carries: the difference from
# the pin is a warning, and the rendered copy records the release's version.
cat >"${WORK}/other-chart.json" <<JSON
{"schemaVersion":"nvkr.dev/release-description/v1","reference":"nvkr.local/nvke/releases@sha256:${zeros}","rootDigest":"sha256:${zeros}",
 "declaredQualification":"unqualified-local-execution","metadata":{"contentSubject":"sha256:${zeros}"},
 "outputs":[$(art cwe oci "${d_cwe}"),$(art capnico-controller-arm64 oci "${d_capnico}" ',"os":"linux","arch":"arm64"'),$(art controller-profiles oci "${d_profiles}"),$(art temporal helm-oci "${d_temporal}" ',"chartVersion":"9.9.9"')]}
JSON
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --release "${WORK}/other-chart.json" --nvkr-run-dir "${run_dir}" --arch linux/arm64 --out "${WORK}/other-chart" 2>&1)" || fail "render with another chart version exited $?: ${out}"
expect "the chart version difference is a warning" '^warning: temporal: the release carries temporal 9.9.9, release.yaml pins 0.74.0' "${out}"
grep -q '^  version: "9.9.9"$' "${WORK}/other-chart/temporal/release.yaml" && pass "the rendered copy records the release chart version" || fail "temporal version not recorded"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --release "${HERE}/nvkr-release/bad-schema.json" --out "${WORK}/bad" 2>&1)" && fail "bad description exited 0"
expect "a bad description is named" 'bad-schema.json' "${out}"
out="$("${NVKECTL}" render deploy-path --deploy-path "${NICO}" --release "${WORK}/local.json" --nvkr-run-dir "${run_dir}" --arch linux/arm64 --out "${rendered}" 2>&1)" && fail "existing --out exited 0"
expect "an existing --out is refused" 'exists; render writes a new directory' "${out}"

# The executor renders by itself when given the two inputs, into a private
# scratch directory that is gone when it exits. --arch stands in for the
# node read here: there is no cluster.
state="${WORK}/state"
renders="${state}/renders"
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --dry-run 2>&1)" || fail "wrapped render dry run exited $?: ${out}"
expect "the executor logs the inputs before it renders" "^=== inputs: site spec ${SITESPEC}, site nvke-site, stack config ${STACK}, nico https://nico.example.com org org-1$" "${out}"
expect "the executor renders before installing" "^=== rendering ${NICO}$" "${out}"
expect "and installs the rendered tree" "helm upgrade --install nvke-controller .*--values ${renders}/install\.[A-Za-z0-9]+/rendered/controller/site-values.yaml" "${out}"
[[ -z "$(ls -A "${renders}" 2>/dev/null)" ]] && pass "the scratch render is removed when the install ends" || fail "scratch left: $(ls -A "${renders}")"
find "${state}" -maxdepth 0 -perm 0700 | grep -q . && pass "the state directory is private" || fail "state directory mode"
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --release "${HERE}/nvkr-release/bad-schema.json" --dry-run 2>&1)" && fail "failed render exited 0"
[[ -z "$(ls -A "${renders}" 2>/dev/null)" ]] && pass "a failed render leaves no scratch behind" || fail "scratch left after a failed render: $(ls -A "${renders}")"
out="$(PATH="${stubs}:${PATH}" NVKECTL=/nonexistent/nvkectl bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch linux/arm64 --dry-run 2>&1)" && fail "render without nvkectl exited 0"
expect "a render without nvkectl is named" 'nvkectl is needed to render' "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --release "${WORK}/local.json" --dry-run 2>&1)" && fail "release without sitespec exited 0"
expect "a release without the two inputs is refused" '^nvke-deploy: --release needs --sitespec and --stack-config$' "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --dry-run 2>&1)" && fail "--sitespec alone exited 0"
expect "the executor refuses --sitespec without --stack-config" "^nvke-deploy: --sitespec needs --stack-config: the stack's input is the stack config file$" "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --stack-config "${STACK}" --dry-run 2>&1)" && fail "--stack-config alone exited 0"
expect "the executor refuses --stack-config without --sitespec" '^nvke-deploy: --stack-config needs --sitespec: the site name, VIP and trust domain come from the site spec$' "${out}"
out="$(bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --arch arm64 --dry-run 2>&1)" && fail "a bare --arch exited 0"
expect "the executor refuses an --arch outside linux/amd64 and linux/arm64" '^nvke-deploy: --arch arm64 is not linux/amd64 or linux/arm64$' "${out}"
# A stack config with placeholders stops the render; nothing is applied.
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${state}" bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${REPO}/deploy/examples/stack-config.json" --arch linux/arm64 --dry-run 2>&1)" && fail "executor with placeholders exited 0"
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
    bash "${EXECUTOR}" install --deploy-path "${NICO}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --kubeconfig "${WORK}/stub.kubeconfig" "$@" 2>&1
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
check() { PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${WORK}/check-state" bash "${CHECK}" --sitespec "${SITESPEC}" --stack-config "${STACK}" --kubeconfig "${WORK}/stub.kubeconfig" "$@" 2>&1; }
out="$(STUB_LOG="${WORK}/stub.log" check --release "${WORK}/local.json" --nvkr-run-dir "${run_dir}")" || fail "check-site on a ready cluster exited $?: ${out}"
grep -qE ' (apply|create|delete|patch|taint|annotate) ' "${WORK}/stub.log" && fail "check-site made a mutating kubectl call: $(grep -E ' (apply|create|delete|patch|taint|annotate) ' "${WORK}/stub.log")" || pass "check-site made no mutating kubectl call ($(wc -l <"${WORK}/stub.log" | tr -d ' ') reads)"
reject "no trace id line between findings" '^Trace ID: ' "${out}"
expect "the inputs are said first, with their absolute paths and facts" "^INFO  inputs: site spec ${SITESPEC}, site nvke-site, stack config ${STACK}, nico https://nico.example.com org org-1$" "${out}"
[[ "$(grep -n '^INFO  inputs:' <<<"${out}" | cut -d: -f1)" -lt "$(grep -n '^PASS  site spec:' <<<"${out}" | cut -d: -f1)" ]] && pass "the inputs line comes before the input findings" || fail "inputs line order"
expect "the site spec passes" "^PASS  site spec: ${SITESPEC} is valid$" "${out}"
expect "the stack config passes" "^PASS  stack config: ${STACK}$" "${out}"
expect "the release is named with its verify command" '^PASS  release: .* verify it with: nvkr release verify --reference ' "${out}"
expect "the nodes' architecture is reported" '^PASS  cluster: nodes are arm64$' "${out}"
expect "no taint" '^PASS  cluster: no node carries the control-plane NoSchedule taint$' "${out}"
expect "one default StorageClass" '^PASS  cluster: default StorageClass local-path$' "${out}"
expect "Cluster API is not installed yet and the init command is printed" '^PASS  Cluster API: not installed yet; run: clusterctl init .*--core cluster-api:v[0-9.]+ --bootstrap kubeadm:v[0-9.]+ --control-plane kubeadm:v[0-9.]+ --wait-providers$' "${out}"
expect "the check ends ready with the init command" '^  clusterctl init --kubeconfig .* --wait-providers$' "${out}"
expect "the check ends ready with the executor command, by its resolved path" "^  ${DEPLOY_DIR}/nvke-deploy.sh install --deploy-path ${NICO} --sitespec ${SITESPEC} --stack-config ${STACK} --kubeconfig ${WORK}/stub.kubeconfig --release ${WORK}/local.json --nvkr-run-dir ${run_dir}$" "${out}"
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
out="$(PATH="${stubs}:${PATH}" NVKE_STATE_DIR="${WORK}/check-state" bash "${CHECK}" --sitespec "${SITESPEC}" --stack-config "${REPO}/deploy/examples/stack-config.json" --kubeconfig "${WORK}/stub.kubeconfig" 2>&1)" && fail "check-site with a bad stack config exited 0"
expect "a bad stack config is a finding with its first problem" "^FAIL  stack config: ${REPO}/deploy/examples/stack-config.json is not ready: nico.endpoint: still a REPLACE_\* placeholder; fill it in; run: $(printf '%q' "${NVKECTL}") inspect stack-config ${REPO}/deploy/examples/stack-config.json$" "${out}"
expect "the inputs line leaves out what the bad file could not give" "^INFO  inputs: site spec ${SITESPEC}, site nvke-site, stack config ${REPO}/deploy/examples/stack-config.json$" "${out}"
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
expect "the inputs line is printed without nvkectl, paths only" "^INFO  inputs: site spec ${SITESPEC}, stack config ${STACK}$" "${out}"
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

# hack/local-site.sh and deploy/nvke-deploy-local.sh. The stages shell out to
# the runtimes, so they run here against stubs on PATH that answer "nothing is
# up": the tests cannot touch a node or a simulator on this machine.
LOCAL_SITE="${REPO}/hack/local-site.sh"
LOCAL_DEPLOY="${DEPLOY_DIR}/nvke-deploy-local.sh"
[[ -x "${LOCAL_SITE}" && -x "${LOCAL_DEPLOY}" ]] && pass "the local-site scripts are executable, as make and the page run them" || fail "a local-site script is not executable"
lstubs="${WORK}/local-site-stubs"
mkdir -p "${lstubs}"
printf '#!/usr/bin/env bash\n[[ -z "${STUB_LOG:-}" ]] || printf "%%s\\n" "nerdctl $*" >>"${STUB_LOG}"\ncase "$*" in *inspect*) exit 1 ;; esac\nexit 0\n' >"${lstubs}/nerdctl"
# docker ps answers node-<id> for each instance id in STUB_DOCKER_NODES that
# its filter names, as Docker would for a node with that instance label.
cat >"${lstubs}/docker" <<'STUB'
#!/usr/bin/env bash
[[ -z "${STUB_LOG:-}" ]] || printf '%s\n' "docker $*" >>"${STUB_LOG}"
case "$*" in
  *"network inspect"*) exit 1 ;;
  "ps "*)
    [[ -z "${STUB_DOCKER_PS_FAIL:-}" ]] || exit 1
    for arg in "$@"; do
      [[ "${arg}" == label=nvidia.com/nico-containers.instance-id=* ]] || continue
      for node in ${STUB_DOCKER_NODES:-}; do
        [[ "${node}" != "${arg#*instance-id=}" ]] || printf 'node-%s\n' "${node}"
      done
    done
    ;;
esac
exit 0
STUB
printf '#!/usr/bin/env bash\n[[ -z "${STUB_LOG:-}" ]] || printf "%%s\\n" "kubectl $*" >>"${STUB_LOG}"\nexit 1\n' >"${lstubs}/kubectl"
printf '#!/usr/bin/env bash\nexit 0\n' >"${lstubs}/iptables"
printf '#!/usr/bin/env bash\n[[ -z "${STUB_LOG:-}" ]] || printf "%%s\\n" "ip $*" >>"${STUB_LOG}"\nexit 1\n' >"${lstubs}/ip"
chmod +x "${lstubs}"/*
lsite() { PATH="${lstubs}:${PATH}" NVKE_LOCAL_SITE_DIR="${WORK}/local-site" bash "${LOCAL_SITE}" "$@" 2>&1; }
out="$(lsite)" && fail "local-site.sh without a verb exited 0"
expect "local-site.sh prints its usage" 'hack/local-site.sh setup ' "${out}"
out="$(lsite status)" || fail "local-site.sh status on nothing exited $?: ${out}"
expect "status says the node is down" '^node: *down$' "${out}"
expect "status says the images are not staged" '^images: *not staged$' "${out}"
expect "status says the management cluster is down" '^management: *down$' "${out}"
expect "status says nico-sim is down" '^nico-sim: *down$' "${out}"
: >"${WORK}/local-site.log"
out="$(STUB_LOG="${WORK}/local-site.log" lsite down)" || fail "local-site.sh down on nothing exited $?: ${out}"
expect "down on nothing ends down" '^\[local-site\] down$' "${out}"
reject "down on nothing removes no container" 'docker rm|nerdctl rm|nodes-down' "$(cat "${WORK}/local-site.log")"
reject "down on nothing touches no bridge" '^ip ' "$(cat "${WORK}/local-site.log")"
[[ ! -e "${WORK}/local-site" ]] && pass "down leaves no workspace" || fail "workspace left"
# A directory that is not a workspace is refused, not removed.
mkdir -p "${WORK}/local-site/keep"
out="$(lsite down)" && fail "down on a directory without env.sh exited 0"
expect "down refuses a directory it did not write" 'is not a workspace this script wrote' "${out}"
[[ -d "${WORK}/local-site/keep" ]] && pass "the directory is left alone" || fail "the directory was removed"
rm -rf "${WORK}/local-site"
# An env.sh this script wrote is the other credential; an unrelated one is not.
mkdir -p "${WORK}/local-site" && printf '# shellcheck shell=bash\n# Written by hack/local-site.sh setup; the credentials are the lab'"'"'s.\n' >"${WORK}/local-site/env.sh"
out="$(lsite down)" || fail "down on a workspace with this script's env.sh exited $?: ${out}"
[[ ! -e "${WORK}/local-site" ]] && pass "down removes a workspace by its env.sh header" || fail "workspace with env.sh left"
mkdir -p "${WORK}/local-site" && printf 'export FOO=bar\n' >"${WORK}/local-site/env.sh"
out="$(lsite down)" && fail "down on a directory with an unrelated env.sh exited 0"
expect "down refuses an unrelated env.sh" 'is not a workspace this script wrote' "${out}"
rm -rf "${WORK}/local-site"
# A setup that stopped before env.sh left the marker: down removes it.
mkdir -p "${WORK}/local-site/releases" && : >"${WORK}/local-site/.local-site"
out="$(lsite down)" || fail "down on a half-finished workspace exited $?: ${out}"
[[ ! -e "${WORK}/local-site" ]] && pass "down removes a workspace the setup started" || fail "half-finished workspace left"
# down removes the tenant nodes this workspace's nico-sim created, by the
# instance ids in its database, not every node of the seeded site.
if command -v python3 >/dev/null 2>&1; then
  instance_id=7f3c9a2e-4b1d-4e8a-9c6f-2d5e8b1a0c47
  mkdir -p "${WORK}/local-site" && : >"${WORK}/local-site/.local-site"
  python3 -c 'import sqlite3, sys; c = sqlite3.connect(sys.argv[1]); c.execute("CREATE TABLE instances (id TEXT PRIMARY KEY)"); c.execute("INSERT INTO instances VALUES (?)", (sys.argv[2],)); c.commit()' "${WORK}/local-site/.nico-sim.db" "${instance_id}"
  # Another nico-sim's node on the same Docker daemon, which down must keep.
  decoy_id=0b8e4d1f-6a2c-4f3e-8d7b-5c9a1e2f3d64
  : >"${WORK}/local-site.log"
  out="$(STUB_LOG="${WORK}/local-site.log" STUB_DOCKER_NODES="${instance_id} ${decoy_id}" lsite down)" || fail "down on a workspace with a nico-sim database exited $?: ${out}"
  expect "down looks up this nico-sim's instances by id" "^docker ps .*label=nvidia\\.com/nico-containers\\.instance-id=${instance_id}\$" "$(cat "${WORK}/local-site.log")"
  reject "down does not select by the shared seeded site" 'nico-containers\.site-id' "$(cat "${WORK}/local-site.log")"
  expect "down removes the node it found, with its volumes" "^docker rm -f -v node-${instance_id}\$" "$(cat "${WORK}/local-site.log")"
  reject "down keeps another nico-sim's node" "${decoy_id}" "$(cat "${WORK}/local-site.log")"
  [[ ! -e "${WORK}/local-site" ]] && pass "down removes a workspace with a nico-sim database" || fail "workspace with a nico-sim database left"
  # The database is the only record of those nodes: when it cannot be read,
  # or Docker cannot list them, down stops and keeps the workspace.
  mkdir -p "${WORK}/local-site" && : >"${WORK}/local-site/.local-site"
  printf 'not a database\n' >"${WORK}/local-site/.nico-sim.db"
  out="$(lsite down)" && fail "down on an unreadable nico-sim database exited 0"
  expect "down says it cannot find the tenant nodes" 'cannot tell which tenant nodes are this workspace' "${out}"
  [[ -f "${WORK}/local-site/.nico-sim.db" ]] && pass "down keeps a workspace whose database it cannot read" || fail "workspace with an unreadable database removed"
  rm -rf "${WORK}/local-site"
  mkdir -p "${WORK}/local-site" && : >"${WORK}/local-site/.local-site"
  python3 -c 'import sqlite3, sys; c = sqlite3.connect(sys.argv[1]); c.execute("CREATE TABLE instances (id TEXT PRIMARY KEY)"); c.execute("INSERT INTO instances VALUES (?)", (sys.argv[2],)); c.commit()' "${WORK}/local-site/.nico-sim.db" "${instance_id}"
  out="$(STUB_DOCKER_PS_FAIL=1 lsite down)" && fail "down with a failing docker ps exited 0"
  expect "down says it cannot list the tenant nodes" 'cannot list the tenant nodes in Docker' "${out}"
  [[ -f "${WORK}/local-site/.nico-sim.db" ]] && pass "down keeps the workspace when Docker cannot list the tenant nodes" || fail "workspace removed after a failing docker ps"
  rm -rf "${WORK}/local-site"
fi
if [[ "$(uname -s)" != Linux || "$(id -u)" != 0 ]]; then
  out="$(lsite setup)" && fail "local-site.sh setup off Linux or without root exited 0"
  expect "setup names what it needs" '^local-site: run as root|^local-site: run this inside the Colima machine' "${out}"
fi
# The example stack config loads once the setup's two image names are in.
sed -e 's|REPLACE_CONTROLLER_IMAGE|nvke.local/nke-controller:local|' -e 's|REPLACE_PROFILES_IMAGE|nvke.local/controller-profiles:local|' "${REPO}/deploy/examples/local-site/stack-config.json" >"${WORK}/local-stack-config.json"
out="$("${NVKECTL}" inspect stack-config "${WORK}/local-stack-config.json" 2>&1)" || fail "the local site's stack config does not load: ${out}"
expect "the local site's stack config names nico-sim" "^STACK_NICO_ENDPOINT='http://172.26.0.1:18080'$" "${out}"

# The wrapper: its inputs come from the environment; a dry run prints the
# storage and patch steps around the executor's dry run with the inputs.
out="$(bash "${LOCAL_DEPLOY}" 2>&1)" && fail "nvke-deploy-local.sh without a verb exited 0"
expect "nvke-deploy-local.sh prints its usage" 'deploy/nvke-deploy-local.sh install' "${out}"
out="$(NVKE_LOCAL_SITE_DIR="${WORK}/no-such-site" bash "${LOCAL_DEPLOY}" install --dry-run 2>&1)" && fail "nvke-deploy-local.sh without inputs exited 0"
expect "a missing input is named with the setup to run" 'sitespec.yaml is missing; run make local-site-setup and nvkectl bootstrap site first' "${out}"
: >"${WORK}/stub.log"
out="$(PATH="${stubs}:${PATH}" STUB_LOG="${WORK}/stub.log" NVKE_STATE_DIR="${state}" NVKE_SITESPEC="${SITESPEC}" NVKE_STACK_CONFIG="${STACK}" NVKE_KUBECONFIG="${WORK}/stub.kubeconfig" bash "${LOCAL_DEPLOY}" install --dry-run 2>&1)" || fail "nvke-deploy-local.sh --dry-run exited $?: ${out}"
expect "the dry run names the StorageClass step" '^=== dry run: the lab StorageClass local-path-provisioner v[0-9.]+ would be installed' "${out}"
expect "the executor runs with the inputs from the environment" "^=== inputs: site spec ${SITESPEC}, site nvke-site, stack config ${STACK}, nico https://nico.example.com org org-1$" "${out}"
expect "the dry run names the exec-auth patch" "^=== dry run: controller/hack/apply-capi-spiffe-exec-auth.sh --kubeconfig ${WORK}/stub.kubeconfig would run$" "${out}"
reject "the dry run makes no kubectl call" 'storageclass|apply' "$(cat "${WORK}/stub.log")"
# --dry-run after -- belongs to helm: the wrapper's own steps run for real,
# which against the stub kubectl means it tries the StorageClass.
: >"${WORK}/stub.log"
out="$(PATH="${stubs}:${PATH}" STUB_LOG="${WORK}/stub.log" NVKE_STATE_DIR="${state}" NVKE_SITESPEC="${SITESPEC}" NVKE_STACK_CONFIG="${STACK}" NVKE_KUBECONFIG="${WORK}/stub.kubeconfig" bash "${LOCAL_DEPLOY}" install -- --dry-run 2>&1)" && fail "a helm --dry-run against the stub kubectl exited 0"
reject "a --dry-run after -- is not the wrapper's" '^=== dry run:' "${out}"
expect "the wrapper reaches the StorageClass for real" 'storageclass' "$(cat "${WORK}/stub.log")"

if [[ "${failures}" -eq 0 ]]; then
  echo "all nvke-deploy tests passed"
else
  echo "${failures} nvke-deploy test(s) failed"
  exit 1
fi
