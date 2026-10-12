#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Write an SPDX SBOM for a finished node qcow2.
#
# Usage: extract-image-sbom.sh <disk.qcow2> <out.spdx.json>
#
# The document describes the disk that was just built, not this source tree:
# the package set is read out of the image's own dpkg database. A node runs
# what curtin and provision.sh left on the root filesystem, which is not
# derivable from the Packer templates -- the autoinstall pulls whatever the
# Ubuntu archive is serving on build day, and provision.sh then purges a
# kernel flavor and fifteen firmware packages back out again.
#
# Subject granularity is the OS package, not the file or the Go module. The
# question this answers is "does any shipped image contain <component>", and
# every component that arrives through the archive or through one of our own
# .debs is a dpkg entry. The agent's own module graph is a finer claim and is
# published separately, as the SBOM beside the .deb in the agent release.
#
# libguestfs, not qemu-nbd: it needs no root, no `modprobe nbd` and no
# cryptsetup/lvm on the host, and it walks LVM and LUKS itself. That matters
# for the FDE variant, whose root filesystem is an LVM logical volume inside a
# LUKS container -- the two-tool host-side path has to reimplement that
# stacking by hand, and has to do it as root on a shared runner.

set -euo pipefail

DISK="${1:?usage: extract-image-sbom.sh <disk.qcow2> <out.spdx.json>}"
OUT="${2:?usage: extract-image-sbom.sh <disk.qcow2> <out.spdx.json>}"

# Only the FDE image needs it. Empty is the correct value for the non-FDE
# image, which has no LUKS container to open.
LUKS_PASSPHRASE="${LUKS_PASSPHRASE:-}"

# The appliance runs qemu directly. The libguestfs default backend would look
# for a libvirt session, which no runner here has.
export LIBGUESTFS_BACKEND="${LIBGUESTFS_BACKEND:-direct}"

for tool in guestfish syft tar jq; do
  command -v "${tool}" >/dev/null || {
    echo "::error::${tool} is not on PATH" >&2
    exit 1
  }
done

[[ -f "${DISK}" ]] || { echo "::error::no such disk: ${DISK}" >&2; exit 1; }

# supermin builds its appliance out of the host's own kernel. Two ways that is
# not usable, and both fail with a "supermin exited with error" that names
# neither cause:
#
#   1. On a VM runner the kernel is there but Ubuntu ships it mode 0600.
#   2. On a container runner (the arm64 leg is an ARC pod) there is no kernel
#      package installed at all, because a container does not boot one.
ensure_appliance_kernel() {
  local vmlinuz found=""
  shopt -s nullglob
  for vmlinuz in /boot/vmlinuz-*; do
    [[ -f "${vmlinuz}" ]] || continue
    found="${vmlinuz}"
    if [[ ! -r "${vmlinuz}" ]] && command -v sudo >/dev/null; then
      sudo chmod 0644 "${vmlinuz}"
    fi
  done
  shopt -u nullglob

  if [[ -z "${found}" ]]; then
    echo "No kernel in /boot for the libguestfs appliance; installing one..."
    # -virtual, not -generic: this kernel is never booted, it is only read to
    # build the appliance, so the smaller image is the right one.
    sudo apt-get install -y -qq linux-image-virtual
    ensure_appliance_kernel
  fi
}
ensure_appliance_kernel

# The disk is opened explicitly -- LUKS, then LVM, then the root filesystem --
# rather than with guestfish --inspector, so the same sequence serves both
# variants and every step can be checked.
#
# Two libguestfs behaviours this depends on, both measured rather than assumed:
#
#   * list-filesystems does not report a LUKS partition at all. It is not that
#     the type comes back as something unexpected -- the device is simply
#     absent from the listing, so a container is found with vfs-type on each
#     partition instead.
#   * cryptsetup-open will not take the passphrase as an argument; it prompts,
#     which fails on a runner with "getline: Inappropriate ioctl for device".
#     --keys-from-stdin is what supplies it, and it drops the key parameter
#     from the command's signature.
WORK="$(mktemp -d)"
ROOT="${WORK}/root"
mkdir -p "${ROOT}/var/lib/dpkg" "${ROOT}/etc" "${ROOT}/usr/lib"

# One appliance for the whole sequence: booting it is the expensive part, and
# finding the root filesystem takes several steps that each need a decision
# made in the shell.
if [[ -n "${LUKS_PASSPHRASE}" ]]; then
  # One line is consumed per prompt, so the file holds more than can be asked
  # for. It lives in the mktemp directory the trap below removes, and carries
  # the build-time placeholder passphrase the release notes already publish,
  # not a secret.
  KEYS="${WORK}/keys"
  : > "${KEYS}"
  chmod 0600 "${KEYS}"
  for _ in $(seq 1 16); do printf '%s\n' "${LUKS_PASSPHRASE}" >> "${KEYS}"; done
  eval "$(guestfish --listen --keys-from-stdin < "${KEYS}")"
else
  eval "$(guestfish --listen)"
fi

cleanup() {
  guestfish --remote -- exit >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

gf() { guestfish --remote -- "$@"; }

gf add-drive "${DISK}" readonly:true
gf run

if [[ -n "${LUKS_PASSPHRASE}" ]]; then
  opened=0
  while read -r partition; do
    [[ -n "${partition}" ]] || continue
    [[ "$(gf vfs-type "${partition}" 2>/dev/null)" == "crypto_LUKS" ]] || continue
    gf cryptsetup-open "${partition}" "nvkeluks${opened}"
    echo "Opened the LUKS container on ${partition}"
    opened=$((opened + 1))
  done < <(gf list-partitions)

  if [[ "${opened}" -eq 0 ]]; then
    echo "::error::${DISK} was given a LUKS passphrase but carries no LUKS partition" >&2
    echo "partitions and their types:" >&2
    while read -r partition; do
      echo "  ${partition}: $(gf vfs-type "${partition}" 2>/dev/null)" >&2
    done < <(gf list-partitions)
    exit 1
  fi

  # The FDE layout is LVM-on-LUKS, so the volume group only appears once the
  # container above is open.
  gf vgscan
  gf vg-activate-all true
fi

# The root filesystem is identified by what is on it, not by its device path:
# that path differs between the two variants and between Ubuntu releases, and
# the package database is what is being looked for anyway.
root_device=""
while read -r line; do
  device="${line%%:*}"
  fstype="${line#*: }"
  case "${fstype}" in
    ext2 | ext3 | ext4 | xfs | btrfs) ;;
    *) continue ;;
  esac
  gf umount-all >/dev/null 2>&1 || true
  gf mount-ro "${device}" / >/dev/null 2>&1 || continue
  if [[ "$(gf exists /var/lib/dpkg/status)" == "true" ]]; then
    root_device="${device}"
    break
  fi
done < <(gf list-filesystems)

if [[ -z "${root_device}" ]]; then
  echo "::error::no filesystem in ${DISK} carries /var/lib/dpkg/status" >&2
  echo "filesystems found:" >&2
  gf list-filesystems >&2 || true
  exit 1
fi
echo "Reading the package database from ${root_device} in ${DISK}..."

gf tar-out /var/lib/dpkg "${WORK}/dpkg.tar"
gf download /usr/lib/os-release "${ROOT}/usr/lib/os-release"

tar -xf "${WORK}/dpkg.tar" -C "${ROOT}/var/lib/dpkg"

# syft reads the distro from /etc/os-release, and without it every purl comes
# out as a bare `pkg:deb/<name>` with no `distro=` qualifier -- which is what a
# vulnerability matcher keys on to decide whether an Ubuntu security update
# applies. In the image this is a symlink into /usr/lib; a copy is what lands
# here, because the download above resolved it already.
cp "${ROOT}/usr/lib/os-release" "${ROOT}/etc/os-release"

# An empty database scans clean and produces a valid, useless document. It has
# to be an error: a silent zero-package SBOM is worse than no SBOM, because it
# answers "is <component> in this image" with a confident no.
[[ -s "${ROOT}/var/lib/dpkg/status" ]] || {
  echo "::error::${DISK} yielded no /var/lib/dpkg/status" >&2
  exit 1
}

# --source-name, so the document names the disk it describes rather than the
# throwaway directory it was assembled in.
syft scan "dir:${ROOT}" \
  --output "spdx-json=${OUT}" \
  --source-name "$(basename "${DISK}")"

packages="$(jq '.packages | length' "${OUT}")"
if [[ "${packages}" -lt 2 ]]; then
  echo "::error::${OUT} lists ${packages} packages; the dpkg database did not catalog" >&2
  exit 1
fi
echo "Wrote ${OUT}: ${packages} packages from $(basename "${DISK}")"
