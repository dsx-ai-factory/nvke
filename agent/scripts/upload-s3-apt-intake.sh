#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#
# Upload built .deb files to the apt-publisher S3 intake and write index.json.
#
# Layout produced (consumed by nke/packages/apt-publisher):
#   <intake>/<project>/releases/<tag>/<distro>/<arch>/<filename>
#   <intake>/<project>/releases/<tag>/<distro>/<arch>/<filename>.sha256
#   <intake>/<project>/index.json           (merged + written last)
#
# nke-agent .debs are distro-agnostic (identical content for resolute + noble),
# so each .deb is uploaded under every distro in APT_INTAKE_DISTROS. Package,
# Version, and Architecture are read from the .deb control metadata rather than
# parsed from the filename.
#
# Required env:
#   APT_INTAKE_S3_URI   e.g. s3://example-bucket/nvke-packages
#   APT_INTAKE_PROJECT  e.g. nke-agent
#   APT_INTAKE_RELEASE  e.g. v0.1.26 (agent/v* tag with the component prefix stripped)
# Optional env:
#   APT_INTAKE_DISTROS  space-separated; default "resolute noble"
#   ASSETS_DIR          default "bin" (nke-agent package job writes bin/*.deb)
#
# index.json is accumulating: each run merges the current release artifacts into
# the existing index rather than replacing it. The merge key is
# package|version|architecture|distro, which keeps historical package versions
# discoverable for from-scratch apt-publisher rebuilds.

set -euo pipefail

: "${APT_INTAKE_S3_URI:?APT_INTAKE_S3_URI must be set (e.g. s3://example-bucket/nvke-packages)}"
: "${APT_INTAKE_PROJECT:?APT_INTAKE_PROJECT must be set (e.g. nke-agent)}"
: "${APT_INTAKE_RELEASE:?APT_INTAKE_RELEASE must be set (e.g. v0.1.26)}"
: "${PUBLISH_INDEX_JSON:?PUBLISH_INDEX_JSON must be set to true or false}"

DISTROS_RAW="${APT_INTAKE_DISTROS:-resolute noble}"
read -r -a DISTROS <<<"$DISTROS_RAW"
ASSETS_DIR="${ASSETS_DIR:-bin}"

INTAKE_BASE="${APT_INTAKE_S3_URI%/}/${APT_INTAKE_PROJECT}"
RELEASE_BASE="${INTAKE_BASE}/releases/${APT_INTAKE_RELEASE}"

shopt -s nullglob
debs=("${ASSETS_DIR}"/*.deb)
shopt -u nullglob
if [ "${#debs[@]}" -eq 0 ]; then
  echo "error: no .deb files under ${ASSETS_DIR}/" >&2
  exit 1
fi

now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
tmp_artifacts="$(mktemp)"
tmp_existing="$(mktemp)"
tmp_new_artifacts="$(mktemp)"
index_tmp="$(mktemp)"
trap 'rm -f "$tmp_artifacts" "$tmp_existing" "$tmp_new_artifacts" "$index_tmp"' EXIT

deb_field() {
  dpkg-deb -f "$1" "$2"
}

for deb in "${debs[@]}"; do
  bn="$(basename "$deb")"
  pkg="$(deb_field "$deb" Package)"
  ver="$(deb_field "$deb" Version)"
  arch="$(deb_field "$deb" Architecture)"
  if [ -z "$pkg" ] || [ -z "$ver" ] || [ -z "$arch" ]; then
    echo "error: could not read control fields from ${bn}" >&2
    exit 1
  fi
  sha="$(sha256sum "$deb" | awk '{print $1}')"
  size="$(stat -c '%s' "$deb")"

  for distro in "${DISTROS[@]}"; do
    dest="${RELEASE_BASE}/${distro}/${arch}/${bn}"
    key="${dest#s3://*/}"
    echo "intake upload: ${distro}/${arch}/${bn}"
    aws s3 cp "$deb" "$dest" --only-show-errors
    printf '%s' "$sha" | aws s3 cp - "${dest}.sha256" --only-show-errors

    jq -n \
      --arg package "$pkg" \
      --arg version "$ver" \
      --arg architecture "$arch" \
      --arg distro "$distro" \
      --arg filename "$bn" \
      --arg s3Key "$key" \
      --arg sha256 "$sha" \
      --argjson size "$size" \
      --arg release "$APT_INTAKE_RELEASE" \
      --arg project "$APT_INTAKE_PROJECT" \
      --arg dateAdded "$now" \
      '{package:$package, version:$version, architecture:$architecture,
              distro:$distro, filename:$filename, s3Key:$s3Key, sha256:$sha256,
              size:$size, release:$release, project:$project, dateAdded:$dateAdded}' \
      >>"$tmp_artifacts"
  done
done

if [[ "${PUBLISH_INDEX_JSON}" != true ]]; then
  echo "S3 upload complete: ${RELEASE_BASE}"
  echo "Skipping index.json update because PUBLISH_INDEX_JSON is not true."
  exit 0
fi

jq -s '.' "$tmp_artifacts" >"$tmp_new_artifacts"

if ! aws s3 cp "${INTAKE_BASE}/index.json" "$tmp_existing" --only-show-errors; then
  echo "No existing intake index found at ${INTAKE_BASE}/index.json; creating a new one."
  jq -n \
    --arg project "$APT_INTAKE_PROJECT" \
    '{schemaVersion:1, project:$project, releases:[], artifacts:[]}' \
    >"$tmp_existing"
fi

jq \
  --slurpfile new "$tmp_new_artifacts" \
  --arg project "$APT_INTAKE_PROJECT" \
  --arg release "$APT_INTAKE_RELEASE" \
  --arg updatedAt "$now" \
  '
    def key($a): [$a.package, $a.version, $a.architecture, $a.distro] | join("|");

    . as $existing
    | {
        schemaVersion: 1,
        project: $project,
        updatedAt: $updatedAt,
        latestRelease: $release,
        releases: ((($existing.releases // []) + [$release]) | unique | sort),
        artifacts: (
          (($existing.artifacts // []) + $new[0])
        | reduce .[] as $artifact ({};
            .[key($artifact)] = (
              if has(key($artifact)) then
                $artifact + {dateAdded: (.[key($artifact)].dateAdded // $artifact.dateAdded)}
              else
                $artifact
              end
            )
          )
        | to_entries
        | map(.value)
        | sort_by(.distro, .architecture, .package, .version)
        )
      }
    ' "$tmp_existing" >"$index_tmp"

echo "Writing index.json with $(jq '.artifacts | length' "$index_tmp") artifact entr(ies)"
aws s3 cp "$index_tmp" "${INTAKE_BASE}/index.json" \
  --content-type "application/json" --only-show-errors

echo "S3 apt intake upload complete: ${INTAKE_BASE}"
