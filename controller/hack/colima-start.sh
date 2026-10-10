#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

COLIMA_PROFILE="${COLIMA_PROFILE:-default}"
COLIMA_CPUS="${COLIMA_CPUS:-6}"
COLIMA_MEMORY_GIB="${COLIMA_MEMORY_GIB:-12}"
COLIMA_DISK_GIB="${COLIMA_DISK_GIB:-100}"
COLIMA_RUNTIME="${COLIMA_RUNTIME:-docker}"
COLIMA_INOTIFY_MAX_USER_INSTANCES="${COLIMA_INOTIFY_MAX_USER_INSTANCES:-1024}"
COLIMA_INOTIFY_MAX_USER_WATCHES="${COLIMA_INOTIFY_MAX_USER_WATCHES:-1048576}"
COLIMA_FILE_MAX="${COLIMA_FILE_MAX:-2097152}"
COLIMA_NOFILE_LIMIT="${COLIMA_NOFILE_LIMIT:-1048576}"

# This script has two responsibilities for the local Kind/CAPD flow:
#   1. prepare the Colima VM for Docker- and Kind-heavy development
#   2. prepare the Colima VM kernel for the local TPM-backed Forge flow
#
# The second responsibility is the new TPM-specific piece. The node image later
# starts `swtpm chardev --vtpm-proxy` inside each Docker node container. That
# mode does not create a standalone `/dev/tpm0` on its own; it asks the Colima
# VM kernel to allocate a vTPM-backed TPM device via `/dev/vtpmx`. Ensuring the
# `tpm_vtpm_proxy` module is installed and loaded here keeps that dependency
# explicit and fails fast before Kind or Tilt start.

docker_host_for_profile() {
    echo "unix://${HOME}/.colima/${COLIMA_PROFILE}/docker.sock"
}

fail() {
    echo "colima-start: $*" >&2
    exit 1
}

ensure_command() {
    local cmd="$1"
    command -v "${cmd}" >/dev/null 2>&1 || fail "'${cmd}' is required but not installed"
}

setup_failed() {
    local step="$1"
    echo "" >&2
    echo "colima-start: failed while ${step}." >&2
    echo "colima-start: the VM is NOT ready for controller local development." >&2
    echo "" >&2
    echo "If Docker did not come up:" >&2
    echo "  colima ssh --profile ${COLIMA_PROFILE} -- sudo systemctl reset-failed docker.service && colima ssh --profile ${COLIMA_PROFILE} -- sudo systemctl start docker.service" >&2
    echo "  colima ssh --profile ${COLIMA_PROFILE} -- sudo journalctl -u docker.service -n 120 --no-pager" >&2
    echo "If /dev/vtpmx is missing, the vTPM proxy module is not loaded:" >&2
    echo "  colima ssh --profile ${COLIMA_PROFILE} -- modinfo tpm_vtpm_proxy && colima ssh --profile ${COLIMA_PROFILE} -- ls -l /dev/vtpmx" >&2
}

# Check steps individually: bash ignores `set -e` where a command's status is
# tested, so `if ! ( set -euo pipefail; ... )` silently swallowed every failure.
run_step() {
    local description="$1"
    shift
    if ! "$@"; then
        setup_failed "${description}"
        return 1
    fi
}

configure_colima_vm() {
    colima ssh --profile "${COLIMA_PROFILE}" -- sudo sh -c "
set -eu

cat >/etc/sysctl.d/99-capi.conf <<EOF
fs.inotify.max_user_instances=${COLIMA_INOTIFY_MAX_USER_INSTANCES}
fs.inotify.max_user_watches=${COLIMA_INOTIFY_MAX_USER_WATCHES}
fs.file-max=${COLIMA_FILE_MAX}
EOF

sysctl --system >/dev/null

tmpfile=\$(mktemp)
cat >\"\${tmpfile}\" <<EOF
{
  \"default-ulimits\": {
    \"nofile\": {
      \"Name\": \"nofile\",
      \"Soft\": ${COLIMA_NOFILE_LIMIT},
      \"Hard\": ${COLIMA_NOFILE_LIMIT}
    }
  }
}
EOF

docker_changed=0
if [ ! -f /etc/docker/daemon.json ] || ! cmp -s \"\${tmpfile}\" /etc/docker/daemon.json; then
  install -m 0644 \"\${tmpfile}\" /etc/docker/daemon.json
  docker_changed=1
fi
rm -f \"\${tmpfile}\"

if [ \"\${docker_changed}\" -eq 1 ]; then
  systemctl reset-failed docker.service || true
  systemctl restart docker.service
else
  if ! systemctl is-active --quiet docker.service; then
    systemctl reset-failed docker.service || true
    systemctl start docker.service
  fi
fi
"
}

ensure_colima_vtpm_proxy() {
    # Same helper the Linux host runs. Docker's kernel here is the Colima VM.
    local script
    script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ensure-vtpm-proxy.sh"
    [[ -f "${script}" ]] || fail "missing ${script}"
    colima ssh --profile "${COLIMA_PROFILE}" -- sudo bash -s <"${script}"
}

# Colima ignores --cpu and --memory when the profile already exists, so a VM
# created smaller stays smaller and the E2E starves without saying why.
verify_colima_size() {
    local cpus mem_gib
    cpus="$(colima ssh --profile "${COLIMA_PROFILE}" -- nproc 2>/dev/null || echo 0)"
    mem_gib="$(colima ssh --profile "${COLIMA_PROFILE}" -- \
        awk '/MemTotal/{printf "%d", $2/1048576}' /proc/meminfo 2>/dev/null || echo 0)"
    # The VM reports a little less RAM than it was given, hence the slack.
    if (( cpus < COLIMA_CPUS || mem_gib + 2 < COLIMA_MEMORY_GIB )); then
        echo "colima-start: profile ${COLIMA_PROFILE} has ${cpus} CPUs and ${mem_gib} GiB, not the ${COLIMA_CPUS} CPUs and ${COLIMA_MEMORY_GIB} GiB requested." >&2
        echo "colima-start: Colima cannot resize a VM that exists. Run: colima stop --profile ${COLIMA_PROFILE} && colima start --cpu ${COLIMA_CPUS} --memory ${COLIMA_MEMORY_GIB} --disk ${COLIMA_DISK_GIB} --profile ${COLIMA_PROFILE}" >&2
        return 1
    fi
}

verify_colima_vm() {
    colima ssh --profile "${COLIMA_PROFILE}" -- sh -lc "
set -eu
# Fail fast on the exact VM-side contract the local node image depends on:
# tuned limits, a running Docker daemon, and /dev/vtpmx from the vTPM proxy.
sysctl fs.inotify.max_user_instances fs.inotify.max_user_watches fs.file-max
sudo systemctl is-active docker.service
test -c /dev/vtpmx
ls -l /dev/vtpmx
"
}

main() {
    local docker_host
    docker_host="$(docker_host_for_profile)"

    ensure_command colima

    if [[ "$(uname -s)" != "Darwin" ]]; then
        echo "colima-start: Colima setup is primarily intended for macOS." >&2
    fi

    run_step "starting the Colima VM" \
        colima start \
            --cpu "${COLIMA_CPUS}" \
            --memory "${COLIMA_MEMORY_GIB}" \
            --disk "${COLIMA_DISK_GIB}" \
            --runtime "${COLIMA_RUNTIME}" \
            --profile "${COLIMA_PROFILE}" || return 1

    run_step "configuring the Colima VM" configure_colima_vm || return 1
    run_step "installing the vTPM proxy module" ensure_colima_vtpm_proxy || return 1
    run_step "verifying the Colima VM" verify_colima_vm || return 1
    run_step "verifying the Colima VM size" verify_colima_size || return 1

    echo ""
    echo "Colima is ready for controller local development."
    echo "Docker socket: ${docker_host}"
    echo "vTPM proxy device: /dev/vtpmx"
    echo ""
    echo "Set DOCKER_HOST in your shell before running kind or tilt:"
    echo "  export DOCKER_HOST=${docker_host}"
    echo ""
    echo "To stop Colima: colima stop --profile ${COLIMA_PROFILE}"
}

main "$@"
