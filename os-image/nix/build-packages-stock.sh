#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# The macOS package build, in nixpkgs' stock Linux builder (darwin.linux-builder).
# Included by the flake's writeShellApplication, which prepends the variables
# checked below and puts every host command used here on PATH.
#
# The guest has no SSH session and no forwarded port. Its serial console, which
# is QEMU's stdin, logs in the unprivileged `builder` user automatically, and
# this types one line there. Files move through the VM's /tmp/xchg share
# ($xchg here): the source, an env file and stock-guest.sh go in; a log, the
# artifacts and a status come back. stock-guest.sh powers the VM off when done.
set -euo pipefail
: "${arch:?}" "${vm_drv:?}" "${vm_program:?}" "${guest_script:?}"
source_dir=$PWD
artifacts=
check=0
while (($#)); do
  case "$1" in
    --source) source_dir=${2:?--source requires a path}; shift 2 ;;
    --output) artifacts=${2:?--output requires a path}; shift 2 ;;
    --check) check=1; shift ;;
    --help|-h) echo 'Usage: build-packages [--check] --output PATH [--source CHECKOUT]'; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
if [[ "$check" == 0 ]]; then : "${artifacts:?--output is required}"; fi

umask 077
state=${NKE_BUILDER_STATE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/nke/builders/$arch}/stock
mkdir -p "$state/keys"
chmod 700 "$state"
state=$(cd "$state" && pwd -P)
exec 8>"$state/build.lock"
flock -n 8 || { echo "Builder state is already in use: $state" >&2; exit 1; }

# The guest is nixpkgs' own, changed only on the host side, so it is fetched
# from the binary cache; a Mac has no Linux builder to build it.
vm=$(nix build --no-link --print-out-paths "$vm_drv^out") || {
  echo "nixpkgs' Linux builder guest is not in the binary cache for this nixpkgs pin, and a Mac cannot build it. See os-image/nix/README.md." >&2
  exit 1
}
if [[ "$check" == 1 ]]; then exit 0; fi

repo=$(git -C "$source_dir" rev-parse --show-toplevel)
mkdir -p "$artifacts"
artifacts=$(cd "$artifacts" && pwd -P)
# The runtime directory is deleted at exit, and the rest belongs to the VM.
case "$artifacts/" in
  "$state/"*) echo "--output must be outside the builder state directory, $state" >&2; exit 2 ;;
esac
version=${VERSION:-$(git -C "$repo" describe --tags --always --dirty --match 'v*')}
disk=$state/root.qcow2
runtime=$state/runtime
xchg=$runtime/xchg
console=$state/console.log
build_log=$state/build.log
# The VM must not listen on the host at all.
unset QEMU_NET_OPTS
vm_pid=
tail_pid=
ready=0

stop_vm() {
  if [[ -n "$tail_pid" ]]; then
    kill "$tail_pid" 2>/dev/null || true
    wait "$tail_pid" 2>/dev/null || true
    tail_pid=
  fi
  # A signal can arrive after launch but before vm_pid=$! records the only job.
  vm_pid=${vm_pid:-$(jobs -pr)}
  [[ -n "$vm_pid" ]] || return 0
  if [[ "$ready" == 1 ]] && kill -0 "$vm_pid" 2>/dev/null; then
    # The build runs in the background, so the console is free to power off.
    printf 'systemctl poweroff\n' >&3 || true
    for ((i = 0; i < 60; i++)); do kill -0 "$vm_pid" 2>/dev/null || break; sleep 1; done
  fi
  kill -TERM -- "-$vm_pid" 2>/dev/null || true
  for ((i = 0; i < 10; i++)); do kill -0 "$vm_pid" 2>/dev/null || break; sleep 1; done
  kill -KILL -- "-$vm_pid" 2>/dev/null || true
  wait "$vm_pid" 2>/dev/null || true
  vm_pid=
  ready=0
}
cleanup() {
  status=$?
  trap - EXIT
  trap '' INT TERM  # a second signal must not orphan the VM mid-cleanup
  stop_vm
  # The runtime directory holds the source and the env file, credentials included.
  rm -rf -- "$runtime"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# stage <id> writes this run's inputs to a fresh share.
stage() {
  rm -rf -- "$runtime"
  mkdir -p "$xchg"
  # The guest's user is not the host's, and the guest checks share permissions
  # itself, so the share is open to others; the 0700 state directory keeps
  # other host users out.
  chmod 0777 "$xchg"
  # Tracked and untracked files, but not ignored ones or deleted tracked paths.
  # The list is written first, so a git failure stops the run.
  git -C "$repo" ls-files -z --cached --others --exclude-standard >"$runtime/files"
  while IFS= read -r -d '' file; do
    if [[ -e "$repo/$file" || -L "$repo/$file" ]]; then printf '%s\0' "$file"; fi
  done <"$runtime/files" |
    # Git lists an untracked repository, such as a worktree kept in the checkout,
    # as one directory; --no-recursion keeps its files, ignored ones included, out.
    # GNU tar is on PATH; COPYFILE_DISABLE keeps macOS's tar from adding ._ files.
    COPYFILE_DISABLE=1 tar -C "$repo" --null --no-recursion -T - -cf "$xchg/src-$1.tar"
  # Credentials such as NGC_API_KEY travel only in this file, which the guest
  # deletes once read; nothing secret is typed on the console, which is logged.
  {
    printf 'export ARCH=%q VERSION=%q\n' "$arch" "$version"
    for name in K8S_VERSIONS CALICO_VERSIONS OVN_STREAMS NGC_API_KEY SPIRE_VERSION TPM_PLUGIN_VERSION \
      NKE_TARGET_UBUNTU_SERIES DOCA_VERSION; do
      if [[ -n "${!name+x}" ]]; then printf 'export %s=%q\n' "$name" "${!name}"; fi
    done
  } >"$xchg/env-$1.sh"
  cp "$guest_script" "$xchg/run-$1.sh"
  chmod 0644 "$xchg/src-$1.tar" "$xchg/env-$1.sh" "$xchg/run-$1.sh"
}

# boot starts the VM and waits for the console login, setting ready=1 once it
# is there. It returns 0 either way, so set -e stays in force inside it.
boot() {
  rm -f -- "$state/console.in"
  mkfifo -m 0600 "$state/console.in"
  # Emptied here, not by QEMU's redirection below, so the wait for the login
  # can never read the previous run's log.
  : >"$console"
  # Held open here, so QEMU never reads end-of-file on its console.
  exec 3<>"$state/console.in"
  # Job control gives the launcher and its QEMU child their own process group.
  set -m
  # QEMU inherits fd 8, so the lock is held until the VM exits, even if this driver is killed.
  # The launcher copies the host's CA bundle into a share the guest reads as
  # another user, so it runs with umask 022; the 0700 state directory keeps
  # other host users out.
  (
    umask 022
    KEYS="$state/keys" NIX_DISK_IMAGE="$disk" TMPDIR="$runtime" USE_TMPDIR=1 \
      exec "$vm/bin/$vm_program"
  ) <"$state/console.in" >"$console" 2>&1 &
  vm_pid=$!
  # Only QEMU needs its own process group. With job control on, every later
  # command would get one too, and a Ctrl-C typed on the terminal would stop
  # only that command, not this script.
  set +m
  local deadline=$((SECONDS + ${NKE_BUILDER_BOOT_TIMEOUT:-900}))
  until grep -q "automatic login" "$console" 2>/dev/null; do
    if ! kill -0 "$vm_pid" 2>/dev/null || ((SECONDS >= deadline)); then return 0; fi
    sleep 1
  done
  ready=1
}

# run_build <attempt> returns once the artifacts are in place, or with
# retry_reason set when the VM did not boot or the guest's Nix database is
# corrupt; it exits on any other failure. Its callers run it as a plain
# command, so set -e stays in force inside it.
run_build() {
  local id=$(($(date +%s)))-$$-$1 status
  retry_reason=
  # An earlier run's log must not be blamed for this one.
  rm -f -- "$build_log"
  stage "$id"
  boot
  if [[ "$ready" != 1 ]]; then
    stop_vm
    retry_reason="The builder VM did not reach its console login; see $console"
    return 0
  fi
  echo "Builder console: $console"
  printf 'bash /tmp/xchg/run-%s.sh %s &\n' "$id" "$id" >&3
  local deadline=$((SECONDS + ${NKE_BUILDER_START_TIMEOUT:-120}))
  until [[ -e "$xchg/started-$id" ]]; do
    if ! kill -0 "$vm_pid" 2>/dev/null || ((SECONDS >= deadline)); then
      echo "The guest did not start the build; see $console" >&2
      exit 1
    fi
    sleep 1
  done
  # Stream the guest's log; without the lock, so a killed driver cannot leave it held.
  tail -n +1 -F "$xchg/log-$id" 2>/dev/null 8>&- 3>&- &
  tail_pid=$!
  wait "$vm_pid" || true
  vm_pid=
  ready=0
  sleep 1  # let tail print the last lines
  stop_vm
  if [[ -e "$xchg/log-$id" ]]; then cp "$xchg/log-$id" "$build_log"; fi
  if [[ ! -e "$xchg/status-$id" ]]; then
    echo "The guest powered off with no status; see $build_log and $console" >&2
    exit 1
  fi
  status=$(<"$xchg/status-$id")
  if [[ "$status" != 0 ]]; then
    if grep -q 'database disk image is malformed' "$build_log" 2>/dev/null; then
      retry_reason="The guest's Nix database is corrupt; see $build_log"
      return 0
    fi
    echo "The package build failed in the guest (status $status); see $build_log" >&2
    exit 1
  fi
  if [[ ! -s "$xchg/out-$id.tar" ]]; then
    echo "The guest returned no artifacts; see $build_log" >&2
    exit 1
  fi
  local out=$runtime/out
  mkdir -p "$out"
  tar -xpf "$xchg/out-$id.tar" -C "$out"
  for expected in nvke-agent.deb debs/Packages; do
    [[ -e "$out/$expected" ]] || { echo "The guest returned no $expected; see $build_log" >&2; exit 1; }
  done
  # debs/ is the builder's own tree, so stale debs are deleted there; the output
  # directory itself belongs to the caller and keeps whatever else is in it.
  rsync -a --exclude=/debs "$out/" "$artifacts/"
  rsync -a --delete "$out/debs/" "$artifacts/debs/"
}

attempt=1
while :; do
  had_disk=0
  if [[ -e "$disk" ]]; then had_disk=1; fi
  run_build "$attempt"
  if [[ -z "$retry_reason" ]]; then break; fi
  # A persistent disk left half-written by a hard stop can fail to boot or hold
  # a corrupt store; a fresh disk cannot, so only a reused disk is worth one retry.
  if [[ "$attempt" == 1 && "$had_disk" == 1 ]]; then
    echo "$retry_reason; recreating the builder disk and retrying once" >&2
    rm -f -- "$disk"
    attempt=2
    continue
  fi
  echo "$retry_reason" >&2
  exit 1
done
echo "Packages returned to $artifacts"
