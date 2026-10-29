#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Walk through bringing up NVKE on an Apple silicon Mac with tenant clusters on
# real VMs: the management plane runs in kind inside Colima, and nico-sim boots
# each tenant node as a QEMU VM under Apple's Hypervisor.framework (HVF) on the
# vmnet shared network. Each step explains itself and pauses; pass --yes to run
# straight through. Tear down with hack/mac-vm-dev-down.sh.
#
# Prerequisites: colima, and either Flox (which pins the rest) or these on PATH:
# kind, tilt, kubectl, helm, go, jq, qemu, swtpm, and cloud-localds or mkisofs.
# Step 0 checks them all. The image bake needs xorriso and 7z, and the final
# check needs sshpass; with Nix installed, missing ones are borrowed from
# nixpkgs. vmnet-helper 0.13 (https://github.com/nirs/vmnet-helper) must be on
# PATH or given as NICO_VMS_VMNET_HELPER_PATH and NICO_VMS_VMNET_RUN_PATH.
# nico-sim installs control-plane VIP routes with `sudo route`, which needs a
# one-time sudoers rule (see "Sudoers rule for VIP routes" in
# controller/nico-sim/README.md).
# On Homebrew, `make -C controller/nico-sim install-deps-homebrew` installs
# QEMU, vmnet-helper, cloud-localds and swtpm.
#
# Environment:
#   CLUSTER_NAME        tenant cluster to create (default mac-ovn)
#   CAPNICO_SOURCE_DIR  cluster-api-provider-nico checkout (default ~/git/cluster-api-provider-nico)
#   COLIMA_PROFILE      Colima profile for kind (default nvke-mac)

set -euo pipefail
# shellcheck source=hack/mac-vm-dev-lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/mac-vm-dev-lib.sh"
reexec_in_flox "$@"

for arg in "$@"; do
  case "${arg}" in
    --yes) ASSUME_YES=true ;;
    *) die "unknown argument ${arg}" ;;
  esac
done

CLUSTER_NAME="${CLUSTER_NAME:-mac-ovn}"
export CAPNICO_SOURCE_DIR="${CAPNICO_SOURCE_DIR:-${HOME}/git/cluster-api-provider-nico}"
RELEASE=nvke-nico-vms-24.04-arm64-fde
PACKER_DIR="${REPO_ROOT}/os-image/packer"
mkdir -p "${STATE_DIR}"

# with_nix_tools runs a command with the named tools on PATH, borrowing them
# from nixpkgs when they are not installed.
with_nix_tools() {
  local missing=() tool pkg args=()
  while [[ "$1" != -- ]]; do
    tool="${1%%=*}" pkg="${1#*=}"
    command -v "${tool}" >/dev/null || missing+=("nixpkgs#${pkg}")
    shift
  done
  shift
  if ((${#missing[@]} == 0)); then
    "$@"
    return
  fi
  command -v nix >/dev/null || die "missing ${missing[*]#nixpkgs#}; install them (e.g. with Homebrew) and rerun"
  for pkg in "${missing[@]}"; do args+=("${pkg}"); done
  nix shell "${args[@]}" -c "$@"
}

# set_env_line sets KEY=VALUE in .env.tilt, replacing an existing line.
set_env_line() {
  local file="${CONTROLLER_DIR}/.env.tilt"
  if grep -q "^$1=" "${file}"; then
    sed -i '' "s|^$1=.*|$1=$2|" "${file}"
  else
    printf '%s=%s\n' "$1" "$2" >>"${file}"
  fi
}

ssh_node() {
  # Password auth only: with several keys in an ssh-agent, ssh offers each one
  # first and the server drops the connection for too many failures before it
  # reaches the password.
  with_nix_tools sshpass=sshpass -- sshpass -p nico-diag ssh -o PubkeyAuthentication=no \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    "nico-diag@$1" "${@:2}"
}

# ─────────────────────────────────────────────────────────────────────────────
step "0. Check the host" \
  "HVF needs Apple silicon, and nico-sim needs vmnet-helper to put VMs on" \
  "Apple's shared network without root. It also needs a sudoers rule to install" \
  "control-plane VIP routes with route(8), which only root may change."
[[ "$(uname -s)/$(uname -m)" == Darwin/arm64 ]] || die "this script is for Apple silicon Macs"
sudo -n -l /sbin/route -n add -host 172.31.0.10 192.168.64.2 >/dev/null 2>&1 ||
  die "passwordless sudo for VIP routes is not set up. Run 'sudo visudo -f /etc/sudoers.d/nvke-nico-sim' and add:
%admin ALL=(root) NOPASSWD: /sbin/route -n add -host 172.31.0.*, /sbin/route -n delete -host 172.31.0.*
See \"Sudoers rule for VIP routes\" in controller/nico-sim/README.md."
[[ "$(sysctl -n kern.hv_support)" == 1 ]] || die "Hypervisor.framework is not available"
missing=()
for cmd in colima jq curl openssl kind tilt kubectl helm go qemu-system-aarch64 swtpm; do
  command -v "${cmd}" >/dev/null || missing+=("${cmd}")
done
command -v cloud-localds >/dev/null || command -v mkisofs >/dev/null || command -v genisoimage >/dev/null ||
  missing+=("cloud-localds (or mkisofs)")
((${#missing[@]} == 0)) || die "not found: ${missing[*]}. Install Flox, or see the prerequisites at the top of this script."
if [[ -z "${NICO_VMS_VMNET_HELPER_PATH:-}" ]]; then
  NICO_VMS_VMNET_HELPER_PATH="$(command -v vmnet-helper)" || die "vmnet-helper not found; install it or set NICO_VMS_VMNET_HELPER_PATH"
fi
export NICO_VMS_VMNET_HELPER_PATH
export NICO_VMS_VMNET_RUN_PATH="${NICO_VMS_VMNET_RUN_PATH:-$(dirname "${NICO_VMS_VMNET_HELPER_PATH}")/vmnet-run}"
[[ -x "${NICO_VMS_VMNET_RUN_PATH}" ]] || die "vmnet-run not found next to vmnet-helper; set NICO_VMS_VMNET_RUN_PATH"
pgrep -f "tilt up .*--port ${TILT_PORT}" >/dev/null && die "Tilt is already running on port ${TILT_PORT}; run hack/mac-vm-dev-down.sh first"
note "vmnet-helper: ${NICO_VMS_VMNET_HELPER_PATH}"
note "all tools present"

# ─────────────────────────────────────────────────────────────────────────────
step "1. Bake the node OS image" \
  "Tenant VMs boot the NVKE node image built from this checkout, so their" \
  "nvke-agent is your local agent. Apple silicon has no 64K pages, so the image" \
  "uses linux-generic instead of linux-nvidia-64k. Baking takes about 10 minutes" \
  "and needs a clean git checkout; it is skipped when the image already exists."
shopt -s nullglob
images=("${PACKER_DIR}"/output-non-fde/nvke-node-non-fde-ubuntu-*-arm64.qcow2)
if ((${#images[@]} > 0)); then
  note "found ${images[0]#"${REPO_ROOT}/"}"
else
  pause
  # Packer refuses to write into an existing output directory, such as one
  # left by an interrupted bake or with its image deleted.
  run make -C "${PACKER_DIR}" clean-non-fde-output
  run with_nix_tools xorriso=xorriso 7z=p7zip -- \
    make -C "${PACKER_DIR}" os-image-non-fde ARCH=arm64
  images=("${PACKER_DIR}"/output-non-fde/nvke-node-non-fde-ubuntu-*-arm64.qcow2)
  ((${#images[@]} > 0)) || die "the bake finished but left no image in ${PACKER_DIR}/output-non-fde"
fi
image="${images[0]}"

# ─────────────────────────────────────────────────────────────────────────────
step "2. Write local configuration (gitignored)" \
  "controller/.env.tilt      NVKE controller settings; generated with random passwords if missing" \
  "controller/.local/fixtures  the test profiles and releases, with the arm64 release's" \
  "                          image-url pointed at the image from step 1" \
  "controller/tilt_config.json  CAPI core + NICo providers, VM tenants only" \
  "controller/tilt.d/mac-vm-dev.tilt  stops two Tilt rebuild loops seen on macOS"
pause
cd "${CONTROLLER_DIR}"
if [[ ! -f .env.tilt ]]; then
  (umask 077 && cp .env.tilt.example .env.tilt)
  set_env_line DGXC_TENANT_USER nvke-dev
  for key in DGXC_TENANT_PASSWORD CNPG_SUPERUSER_PASSWORD CNPG_CLOUD_PASSWORD CNPG_TEMPORAL_PASSWORD; do
    set_env_line "${key}" "$(openssl rand -hex 16)"
  done
  note "created .env.tilt"
fi
# The Tilt controller image bakes profiles from NKE_CONTROLLER_PROFILES_DIR,
# whose parent must hold the fixture Dockerfile and runbooks too. Tilt applies
# the releases from the same parent's releases/ directory.
set_env_line NKE_CONTROLLER_PROFILES_DIR .local/fixtures/profiles

rm -rf .local/fixtures && mkdir -p .local/fixtures
cp -R test/fixtures/Dockerfile test/fixtures/runbooks test/fixtures/profiles test/fixtures/releases .local/fixtures/
# The release's MachineImage names its image in the iPXE script's indented
# "set image-url" line; nico-sim copies a local path straight into its cache.
machine_image=".local/fixtures/releases/${RELEASE}-machineimage.yaml"
sed -i '' -E "s|^( +)set image-url .*|\\1set image-url ${image}|" "${machine_image}"
note "${RELEASE} boots $(grep -E '^ +set image-url' "${machine_image}" | awk '{print $3}')"

if [[ -f tilt_config.json ]] && ! cmp -s tilt_config.json <(jq -n '{providers: ["capi_core", "nico"], nico_sim_backends: ["vm"]}'); then
  cp tilt_config.json "${STATE_DIR}/tilt_config.json.bak"
  note "saved your tilt_config.json to ${STATE_DIR#"${CONTROLLER_DIR}/"}/tilt_config.json.bak"
fi
jq -n '{providers: ["capi_core", "nico"], nico_sim_backends: ["vm"]}' >tilt_config.json

mkdir -p tilt.d
cat >tilt.d/mac-vm-dev.tilt <<'EOF'
# Written by hack/mac-vm-dev-up.sh. The controller image build rewrites bin/controller,
# which is also its dependency, and macOS reports files a build merely reads
# (the Dockerfile, the CAPNICo chart) as changed, so both resources would
# rebuild forever. Rebuild them from the Tilt UI instead.
_capnico = os.getenv("CAPNICO_SOURCE_DIR", "")
watch_settings(ignore=[
    os.path.join(config.main_dir, "bin", "controller"),
    os.path.join(config.main_dir, "hack", "Dockerfile.cwe.tilt"),
] + ([_capnico] if _capnico else []))
EOF

if [[ ! -d "${CAPNICO_SOURCE_DIR}" ]]; then
  note "CAPNICo builds from source, so no NGC account is needed."
  run git clone https://github.com/dsx-ai-factory/cluster-api-provider-nico "${CAPNICO_SOURCE_DIR}"
fi
note "CAPNICo source: ${CAPNICO_SOURCE_DIR}"

# ─────────────────────────────────────────────────────────────────────────────
step "3. Start Colima" \
  "Colima runs the Docker VM that hosts kind. Only the management plane lives" \
  "there; tenant nodes are separate VMs on vmnet."
pause
run make -C "${CONTROLLER_DIR}" colima-up
use_colima_docker
note "DOCKER_HOST=${DOCKER_HOST}"

# ─────────────────────────────────────────────────────────────────────────────
step "4. Create the kind management cluster with Cluster API core" \
  "CAPNICo and NVKE come later from Tilt."
pause
run make -C "${CONTROLLER_DIR}" capi-core-up

# ─────────────────────────────────────────────────────────────────────────────
step "5. Start Tilt" \
  "Tilt builds and deploys NVKE, Temporal, Postgres, SPIRE and CAPNICo into kind," \
  "and runs nico-sim on this Mac with the darwin-qemu backend. The first run" \
  "builds everything and takes 10-20 minutes." \
  "Watch it at http://localhost:${TILT_PORT} or in ${STATE_DIR#"${CONTROLLER_DIR}/"}/tilt.log."
pause
nohup tilt up --stream --port "${TILT_PORT}" >"${STATE_DIR}/tilt.log" 2>&1 &
tilt_pid=$!
echo "${tilt_pid}" >"${STATE_DIR}/tilt.pid"
for resource in nico-sim capnico-provider cwe; do
  note "waiting for ${resource}"
  # tilt wait fails on a resource the Tiltfile has not registered yet.
  deadline=$((SECONDS + 600))
  until tilt get "uiresource/${resource}" --port "${TILT_PORT}" >/dev/null 2>&1; do
    kill -0 "${tilt_pid}" 2>/dev/null || die "tilt exited before registering ${resource}; see ${STATE_DIR}/tilt.log"
    ((SECONDS < deadline)) || die "tilt did not register ${resource} within 10 minutes; see ${STATE_DIR}/tilt.log"
    sleep 2
  done
  run tilt wait --port "${TILT_PORT}" --for=condition=Ready "uiresource/${resource}" --timeout=45m
done

# ─────────────────────────────────────────────────────────────────────────────
step "6. Create a tenant cluster" \
  "One control-plane VM and one worker, OVN-Kubernetes. On a Mac the script picks" \
  "8 GiB nodes and a pod CIDR outside vmnet's 192.168.64.0/24. The VIP comes from" \
  "172.31.0.10 up; kube-vip announces it over BGP and nico-sim routes it to the" \
  "control plane that holds it."
pause
created="$(CLUSTER_NAME="${CLUSTER_NAME}" RELEASE="${RELEASE}" run "${CONTROLLER_DIR}/hack/create-nico-vms-cluster.sh")"
cluster_id="$(jq -r .id <<<"${created}")"
vip="$(jq -r .controlPlaneVIP <<<"${created}")"
note "cluster ${CLUSTER_NAME}: id ${cluster_id}, VIP ${vip}"
note "Each VM iPXE-boots the image, runs cloud-init, attests to SPIRE with its TPM," \
  "then NVKE drives kubeadm through nvke-agent. Progress:"
last="" deadline=$((SECONDS + 1800))
while :; do
  status="$(api "/clusters/${cluster_id}" | jq -r '"\(.conditions.provisioned.status)|\(.conditions.provisioning.message)"')"
  vms="$(nico-sim/bin/nico-simctl instance list | jq -r --arg c "${cluster_id}" \
    '[.[] | select(.labels["nke.nvidia.com/cluster-id"] == $c) | "\(.name): \(.status)"] | join(", ")')"
  if [[ "${status}|${vms}" != "${last}" ]]; then
    note "$(date +%T) [${vms:-no VMs yet}] ${status#*|}"
    last="${status}|${vms}"
  fi
  [[ "${status%%|*}" == True ]] && break
  if [[ "${vms}" == *": Error"* ]]; then
    grep '"reconcile failed"' nico-sim/.dev/logs/nico-sim.log | tail -2 | jq -r .error >&2
    die "a VM failed in nico-sim (above); full log: nico-sim/.dev/logs/nico-sim.log"
  fi
  ((SECONDS < deadline)) || die "cluster not provisioned after 30 minutes"
  sleep 15
done

# ─────────────────────────────────────────────────────────────────────────────
step "7. Check the cluster" \
  "From the control-plane VM: nodes, system pods, then a pod on the worker" \
  "reached from a pod on the control plane, by pod IP and by service DNS."
pause
cp_ip="$(nico-sim/bin/nico-simctl instance list |
  jq -r --arg p "${CLUSTER_NAME}-cp-" '[.[] | select(.name | startswith($p))][0].interfaces[0].ipAddresses[0]')"
worker="$(nico-sim/bin/nico-simctl instance list |
  jq -r --arg p "${CLUSTER_NAME}-w-" '[.[] | select(.name | startswith($p))][0].name')"
cp_name="$(nico-sim/bin/nico-simctl instance list |
  jq -r --arg p "${CLUSTER_NAME}-cp-" '[.[] | select(.name | startswith($p))][0].name')"
note "control plane ${cp_name} at ${cp_ip}; diagnostic SSH: nico-diag/nico-diag"
ssh_node "${cp_ip}" bash -s -- "${cp_name}" "${worker}" <<'EOF'
set -euo pipefail
k() { sudo kubectl --kubeconfig /etc/kubernetes/admin.conf "$@"; }
# NVKE reports the cluster provisioned before the worker has joined.
deadline=$((SECONDS + 600))
until k get node "$2" >/dev/null 2>&1; do
  ((SECONDS < deadline)) || { echo "worker $2 did not join within 10 minutes" >&2; exit 1; }
  sleep 5
done
k wait --for=condition=Ready node --all --timeout=5m >/dev/null
k get nodes -o wide
k wait -n ovn-kubernetes --for=condition=Ready pod --all --timeout=5m >/dev/null
k wait -n kube-system --for=condition=Ready pod --all --timeout=5m >/dev/null
echo; k get pods -A
img=registry.k8s.io/e2e-test-images/agnhost:2.53
k run srv --image="${img}" --overrides="{\"spec\":{\"nodeName\":\"$2\"}}" -- netexec --http-port=8080 >/dev/null
k expose pod srv --port=80 --target-port=8080 >/dev/null
k wait --for=condition=Ready pod/srv --timeout=3m >/dev/null
ip="$(k get pod srv -o jsonpath='{.status.podIP}')"
echo
# Each URL must answer with the server pod's hostname; a failed request fails the pod and the pipeline.
check='set -e
for url in "$@"; do
  h=$(curl -fsS -m5 "${url}")
  [ "${h}" = srv ] || { echo "${url} answered \"${h}\", want srv" >&2; exit 1; }
  echo "${url}: ${h}"
done'
k run cli --rm -i --restart=Never --image="${img}" \
  --overrides="{\"spec\":{\"nodeName\":\"$1\",\"tolerations\":[{\"operator\":\"Exists\"}]}}" --command -- \
  sh -c "${check}" sh "http://${ip}:8080/hostname" http://srv.default.svc.cluster.local/hostname |
  grep -v 'deleted'
k delete pod srv --wait=false >/dev/null
k delete svc srv >/dev/null
EOF

step "Done" \
  "Tilt UI:       http://localhost:${TILT_PORT}" \
  "NVKE API:      ${NVKE_API} (token: hack/nvke-token.sh)" \
  "Cluster:       ${CLUSTER_NAME} (${cluster_id}), API server at https://${vip}:6443" \
  "Node shell:    sshpass -p nico-diag ssh -o PubkeyAuthentication=no nico-diag@${cp_ip}" \
  "Tear down:     hack/mac-vm-dev-down.sh"
