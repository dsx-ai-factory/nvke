#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Publisher entry points. Called from the protected Cluster Service workflow.
set -euo pipefail
root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "${root}"
operation=${1:?expected version, build, index, tag or promote}

version() {
  local revision=${GITHUB_SHA:?source commit is required}
  [[ "${revision}" =~ ^[0-9a-f]{40}$ ]] || { echo "invalid source commit" >&2; return 1; }
  case "${GITHUB_REF:?source ref is required}" in
    refs/tags/management-engine/*)
      local value=${GITHUB_REF#refs/tags/management-engine/}
      local number='(0|[1-9][0-9]*)'
      [[ "${value}" =~ ^v${number}\.${number}\.${number}(-rc\.${number})?$ ]] || { echo "expected management-engine/vX.Y.Z or management-engine/vX.Y.Z-rc.N" >&2; return 1; }
      printf '%s\n' "${value}" ;;
    refs/heads/main) printf 'v0.0.0-sha.g%s\n' "${revision}" ;;
    *) echo "publication requires main or a management-engine release tag" >&2; return 1 ;;
  esac
}
if [[ "${operation}" == version ]]; then version; exit; fi
VERSION=$(version)
: "${RUNTIME:?runtime target is required}" "${NVKE_NGC_ORG:?source NGC org is required}" "${ARTIFACT_DIR:?artifact directory is required}"
case "${RUNTIME}" in manager|tenant-api|operator-api) ;; *) echo "invalid runtime target" >&2; exit 1 ;; esac
IMAGE="nvcr.io/${NVKE_NGC_ORG}/cluster-service-${RUNTIME}"
mkdir -p "${ARTIFACT_DIR}"

validate_digest() { [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "invalid image digest" >&2; return 1; }; }
write_receipt() {
  jq -n --arg revision "${GITHUB_SHA}" --arg version "${VERSION}" --arg image "$1" --arg digest "$2" \
    '{revision:$revision, version:$version, image:$image, digest:$digest, architectures:["amd64","arm64"]}' > "${ARTIFACT_DIR}/${RUNTIME}.json"
}
# Lookup errors must not be mistaken for permission to overwrite a version.
immutable_tag() {
  local image=$1 digest=$2 tags current
  tags=$(crane ls "${image}")
  if grep -Fxq "${VERSION}" <<< "${tags}"; then
    current=$(crane digest "${image}:${VERSION}")
    [[ "${current}" == "${digest}" ]] || { echo "refusing to replace ${image}:${VERSION}" >&2; return 1; }
  else
    crane tag "${image}@${digest}" "${VERSION}"
  fi
  [[ "$(crane digest "${image}:${VERSION}")" == "${digest}" ]] || { echo "published digest mismatch" >&2; return 1; }
}
case "${operation}" in
  build)
    case "${ARCH:?architecture is required}" in amd64|arm64) ;; *) echo "unsupported architecture" >&2; exit 1 ;; esac
    docker buildx build --platform "linux/${ARCH}" --target "${RUNTIME}" \
      -f management-engine/Dockerfile --build-arg "VERSION=${VERSION}" \
      --label "org.opencontainers.image.revision=${GITHUB_SHA}" \
      --build-arg "SOURCE_DATE_EPOCH=$(git show -s --format=%ct HEAD)" --provenance=false --sbom=false \
      --metadata-file "${ARTIFACT_DIR}/metadata.json" \
      --output "type=image,name=${IMAGE},push-by-digest=true,name-canonical=true,push=true,rewrite-timestamp=true" .
    digest=$(jq -er '."containerimage.digest"' "${ARTIFACT_DIR}/metadata.json")
    validate_digest "${digest}"
    printf '%s\n' "${digest}" > "${ARTIFACT_DIR}/${RUNTIME}-${ARCH}.txt" ;;
  index)
    refs=()
    for arch in amd64 arm64; do
      digest=$(cat "${ARTIFACT_DIR}/${RUNTIME}-${arch}.txt")
      validate_digest "${digest}"
      refs+=("${IMAGE}@${digest}")
    done
    # Run-scoped staging tags are never installation coordinates.
    staging="${IMAGE}:build-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
    docker buildx imagetools create --tag "${staging}" "${refs[@]}"
    digest=$(crane digest "${staging}")
    validate_digest "${digest}"
    crane manifest "${IMAGE}@${digest}" | jq -e '[.manifests[].platform | .os+"/"+.architecture] | sort == ["linux/amd64","linux/arm64"]' >/dev/null
    write_receipt "${IMAGE}" "${digest}"
    echo "image=${IMAGE}" >> "${GITHUB_OUTPUT:?}"
    echo "digest=${digest}" >> "${GITHUB_OUTPUT}"
    echo "version=${VERSION}" >> "${GITHUB_OUTPUT}" ;;
  tag)
    digest=$(jq -er '.digest' "${ARTIFACT_DIR}/${RUNTIME}.json")
    validate_digest "${digest}"
    immutable_tag "${IMAGE}" "${digest}" ;;
  promote)
    : "${DSX_NGC_ORG_TEAM:?destination NGC org/team is required}" "${DSX_NGC_TOKEN:?destination token is required}"
    digest=$(jq -er '.digest' "${ARTIFACT_DIR}/${RUNTIME}.json")
    validate_digest "${digest}"
    destination="nvcr.io/${DSX_NGC_ORG_TEAM}/cluster-service-${RUNTIME}"
    layout="${ARTIFACT_DIR}/${RUNTIME}.oci"
    # Pull every platform before replacing the one credential stored for nvcr.io.
    crane pull --format=oci "${IMAGE}@${digest}" "${layout}"
    # shellcheck disable=SC2016
    printf '%s' "${DSX_NGC_TOKEN}" | crane auth login nvcr.io -u '$oauthtoken' --password-stdin
    tags=$(crane ls "${destination}")
    if ! grep -Fxq "${VERSION}" <<< "${tags}"; then
      crane push "${layout}" "${destination}:build-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
    fi
    immutable_tag "${destination}" "${digest}"
    write_receipt "${destination}" "${digest}" ;;
  *) echo "unknown publisher operation: ${operation}" >&2; exit 1 ;;
esac
