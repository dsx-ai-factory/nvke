#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Build the artifacts deploy/nico installs from this checkout and write the
# stack artifacts file that names them: nvke.nvidia.com/stack-artifacts/v1
# JSON, one artifact per line of OUTPUTS below, each as an OCI layout under
# OUT/images/<name>/.
#
#   cwe                  the controller image, controller/Dockerfile
#   controller-profiles  the profiles image, controller/test/fixtures/Dockerfile
#                        over --profiles-src
#   capnico-controller   the CAPNICo image deploy/nico/capnico/release.yaml
#                        pins, copied from its registry
#   controller-chart     the controller chart, packaged
#   nktp                 the NKTP chart, packaged with its dependencies
#
# The four NVKE-built artifacts are referenced as nvke.local/<name>@<digest>:
# the install imports a layout into the node's containerd under that name,
# so the host is only a name. CAPNICo keeps its upstream repository, so the
# record says where it came from. The other charts deploy/nico names come
# from their public repositories at install time, as the deploy path pins
# them.
#
# Usage: build/stack-artifacts.sh --profiles-src DIR --out DIR [--arch amd64|arm64] [--container-cli docker|nerdctl]
#
# `make local-stack-artifacts` runs it with the repository's defaults. The
# build goes to OUT.tmp, is checked there, and replaces OUT at the end, so a
# failed build leaves the previous OUT as it was.
#
# Producers are functions over the one OUTPUTS list; an unknown flag prints
# the usage.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_PATH="${REPO}/deploy/nico"
NVKECTL="${NVKECTL:-${REPO}/cli/bin/nvkectl}"
HELM_CONFIG_TYPE="application/vnd.cncf.helm.config.v1+json"
HELM_CHART_TYPE="application/vnd.cncf.helm.chart.content.v1.tar+gzip"
LOCAL_REPOSITORY="nvke.local"

PROFILES_SRC=""
OUT=""
ARCH=""
CONTAINER_CLI=""

log() { printf '[stack-artifacts] %s\n' "$*" >&2; }
die() { printf 'stack-artifacts: %s\n' "$*" >&2; exit 1; }

usage() {
  cat >&2 <<'USAGE'
usage: build/stack-artifacts.sh --profiles-src DIR --out DIR [--arch amd64|arm64] [--container-cli docker|nerdctl]

  Builds the images and charts deploy/nico installs from this checkout into
  OCI layouts under OUT/images/ and writes OUT/stack-artifacts.json naming
  them, for deploy/nvke-deploy.sh install --stack-artifacts.

  --profiles-src DIR  the profile content: a directory with profiles/ and
                      runbooks/, built with controller/test/fixtures/Dockerfile
  --out DIR           where to write; a failed build leaves the previous
                      one as it was
  --arch ARCH         amd64 or arm64; default: this machine's
  --container-cli CLI docker (with buildx) or nerdctl; default: docker when
                      it is on PATH, else nerdctl

  NVKECTL             the nvkectl that checks the written file; default:
                      cli/bin/nvkectl from make -C cli build
USAGE
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profiles-src) PROFILES_SRC="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --arch) ARCH="$2"; shift 2 ;;
    --container-cli) CONTAINER_CLI="$2"; shift 2 ;;
    -h | --help) usage ;;
    *) printf 'stack-artifacts: unknown argument: %s\n' "$1" >&2; usage ;;
  esac
done
[[ -n "${PROFILES_SRC}" && -n "${OUT}" ]] || usage
[[ -d "${PROFILES_SRC}" ]] || die "--profiles-src ${PROFILES_SRC} is not a directory"
if grep -rl 'REPLACE_' "${PROFILES_SRC}" >/dev/null 2>&1; then
  die "--profiles-src ${PROFILES_SRC} still holds REPLACE_ placeholders: $(grep -rl 'REPLACE_' "${PROFILES_SRC}" | head -n 3 | tr '\n' ' ')"
fi
if [[ -z "${ARCH}" ]]; then
  case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) die "cannot map $(uname -m) to amd64 or arm64; pass --arch" ;;
  esac
fi
case "${ARCH}" in amd64 | arm64) ;; *) die "--arch ${ARCH} is not amd64 or arm64" ;; esac
if [[ -z "${CONTAINER_CLI}" ]]; then
  if command -v docker >/dev/null 2>&1; then CONTAINER_CLI=docker; else CONTAINER_CLI=nerdctl; fi
fi
case "${CONTAINER_CLI}" in docker | nerdctl) ;; *) die "--container-cli ${CONTAINER_CLI} is not docker or nerdctl" ;; esac
for tool in "${CONTAINER_CLI}" helm oras python3 tar; do
  command -v "${tool}" >/dev/null 2>&1 || die "${tool} is not on PATH"
done
command -v "${NVKECTL}" >/dev/null 2>&1 || die "${NVKECTL} is not available; run make -C cli build, or set NVKECTL"
if [[ "${CONTAINER_CLI}" == docker ]]; then
  docker buildx version >/dev/null 2>&1 || die "docker buildx is needed to write an OCI layout"
fi
case "${OUT}" in /*) ;; *) OUT="${PWD}/${OUT}" ;; esac
OUT="${OUT%/}"
TMP="${OUT}.tmp"
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/stack-artifacts.XXXXXX")"
# A failed build leaves OUT as it was: only the scratch and the tmp tree go.
trap 'rm -rf "${SCRATCH}" "${TMP}"' EXIT
rm -rf "${TMP}"
# A run interrupted between the two renames of the swap left the previous
# output as OUT.old: put it back before building.
[[ -e "${OUT}" || ! -d "${OUT}.old" ]] || mv "${OUT}.old" "${OUT}"
mkdir -p "${TMP}/images"
# name kind digest chartVersion platform repository, one artifact per line.
RECORDS="${SCRATCH}/outputs.tsv"
: >"${RECORDS}"

# layout_digest prints the one manifest digest an OCI layout's index lists.
# An index with more than one manifest is refused: the file names one digest
# per artifact, and a build with provenance or several platforms would leave
# the install a choice it cannot make.
layout_digest() {
  python3 -c '
import json, sys
index = json.load(open(sys.argv[1] + "/index.json"))
manifests = index.get("manifests", [])
if len(manifests) != 1:
    sys.exit("%s: index.json lists %d manifests; a layout holds one artifact" % (sys.argv[1], len(manifests)))
print(manifests[0]["digest"])
' "$1"
}

record() { printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" "$5" "$6" >>"${RECORDS}"; }

# item_value ITEM KEY prints KEY's value from the item's release.yaml, as the
# executor reads it: one unquoted token.
item_value() { awk -v k="$2:" '$1 == k { print $2; exit }' "${DEPLOY_PATH}/$1/release.yaml"; }

# build_layout NAME DOCKERFILE CONTEXT [build args]: a single-platform image
# straight into an OCI layout under images/NAME. No provenance and no SBOM
# attestation, so the index holds one manifest.
build_layout() {
  local name="$1" file="$2" context="$3"; shift 3
  local tar="${SCRATCH}/${name}.tar" dir="${TMP}/images/${name}"
  log "building ${name} (linux/${ARCH}) from ${file#"${REPO}"/}"
  case "${CONTAINER_CLI}" in
    docker) docker buildx build --file "${file}" --platform "linux/${ARCH}" --provenance=false --sbom=false --output "type=oci,dest=${tar}" "$@" "${context}" ;;
    nerdctl) nerdctl build --file "${file}" --platform "linux/${ARCH}" --provenance=false --output "type=oci,dest=${tar}" "$@" "${context}" ;;
  esac
  mkdir -p "${dir}"
  tar -xf "${tar}" -C "${dir}"
  rm -f "${tar}"
  [[ -f "${dir}/index.json" ]] || die "${name}: the build wrote no index.json"
  record "${name}" oci "$(layout_digest "${dir}")" "" "linux/${ARCH}" "${LOCAL_REPOSITORY}/${name}"
}

# copy_capnico NAME: the CAPNICo image deploy/nico pins, copied into a layout
# for this architecture. The reference keeps the upstream repository.
copy_capnico() {
  local name="$1" image dir="${TMP}/images/$1"
  image="$(item_value capnico image)"
  [[ -n "${image}" ]] || die "deploy/nico/capnico/release.yaml names no image"
  log "copying ${image} (linux/${ARCH}) into a layout"
  oras cp --platform "linux/${ARCH}" --to-oci-layout "${image}" "${dir}:${image##*:}"
  [[ -f "${dir}/index.json" ]] || die "${name}: oras wrote no index.json"
  record "${name}" oci "$(layout_digest "${dir}")" "" "linux/${ARCH}" "${image%:*}"
}

# package_chart ITEM: the item's chart from this checkout, its dependencies
# built in a throwaway helm home, packaged, and pushed into a layout as the
# one layer of a Helm OCI artifact. The artifact is named as the item's
# release.yaml binds it (chart.artifact, default the item name).
package_chart() {
  local item="$1" path artifact chart tgz ver helm_home url n=0 dir
  path="$(item_value "${item}" path)"
  [[ -n "${path}" ]] || die "deploy/nico/${item}/release.yaml names no chart path"
  artifact="$(item_value "${item}" artifact)"
  artifact="${artifact:-${item}}"
  chart="${SCRATCH}/charts/${artifact}/src"
  mkdir -p "${chart}"
  # A copy: dependency build writes into the chart's charts/, and the
  # checkout stays as it was.
  # chart.path is relative to the deploy path, as the executor resolves it.
  cp -R "${DEPLOY_PATH}/${path}/." "${chart}/"
  log "packaging ${artifact} from ${path#../../}"
  if [[ -f "${chart}/Chart.lock" ]]; then
    helm_home="${SCRATCH}/charts/${artifact}/helm"
    mkdir -p "${helm_home}"
    while IFS= read -r url; do
      [[ "${url}" == oci://* ]] && continue
      n=$((n + 1))
      HELM_REPOSITORY_CONFIG="${helm_home}/repositories.yaml" HELM_REPOSITORY_CACHE="${helm_home}/cache" \
        helm repo add "dependency-${n}" "${url}" >/dev/null
    done < <(awk '$1 == "repository:" { print $2 }' "${chart}/Chart.yaml" | sort -u)
    HELM_REPOSITORY_CONFIG="${helm_home}/repositories.yaml" HELM_REPOSITORY_CACHE="${helm_home}/cache" \
      helm dependency build "${chart}" --skip-refresh >/dev/null
  fi
  helm package "${chart}" --destination "${SCRATCH}/charts/${artifact}" >/dev/null
  tgz="$(find "${SCRATCH}/charts/${artifact}" -maxdepth 1 -name '*.tgz' | head -n 1)"
  [[ -n "${tgz}" ]] || die "${artifact}: helm package wrote no archive"
  # <name>-<version>.tgz, as helm writes it from Chart.yaml.
  ver="$(basename "${tgz}" .tgz)"
  ver="${ver#"$(awk '$1 == "name:" { print $2; exit }' "${chart}/Chart.yaml")"-}"
  dir="${TMP}/images/${artifact}"
  mkdir -p "${dir}"
  (cd "$(dirname "${tgz}")" && oras push --oci-layout "${dir}:${ver}" --artifact-type "${HELM_CONFIG_TYPE}" "$(basename "${tgz}"):${HELM_CHART_TYPE}" >/dev/null)
  [[ -f "${dir}/index.json" ]] || die "${artifact}: oras wrote no index.json"
  record "${artifact}" helm-oci "$(layout_digest "${dir}")" "${ver}" "" "${LOCAL_REPOSITORY}/${artifact}"
}

# emit writes OUT.tmp/stack-artifacts.json from the records: digests
# checked, artifacts sorted by name, layouts relative to the file.
emit() {
  python3 - "${RECORDS}" "${TMP}/stack-artifacts.json" <<'PY'
import json, re, sys
artifacts = []
for line in open(sys.argv[1]):
    name, kind, digest, chart_version, platform, repository = line.rstrip("\n").split("\t")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        sys.exit("%s: digest %r is not sha256:<64 hex>" % (name, digest))
    if chart_version and not re.fullmatch(r"[\w.+-]+", chart_version):
        sys.exit("%s: chart version %r is not a version" % (name, chart_version))
    artifact = {"name": name, "kind": kind, "digest": digest, "reference": "%s@%s" % (repository, digest)}
    if platform:
        artifact["os"], artifact["arch"] = platform.split("/", 1)
    if chart_version:
        artifact["chartVersion"] = chart_version
    artifact["layout"] = "images/" + name
    artifacts.append(artifact)
artifacts.sort(key=lambda a: a["name"])
doc = {"schemaVersion": "nvke.nvidia.com/stack-artifacts/v1", "artifacts": artifacts}
with open(sys.argv[2], "w") as out:
    json.dump(doc, out, indent=1)
    out.write("\n")
PY
}

# The artifacts, in build order: name, producer, producer arguments.
OUTPUTS=(
  "cwe build_layout ${REPO}/controller/Dockerfile ${REPO}"
  "controller-profiles build_layout ${REPO}/controller/test/fixtures/Dockerfile ${PROFILES_SRC}"
  "capnico-controller copy_capnico"
  "controller-chart package_chart controller"
  "nktp package_chart nktp"
)
commit="$(git -C "${REPO}" rev-parse HEAD 2>/dev/null || echo unknown)"
version="$(git -C "${REPO}" describe --tags --always --dirty 2>/dev/null || echo "${commit}")"
for entry in "${OUTPUTS[@]}"; do
  read -r name producer rest <<<"${entry}"
  case "${producer}" in
    build_layout)
      read -r file context <<<"${rest}"
      if [[ "${name}" == cwe ]]; then
        build_layout "${name}" "${file}" "${context}" --build-arg "BUILD_VERSION=${version}" --build-arg "GIT_VERSION=${commit}"
      else
        build_layout "${name}" "${file}" "${context}"
      fi ;;
    copy_capnico) copy_capnico "${name}" ;;
    package_chart) package_chart "${rest}" ;;
    *) die "unknown producer ${producer}" ;;
  esac
done
emit

# The file is checked where it was built, before it replaces anything.
"${NVKECTL}" inspect stack-artifacts "${TMP}/stack-artifacts.json" --meta >/dev/null || die "the file does not load; this is a bug in this script"

# The swap: the previous OUT goes only once the new one is complete and
# checked. From here the new tree is the one to keep, so the trap no longer
# removes it, and an interruption between the two renames puts the previous
# output back.
trap 'rm -rf "${SCRATCH}"; [[ -e "${OUT}" || ! -d "${OUT}.old" ]] || mv "${OUT}.old" "${OUT}"' EXIT
rm -rf "${OUT}.old"
[[ ! -e "${OUT}" ]] || mv "${OUT}" "${OUT}.old"
mv "${TMP}" "${OUT}"
rm -rf "${OUT}.old"

log "wrote ${OUT}/stack-artifacts.json (${version})"
# awk, not read: a read with IFS=tab collapses the empty chart version.
awk -F'\t' '{ printf "%s\t%s\t%s@%s%s\n", $1, $2, $6, $3, ($4 == "" ? "" : " (" $4 ")") }' "${RECORDS}" | sort
