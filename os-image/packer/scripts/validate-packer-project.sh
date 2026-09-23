#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
packer_bin="${PACKER:-packer}"
work="$(mktemp -d)"
trap 'chmod -R u+w "${work}" 2>/dev/null || true; rm -rf -- "${work}"' EXIT

export PACKER_PLUGIN_PATH="${work}/plugins"
"${packer_bin}" init "${project_root}"

ssh-keygen -q -t ed25519 -N '' -f "${work}/id_ed25519"

# Validation does not boot a VM; provide a complete fixture firmware pair.
export PKR_VAR_efi_firmware_code="${work}/code.fd"
export PKR_VAR_efi_firmware_vars="${work}/vars.fd"
touch "$PKR_VAR_efi_firmware_code" "$PKR_VAR_efi_firmware_vars"

for architecture in amd64 arm64; do
  fixture="${work}/${architecture}"
  mkdir -p "${fixture}/materials" "${fixture}/payload" "${fixture}/output" "${fixture}/logs"
  printf 'installer fixture for %s\n' "${architecture}" >"${fixture}/materials/installer-iso"
  printf 'image input fixture for %s\n' "${architecture}" >"${fixture}/payload/input"
  tar -C "${fixture}/payload" -cf "${fixture}/materials/image-inputs" .

  iso_digest="$(sha256sum "${fixture}/materials/installer-iso" | cut -d' ' -f1)"
  inputs_digest="$(sha256sum "${fixture}/materials/image-inputs" | cut -d' ' -f1)"
  printf '%s\n' \
    "{\"schemaVersion\":\"nke.dev/os-image-materials/v1\",\"materials\":[{\"binding\":\"installer-iso\",\"component\":\"validation-installer\",\"kind\":\"disk-image\",\"architecture\":\"${architecture}\",\"digest\":\"sha256:${iso_digest}\",\"path\":\"materials/installer-iso\"},{\"binding\":\"image-inputs\",\"component\":\"validation-inputs\",\"kind\":\"archive\",\"architecture\":\"${architecture}\",\"digest\":\"sha256:${inputs_digest}\",\"path\":\"materials/image-inputs\"}]}" \
    >"${fixture}/material-manifest.json"

  for selector in \
    nke-node-fde.qemu.nke-fde \
    nke-node-non-fde.qemu.nke-non-fde; do
    "${packer_bin}" validate \
      -only="${selector}" \
      -var "architecture=${architecture}" \
      -var "material_manifest=${fixture}/material-manifest.json" \
      -var "output_directory=${fixture}/output" \
      -var "build_log_directory=${fixture}/logs" \
      -var "ssh_private_key_file=${work}/id_ed25519" \
      -var "ssh_public_key_file=${work}/id_ed25519.pub" \
      "${project_root}"
  done
done

if output="$("${packer_bin}" validate \
  -only=nke-node-non-fde.qemu.nke-non-fde \
  -var "architecture=arm64" \
  -var "material_manifest=${work}/amd64/material-manifest.json" \
  -var "output_directory=${work}/amd64/output" \
  -var "build_log_directory=${work}/amd64/logs" \
  -var "ssh_private_key_file=${work}/id_ed25519" \
  -var "ssh_public_key_file=${work}/id_ed25519.pub" \
  "${project_root}" 2>&1)"; then
  echo "Packer accepted a requested architecture that does not match its materials" >&2
  exit 1
fi
grep -F "Invalid index" <<<"${output}" >/dev/null || {
  printf '%s\n' "${output}" >&2
  echo "Packer rejected the requested architecture without the material-binding guard" >&2
  exit 1
}

# Direct runners supply an authenticated plugin closure and must be able to
# validate the project without packer init or any write beneath the source.
readonly_source="${work}/read-only-source"
mkdir -p "${readonly_source}"
tar -C "$(dirname "${project_root}")" \
  --exclude='packer/.packer-inputs' \
  --exclude='packer/.packer-output*' \
  --exclude='packer/.packer_cache' \
  --exclude='packer/.ssh' \
  --exclude='packer/artifacts' \
  --exclude='packer/bin' \
  --exclude='packer/build-logs' \
  --exclude='packer/iso-cache' \
  --exclude='packer/output-*' \
  -cf - packer files | tar -C "${readonly_source}" -xf -
readonly_project="${readonly_source}/packer"
chmod -R a-w "${readonly_source}"
chmod -R a-w "${PACKER_PLUGIN_PATH}"
"${packer_bin}" validate \
  -only=nke-node-non-fde.qemu.nke-non-fde \
  -var "architecture=amd64" \
  -var "material_manifest=${work}/amd64/material-manifest.json" \
  -var "output_directory=${work}/amd64/output" \
  -var "build_log_directory=${work}/amd64/logs" \
  -var "ssh_private_key_file=${work}/id_ed25519" \
  -var "ssh_public_key_file=${work}/id_ed25519.pub" \
  "${readonly_project}"

invalid_manifest="${work}/amd64/material-manifest-invalid.json"
sed 's#nke.dev/os-image-materials/v1#nke.dev/os-image-materials/invalid#' \
  "${work}/amd64/material-manifest.json" >"${invalid_manifest}"
if output="$("${packer_bin}" validate \
  -only=nke-node-non-fde.qemu.nke-non-fde \
  -var "architecture=amd64" \
  -var "material_manifest=${invalid_manifest}" \
  -var "output_directory=${work}/amd64/output" \
  -var "build_log_directory=${work}/amd64/logs" \
  -var "ssh_private_key_file=${work}/id_ed25519" \
  -var "ssh_public_key_file=${work}/id_ed25519.pub" \
  "${project_root}" 2>&1)"; then
  echo "Packer accepted an unsupported material-manifest schema" >&2
  exit 1
fi
grep -F "must use schemaVersion nke.dev/os-image-materials/v1" <<<"${output}" >/dev/null || {
  printf '%s\n' "${output}" >&2
  echo "Packer rejected the schema without the contract error" >&2
  exit 1
}

missing_digest_manifest="${work}/amd64/material-manifest-missing-digest.json"
"${PYTHON:-python3}" - "${work}/amd64/material-manifest.json" "${missing_digest_manifest}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    manifest = json.load(source)
del manifest["materials"][0]["digest"]
with open(sys.argv[2], "w", encoding="utf-8") as destination:
    json.dump(manifest, destination)
PY
if output="$("${packer_bin}" validate \
  -only=nke-node-non-fde.qemu.nke-non-fde \
  -var "architecture=amd64" \
  -var "material_manifest=${missing_digest_manifest}" \
  -var "output_directory=${work}/amd64/output" \
  -var "build_log_directory=${work}/amd64/logs" \
  -var "ssh_private_key_file=${work}/id_ed25519" \
  -var "ssh_public_key_file=${work}/id_ed25519.pub" \
  "${project_root}" 2>&1)"; then
  echo "Packer accepted a material record without a digest" >&2
  exit 1
fi
grep -F "lowercase sha256 digests" <<<"${output}" >/dev/null || {
  printf '%s\n' "${output}" >&2
  echo "Packer rejected a missing digest without the contract error" >&2
  exit 1
}

if output="$("${packer_bin}" validate \
  -only=nke-node-non-fde.qemu.nke-non-fde \
  -var "architecture=amd64" \
  -var "material_manifest=${work}/amd64/material-manifest.json" \
  -var "output_directory=${work}/amd64/output/../escaped" \
  -var "build_log_directory=${work}/amd64/logs" \
  -var "ssh_private_key_file=${work}/id_ed25519" \
  -var "ssh_public_key_file=${work}/id_ed25519.pub" \
  "${project_root}" 2>&1)"; then
  echo "Packer accepted an output directory containing a dot segment" >&2
  exit 1
fi
grep -F "clean absolute path without dot segments" <<<"${output}" >/dev/null || {
  printf '%s\n' "${output}" >&2
  echo "Packer rejected an unsafe output directory without the contract error" >&2
  exit 1
}

mismatched_manifest="${work}/amd64/material-manifest-mismatched-architecture.json"
"${PYTHON:-python3}" - "${work}/amd64/material-manifest.json" "${mismatched_manifest}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    manifest = json.load(source)
manifest["materials"][1]["architecture"] = "arm64"
with open(sys.argv[2], "w", encoding="utf-8") as destination:
    json.dump(manifest, destination)
PY
if output="$("${packer_bin}" validate \
  -only=nke-node-non-fde.qemu.nke-non-fde \
  -var "architecture=amd64" \
  -var "material_manifest=${mismatched_manifest}" \
  -var "output_directory=${work}/amd64/output" \
  -var "build_log_directory=${work}/amd64/logs" \
  -var "ssh_private_key_file=${work}/id_ed25519" \
  -var "ssh_public_key_file=${work}/id_ed25519.pub" \
  "${project_root}" 2>&1)"; then
  echo "Packer accepted mismatched material architectures" >&2
  exit 1
fi
grep -F "must contain one installer-iso" <<<"${output}" >/dev/null || {
  printf '%s\n' "${output}" >&2
  echo "Packer rejected mismatched architectures without the contract error" >&2
  exit 1
}

duplicate_manifest="${work}/amd64/material-manifest-duplicate-binding.json"
"${PYTHON:-python3}" - "${work}/amd64/material-manifest.json" "${duplicate_manifest}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    manifest = json.load(source)
duplicate = dict(manifest["materials"][0])
duplicate["kind"] = "archive"
duplicate["architecture"] = "arm64"
manifest["materials"].insert(0, duplicate)
with open(sys.argv[2], "w", encoding="utf-8") as destination:
    json.dump(manifest, destination)
PY
if output="$("${packer_bin}" validate \
  -only=nke-node-non-fde.qemu.nke-non-fde \
  -var "architecture=amd64" \
  -var "material_manifest=${duplicate_manifest}" \
  -var "output_directory=${work}/amd64/output" \
  -var "build_log_directory=${work}/amd64/logs" \
  -var "ssh_private_key_file=${work}/id_ed25519" \
  -var "ssh_public_key_file=${work}/id_ed25519.pub" \
  "${project_root}" 2>&1)"; then
  echo "Packer accepted duplicate installer material bindings" >&2
  exit 1
fi
grep -F "must contain one installer-iso" <<<"${output}" >/dev/null || {
  printf '%s\n' "${output}" >&2
  echo "Packer rejected duplicate bindings without the contract error" >&2
  exit 1
}
