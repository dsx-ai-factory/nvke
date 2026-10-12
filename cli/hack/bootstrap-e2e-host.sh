#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Host preparation for the "nvkectl bootstrap e2e" job
# (.github/workflows/nvkectl-bootstrap-e2e.yml), and for reproducing that host
# locally. cli/e2e/bootstrap/run.sh needs a standalone containerd with nerdctl
# and buildkitd, and must not be able to reach Docker.
#
#   install-runtime   nerdctl-full pinned by NERDCTL_VERSION and the
#                     NERDCTL_FULL_SHA256_<ARCH> for the host's architecture;
#                     a containerd on CONTAINERD_ADDRESS with its own data root;
#                     a buildkitd on BUILDKIT_HOST building into
#                     CONTAINERD_NAMESPACE.
#   remove-docker     mask Docker, its socket and Docker's own containerd, put a
#                     failing `docker` shim first on PATH, and assert both took.
#
# Rootful, not rootless: kind's nerdctl provider and bootstrapengine exec a bare
# `nerdctl`/`kind` with no `sudo` baked in, so every caller runs them under
# `sudo -E`. Rootless would need cgroup delegation, subuid/subgid ranges and
# rootlesskit for no gain on a single-tenant CI runner.
#
# Docker is masked rather than left unused so that a reintroduced dependency
# fails here instead of passing unnoticed: a `docker build` back in a Makefile,
# kind falling back to its Docker default, nerdctl reaching Docker's containerd.
# The binaries are untouched -- masking is what makes an absolute-path
# `/usr/bin/docker build` fail too, and remove-docker proves that against every
# real binary on PATH.
#
# Nothing is undone: the runners are single-use, so the masks, the shim and the
# runtime all die with the job. The shim lives in STATE_DIR, RUNNER_TEMP on a
# runner, which is per-job.
set -Eeuo pipefail

STATE_DIR="${RUNNER_TEMP:-${TMPDIR:-/tmp}/bootstrap-e2e-host}"

# nerdctl's data root, which is where anonymous volumes live -- and the kind
# node image declares VOLUME /var/lib/containerd, so the node's own containerd
# keeps its snapshots there.
#
# Not nerdctl's default /var/lib/nerdctl. On a container runner /var/lib is
# the pod's overlayfs root, and an inner containerd mounting overlay with its
# upperdir on overlayfs is refused by the kernel:
#
#   failed to mount rootfs component: mount source: "overlay" ...
#   err: invalid argument
#
# Every control-plane sandbox then fails to start, so `kubeadm init` waits on
# an API server that never exists and reports only "connection refused" on
# 6443. On a VM runner /var/lib is real disk, which is why the same code
# passes there -- the fleet, not the suite, was the variable.
#
# Under containerd's own root on purpose: that path already has to be
# non-overlay for the controller image build, so this inherits one requirement
# instead of adding a second one somewhere else.
NERDCTL_DATA_ROOT="/var/lib/containerd-nerdctl/nerdctl"

# Where the unmanaged daemons' logs go, root-owned and 0700, rather than
# /tmp/<name>.log. The daemons are started as root, so on a host where another
# local user can pre-create /tmp/containerd-nerdctl.log as a symlink, that
# redirect writes root-owned output wherever the link points. Created by
# start_unmanaged before the first redirect, and read back by the failure
# diagnostics below.
UNMANAGED_LOG_DIR="/var/log/nke-bootstrap-e2e"

log() { printf '[bootstrap-e2e-host] %s\n' "$*"; }

# wait_for_socket waits up to $2 seconds for socket $1. As root: containerd's
# and buildkitd's sockets are root-owned.
wait_for_socket() {
  local path="$1" tries="${2:-30}"
  for _ in $(seq 1 "${tries}"); do
    if sudo test -S "${path}"; then return 0; fi
    sleep 1
  done
  return 1
}

# have_systemd reports whether PID 1 is systemd. False on a container runner,
# where /run/systemd/system is absent and systemctl exits with
#   System has not been booted with systemd as init system (PID 1).
have_systemd() { [ -d /run/systemd/system ]; }

# disposable_host reports whether the caller has declared this host throwaway,
# which is what makes deleting its Docker binaries acceptable rather than
# vandalism. Nothing here can detect that: have_systemd tells us there is no
# init system, not that the host gets discarded, and an ephemeral ARC pod and a
# reusable non-systemd VM are indistinguishable from in here. So the workflow,
# which does know, says so -- and anything that has not said so gets the
# reversible path in remove_docker instead.
disposable_host() {
  case "${NKE_E2E_DISPOSABLE_HOST:-}" in
    1 | true | TRUE | yes) return 0 ;;
    *) return 1 ;;
  esac
}

# start_unmanaged runs a daemon directly, for hosts with no init system to
# hand it to. $1 names it for the log file; the rest is the command.
start_unmanaged() {
  local name="$1"; shift
  log "no systemd: starting ${name} directly"
  # install -d rather than mkdir -p: it sets the mode as it creates, so there
  # is no window where the directory exists world-writable. See
  # UNMANAGED_LOG_DIR for why the logs are not in /tmp.
  sudo install -d -m 700 -o root -g root "${UNMANAGED_LOG_DIR}"
  # argv as positional parameters, not joined into the -c string: "$*" would
  # flatten it and let the root shell re-split on whitespace and act on
  # metacharacters. The values come from the environment -- CONTAINERD_ADDRESS
  # and BUILDKIT_HOST among them -- so what survives the join is the caller's
  # quoting, and that is not something to hand a root shell. CI sets fixed
  # values, so this is about the general case rather than a live hole. The
  # trailing `sh` is $0 for the inner shell; the log path is $1, shifted off.
  sudo sh -c 'logfile="$1"; shift; nohup "$@" >"${logfile}" 2>&1 &' \
    sh "${UNMANAGED_LOG_DIR}/${name}.log" "$@"
}

# install_runtime installs nerdctl-full and starts the standalone containerd
# and buildkitd, pinned by the environment guarded below.
install_runtime() {
  : "${NERDCTL_VERSION:?}" "${CONTAINERD_ADDRESS:?}" "${BUILDKIT_HOST:?}" "${CONTAINERD_NAMESPACE:?}"
  # One checksum per arch. A single pin passes on the wrong-arch archive and
  # defers the failure to the first exec.
  deb_arch="$(dpkg --print-architecture)"
  case "${deb_arch}" in
    amd64) : "${NERDCTL_FULL_SHA256_AMD64:?}"; sha256="${NERDCTL_FULL_SHA256_AMD64}" ;;
    arm64) : "${NERDCTL_FULL_SHA256_ARM64:?}"; sha256="${NERDCTL_FULL_SHA256_ARM64}" ;;
    *) echo "nerdctl-full publishes no release asset for ${deb_arch}" >&2; return 1 ;;
  esac
  archive="nerdctl-full-${NERDCTL_VERSION}-linux-${deb_arch}.tar.gz"
  # A 500 from the releases CDN otherwise ends the lane before the test starts.
  curl -fsSL --retry 8 --retry-all-errors --retry-max-time 240 -o "/tmp/${archive}" \
    "https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/${archive}"
  echo "${sha256}  /tmp/${archive}" | sha256sum -c -
  sudo tar Cxzf /usr/local "/tmp/${archive}"

  # A dedicated unit, config and data root, so this never reuses Docker's
  # `containerd`.
  # /etc/systemd/system included deliberately: the unit files below are
  # written on every host, including the ones with no systemd to read them,
  # where the directory is incidental rather than guaranteed. It happens to
  # exist on both fleets today, so this is about not resting a suite whose
  # point is working without systemd on a directory systemd owns.
  sudo mkdir -p /etc/containerd-nerdctl /var/lib/containerd-nerdctl /run/containerd-nerdctl /run/buildkit-nerdctl \
    /etc/systemd/system
  /usr/local/bin/containerd config default | sudo tee /etc/containerd-nerdctl/config.toml >/dev/null
  # Match on the path only, not the quoting, which differs across containerd
  # versions. The grpc address is the only line carrying this path; the
  # debug, grpc-tcp and metrics addresses are empty.
  sudo sed -i \
    -e "s#^root = .*#root = \"/var/lib/containerd-nerdctl\"#" \
    -e "s#^state = .*#state = \"/run/containerd-nerdctl\"#" \
    -e "s#/run/containerd/containerd.sock#${CONTAINERD_ADDRESS}#" \
    /etc/containerd-nerdctl/config.toml

  # Asserted, not assumed: if this lands on overlayfs the failure surfaces
  # twelve minutes later as an API server that never came up, with nothing
  # pointing here. See NERDCTL_DATA_ROOT above.
  sudo mkdir -p /etc/nerdctl "${NERDCTL_DATA_ROOT}"
  data_root_fstype="$(stat -f -c %T "${NERDCTL_DATA_ROOT}")"
  if [ "${data_root_fstype}" = "overlayfs" ]; then
    echo "::error::${NERDCTL_DATA_ROOT} is on overlayfs; the kind node's containerd cannot mount overlay there"
    exit 1
  fi
  log "nerdctl data root ${NERDCTL_DATA_ROOT} is on ${data_root_fstype}"
  printf 'data_root = "%s"\n' "${NERDCTL_DATA_ROOT}" | sudo tee /etc/nerdctl/nerdctl.toml >/dev/null

  sudo tee /etc/systemd/system/containerd-nerdctl.service >/dev/null <<'UNIT'
[Unit]
Description=standalone containerd for the nerdctl kind provider
After=network.target

[Service]
ExecStart=/usr/local/bin/containerd --config /etc/containerd-nerdctl/config.toml
Restart=on-failure
Delegate=yes
KillMode=process
OOMScoreAdjust=-999
LimitNOFILE=infinity
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity

[Install]
WantedBy=multi-user.target
UNIT

  # `nerdctl build` is a BuildKit client, not a builder, and buildkitd ships
  # in the archive unpacked above. --containerd-worker builds into the same
  # containerd and namespace nerdctl and kind read from; the default OCI
  # worker would build into a store nothing else can see, surfacing much
  # later as an ImagePullBackOff rather than as a build error.
  sudo tee /etc/systemd/system/buildkit-nerdctl.service >/dev/null <<UNIT
[Unit]
Description=buildkitd for nerdctl build, on the standalone containerd
After=containerd-nerdctl.service
Requires=containerd-nerdctl.service

[Service]
ExecStart=/usr/local/bin/buildkitd \\
  --addr unix://${BUILDKIT_HOST#unix://} \\
  --oci-worker=false \\
  --containerd-worker=true \\
  --containerd-worker-addr ${CONTAINERD_ADDRESS} \\
  --containerd-worker-namespace ${CONTAINERD_NAMESPACE}
Restart=on-failure
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT

  # The units above are still written either way: they document the exact
  # flags, and a VM runner uses them. Only the starting differs.
  if have_systemd; then
    sudo systemctl daemon-reload
    sudo systemctl enable --now containerd-nerdctl
  else
    start_unmanaged containerd-nerdctl \
      /usr/local/bin/containerd --config /etc/containerd-nerdctl/config.toml
  fi

  if ! wait_for_socket "${CONTAINERD_ADDRESS}" 30; then
    echo "::error::containerd-nerdctl never opened ${CONTAINERD_ADDRESS}"
    if have_systemd; then
      echo "::group::containerd-nerdctl.service status"
      sudo systemctl status containerd-nerdctl --no-pager -l || true
      echo "::endgroup::"
      echo "::group::containerd-nerdctl journal"
      sudo journalctl -u containerd-nerdctl --no-pager || true
      echo "::endgroup::"
    else
      echo "::group::containerd-nerdctl output"
      sudo cat "${UNMANAGED_LOG_DIR}/containerd-nerdctl.log" || true
      echo "::endgroup::"
    fi
    echo "::group::/etc/containerd-nerdctl/config.toml"
    sudo cat /etc/containerd-nerdctl/config.toml || true
    echo "::endgroup::"
    exit 1
  fi

  sudo -E env "PATH=$PATH" nerdctl info

  if have_systemd; then
    sudo systemctl enable --now buildkit-nerdctl
  else
    start_unmanaged buildkit-nerdctl \
      /usr/local/bin/buildkitd \
      --addr "unix://${BUILDKIT_HOST#unix://}" \
      --oci-worker=false \
      --containerd-worker=true \
      --containerd-worker-addr "${CONTAINERD_ADDRESS}" \
      --containerd-worker-namespace "${CONTAINERD_NAMESPACE}"
  fi
  if ! wait_for_socket "${BUILDKIT_HOST#unix://}" 30; then
    echo "::error::buildkitd never opened ${BUILDKIT_HOST}"
    if have_systemd; then
      echo "::group::buildkit-nerdctl journal"
      sudo journalctl -u buildkit-nerdctl --no-pager || true
      echo "::endgroup::"
    else
      echo "::group::buildkit-nerdctl output"
      sudo cat "${UNMANAGED_LOG_DIR}/buildkit-nerdctl.log" || true
      echo "::endgroup::"
    fi
    exit 1
  fi
}

# remove_docker makes Docker unusable, shims it on PATH, and asserts it. How it
# does the first part depends on the runner: masked units where there is
# systemd, a deleted client where Docker is a dind sidecar on a host the caller
# has declared disposable, and shim-plus-DOCKER_HOST alone anywhere else.
remove_docker() {
  # containerd.service here is Docker's, on /run/containerd/containerd.sock:
  # the socket nerdctl falls back to without CONTAINERD_ADDRESS. `disable`
  # may fail for a unit that is not loaded; `mask` must not, because the
  # assertions below rely on it.
  # On a container runner there are no units to mask: Docker is a dind
  # sidecar reached over the mounted socket. Deleting the socket alone would
  # be weaker than masking -- the kubelet can restart that sidecar, and it
  # recreates the socket on startup. So take the client away instead, and
  # point DOCKER_HOST at nothing in case a daemon reappears anyway.
  #
  # Deleting it is only acceptable on a host that is discarded after the job,
  # so it waits on disposable_host rather than on have_systemd: the two are not
  # the same test, and on a reusable non-systemd host the deletion outlives the
  # run and takes the next one -- and whoever's Docker install -- with it.
  # Without that declaration the shim and DOCKER_HOST below still make docker
  # unusable for this run, reversibly, which is weaker only against a caller
  # that invokes a real binary by absolute path with its own -H.
  docker_deleted=no
  if have_systemd; then
    sudo systemctl disable --now docker.socket docker.service containerd.service 2>/dev/null || true
    sudo systemctl mask docker.socket docker.service containerd.service
  else
    # GITHUB_ENV so every later step of the job inherits it, the same way
    # the shim below uses GITHUB_PATH.
    if [ -n "${GITHUB_ENV:-}" ]; then
      echo "DOCKER_HOST=unix:///nonexistent/docker.sock" >> "${GITHUB_ENV}"
    fi
    export DOCKER_HOST="unix:///nonexistent/docker.sock"
    if disposable_host; then
      log "no systemd: removing the Docker client rather than masking units"
      while read -r real; do
        sudo rm -f "${real}"
      done < <(type -aP docker || true)
      docker_deleted=yes
    else
      log "no systemd, and NKE_E2E_DISPOSABLE_HOST is unset: keeping the Docker binaries."
      log "The shim and DOCKER_HOST still make docker unusable for this run, and the"
      log "assertions below still hold. Set NKE_E2E_DISPOSABLE_HOST=1 only on a runner"
      log "that is discarded afterwards, which is what makes deleting them recoverable."
    fi
  fi
  sudo rm -f /var/run/docker.sock /run/docker.sock

  shim_dir="${STATE_DIR}/no-docker"
  mkdir -p "${shim_dir}"
  cat >"${shim_dir}/docker" <<'SHIM'
#!/bin/sh
echo "docker was invoked by the bootstrap e2e suite: docker $*" >&2
echo "this job removes Docker so a reintroduced dependency fails here rather than passing unnoticed." >&2
echo "The suite runs on nerdctl/containerd. See cli/Makefile's check-no-docker and the 'Remove Docker' step in nvkectl-bootstrap-e2e.yml." >&2
exit 1
SHIM
  chmod 0755 "${shim_dir}/docker"
  # GITHUB_PATH prepends for every later step of the job; off a runner the
  # caller has to do it. Both it and RUNNER_TEMP are per-job.
  if [ -n "${GITHUB_PATH:-}" ]; then
    echo "${shim_dir}" >> "${GITHUB_PATH}"
  else
    log "not on a runner: prepend ${shim_dir} to PATH for the commands that follow"
  fi
  export PATH="${shim_dir}:${PATH}"

  # Prove it: a removal that did not take reads as a guarantee while
  # providing nothing.
  resolved="$(command -v docker || true)"
  if [ "${resolved}" != "${shim_dir}/docker" ]; then
    echo "::error::docker resolves to '${resolved:-nothing}', not the shim"; exit 1
  fi
  if docker version >/dev/null 2>&1; then
    echo "::error::docker still runs after removal"; exit 1
  fi
  # Every real binary too, by its own path: this proves the masking, not the
  # shim.
  while read -r real; do
    if [ "${real}" = "${shim_dir}/docker" ]; then
      continue
    fi
    if "${real}" version >/dev/null 2>&1; then
      echo "::error::${real} still reaches a daemon after removal"; exit 1
    fi
  done < <(type -aP docker || true)
  if have_systemd && systemctl is-active --quiet docker.service; then
    echo "::error::docker.service is still active after removal"; exit 1
  fi
  if [ -S /var/run/docker.sock ]; then
    echo "::error::/var/run/docker.sock still exists after removal"; exit 1
  fi
  # Masked, not merely stopped: docker.socket could otherwise socket-activate
  # the daemon back after the check above.
  if have_systemd; then
    for unit in docker.socket docker.service containerd.service; do
      state="$(systemctl is-enabled "${unit}" 2>/dev/null || true)"
      if [ "${state}" != "masked" ]; then
        echo "::error::${unit} is '${state:-absent}' after removal, want masked"; exit 1
      fi
    done
  else
    # The equivalent without units: no real client left to reach a restarted
    # sidecar with. Asserted rather than assumed, for the same reason the
    # masking is -- a removal that did not take reads as a guarantee while
    # providing nothing.
    #
    # Only when the deletion was the path taken. Where it was declined the
    # binaries are meant to still be there, and what carries the guarantee
    # instead is the loop above, which ran every one of them and required it to
    # fail to reach a daemon.
    if [ "${docker_deleted}" = yes ]; then
      while read -r real; do
        if [ "${real}" != "${shim_dir}/docker" ]; then
          echo "::error::${real} still exists after removal, want it gone"; exit 1
        fi
      done < <(type -aP docker || true)
    fi
    if [ "${DOCKER_HOST:-}" = "" ]; then
      echo "::error::DOCKER_HOST is unset after removal, want it pointing nowhere"; exit 1
    fi
  fi
  echo "docker removed; invoking it now fails with:"
  docker version 2>&1 | sed 's/^/  /' || true
}

usage() {
  echo "usage: $0 install-runtime | remove-docker" >&2
  exit 2
}

case "${1:-}" in
  install-runtime) install_runtime ;;
  remove-docker) remove_docker ;;
  *) usage ;;
esac
