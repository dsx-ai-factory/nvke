#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# Included by the flake's writeShellApplication; arch, launcher and guest_script
# are supplied by Nix, along with every host command used below.
set -euo pipefail
source_dir=$PWD
artifacts=
while (($#)); do
  case "$1" in
    --source) source_dir=${2:?--source requires a path}; shift 2 ;;
    --output) artifacts=${2:?--output requires a path}; shift 2 ;;
    --help|-h) echo 'Usage: build-packages --output PATH [--source CHECKOUT]'; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
: "${artifacts:?--output is required}" "${arch:?}" "${launcher:?}" "${guest_script:?}" "${default_ssh_port:?}"
repo=$(git -C "$source_dir" rev-parse --show-toplevel)
mkdir -p "$artifacts"
artifacts=$(cd "$artifacts" && pwd -P)
state=${NKE_BUILDER_STATE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/nke/builders/$arch}
umask 077
mkdir -p "$state/runtime"
chmod 700 "$state"
state=$(cd "$state" && pwd -P)
exec 9>"$state/build.lock"
flock -n 9 || { echo "Builder state is already in use: $state" >&2; exit 1; }
key=$state/client-key
[[ -e "$key" ]] || ssh-keygen -q -t ed25519 -N '' -f "$key"
[[ -s "$key.pub" ]] || ssh-keygen -y -f "$key" > "$key.pub"
export NKE_BUILDER_STATE_DIR=$state NKE_BUILDER_SSH_KEY=$key.pub
export NKE_BUILDER_HOST_KEY=$state/host-key
[[ -e "$NKE_BUILDER_HOST_KEY" ]] || ssh-keygen -q -t ed25519 -N '' -f "$NKE_BUILDER_HOST_KEY"
host_public_key=$(ssh-keygen -y -f "$NKE_BUILDER_HOST_KEY")
printf 'nke-package-builder %s\n' "$host_public_key" > "$state/known_hosts"
export NIX_DISK_IMAGE=$state/root.qcow2 TMPDIR=$state/runtime USE_TMPDIR=1
export NKE_BUILDER_SSH_PORT=${NKE_BUILDER_SSH_PORT:-$default_ssh_port}
unset ACCELERATOR  # the Packer Makefile exports its own; the launcher picks one for this VM
run_id=$(date +%s)-$$
rm -f "$state"/console-*.log
log=$state/console-$run_id.log
ready=0
vm_pid=
cleanup() {
  status=$?
  trap '' INT TERM  # a second signal must not orphan the VM mid-cleanup
  # A signal can arrive after launch but before vm_pid=$! records the only job.
  vm_pid=${vm_pid:-$(jobs -pr)}
  if [[ -n "$vm_pid" ]]; then
    if [[ "$ready" == 1 ]]; then
      timeout 15 nke-builder-ssh root@127.0.0.1 poweroff >/dev/null 2>&1 || true
      for ((i=0; i<60; i++)); do kill -0 "$vm_pid" 2>/dev/null || break; sleep 1; done
    fi
    kill -TERM -- "-$vm_pid" 2>/dev/null || true
    for ((i=0; i<10; i++)); do kill -0 "$vm_pid" 2>/dev/null || break; sleep 1; done
    kill -KILL -- "-$vm_pid" 2>/dev/null || true
    wait "$vm_pid" 2>/dev/null || true
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# The host key is provisioned independently of the guest disk, before connecting.
# Job control gives the launcher and its QEMU child their own process group.
set -m
# QEMU inherits fd 9, so the lock is held until the VM exits, even if this driver is killed.
"$launcher" </dev/null >"$log" 2>&1 &
vm_pid=$!
echo "Builder console: $log"
deadline=$((SECONDS + ${NKE_BUILDER_BOOT_TIMEOUT:-900}))
until nke-builder-ssh root@127.0.0.1 true >/dev/null 2>&1; do
  if ! kill -0 "$vm_pid" 2>/dev/null || ((SECONDS >= deadline)); then
    echo "Builder failed to become ready; see $log" >&2
    exit 1
  fi
  sleep 2
done
ready=1
work=/root/nke-builds/$run_id
# Previous runs would otherwise fill the disk; the caches under /root/.cache stay.
nke-builder-ssh root@127.0.0.1 "rm -rf /root/nke-builds && mkdir -p $work/src $work/artifacts"
# Skip deleted tracked paths and ignored host outputs; preserve unusual filenames.
while IFS= read -r -d '' file; do
  if [[ -e "$repo/$file" || -L "$repo/$file" ]]; then printf '%s\0' "$file"; fi
done < <(git -C "$repo" ls-files -z --cached --others --exclude-standard) |
  rsync -a --from0 --files-from=- -e nke-builder-ssh "$repo/" "root@127.0.0.1:$work/src/"
version=${VERSION:-$(git -C "$repo" describe --match 'agent/v*' --tags --always --dirty)}
# Send shell-quoted assignments over SSH stdin, including optional credentials.
{
  printf 'export ARCH=%q ARTIFACTS_DIR=%q REPO_ROOT=%q VERSION=%q\n' \
    "$arch" "$work/artifacts" "$work/src" "${version#agent/}"
  for name in K8S_VERSIONS CALICO_VERSIONS OVN_STREAMS NGC_API_KEY SPIRE_VERSION TPM_PLUGIN_VERSION \
    NKE_TARGET_UBUNTU_SERIES DOCA_VERSION; do
    if [[ -n "${!name+x}" ]]; then printf 'export %s=%q\n' "$name" "${!name}"; fi
  done
  cat "$guest_script"
} | nke-builder-ssh root@127.0.0.1 'bash -s'
# debs/ is the builder's own tree, so stale debs are deleted there; the output
# directory itself belongs to the caller and keeps whatever else is in it.
rsync -a --exclude=/debs -e nke-builder-ssh \
  "root@127.0.0.1:$work/artifacts/" "$artifacts/"
rsync -a --delete -e nke-builder-ssh \
  "root@127.0.0.1:$work/artifacts/debs/" "$artifacts/debs/"
for expected in nvke-agent.deb debs/Packages; do
  [[ -e "$artifacts/$expected" ]] ||
    { echo "Builder produced no $expected; see $log" >&2; exit 1; }
done
echo "Packages returned to $artifacts"
