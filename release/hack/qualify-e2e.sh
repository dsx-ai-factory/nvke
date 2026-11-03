#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# One stream of a qualification run, as steps. .github/workflows/qualify-e2e.yml
# calls each step from its own workflow step, and `qualify local` calls the same
# steps in the same order, so a run from a workstation is the run CI does.
#
#   matrix PLAN                        the lanes' matrix, one entry per stream
#   stream-env PLAN COMBINATION STREAM one stream's values, as the steps read them
#   resolve                            the controller, capi-provisioned and nvkectl, and the run's shape
#   controller-image                   the name the published controller is under
#   site-inputs                        build and stage what the node runs
#   config                             write the e2e config the suite reads
#   run                                run the suite
#   withhold [DIR]                     scrub credentials from what the run wrote
#   artifacts                          list what the suite wrote, for qualify local to collect
#
# A step reads its inputs from the environment and writes what it resolves as
# NAME=value lines to $STEP_ENV, which is $GITHUB_ENV in a workflow. Each later
# step reads them back from its environment. Messages go to stderr.
#
# Runs on the bash macOS ships, 3.2, as well as on the runner's.

# The upper-case names are set by an earlier step, through $STEP_ENV.
# shellcheck disable=SC2153

set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
e2e="${root}/controller/test/e2e"
cd "${root}"

STEP_ENV="${STEP_ENV:-${GITHUB_ENV:-/dev/stdout}}"
# The config the suite reads, relative to the suite's package or absolute.
E2E_CONFIG="${E2E_CONFIG:-e2e-config.yaml}"

say() { echo "$*" >&2; }
fail() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::error::$*"; else echo "error: $*" >&2; fi
  exit 1
}
warn() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning::$*"; else echo "warning: $*" >&2; fi
}
emit() {
  # One line per name: a newline in a value would start another assignment
  # in $GITHUB_ENV, and the steps after this one hold the job's secrets.
  case "$2" in
    *$'\n'* | *$'\r'*) fail "$1 has a line break in its value, which cannot be passed on" ;;
  esac
  printf '%s=%s\n' "$1" "$2" >>"${STEP_ENV}"
  export "$1=$2"
}
upper() { printf '%s' "$1" | tr '[:lower:]' '[:upper:]'; }

# What each step reads. Every name has to be set, though most may be empty:
# empty is a choice (build it, no GPU, the default name), unset is a caller
# that does not know the name exists. The workflow sets them in the job's env
# and through $GITHUB_ENV; `qualify local` sets them itself.
inputs() {
  case "$1" in
    stream-env) echo QUALIFY_E2E_SITE_CONTROLLER_VERSION ;;
    resolve)
      echo SITE_NVKE_CONTROLLER_VERSION NVKE_CONTROLLER_VERSION_STRATEGY PLAN_COMMIT \
        SITE_STAGE DRY_RUN SITE_IMAGE_REGISTRY QUALIFY_E2E_NGC_ORG NGC_API_KEY \
        ARCH PROVIDER GPU OS_IMAGE_AMD64 OS_IMAGE_BOOTLOADER_AMD64 OS_IMAGE_ARM64 OS_IMAGE_BOOTLOADER_ARM64 \
        KUBERNETES_VERSION CNI_OVN_VERSION KIND_VERSION CLUSTERCTL_VERSION NERDCTL_VERSION \
        CLI_VERSION CLI_REF CAPI_PROVISIONED_VERSION SITE_INPUTS
      ;;
    controller-image) echo SITE_NVKE_CONTROLLER_BUILD SITE_IMAGE_REGISTRY SITE_IMAGES SITE_NVKE_CONTROLLER_VERSION ;;
    site-inputs)
      echo SITE_NVKE_CONTROLLER_VERSION SITE_NVKE_CONTROLLER_BUILD SITE_NVKE_CONTROLLER_REPOSITORY \
        SITE_ARCH SITE_CLI_BUILD CLI_VERSION SITE_IMAGES CAPI_PROVISIONED_VERSION SITE_PROVISIONED_BUILD \
        SITE_INPUTS
      ;;
    config)
      echo NODE_USER SITE_NVKECTL SITE_DEPLOY_PATH_REF SITE_IMAGES SITE_NVKE_CONTROLLER_REPOSITORY \
        SITE_INPUTS KEEP_SITE KEEP_TENANT
      ;;
    # The controller's name is the suite's, not this script's: empty leaves it
    # on nvke-controller, and a release from before the rename needs it named.
    run)
      echo KEEP_SITE KEEP_TENANT DRY_RUN SITE_TENANT_EXCLUDE SITE_STAGE SITE_NVKE_CONTROLLER_VERSION CLI_VERSION \
        CAPI_PROVISIONED_VERSION OS_IMAGE_VERSIONS CAPABILITIES SITE_ENVIRONMENT \
        NVKE_CONTROLLER_NAMESPACE NVKE_CONTROLLER_SERVICE
      ;;
  esac
}

require_inputs() {
  local unset=() name
  for name in $(inputs "$1"); do
    [ -n "${!name+set}" ] || unset+=("${name}")
  done
  if [ "${#unset[@]}" -gt 0 ]; then
    fail "$1 reads ${unset[*]}, which the caller did not set (empty is allowed; unset is not)"
  fi
}

# What the suite writes in its package directory, and the ones of them that
# are text and so can carry a credential in an error message.
artifacts=(qualify-e2e.log qualify-e2e.junit.xml qualify-e2e.report.json
  nvkectl-diagnostics.tar.gz tenant-diagnostics.json site-cluster-state.txt)
scrubbed=(qualify-e2e.log qualify-e2e.junit.xml qualify-e2e.report.json tenant-diagnostics.json site-cluster-state.txt)

# One entry per stream of every combination.
matrix_entries() {
  jq -c '
    def entry($all; $name): ([$all[] | select(.name == $name)] | first);
    def value($all; $name): entry($all; $name) as $e | ($e.url // $e.version // "");
    def version($all; $name): entry($all; $name) as $e | ($e.version // "");
    def url($all; $name): entry($all; $name) as $e | ($e.url // "");
    def versionStrategy($all; $name): entry($all; $name) as $e | ($e.versionStrategy // "");
    [
      .combinations[] as $c
      | $c.streams[] as $s
      | ($c.components + $c.dependencies + $s.dependencies) as $all
      | {
          combination: $c.id,
          stream: $s.name,
          arch: ($s.capabilities.arch // ""),
          provider: ($s.capabilities.provider // ""),
          gpu: ($s.capabilities.gpu // ""),
          controller: version($all; "controller"),
          cli: version($all; "cli"),
          capi_provisioned: version($all; "capi-provisioned"),
          kube: value($all; "kube"),
          cni_ovn: value($all; "cni-ovn"),
          os_image: version($all; "os-image"),
          kind: value($all; "kind"),
          clusterctl: value($all; "clusterctl"),
          nerdctl: value($all; "nerdctl"),
          os_image_amd64: value($all; "os-image-amd64"),
          os_image_arm64: value($all; "os-image-arm64"),
          os_image_bootloader_amd64: value($all; "os-image-bootloader-amd64"),
          os_image_bootloader_arm64: value($all; "os-image-bootloader-arm64"),
          cli_ref: url($all; "cli-" + $s.capabilities.arch),
          controller_version_strategy: versionStrategy($all; "controller")
        }
    ]' "$1"
}

step_matrix() {
  matrix_entries "$1" | jq -c '{include: .}'
}

step_stream_env() {
  local plan="$1" combination="$2" stream="$3" entry
  entry="$(matrix_entries "${plan}" | jq -c --arg c "${combination}" --arg s "${stream}" \
    '[.[] | select(($c == "" or .combination == $c) and .stream == $s)]')"
  case "$(jq length <<<"${entry}")" in
    1) entry="$(jq -c '.[0]' <<<"${entry}")" ;;
    0) fail "the plan has no stream ${stream}${combination:+ in combination ${combination}}" ;;
    *) fail "the plan has ${stream} in more than one combination; name the combination" ;;
  esac
  field() { jq -r --arg k "$1" '.[$k]' <<<"${entry}"; }

  emit COMBINATION "$(field combination)"
  emit STREAM "$(field stream)"
  # Where the nvkectl artifact for this stream's architecture is. The plan
  # names it, because nvkectl is published as one artifact per platform and
  # the cli version below names none of them.
  emit CLI_REF "$(field cli_ref)"
  # The plan's controller, unless a stream's environment names another.
  # QUALIFY_E2E_CONTROLLER_VERSION cannot do this: it feeds the plan, which
  # runs under ci-shared, so it is repository-wide and lands on every
  # concurrent run. This one is read here, where the environment the stream
  # selected is attached, so a value set on dsx is invisible to forge.
  #
  # It overrides what is INSTALLED, not what the plan RECORDS, so a run
  # using it reports the plan's controller while the node runs another.
  # That is a deliberate hole for bringing up a lane against an unreleased
  # controller; a qualification anyone relies on sets nothing here.
  emit SITE_NVKE_CONTROLLER_VERSION "${QUALIFY_E2E_SITE_CONTROLLER_VERSION:-$(field controller)}"
  # How the controller's version was resolved, and the commit a described
  # one was taken at. Together they say what the deploy path is exported
  # from, which is a tag for one and a commit for the other.
  emit NVKE_CONTROLLER_VERSION_STRATEGY "$(field controller_version_strategy)"
  emit PLAN_COMMIT "$(jq -r '.commit // ""' "${plan}")"
  emit CLI_VERSION "$(field cli)"
  emit KUBERNETES_VERSION "$(field kube)"
  emit CNI_OVN_VERSION "$(field cni_ovn)"
  emit KIND_VERSION "$(field kind)"
  emit CLUSTERCTL_VERSION "$(field clusterctl)"
  emit NERDCTL_VERSION "$(field nerdctl)"
  # The os-image release the images were built from.
  emit OS_IMAGE_RELEASE "$(field os_image)"
  emit OS_IMAGE_BOOTLOADER_AMD64 "$(field os_image_bootloader_amd64)"
  emit OS_IMAGE_AMD64 "$(field os_image_amd64)"
  emit OS_IMAGE_BOOTLOADER_ARM64 "$(field os_image_bootloader_arm64)"
  emit OS_IMAGE_ARM64 "$(field os_image_arm64)"
  emit ARCH "$(field arch)"
  emit PROVIDER "$(field provider)"
  emit GPU "$(field gpu)"
  emit CAPI_PROVISIONED_VERSION "$(field capi_provisioned)"
}

# nvkectl is a client of the controller it bootstraps, so the two have to agree
# on the cluster API. By default both are built from this checkout,
# which makes them one commit. A published pair has to be chosen to
# agree: GHCR carries release tags only and its newest, v0.1.1-rc.8, is
# 43 commits before #850, which removed infra.spireAttestationMethod
# from the cluster API. Run 35999250795 spent forty minutes reaching
#   400 Bad Request: infra.spireAttestationMethod is required
# on exactly that pairing.
#
# So the default is to build, tagged with the commit under test -- on a
# pull_request that is the merge commit, which is the thing being
# qualified. Not `git describe`: the checkout is shallow and carries no
# tags, so it would report v0.0.0 and silently stop being unique.
#
# Naming a version instead pulls that published tag, for qualifying a
# release once one is cut past #850. The deploy path follows the
# controller -- it carries the controller's own chart -- so that
# version's controller/<version> release tag is fetched for the suite
# to export it from.
resolve_controller() {
  if [ -z "${SITE_NVKE_CONTROLLER_VERSION}" ]; then
    local version
    version="$(git rev-parse --short=12 HEAD)"
    emit SITE_NVKE_CONTROLLER_BUILD 1
    emit SITE_NVKE_CONTROLLER_VERSION "${version}"
    say "building the controller from ${version}"
    return
  fi

  emit SITE_NVKE_CONTROLLER_BUILD 0
  say "qualifying the published ${SITE_NVKE_CONTROLLER_VERSION}"
  warn "pulling a published controller, ${SITE_NVKE_CONTROLLER_VERSION}; if it predates #850 the bootstrap fails at \"creating the site cluster\""

  # The deploy path is exported from the controller's own tree, so that tree
  # has to be here. What names it depends on how the version was resolved.
  #
  # A version read from the tags is a tag. A description is not a ref,
  # so there is nothing by that name to ask the remote for, and the
  # abbreviated hash inside it is refused over the wire. It resolves
  # locally through that hash once the commit is here, and the full
  # hash is what the plan recorded.
  local have want missing
  if [ "${NVKE_CONTROLLER_VERSION_STRATEGY}" = commitDescribe ]; then
    [ -n "${PLAN_COMMIT}" ] || fail "the controller was described at a commit, but the plan records none"
    have="${PLAN_COMMIT}"
    want="${PLAN_COMMIT}"
    missing="could not fetch ${PLAN_COMMIT}, the commit ${SITE_NVKE_CONTROLLER_VERSION} describes"
  else
    local tag="controller/${SITE_NVKE_CONTROLLER_VERSION}"
    have="refs/tags/${tag}"
    want="refs/tags/${tag}:refs/tags/${tag}"
    missing="no ${tag} release tag; the deploy path is exported from the controller's own commit"
  fi
  if git cat-file -e "${have}^{commit}" 2>/dev/null; then
    return
  fi
  # The workflow's checkout is shallow and tagless, and without credentials,
  # hence the one-off header.
  local auth=()
  if [ -n "${GITHUB_ACTIONS:-}" ] && [ -n "${GITHUB_TOKEN:-}" ]; then
    local basic
    basic="$(printf 'x-access-token:%s' "${GITHUB_TOKEN}" | base64 | tr -d '\n')"
    echo "::add-mask::${basic}"
    auth=(-c "http.https://github.com/.extraheader=AUTHORIZATION: basic ${basic}")
  fi
  # A depth in a full clone would make it shallow, which a workstation's
  # clone has no reason to become.
  local depth=()
  [ "$(git rev-parse --is-shallow-repository)" = true ] && depth=(--depth=1)
  git ${auth[@]+"${auth[@]}"} fetch --no-tags ${depth[@]+"${depth[@]}"} "${FETCH_REMOTE:-origin}" "${want}" || fail "${missing}"
}

# Two checks that each cost a second and each save a run: the ref the
# join token will see, and the values a pull_request event does not
# supply. Both fail here rather than after a machine exists.
check_viable() {
  # The join token allows refs/heads/main, refs/pull/*/merge and a
  # repo-wide beta tag, and nothing else, so a dispatch from any other
  # branch mints nothing. The patterns are the token's own, so a
  # change there is a change here. Only a workflow joins with it: a person
  # runs on their own Teleport login.
  if [ -n "${GITHUB_ACTIONS:-}" ] && [ "${DRY_RUN:-}" != "true" ]; then
    case "${GITHUB_REF}" in
      refs/heads/main | refs/pull/*/merge | refs/tags/v*-beta.*) ;;
      *)
        fail "running from ${GITHUB_REF}; the Teleport join token allows only" \
          "refs/heads/main, refs/pull/*/merge and refs/tags/v*-beta.*." \
          "Dispatch from main, label a pull request, or push a beta tag."
        ;;
    esac
  fi

  # Empty on a pull_request run whose QUALIFY_E2E_* variable is unset.
  # SITE_NVKE_CONTROLLER_VERSION is not here: the step above resolves it.
  local missing=() name
  # shellcheck disable=SC2043
  for name in SITE_STAGE; do
    [ -n "${!name:-}" ] || missing+=("${name}")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    fail "no value for ${missing[*]}. A dispatch takes these from its inputs;" \
      "a labelled pull request takes them from the QUALIFY_E2E_* repository variables."
  fi

  # The image path, resolved here so every later step reads one value.
  # Same image names on both except the controller on GHCR before the
  # rename, resolved below; only the prefix differs.
  local images
  case "${SITE_IMAGE_REGISTRY}" in
    ghcr) images="ghcr.io/dsx-ai-factory/nvke" ;;
    # Nothing is pulled: every image is built here and reaches the node in
    # an archive, so the prefix only has to be a name no registry answers
    # to. check_local_images refuses a run that would pull one anyway.
    local) images="localhost/nvke" ;;
    nvcr)
      if [ -z "${QUALIFY_E2E_NGC_ORG:-}" ] || [ -z "${NGC_API_KEY:-}" ]; then
        fail "image-registry nvcr needs the QUALIFY_E2E_NGC_ORG repository variable and the NGC_API_KEY secret"
      fi
      images="nvcr.io/${QUALIFY_E2E_NGC_ORG}"
      ;;
    *) fail "image-registry ${SITE_IMAGE_REGISTRY} is not ghcr, nvcr or local" ;;
  esac
  emit SITE_IMAGES "${images}"
  say "stage=${SITE_STAGE} controller=${SITE_NVKE_CONTROLLER_VERSION} images=${images}"
}

# ARCH is both a capability the suite matches machines on and the
# architecture `make site-inputs` builds for, so it is one value used
# twice rather than two that could disagree.
resolve_capabilities() {
  local arch="${ARCH:-amd64}"
  case "${arch}" in
    amd64 | arm64) ;;
    *) fail "arch is ${arch}, which is neither amd64 nor arm64" ;;
  esac
  emit SITE_ARCH "${arch}"

  # The stream names a provider; this names the file that describes
  # that provider's infrastructure. Mapped rather than used as the
  # filename directly: -e2e.environment names a file, not a backend,
  # and collapsing the two would rule out a second environment for
  # one provider -- a staging Forge beside the production one.
  #
  # tenant_exclude is the other half of the same mapping. At
  # -e2e.site-stage full the site lane registers one tenant entry per
  # dialect and selects them all with one label filter, so an entry
  # built for another provider reaches this run and fails: each site
  # declaration decodes strictly, and a DSX environment is "unknown
  # field api" to Forge's decoder (the reverse holds too). Naming what
  # to leave out keeps the site-level specs, which carry
  # site-controller and no nico-* label and would be dropped by a
  # filter that required one.
  #
  # A provider added here needs a tenant_exclude on every other arm as
  # well as its own. That is the cost of excluding by name; the arms
  # are at least all in this one block, and an unknown provider
  # already fails below rather than running the wrong tenant.
  local environment tenant_exclude
  case "${PROVIDER:-}" in
    forge) environment=forge tenant_exclude=nico-dsx ;;
    dsx) environment=dsx tenant_exclude=nico-forge ;;
    "") fail "the stream names no provider capability" ;;
    *) fail "provider is ${PROVIDER}, which this workflow has no environment for" ;;
  esac
  emit SITE_ENVIRONMENT "${environment}"
  emit SITE_TENANT_EXCLUDE "${tenant_exclude}"
  say "environment: ${environment}, tenant entries excluded: ${tenant_exclude}"

  # Joined here rather than taken as a string, so each key is set and
  # validated on its own. Empty keys are left out: the suite reads an
  # absent gpu as "the workers take the CPU shape", which is not the
  # same as asking for a GPU named "".
  local capabilities="arch=${arch}"
  if [ -n "${GPU:-}" ]; then
    capabilities="${capabilities},gpu=${GPU}"
  fi
  emit CAPABILITIES "${capabilities}"
  say "capabilities: ${capabilities}"

  # Every architecture that has a complete pair is passed, and the
  # suite takes the one each machine needs. Which architecture the GPU
  # workers are is a property of the instance type, which only the
  # environment file knows, so deciding it here would mean guessing at
  # something the suite can read. Passing both costs nothing and is
  # never wrong.
  #
  # The run fails on a missing pair rather than here, in requireNodeArtifacts,
  # which names the key it looked for and the machine that needed it.
  local versions="" pairs=0 pair_arch image loader name
  for pair_arch in amd64 arm64; do
    name="OS_IMAGE_$(upper "${pair_arch}")"
    image="${!name:-}"
    name="OS_IMAGE_BOOTLOADER_$(upper "${pair_arch}")"
    loader="${!name:-}"
    if [ -z "${image}" ] && [ -z "${loader}" ]; then
      continue
    fi
    if [ -z "${image}" ] || [ -z "${loader}" ]; then
      fail "os-image-${pair_arch} and os-image-bootloader-${pair_arch} go together: a qcow2 needs its imager"
    fi
    versions="${versions},os-image-bootloader-${pair_arch}=${loader},os-image-${pair_arch}=${image}"
    pairs=$((pairs + 1))
    say "${pair_arch} OS image: ${image}"
  done
  if [ "${pairs}" -eq 0 ]; then
    fail "no OS image pair for any architecture. A dispatch takes these from its inputs;" \
      "a labelled pull request takes them from the QUALIFY_E2E_OS_IMAGE_<ARCH> repository variables."
  fi

  # Keyed as the suite reads them. The suite requires every one of
  # these, so an empty one stops the run here and not after the node
  # exists.
  local pair
  for pair in "kube=${KUBERNETES_VERSION:-}" "cni-ovn=${CNI_OVN_VERSION:-}" \
    "kind=${KIND_VERSION:-}" "clusterctl=${CLUSTERCTL_VERSION:-}" "nerdctl=${NERDCTL_VERSION:-}"; do
    if [ -z "${pair#*=}" ]; then
      fail "no value for ${pair%%=*}. The qualification plan supplies it, so the plan did not resolve one for this stream."
    fi
    versions="${versions},${pair}"
    say "component version: ${pair}"
  done
  emit OS_IMAGE_VERSIONS "${versions#,}"
}

# A named version stages the published nvkectl rather than one built
# here, so a candidate can pair a published CLI with a published controller.
# site-inputs does the pull and the checks
# (controller/test/e2e/hack/fetch-nvkectl.sh); this
# step only names the artifact and where the staged binary lands, which
# is the Makefile's SITE_NVKECTL.
#
# The reference comes from the plan rather than being built here, so the
# artifact this pulls is the one `qualify verify` already waited for. Every
# location in a plan carries a scheme and oras takes a bare reference, so
# the scheme is dropped.
resolve_nvkectl() {
  if [ -z "${CLI_VERSION:-}" ]; then
    local version
    version="$(git rev-parse --short=12 HEAD)"
    emit SITE_CLI_BUILD 1
    emit CLI_VERSION "${version}"
    emit SITE_NVKECTL "cli/dist/nvkectl-linux-${SITE_ARCH}"
    say "building nvkectl from ${version}"
    return
  fi

  [ -n "${CLI_REF:-}" ] || fail "no cli-${SITE_ARCH} in the plan, so there is no nvkectl artifact to stage for ${CLI_VERSION}"
  local ref="${CLI_REF#oci://}"
  emit SITE_CLI_BUILD 0
  emit SITE_CLI_REF "${ref}"
  emit SITE_NVKECTL "${SITE_INPUTS}/nvkectl/nvkectl-linux-${SITE_ARCH}"
  say "staging the published nvkectl ${CLI_VERSION} from ${ref}"
}

# Built as the controller is when no version is named: from this checkout,
# tagged with and reporting the commit under test, which is what the suite
# is told to expect of the provider it finds running.
resolve_provisioned() {
  if [ -z "${CAPI_PROVISIONED_VERSION}" ]; then
    # The workflow has no input that asks for this build, so an empty
    # version there is a plan that named none, not a choice.
    [ -z "${GITHUB_ACTIONS:-}" ] || fail "the plan names no capi-provisioned version"
    local version
    version="$(git rev-parse --short=12 HEAD)"
    emit SITE_PROVISIONED_BUILD 1
    emit CAPI_PROVISIONED_VERSION "${version}"
    say "building capi-provisioned from ${version}"
    return
  fi
  emit SITE_PROVISIONED_BUILD 0
}

# A local registry is a name, not a place: an image that is not built has
# nowhere to come from, and would fail at the pull in site-inputs instead.
check_local_images() {
  [ "${SITE_IMAGE_REGISTRY}" = local ] || return 0
  local pulled=()
  [ "${SITE_NVKE_CONTROLLER_BUILD}" = 1 ] || pulled+=(controller)
  [ "${SITE_CLI_BUILD}" = 1 ] || pulled+=(cli)
  [ "${SITE_PROVISIONED_BUILD}" = 1 ] || pulled+=(capi-provisioned)
  if [ "${#pulled[@]}" -gt 0 ]; then
    fail "image-registry local builds every image, but ${pulled[*]} would be pulled; name no version for them"
  fi
}

# A build takes the working tree and is named for HEAD, so a change not yet
# committed runs under a name that does not contain it, and the deploy path,
# exported from the commit, runs without it.
warn_uncommitted() {
  [ "${SITE_NVKE_CONTROLLER_BUILD}${SITE_CLI_BUILD}${SITE_PROVISIONED_BUILD}" != 000 ] || return 0
  [ -n "$(git status --porcelain --untracked-files=no)" ] || return 0
  warn "the checkout has uncommitted changes: what is built here carries them under $(git rev-parse --short=12 HEAD)'s name, and the deploy path exported from that commit does not"
}

step_resolve() {
  resolve_controller
  resolve_provisioned
  check_viable
  resolve_capabilities
  resolve_nvkectl
  check_local_images
  warn_uncommitted
}

# nvke-controller is the only name on GHCR; images under nke-controller
# are gone, so a release from before the rename cannot be pulled from
# GHCR. Probed after the registry logins, because the GHCR package is
# private, so a missing tag fails before a machine is taken. site-inputs
# and the e2e config both read the result, which is what keeps the name
# on the node's archive and the name the Helm values ask for the same.
step_controller_image() {
  local repository=nvke-controller
  if [ "${SITE_NVKE_CONTROLLER_BUILD}" != 1 ] && [ "${SITE_IMAGE_REGISTRY}" = ghcr ]; then
    if ! docker manifest inspect "${SITE_IMAGES}/nvke-controller:${SITE_NVKE_CONTROLLER_VERSION}" >/dev/null 2>&1; then
      fail "${SITE_IMAGES}/nvke-controller:${SITE_NVKE_CONTROLLER_VERSION} is not on GHCR"
    fi
  fi
  emit SITE_NVKE_CONTROLLER_REPOSITORY "${repository}"
  say "controller image: ${SITE_IMAGES}/${repository}:${SITE_NVKE_CONTROLLER_VERSION}"
}

# nvkectl for the node's architecture, the three image archives, and the
# deploy path out of the checkout. The node has no access to this
# repository, so everything it runs has to be built here and staged.
# SITE_NVKE_CONTROLLER_BUILD, SITE_PROVISIONED_BUILD and SITE_CLI_BUILD come
# from the resolve step and decide whether the controller and capi-provisioned
# archives and nvkectl are built here or were published.
step_site_inputs() {
  make -C controller/test/e2e site-inputs \
    SITE_NVKE_CONTROLLER_VERSION="${SITE_NVKE_CONTROLLER_VERSION}" \
    SITE_NVKE_CONTROLLER_BUILD="${SITE_NVKE_CONTROLLER_BUILD}" \
    SITE_NVKE_CONTROLLER_REPOSITORY="${SITE_NVKE_CONTROLLER_REPOSITORY}" \
    SITE_ARCH="${SITE_ARCH}" \
    SITE_CLI_BUILD="${SITE_CLI_BUILD}" \
    SITE_CLI_VERSION="${CLI_VERSION}" \
    SITE_IMAGE_REGISTRY="${SITE_IMAGES}" \
    SITE_PROVISIONED_VERSION="${CAPI_PROVISIONED_VERSION}" \
    SITE_PROVISIONED_BUILD="${SITE_PROVISIONED_BUILD}" \
    SITE_INPUTS="${SITE_INPUTS}"
}

# Where each staged image came from, and nothing else. Versions are
# -e2e.versions keys, the jump host is the resolved site's and comes
# from that provider's environment file, and every secret is read from
# the environment by the suite itself -- see the const block at the top
# of controller/test/e2e/qualify_e2e_test.go.
step_config() {
  # The jump host is not asserted here. A provider can declare more
  # than one site, each with its own jump host -- forge.yaml does
  # today -- and which one a run reaches is decided inside the suite by
  # capability, so a check up here can only name a site and be wrong
  # about the rest. forge.New and dsx.New each validate the jump host
  # of the site that actually resolved, name that site in the error,
  # and do it before anything is created, which is the check this step
  # was reaching for.
  # The repositories are the ones site-inputs pulled or tagged, so the
  # references the suite hands the bootstrap and the deploy path are
  # the ones the archives carry.
  local config
  case "${E2E_CONFIG}" in
    /*) config="${E2E_CONFIG}" ;;
    *) config="${e2e}/${E2E_CONFIG}" ;;
  esac
  cat >"${config}" <<EOF
# Generated by release/hack/qualify-e2e.sh.
node:
  user: "${NODE_USER:-}"
nvkectl:
  binary: ${SITE_NVKECTL}
site:
  # Empty follows the controller version, which is what a
  # qualification wants: the deploy path carries the controller's own
  # chart and the two have to agree. It is set only when the deploy
  # path itself is under development and the controller predates it,
  # which is the one case the field exists for.
  deployPathRef: "${SITE_DEPLOY_PATH_REF:-}"
  images:
    controller:
      repository: ${SITE_IMAGES}/${SITE_NVKE_CONTROLLER_REPOSITORY}
      archive: ${SITE_INPUTS}/controller.tar
    capi-provisioned:
      repository: ${SITE_IMAGES}/cluster-api-provider-provisioned
      archive: ${SITE_INPUTS}/capi-provisioned.tar
    profiles:
      repository: nke-e2e-profiles
      tag: local
      archive: ${SITE_INPUTS}/profiles.tar
EOF
  # -e2e.keep-site holds only the site node. The tenant's own cleanup
  # still deletes a failed tenant's cluster, which releases the very
  # instances the site was kept to look at. skipDelete keeps every tenant,
  # passed or not, and the site with them, since it owns their instances;
  # it also skips the spec that deletes the tenant.
  if [ "${KEEP_TENANT:-}" = "true" ]; then
    printf 'lifecycle:\n  skipDelete: true\n' >>"${config}"
  elif [ "${KEEP_SITE:-}" = "true" ]; then
    printf 'lifecycle:\n  preserveOnFailure: true\n' >>"${config}"
  fi
  say "--- ${E2E_CONFIG} ---"
  cat "${config}" >&2
}

# Ignores the interrupt a terminal sends the whole process group, so the log
# keeps taking the suite's output while Ginkgo tears the node down.
log_to() {
  trap '' INT
  tee "$1"
}

# Three nested timeouts, shortest first, because only the shortest one
# tears the node down:
#
#   -ginkgo.timeout 150m  fails the suite and runs its cleanup, which
#                         is what releases the Forge instance.
#   -timeout        220m  go's own backstop. It panics rather than
#                         unwinding, so deferred teardown does NOT run
#                         and the instance leaks -- reaching this means
#                         Ginkgo itself is wedged.
#   timeout-minutes       the job's, in qualify-e2e.yml: the runner is
#                         killed. Same leak, no log.
#
# The 70 minutes between the first two is cleanup's, and it is sized
# against cleanup rather than picked. A cleanup node entered after the
# suite is already interrupted is granted its whole NodeTimeout again,
# so the worst case is every one of them end to end: the tenant
# AfterAll's 15 minutes and the site AfterAll's 45, which is 60, plus
# ten for the unwind itself. Shorten this gap below that sum and the
# backstop starts firing during a teardown that was going to succeed --
# which is the one failure that leaks the instance.
#
# There is no fourth clock. The bot certificate expires in an hour, but
# tbot rewrites it every twenty minutes for as long as the job runs --
# see the daemon step. Before that it was a real limit on `full`.
#
# JUMP_HOST_PASSWORD has to be unset: its presence is what switches the
# suite off the Teleport relay and onto plain SSH.
step_run() {
  cd "${e2e}"
  # A run that was interrupted, or the suite run by hand, leaves its outputs
  # here, and the artifacts step would collect them as this run's.
  rm -f "${artifacts[@]}" site-bundle*
  local keep=()
  if [ "${KEEP_SITE:-}" = "true" ]; then
    keep=(-e2e.keep-site)
    warn "-e2e.keep-site is set; the site node, and a failed tenant's nodes, survive this run and must be deleted by hand"
  fi
  if [ "${KEEP_TENANT:-}" = "true" ]; then
    warn "lifecycle.skipDelete is set; every tenant and the site node survive this run and must be deleted by hand, and tenant deletion is not tested"
  fi
  # -v so a green run keeps its output. Without it `go test` buffers a
  # passing package's stdout and throws it away, which is what run
  # 36033032946 did: the first successful bootstrap on hardware, and
  # the artifact came back 1.7 KB with no Ginkgo log in it. A green run
  # is the reference for diagnosing the next red one, and that only holds
  # if it is kept.
  # Four flags every other e2e lane already passes, via
  # backend-e2e-lib.sh:
  #
  #   fail-on-empty  a filter matching nothing is otherwise a pass.
  #                  This lane spends half an hour of bare metal
  #                  before its first assertion, so "green having
  #                  tested nothing" is the one result it must not
  #                  be able to report -- rename the site-controller
  #                  label and that is exactly what you would get.
  #   no-color       the log is an artifact people read, and Ginkgo's
  #                  escapes survive the tee into it.
  #   junit-report   machine-readable beside the human-readable log.
  #                  A qualification run should be able to say which
  #                  specs it ran to something that is not a person.
  #   json-report    JUnit has no equivalent of PreRunStats, and
  #                  without those a leg's skip count is the size of
  #                  the suite rather than the size of the run.
  #                  Written but not uploaded: a Ginkgo report
  #                  carries captured spec output, which on this lane
  #                  is remote command output, so it stays on the
  #                  runner.
  #
  local versions="controller=${SITE_NVKE_CONTROLLER_VERSION},cli=${CLI_VERSION},capi-provisioned=${CAPI_PROVISIONED_VERSION},${OS_IMAGE_VERSIONS}"
  if [ "${DRY_RUN:-}" = "true" ]; then
    {
      echo "dry run: ginkgo is not run. It would have run with:"
      echo "  -ginkgo.label-filter=site-controller && !${SITE_TENANT_EXCLUDE}"
      echo "  -e2e.site-stage=${SITE_STAGE}"
      echo "  -e2e.versions=${versions}"
      echo "  -e2e.capabilities=${CAPABILITIES}"
      echo "  keep: ${keep[*]:-no}"
      echo "  keep tenants: ${KEEP_TENANT}"
    } | tee qualify-e2e.log
    return
  fi
  [ -z "${JUMP_HOST_PASSWORD:-}" ] || fail "JUMP_HOST_PASSWORD is set, which takes the suite off the Teleport relay; unset it"
  go test -v -timeout 220m . -args \
    -ginkgo.label-filter="site-controller && !${SITE_TENANT_EXCLUDE}" \
    -ginkgo.timeout=150m \
    -ginkgo.v \
    -ginkgo.fail-on-empty \
    -ginkgo.no-color \
    -ginkgo.junit-report=qualify-e2e.junit.xml \
    -ginkgo.json-report=qualify-e2e.report.json \
    -e2e.config="${E2E_CONFIG}" \
    -e2e.environment="${SITE_ENVIRONMENT}" \
    -e2e.site-stage="${SITE_STAGE}" \
    -e2e.versions="${versions}" \
    -e2e.capabilities="${CAPABILITIES}" \
    ${keep[@]+"${keep[@]}"} 2>&1 | log_to qualify-e2e.log
}

# The suite names the jump host in its own errors ("not reachable
# through ..."), and the SSA token exchange names its issuer in the
# errors it returns. Both land in the log and the JUnit report, and
# GitHub masks a secret in the job log but not in an uploaded artifact.
#
# Every declared host: a run reaches one of them, and which one is not
# worth branching on here. Discovered by suffix rather than listed, so a
# datahall added to environments/dsx.yaml is withheld by having named its
# secret *_JUMP_HOST, with nothing to update here. A hand-written list
# fails the other way: the value it omits is the one the suite names in
# its errors and the artifact then publishes, with no error to notice.
step_withhold() {
  cd "${1:-${e2e}}"
  local var raw trimmed form value f
  while IFS= read -r var; do
    raw="${!var:-}"
    [ -n "${raw}" ] || continue
    # Both spellings. auth.go trims whitespace and a trailing slash
    # from the issuer before it uses one, so an issuer stored with a
    # slash reaches an error without it and a single literal match
    # would leave that form in the artifact.
    trimmed="$(printf '%s' "${raw}" | sed 's#^[[:space:]]*##; s#[[:space:]]*$##; s#/*$##')"
    for form in "${raw}" "${trimmed}"; do
      [ -n "${form}" ] || continue
      value="$(printf '%s' "${form}" | sed 's/[][\.*^$/]/\\&/g')"
      for f in "${scrubbed[@]}"; do
        [ -f "${f}" ] || continue
        # No < or >: in qualify-e2e.junit.xml they would make the
        # report invalid XML, which neither `qualify publish` nor
        # TestGrid can read.
        sed "s/${value}/[withheld]/g" "${f}" >"${f}.withheld" || fail "could not scrub ${f}"
        mv "${f}.withheld" "${f}"
      done
    done
  done < <(compgen -v | grep -E '_JUMP_HOST$|^SSA_ISSUER$' || true)
}

# What the suite wrote, for a caller to collect after withhold.
step_artifacts() {
  cd "${e2e}"
  local f
  for f in "${artifacts[@]}" site-bundle*; do
    [ -e "${f}" ] && echo "${e2e}/${f}"
  done
  return 0
}

step="${1:-}"
[ "$#" -gt 0 ] && shift
require_inputs "${step}"
# One braced group ending in exit, so bash has parsed all of it before the
# step starts. bash reads a script as it runs it: a step that outlives an
# edit to this file would otherwise resume at its old offset in the new one.
{
  case "${step}" in
    matrix) step_matrix "$@" ;;
    stream-env) step_stream_env "$@" ;;
    resolve) step_resolve ;;
    controller-image) step_controller_image ;;
    site-inputs) step_site_inputs ;;
    config) step_config ;;
    run) step_run ;;
    withhold) step_withhold "$@" ;;
    artifacts) step_artifacts ;;
    *)
      sed -n '5,21p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
      exit 2
      ;;
  esac
  exit
}
