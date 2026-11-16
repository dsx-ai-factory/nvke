#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Run the validation image against a site from a workstation.
#
#   validate-site.sh <site file> [--read-only | --full] [--only=<labels>] [--dry-run]
#
# The site file is a shell file, sourced, that says how to reach the site and
# what to run. It is yours, not the site's: keep it outside the repository.
# What the site is goes in its validation profile, which the file names.
#
#   CONNECT=kubeconfig | teleport | ssh
#   PROFILE=<path>                 validation profile (NVKE_VALIDATE_PROFILE)
#   CONTROLLER_VERSION=<version>   what the controller's /api/version reports
#   KUBERNETES_VERSION=<version>   what the site was built with
#   IMAGE=<image>                  validation image
#   MODE=read-only | full          default read-only
#   RUNS_DIR=<dir>                 default ~/nvke-validate-runs
#
#   kubeconfig: KUBECONFIG_FILE=<path> [KUBE_CONTEXT=<name>]
#   teleport:   TELEPORT_KUBE_CLUSTER=<name>; run `tsh login` first
#   ssh:        SSH_TARGET=<user>@<host> SSH_KEY=<path> KUBECONFIG_FILE=<path>
#               [SSH_PROXY_COMMAND=<command>]: forwards the API server port of
#               KUBECONFIG_FILE's 127.0.0.1 server to the host's loopback
#               QUALIFY_RUN=<dir> fills these and both versions from a
#               `qualify local --keep-site` run.
#
# Any NVKE_* or NKE_E2E_* variable the file exports reaches the image as is,
# which is how a run adds a tenant (NVKE_TENANT_VALIDATE, NVKE_TENANT_CLUSTER).
#
# The container gets a flattened copy of the kubeconfig. Under Docker Desktop
# it reaches a local listener as host.docker.internal, so a loopback server is
# rewritten to that name, with tls-server-name pinned to the original host so
# the certificate still matches; on Linux it shares the host's network
# instead. Reports, the run log and the inputs used (no credentials) go to
# RUNS_DIR/<site>-<time>/.
set -euo pipefail

usage() { sed -n '5,8p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
die() { echo "validate-site: $*" >&2; exit 1; }

[[ $# -ge 1 ]] || usage
SITE_FILE=$1
shift
[[ -f $SITE_FILE ]] || die "no site file $SITE_FILE"

DRY_RUN=""
ONLY=""
MODE_FLAG=""
for arg in "$@"; do
  case $arg in
    --read-only) MODE_FLAG=read-only ;;
    --full) MODE_FLAG=full ;;
    --only=*) ONLY=${arg#--only=} ;;
    --dry-run) DRY_RUN=1 ;;
    -h | --help) usage ;;
    *) die "unknown argument $arg" ;;
  esac
done

# shellcheck source=/dev/null
source "$SITE_FILE"

SITE=$(basename "$SITE_FILE")
SITE=${SITE%.*}
MODE=${MODE_FLAG:-${MODE:-read-only}}
RUNS_DIR=${RUNS_DIR:-$HOME/nvke-validate-runs}
: "${CONNECT:?the site file sets no CONNECT (kubeconfig, teleport or ssh)}"
: "${IMAGE:?the site file sets no IMAGE}"
: "${PROFILE:?the site file sets no PROFILE}"

for tool in docker kubectl; do
  command -v "$tool" >/dev/null || die "$tool is not on PATH"
done
[[ -r $PROFILE ]] || die "cannot read the profile $PROFILE"

# A qualify run records the node, its key, its kubeconfig and both versions.
if [[ $CONNECT == ssh && -n ${QUALIFY_RUN:-} ]]; then
  keys=("$QUALIFY_RUN"/node-keys/*.key)
  [[ ${#keys[@]} -eq 1 && -f ${keys[0]} ]] || die "want exactly one node key in $QUALIFY_RUN/node-keys"
  node=$(basename "${keys[0]}" .key)
  SSH_KEY=${SSH_KEY:-${keys[0]}}
  KUBECONFIG_FILE=${KUBECONFIG_FILE:-$QUALIFY_RUN/node-keys/$node.kubeconfig}
  if [[ -z ${SSH_TARGET:-} ]]; then
    line=$(grep -h '"msg"="the site node can be logged into' "$QUALIFY_RUN/qualify-e2e.log" | tail -1 || true)
    ip=$(sed -nE 's/.*"ip"="([^"]+)".*/\1/p' <<<"$line")
    user=$(sed -nE 's/.*"user"="([^"]+)".*/\1/p' <<<"$line")
    [[ -n $ip && -n $user ]] || die "no node address in $QUALIFY_RUN/qualify-e2e.log"
    SSH_TARGET=$user@$ip
  fi
  CONTROLLER_VERSION=${CONTROLLER_VERSION:-$(sed -n 's/^SITE_NVKE_CONTROLLER_VERSION=//p' "$QUALIFY_RUN/steps.env" | tail -1)}
  KUBERNETES_VERSION=${KUBERNETES_VERSION:-$(sed -n 's/^KUBERNETES_VERSION=//p' "$QUALIFY_RUN/steps.env" | tail -1)}
fi
: "${CONTROLLER_VERSION:?the site file sets no CONTROLLER_VERSION}"
: "${KUBERNETES_VERSION:?the site file sets no KUBERNETES_VERSION}"

docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull -q "$IMAGE" >/dev/null ||
  die "no image $IMAGE; build one with: make -C controller/test/e2e validate-image VALIDATE_IMAGE=$IMAGE"

OUT=$RUNS_DIR/$SITE-$(date +%Y%m%d-%H%M%S)
mkdir -p "$OUT/artifacts"
chmod 0700 "$OUT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/validate-site.XXXXXX")
chmod 0700 "$WORK"
BACKGROUND=""
cleanup() {
  [[ -n $BACKGROUND ]] && kill "$BACKGROUND" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

wait_for_port() {
  local port=$1 pid=$2
  for _ in $(seq 1 30); do
    nc -z 127.0.0.1 "$port" 2>/dev/null && return 0
    kill -0 "$pid" 2>/dev/null || die "the $CONNECT connection exited; see $OUT/connect.log"
    sleep 2
  done
  die "nothing listened on 127.0.0.1:$port; see $OUT/connect.log"
}

# server_port prints the port of the current context's server.
server_port() {
  kubectl --kubeconfig "$1" config view --minify -o jsonpath='{.clusters[0].cluster.server}' |
    sed -nE 's#^https://[^/:]+:([0-9]+).*#\1#p'
}

# container_kubeconfig flattens the current context of $1 into $2, so file
# references are inlined, and points a loopback server at host.docker.internal.
container_kubeconfig() {
  local in=$1 out=$2 cluster server host rest
  kubectl --kubeconfig "$in" config view --raw --minify --flatten >"$out"
  chmod 0600 "$out"
  if kubectl --kubeconfig "$out" config view --raw -o jsonpath='{.users[0].user.exec.command}' | grep -q .; then
    die "the kubeconfig authenticates with an exec plugin, which the image does not have; use CONNECT=teleport, or a kubeconfig with certificates or a token"
  fi
  cluster=$(kubectl --kubeconfig "$out" config view -o jsonpath='{.clusters[0].name}')
  server=$(kubectl --kubeconfig "$out" config view -o jsonpath='{.clusters[0].cluster.server}')
  host=$(sed -nE 's#^https://([^/:]+).*#\1#p' <<<"$server")
  rest=${server#https://"$host"}
  [[ $(uname -s) == Linux ]] && return 0
  case $host in
    127.0.0.1 | localhost | *.localhost)
      local pinned
      pinned=$(kubectl --kubeconfig "$out" config view -o jsonpath='{.clusters[0].cluster.tls-server-name}')
      kubectl --kubeconfig "$out" config set-cluster "$cluster" \
        --server="https://host.docker.internal$rest" --tls-server-name="${pinned:-$host}" >/dev/null
      ;;
  esac
}

KUBE_IN=$WORK/source.kubeconfig
case $CONNECT in
  kubeconfig)
    : "${KUBECONFIG_FILE:?CONNECT=kubeconfig needs KUBECONFIG_FILE}"
    [[ -r $KUBECONFIG_FILE ]] || die "cannot read $KUBECONFIG_FILE"
    cp "$KUBECONFIG_FILE" "$KUBE_IN"
    chmod 0600 "$KUBE_IN"
    if [[ -n ${KUBE_CONTEXT:-} ]]; then
      kubectl --kubeconfig "$KUBE_IN" config use-context "$KUBE_CONTEXT" >/dev/null
    fi
    ;;
  teleport)
    : "${TELEPORT_KUBE_CLUSTER:?CONNECT=teleport needs TELEPORT_KUBE_CLUSTER}"
    command -v tsh >/dev/null || die "tsh is not on PATH"
    port=${TELEPORT_PROXY_PORT:-$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')}
    tsh proxy kube "$TELEPORT_KUBE_CLUSTER" --port "$port" --format=unix \
      >"$WORK/proxy.env" 2>"$OUT/connect.log" &
    BACKGROUND=$!
    wait_for_port "$port" "$BACKGROUND"
    proxy_kubeconfig=$(sed -nE 's/^export KUBECONFIG=(.*)$/\1/p' "$WORK/proxy.env" | tr -d '"')
    [[ -r $proxy_kubeconfig ]] || die "tsh proxy kube printed no kubeconfig; see $OUT/connect.log"
    cp "$proxy_kubeconfig" "$KUBE_IN"
    chmod 0600 "$KUBE_IN"
    ;;
  ssh)
    : "${SSH_TARGET:?CONNECT=ssh needs SSH_TARGET}"
    : "${SSH_KEY:?CONNECT=ssh needs SSH_KEY}"
    : "${KUBECONFIG_FILE:?CONNECT=ssh needs KUBECONFIG_FILE}"
    command -v ssh >/dev/null || die "ssh is not on PATH"
    cp "$KUBECONFIG_FILE" "$KUBE_IN"
    chmod 0600 "$KUBE_IN"
    port=$(server_port "$KUBE_IN")
    [[ -n $port ]] || die "$KUBECONFIG_FILE has no server port"
    ssh_args=(-F /dev/null -N -o ExitOnForwardFailure=yes -o BatchMode=yes
      -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$OUT/known_hosts"
      -i "$SSH_KEY" -L "127.0.0.1:$port:127.0.0.1:$port")
    [[ -n ${SSH_PROXY_COMMAND:-} ]] && ssh_args+=(-o "ProxyCommand=$SSH_PROXY_COMMAND")
    ssh "${ssh_args[@]}" "$SSH_TARGET" 2>"$OUT/connect.log" &
    BACKGROUND=$!
    wait_for_port "$port" "$BACKGROUND"
    ;;
  *) die "CONNECT=$CONNECT: want kubeconfig, teleport or ssh" ;;
esac

KUBE_OUT=$WORK/container.kubeconfig
container_kubeconfig "$KUBE_IN" "$KUBE_OUT"

case $MODE in
  read-only) filter='site-validate && !creates' ;;
  full) filter='site-validate' ;;
  *) die "MODE=$MODE: want read-only or full" ;;
esac
if [[ ${NVKE_TENANT_VALIDATE:-} == true ]]; then
  filter=${filter//site-validate/(site-validate || tenant-validate)}
fi
[[ -n $ONLY ]] && filter="($filter) && ($ONLY)"

passed=$(compgen -e | grep -E '^(NVKE|NKE_E2E)_' | grep -vE '^NVKE_(VALIDATE_PROFILE|CONTROLLER_EXPECTED_VERSION|SITE_KUBERNETES_VERSION)$' || true)
docker_args=(--rm --user "$(id -u):$(id -g)")
for name in $passed; do
  docker_args+=(-e "$name")
done
# Docker Desktop reaches the host's loopback as host.docker.internal; Linux
# reaches it only from the host's network.
if [[ $(uname -s) == Linux ]]; then
  docker_args+=(--network host)
fi

cat >"$OUT/inputs.env" <<EOF
SITE=$SITE
CONNECT=$CONNECT
IMAGE=$IMAGE
IMAGE_ID=$(docker image inspect "$IMAGE" --format '{{.Id}}')
PROFILE=$PROFILE
CONTROLLER_VERSION=$CONTROLLER_VERSION
KUBERNETES_VERSION=$KUBERNETES_VERSION
MODE=$MODE
FILTER=$filter
DRY_RUN=${DRY_RUN:-0}
PASSED_ENV=$(paste -sd' ' - <<<"$passed")
EOF
cp "$PROFILE" "$OUT/profile.yaml"

echo "site $SITE, $CONNECT, image $IMAGE"
echo "profile $PROFILE"
echo "expecting controller $CONTROLLER_VERSION, Kubernetes $KUBERNETES_VERSION"
echo "filter: $filter${DRY_RUN:+ (dry run)}"
echo "output $OUT"

set +e
docker run "${docker_args[@]}" \
  -v "$OUT/artifacts:/artifacts" \
  -v "$KUBE_OUT:/kube/config:ro" \
  -v "$PROFILE:/profile.yaml:ro" \
  -e KUBECONFIG=/kube/config \
  -e NVKE_VALIDATE_PROFILE=/profile.yaml \
  -e NVKE_CONTROLLER_EXPECTED_VERSION="$CONTROLLER_VERSION" \
  -e NVKE_SITE_KUBERNETES_VERSION="$KUBERNETES_VERSION" \
  "$IMAGE" "-ginkgo.label-filter=$filter" ${DRY_RUN:+-ginkgo.dry-run} 2>&1 | tee "$OUT/run.log"
status=${PIPESTATUS[0]}
set -e

junit=$OUT/artifacts/junit-test-report.xml
if [[ -f $junit ]] && command -v python3 >/dev/null; then
  python3 - "$junit" <<'EOF'
import sys
import xml.etree.ElementTree as ET

cases = ET.parse(sys.argv[1]).getroot().iter("testcase")
passed, failed, skipped = [], [], []
for case in cases:
    name = case.get("name", "")
    if case.find("failure") is not None or case.find("error") is not None:
        node = case.find("failure") if case.find("failure") is not None else case.find("error")
        failed.append((name, (node.get("message") or "").splitlines()[0:1]))
    elif case.find("skipped") is not None:
        skipped.append((name, case.find("skipped").get("message") or ""))
    else:
        passed.append(name)
print(f"\n{len(passed)} passed, {len(failed)} failed, {len(skipped)} skipped")
for name, message in failed:
    print(f"  FAILED  {name}" + (f": {message[0]}" if message else ""))
for name, message in skipped:
    print(f"  skipped {name}" + (f": {message}" if message else ""))
EOF
fi
echo "image exit $status; reports in $OUT/artifacts"
exit "$status"
