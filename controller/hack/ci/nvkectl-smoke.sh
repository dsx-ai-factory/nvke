#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Drive nvkectl against the controller a backend E2E lane already deployed. Source it
# from ci/backend-e2e.sh; do not run it.
#
# The Ginkgo suite proves the server. This proves the CLI can talk to it: read
# commands and their negatives, provider inspection, rejected creates, and one
# create, list, patch, delete round trip over a cluster and a node pool.
#
# Needs from the caller: log, NVKECTL_BIN, TMP_DIR, BACKEND, CAPD_NODE_IMAGE.
#
# Two shapes of lane, selected by NVKECTL_SMOKE_RUNNER. Set it to a container
# name and the CLI runs in there (ci/backend-e2e.sh, which drives kind through
# a runner). Leave it unset and the CLI runs on this host against
# NVKECTL_SMOKE_KUBECONFIG (ci/nico-vms-e2e.sh, which has no runner). Every
# exec, copy and file test below branches on it in one place, so no assertion
# has to care.
#
# Deliberately not RUNNER_NAME: GitHub Actions sets that in every job, so
# "unset means host mode" is never true in CI. backend-e2e.sh overwrites it
# with its own container name, which hid that until nico-vms tried to docker
# exec into the Actions runner's hostname.

# The controller service the suite deploys.
NVKECTL_SMOKE_CONTROLLER_NAMESPACE=nvke-controller
NVKECTL_SMOKE_CONTROLLER_SERVICE=nvke-controller
NVKECTL_SMOKE_CONTROLLER_PORT=8080
# A name no lane creates. Used by every negative assertion, so a rename here
# cannot leave one of them silently matching a real object.
NVKECTL_SMOKE_ABSENT=nvkectl-smoke-absent
# Name the rejection bodies carry; asserted absent afterwards.
NVKECTL_SMOKE_REJECT_CLUSTER=nvkectl-smoke-reject
# The suite's default; create validation checks only the semver shape.
NVKECTL_SMOKE_KUBERNETES_VERSION="${NVKECTL_SMOKE_KUBERNETES_VERSION:-1.34.2}"
# Name the round trip creates and deletes.
NVKECTL_SMOKE_WRITE_CLUSTER=nvkectl-smoke-write
# The worker pool the round trip adds to that cluster, then removes.
NVKECTL_SMOKE_WRITE_NODEPOOL=nvkectl-smoke-pool
# Delete wait: this many polls, this many seconds apart. A cluster delete tears
# down a real control-plane node, so it gets the longer cap.
NVKECTL_SMOKE_DELETE_POLLS="${NVKECTL_SMOKE_DELETE_POLLS:-60}"
NVKECTL_SMOKE_CLUSTER_DELETE_POLLS="${NVKECTL_SMOKE_CLUSTER_DELETE_POLLS:-120}"
NVKECTL_SMOKE_DELETE_POLL_SECONDS="${NVKECTL_SMOKE_DELETE_POLL_SECONDS:-5}"

nvkectl_smoke_build() {
  log "compiling nvkectl for linux/${1}"
  (cd ../cli && CGO_ENABLED=0 GOOS=linux GOARCH="${1}" go build -o "${NVKECTL_BIN}" ./cmd/nvkectl)
}

# Bounds every blocking docker exec and docker cp below. A hang is worse than a
# failure: the job cap's SIGTERM makes handle_signal skip diagnostics.
NVKECTL_SMOKE_TIMEOUT="${NVKECTL_SMOKE_TIMEOUT:-60}"

# The site kubeconfig, passed to `config set-site --kubeconfig`. nvkectl reads
# the path out of the site config for token minting, get providers and get
# kubeconfig; it never falls back to $KUBECONFIG, so this is the only thing
# that points those three at a cluster. In a runner it is the path the lane
# mounts; on a host lane the caller points it at kind's own kubeconfig.
NVKECTL_SMOKE_KUBECONFIG="${NVKECTL_SMOKE_KUBECONFIG:-/state/management.kubeconfig}"

# Keeps the CLI's own state out of the invoking user's home. config set-site
# writes $XDG_CONFIG_HOME/nvkectl/config.yaml and token minting caches under
# $XDG_CACHE_HOME, so a host lane would otherwise overwrite a developer's real
# nvkectl config and leave a live token behind.
#
# A function, not a variable: this file is sourced before the caller sets
# TMP_DIR, and reading it at source time aborts under `set -u`.
nvkectl_smoke_home() {
  echo "${TMP_DIR}/nvkectl-home"
}

nvkectl_smoke_stage() {
  [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]] || return 0
  timeout "${NVKECTL_SMOKE_TIMEOUT}" docker cp "${NVKECTL_BIN}" "${NVKECTL_SMOKE_RUNNER}:/usr/local/bin/nvkectl"
}

nvkectl_exec() {
  if [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]]; then
    timeout "${NVKECTL_SMOKE_TIMEOUT}" docker exec \
      -e KUBECONFIG="${NVKECTL_SMOKE_KUBECONFIG}" \
      "${NVKECTL_SMOKE_RUNNER}" nvkectl "$@"
  else
    XDG_CONFIG_HOME="$(nvkectl_smoke_home)" XDG_CACHE_HOME="$(nvkectl_smoke_home)" \
      timeout "${NVKECTL_SMOKE_TIMEOUT}" "${NVKECTL_BIN}" "$@"
  fi
}

nvkectl_no_auth_exec() {
  if [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]]; then
    timeout "${NVKECTL_SMOKE_TIMEOUT}" docker exec \
      -e XDG_CONFIG_HOME=/tmp/nvkectl-no-auth \
      "${NVKECTL_SMOKE_RUNNER}" nvkectl "$@"
  else
    XDG_CONFIG_HOME="${TMP_DIR}/nvkectl-no-auth" XDG_CACHE_HOME="$(nvkectl_smoke_home)" \
      timeout "${NVKECTL_SMOKE_TIMEOUT}" "${NVKECTL_BIN}" "$@"
  fi
}

# `test`, for asserting on files nvkectl did or did not write.
nvkectl_smoke_runner_test() {
  if [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]]; then
    timeout "${NVKECTL_SMOKE_TIMEOUT}" docker exec "${NVKECTL_SMOKE_RUNNER}" test "$@"
  else
    test "$@"
  fi
}

# mkdir, wherever the CLI is going to read it from.
nvkectl_smoke_mkdir() {
  if [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]]; then
    timeout "${NVKECTL_SMOKE_TIMEOUT}" docker exec "${NVKECTL_SMOKE_RUNNER}" mkdir -p "$1"
  else
    mkdir -p "$1"
  fi
}

# Port-forward rather than the apiserver service proxy: the proxy does not
# forward a bearer token to the controller, which is why wait_for_zero_tenant_clusters
# counts CAPI objects instead of calling the API.
# Called bare, not as `nvkectl_smoke || exit 1`: bash disables errexit for a
# whole function body when the call is the left side of ||, which made every
# unguarded command here unable to fail the lane. Keep the call sites in
# ci/backend-e2e.sh bare, and keep each assertion an explicit `return 1`.
nvkectl_smoke() {
  local no_auth_config out rc

  # Only the runner lanes need this. A host lane already has the controller on
  # localhost:${NVKECTL_SMOKE_CONTROLLER_PORT}, forwarded by Tilt for the whole run.
  if [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]]; then
    docker exec -d -e KUBECONFIG="${NVKECTL_SMOKE_KUBECONFIG}" "${NVKECTL_SMOKE_RUNNER}" \
      kubectl -n "${NVKECTL_SMOKE_CONTROLLER_NAMESPACE}" port-forward \
      "svc/${NVKECTL_SMOKE_CONTROLLER_SERVICE}" "${NVKECTL_SMOKE_CONTROLLER_PORT}:${NVKECTL_SMOKE_CONTROLLER_PORT}"
  fi

  nvkectl_exec config set-site --name e2e \
    --url "http://127.0.0.1:${NVKECTL_SMOKE_CONTROLLER_PORT}" \
    --kubeconfig "${NVKECTL_SMOKE_KUBECONFIG}" >/dev/null

  # A separate site with no kubeconfig exercises the no-credential path. The
  # normal site above exercises token minting through the management cluster.
  no_auth_config="${TMP_DIR}/nvkectl-no-auth-config.yaml"
  cat >"${no_auth_config}" <<EOF
sites:
  e2e:
    url: http://127.0.0.1:${NVKECTL_SMOKE_CONTROLLER_PORT}
    kubeconfig: ""
current-site: e2e
EOF
  if [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]]; then
    nvkectl_smoke_mkdir /tmp/nvkectl-no-auth/nvkectl
    timeout "${NVKECTL_SMOKE_TIMEOUT}" docker cp "${no_auth_config}" \
      "${NVKECTL_SMOKE_RUNNER}:/tmp/nvkectl-no-auth/nvkectl/config.yaml" >/dev/null
  else
    nvkectl_smoke_mkdir "${TMP_DIR}/nvkectl-no-auth/nvkectl"
    cp "${no_auth_config}" "${TMP_DIR}/nvkectl-no-auth/nvkectl/config.yaml"
  fi

  # Doubles as the readiness wait for the forward.
  rc=1
  for _ in $(seq 1 15); do
    if out="$(nvkectl_exec get clusters 2>&1)"; then
      rc=0
      break
    fi
    sleep 2
  done
  if ((rc != 0)); then
    log "nvkectl smoke: 'get clusters' never succeeded: ${out}" >&2
    return 1
  fi
  if ! grep -q 'KUBERNETES VERSION' <<<"${out}"; then
    log "nvkectl smoke: 'get clusters' printed no table header: ${out}" >&2
    return 1
  fi

  # Control: without this, a controller that stopped enforcing auth would leave the
  # check above green and prove nothing about the token.
  if [[ "${NKE_E2E_REQUIRE_AUTHN:-}" == "true" ]]; then
    if out="$(nvkectl_no_auth_exec get clusters 2>&1)"; then
      log "nvkectl smoke: 'get clusters' succeeded with no token, so the run above proves nothing: ${out}" >&2
      return 1
    fi
  else
    log "nvkectl smoke: SKIP the no-token control, NKE_E2E_REQUIRE_AUTHN is not true"
  fi

  if ! out="$(nvkectl_exec get clusters -o json 2>&1)"; then
    log "nvkectl smoke: 'get clusters -o json' failed: ${out}" >&2
    return 1
  fi
  case "${out}" in
    '['*|'{'*|null*) ;;
    *) log "nvkectl smoke: -o json did not emit JSON: ${out}" >&2; return 1 ;;
  esac

  # user-flow.md step 7 documents -o yaml beside -o json. Nothing else here
  # asserts it, and the two take different branches of the printer.
  if ! out="$(nvkectl_exec get clusters -o yaml 2>&1)"; then
    log "nvkectl smoke: 'get clusters -o yaml' failed: ${out}" >&2
    return 1
  fi
  case "${out}" in
    '{'*) log "nvkectl smoke: -o yaml emitted JSON: ${out}" >&2; return 1 ;;
  esac

  # The documented aliases. A verb that lost one is a user-visible break the
  # plural-only assertions above would not catch.
  if ! nvkectl_exec get cluster >/dev/null 2>&1; then
    log "nvkectl smoke: 'get cluster' (singular alias) failed" >&2
    return 1
  fi
  if ! nvkectl_exec get nodepool >/dev/null 2>&1; then
    log "nvkectl smoke: 'get nodepool' (singular alias) failed" >&2
    return 1
  fi

  # Read-path negatives. Each asserts the message, not just a non-zero exit: an
  # exit code alone cannot tell "the lookup ran and found nothing" apart from
  # "the request never left the client".

  # Exercises the ?name= filter, which the calls above never hit.
  if out="$(nvkectl_exec get clusters "${NVKECTL_SMOKE_ABSENT}" 2>&1)"; then
    log "nvkectl smoke: 'get clusters ${NVKECTL_SMOKE_ABSENT}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nvkectl smoke: 'get clusters <name>' failed without a not-found message" >&2
    return 1
  fi
  # A failed command still names its trace, so the operator can look it up in
  # the controller's logs. The success side is asserted on create below.
  if ! grep -Eq '^Trace ID: [0-9a-f]{32}$' <<<"${out}"; then
    log "nvkectl smoke: 'get clusters <name>' failed without a Trace ID line: ${out}" >&2
    return 1
  fi

  # The same FindClusterByName lookup patch cluster shares with delete.
  if out="$(nvkectl_exec patch cluster "${NVKECTL_SMOKE_ABSENT}" --patch 'teleportEnabled: true' 2>&1)"; then
    log "nvkectl smoke: 'patch cluster ${NVKECTL_SMOKE_ABSENT}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nvkectl smoke: 'patch cluster' failed without a not-found message" >&2
    return 1
  fi

  # Rejected by the printer before any request is issued.
  if out="$(nvkectl_exec get clusters -o bogus 2>&1)"; then
    log "nvkectl smoke: 'get clusters -o bogus' was accepted" >&2
    return 1
  fi
  if ! grep -qi 'output format' <<<"${out}"; then
    log "nvkectl smoke: '-o bogus' failed for some reason other than the format" >&2
    return 1
  fi

  nvkectl_smoke_local_commands || return 1
  nvkectl_smoke_kubeconfig_negatives || return 1
  nvkectl_smoke_nodepool_negatives || return 1
  nvkectl_smoke_providers || return 1
  nvkectl_smoke_create_rejections || return 1
  nvkectl_smoke_write_round_trip || return 1

  log "PASS: nvkectl reached the controller, authenticated, listed clusters and node pools, read providers, resolved a kubeconfig request, and had malformed creates rejected"
}

# version and config: every command that needs no controller. They are the first two
# steps of the operator journey in cli/docs/user-flow.md, and a broken one is
# invisible to every other assertion here because they all run after set-site.
nvkectl_smoke_local_commands() {
  local out

  if ! out="$(nvkectl_exec version 2>&1)"; then
    log "nvkectl smoke: 'version' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q '^nvkectl ' <<<"${out}"; then
    log "nvkectl smoke: 'version' printed no version line: ${out}" >&2
    return 1
  fi

  if ! out="$(nvkectl_exec version -o json 2>&1)"; then
    log "nvkectl smoke: 'version -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e '.buildVersion' >/dev/null 2>&1 <<<"${out}"; then
    log "nvkectl smoke: 'version -o json' emitted no buildVersion: ${out}" >&2
    return 1
  fi
  if out="$(nvkectl_exec version -o bogus 2>&1)"; then
    log "nvkectl smoke: 'version -o bogus' was accepted" >&2
    return 1
  fi

  # user-flow.md documents short|yaml|json. json is above; these are the rest.
  if ! out="$(nvkectl_exec version -o short 2>&1)"; then
    log "nvkectl smoke: 'version -o short' failed: ${out}" >&2
    return 1
  fi
  if ! out="$(nvkectl_exec version -o yaml 2>&1)"; then
    log "nvkectl smoke: 'version -o yaml' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'buildVersion:' <<<"${out}"; then
    log "nvkectl smoke: 'version -o yaml' emitted no buildVersion: ${out}" >&2
    return 1
  fi

  # view must show the site set-site wrote, not an empty document.
  if ! out="$(nvkectl_exec config view 2>&1)"; then
    log "nvkectl smoke: 'config view' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'e2e' <<<"${out}"; then
    log "nvkectl smoke: 'config view' does not list the e2e site: ${out}" >&2
    return 1
  fi

  # Selecting the only configured site is a no-op, so it cannot strand the
  # calls after it. The absent name is the real assertion.
  if ! out="$(nvkectl_exec config use-site e2e 2>&1)"; then
    log "nvkectl smoke: 'config use-site e2e' failed: ${out}" >&2
    return 1
  fi
  if out="$(nvkectl_exec config use-site "${NVKECTL_SMOKE_ABSENT}" 2>&1)"; then
    log "nvkectl smoke: 'config use-site ${NVKECTL_SMOKE_ABSENT}' succeeded against a site that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found in config' <<<"${out}"; then
    log "nvkectl smoke: 'config use-site' failed without a not-found message: ${out}" >&2
    return 1
  fi

  # Wiring only. generate sitespec needs no controller and its output is exercised for
  # real by nvkectl-bootstrap-e2e, so this just proves the shipped binary still
  # has the command and enforces its required flag.
  if out="$(nvkectl_exec generate sitespec smoke-gen 2>&1)"; then
    log "nvkectl smoke: 'generate sitespec' ran without the required --nodes" >&2
    return 1
  fi
  if ! grep -q 'nodes' <<<"${out}"; then
    log "nvkectl smoke: 'generate sitespec' failed for some reason other than --nodes: ${out}" >&2
    return 1
  fi
}

# get kubeconfig reaches the management cluster directly, not the controller: site
# kubeconfig -> Secret <namespace>/<cluster>-admin-kubeconfig. Nothing else here
# exercises that path, and the not-found message is the only proof the CLI
# looked in the right namespace under the right name. The CLI tries nvke-system,
# then falls back to nke-system, and its error names the last one it tried, so
# either namespace is a correct answer.
nvkectl_smoke_kubeconfig_negatives() {
  local out

  if out="$(nvkectl_exec get kubeconfig "${NVKECTL_SMOKE_ABSENT}" 2>&1)"; then
    log "nvkectl smoke: 'get kubeconfig ${NVKECTL_SMOKE_ABSENT}' succeeded against a cluster that does not exist" >&2
    return 1
  fi
  if ! grep -Eq "(nvke|nke)-system/${NVKECTL_SMOKE_ABSENT}-admin-kubeconfig" <<<"${out}"; then
    log "nvkectl smoke: 'get kubeconfig' named the wrong secret: ${out}" >&2
    return 1
  fi

  if out="$(nvkectl_exec get kubeconfig 2>&1)"; then
    log "nvkectl smoke: 'get kubeconfig' with no cluster name was accepted" >&2
    return 1
  fi

  # user-flow.md step 8 documents --output-file. A failed fetch must not leave
  # a file behind for a later step to read as a kubeconfig.
  if out="$(nvkectl_exec get kubeconfig "${NVKECTL_SMOKE_ABSENT}" \
    --output-file "$(nvkectl_smoke_spec_path kubeconfig)" 2>&1)"; then
    log "nvkectl smoke: 'get kubeconfig --output-file' succeeded for an absent cluster" >&2
    return 1
  fi
  if nvkectl_smoke_runner_test -e "$(nvkectl_smoke_spec_path kubeconfig)"; then
    log "nvkectl smoke: 'get kubeconfig --output-file' wrote a file despite failing" >&2
    return 1
  fi
}

# Node pool read-path negatives, asserting the message like the cluster ones
# above. A node pool command resolves a name through the controller before it acts, so an
# exit code alone cannot tell a lookup that found nothing apart from a flag the
# CLI rejected on its own.
nvkectl_smoke_nodepool_negatives() {
  local out absent="${NVKECTL_SMOKE_ABSENT}"

  if out="$(nvkectl_exec get nodepools "${absent}" 2>&1)"; then
    log "nvkectl smoke: 'get nodepools ${absent}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nvkectl smoke: 'get nodepools <name>' failed without a not-found message: ${out}" >&2
    return 1
  fi

  # --cluster is resolved through FindClusterByName before any pool is listed.
  if out="$(nvkectl_exec get nodepools --cluster "${absent}" 2>&1)"; then
    log "nvkectl smoke: 'get nodepools --cluster ${absent}' succeeded against a cluster that does not exist" >&2
    return 1
  fi
  if ! grep -q "cluster \"${absent}\" not found" <<<"${out}"; then
    log "nvkectl smoke: 'get nodepools --cluster <name>' failed without naming the missing cluster: ${out}" >&2
    return 1
  fi

  # The FindNodePoolByName lookup patch and delete share.
  if out="$(nvkectl_exec patch nodepool "${absent}" --cluster "${absent}" --patch 'desiredNodeCount: 1' 2>&1)"; then
    log "nvkectl smoke: 'patch nodepool ${absent}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q "node pool \"${absent}\" not found in cluster \"${absent}\"" <<<"${out}"; then
    log "nvkectl smoke: 'patch nodepool' failed without a not-found message: ${out}" >&2
    return 1
  fi

  if out="$(nvkectl_exec delete nodepool "${absent}" --cluster "${absent}" 2>&1)"; then
    log "nvkectl smoke: 'delete nodepool ${absent}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q "node pool \"${absent}\" not found in cluster \"${absent}\"" <<<"${out}"; then
    log "nvkectl smoke: 'delete nodepool' failed without a not-found message: ${out}" >&2
    return 1
  fi

  # Pool names are unique per cluster, not per site, so --cluster is required.
  if out="$(nvkectl_exec delete nodepool "${absent}" 2>&1)"; then
    log "nvkectl smoke: 'delete nodepool ${absent}' was accepted with no --cluster" >&2
    return 1
  fi
  if ! grep -q 'required flag(s) "cluster" not set' <<<"${out}"; then
    log "nvkectl smoke: 'delete nodepool' with no --cluster failed for some other reason: ${out}" >&2
    return 1
  fi

  # Rejected by the patch validator before any request is issued: teleportEnabled
  # is a real UpdateNodePoolRequest field that nvkectl does not expose.
  if out="$(nvkectl_exec patch nodepool "${absent}" --cluster "${absent}" --patch 'teleportEnabled: true' 2>&1)"; then
    log "nvkectl smoke: 'patch nodepool --patch teleportEnabled' was accepted" >&2
    return 1
  fi
  if ! grep -q 'nvkectl can set desiredNodeCount' <<<"${out}"; then
    log "nvkectl smoke: an unsupported patch field failed for some reason other than the field: ${out}" >&2
    return 1
  fi
}

# `get providers` reads the site kubeconfig through kubectl rather than the controller
# REST client, so it is the only smoke assertion covering that subsystem. Both
# lanes stage CAPNICo and wait for its CRDs (ci/backend-e2e.sh, needs-capnico-source),
# so crdsEstablished holds on CAPD too; only the NICo lane deploys the manager
# and the credentials Secret.
nvkectl_smoke_providers() {
  local out types

  if ! out="$(nvkectl_exec get providers -o json 2>&1)"; then
    log "nvkectl smoke: 'get providers -o json' failed" >&2
    return 1
  fi

  types="$(jq -r '[.[].type] | join(",")' <<<"${out}" 2>/dev/null || true)"
  if [[ "${types}" != "nico,launchlayer" ]]; then
    log "nvkectl smoke: 'get providers' listed [${types}], want [nico,launchlayer]" >&2
    return 1
  fi

  # The load-bearing assertion: it fails if the CRD names drift from what the
  # provider ships, or if the kubeconfig plumbing breaks. Deliberately NOT a
  # check for state == CONFIGURED -- managerDeployments selects on
  # app.kubernetes.io/name, a label owned by the provider repository.
  if [[ "$(jq -r '.[] | select(.type=="nico") | .crdsEstablished' <<<"${out}")" != "true" ]]; then
    log "nvkectl smoke: nico provider reports crdsEstablished=false" >&2
    return 1
  fi

  # CAPLL is deployed by neither lane, so this pins the not-installed rendering.
  if [[ "$(jq -r '.[] | select(.type=="launchlayer") | .state' <<<"${out}")" != "NOT-INSTALLED" ]]; then
    log "nvkectl smoke: launchlayer is not NOT-INSTALLED, so this lane changed shape" >&2
    return 1
  fi

  if [[ "${NKE_E2E_BACKEND:-}" == "nico" ]]; then
    if [[ "$(jq -r '.[] | select(.type=="nico") | .secretPresent' <<<"${out}")" != "true" ]]; then
      log "nvkectl smoke: nico provider reports secretPresent=false on the NICo lane" >&2
      return 1
    fi
  fi

  if ! out="$(nvkectl_exec get providers 2>&1)"; then
    log "nvkectl smoke: 'get providers' table mode failed" >&2
    return 1
  fi
  if ! grep -q 'PROVIDER' <<<"${out}"; then
    log "nvkectl smoke: 'get providers' printed no table header" >&2
    return 1
  fi

  # A site that is not configured must fail before any cluster contact.
  if out="$(nvkectl_exec --site "${NVKECTL_SMOKE_ABSENT}" get providers 2>&1)"; then
    log "nvkectl smoke: '--site ${NVKECTL_SMOKE_ABSENT}' was accepted" >&2
    return 1
  fi
}

# Where a staged spec lands in the runner container.
nvkectl_smoke_spec_path() {
  if [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]]; then
    echo "/tmp/nvkectl-smoke-$1.yaml"
  else
    echo "${TMP_DIR}/nvkectl-smoke-$1.yaml"
  fi
}

# Stage the YAML on stdin into the runner. nvkectl_exec has no -i, so
# `create cluster -f -` cannot read stdin.
nvkectl_smoke_stage_spec() {
  local name="$1"
  local host_path="${TMP_DIR}/nvkectl-smoke-${name}.yaml"
  cat >"${host_path}" || return 1
  # On a host lane the spec is already where the CLI will read it.
  [[ -n "${NVKECTL_SMOKE_RUNNER:-}" ]] || return 0
  timeout "${NVKECTL_SMOKE_TIMEOUT}" docker cp \
    "${host_path}" "${NVKECTL_SMOKE_RUNNER}:$(nvkectl_smoke_spec_path "${name}")" >/dev/null
}

# Bodies the OpenAPI validator rejects before any handler runs: no row, no
# workflow. Each is valid except for the one fault it names.
nvkectl_smoke_create_rejections() {
  local out

  nvkectl_smoke_stage_spec reject-name <<EOF || return 1
name: Nvkectl_Smoke
controlPlaneKubernetesVersion: ${NVKECTL_SMOKE_KUBERNETES_VERSION}
cniPlugin: ovn-kubernetes
infra:
  cluster:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: DockerCluster
    metadata:
      name: ${NVKECTL_SMOKE_REJECT_CLUSTER}
    spec: {}
EOF
  nvkectl_smoke_expect_create_rejected reject-name "a name that violates the DNS-label pattern" || return 1

  nvkectl_smoke_stage_spec reject-version <<EOF || return 1
name: ${NVKECTL_SMOKE_REJECT_CLUSTER}
cniPlugin: ovn-kubernetes
infra:
  cluster:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: DockerCluster
    metadata:
      name: ${NVKECTL_SMOKE_REJECT_CLUSTER}
    spec: {}
EOF
  nvkectl_smoke_expect_create_rejected reject-version "no controlPlaneKubernetesVersion" || return 1

  nvkectl_smoke_stage_spec reject-provider <<EOF || return 1
name: ${NVKECTL_SMOKE_REJECT_CLUSTER}
controlPlaneKubernetesVersion: ${NVKECTL_SMOKE_KUBERNETES_VERSION}
cniPlugin: ovn-kubernetes
EOF
  nvkectl_smoke_expect_create_rejected reject-provider "neither nico nor infra" || return 1

  # Zero state: a rejected create must leave nothing to find by name.
  if out="$(nvkectl_exec get clusters "${NVKECTL_SMOKE_REJECT_CLUSTER}" 2>&1)"; then
    log "nvkectl smoke: a rejected create left cluster ${NVKECTL_SMOKE_REJECT_CLUSTER} behind: ${out}" >&2
    return 1
  fi
}

# Assert the status, not the message: the text is the middleware's, and a 400
# is the only proof the request reached the controller rather than dying in the CLI.
nvkectl_smoke_expect_create_rejected() {
  local spec="$1" what="$2" out
  if out="$(nvkectl_exec create cluster -f "$(nvkectl_smoke_spec_path "${spec}")" 2>&1)"; then
    log "nvkectl smoke: 'create cluster' with ${what} was accepted: ${out}" >&2
    return 1
  fi
  if ! grep -q '400' <<<"${out}"; then
    log "nvkectl smoke: 'create cluster' with ${what} failed, but not with a 400 from the controller: ${out}" >&2
    return 1
  fi
}

# Prints the write round trip's cluster spec. The control plane is a
# cluster-level controlPlaneConfig, which has no autoProvisionDisabled
# equivalent, so this round trip provisions a real control-plane node. The
# kubeconfig, nodepool-list and delete handling below account for that.
nvkectl_smoke_write_spec() {
  cat <<EOF
name: ${NVKECTL_SMOKE_WRITE_CLUSTER}
controlPlaneKubernetesVersion: ${NVKECTL_SMOKE_KUBERNETES_VERSION}
cniPlugin: ovn-kubernetes
etcdEncrypted: false
controlPlaneConfig:
  desiredCount: 1
  systemReservedMemory: "1Gi"
  infra:
    preKubeadmCommands:
      - "mkdir -p /run/cluster-api"
      - "echo success > /run/cluster-api/bootstrap-success.complete"
    machineTemplate:
      apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
      kind: DockerMachineTemplate
      spec:
        template:
          spec:
            customImage: ${CAPD_NODE_IMAGE}
            extraMounts:
              - containerPath: /var/run/docker.sock
                hostPath: /var/run/docker.sock
infra:
  cluster:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: DockerCluster
    metadata:
      name: ${NVKECTL_SMOKE_WRITE_CLUSTER}
    spec: {}
EOF
}

# Create, list, duplicate-reject, delete, with the node pool round trip nested
# inside it. Runs after the suite, never during it, and removes its own cluster
# before returning, so the cleanup gate that counts tenant clusters still sees
# none.
#
# The control plane is a cluster-level controlPlaneConfig, which has no way to
# hold provisioning back, so the controller starts building a real control-plane node as
# soon as the cluster exists.
nvkectl_smoke_write_round_trip() {
  local out rc=0

  if [[ "${NKE_E2E_NVKECTL_SMOKE_WRITE:-true}" != "true" ]]; then
    log "nvkectl smoke: SKIP the write round trip, NKE_E2E_NVKECTL_SMOKE_WRITE is not true"
    return 0
  fi
  # CAPD only because no NicoCluster spec has been written for the smoke yet.
  # The seed IDs are constants (nicoSiteID in tenant_test.go), not plumbing.
  if [[ "${BACKEND:-}" != "capd" ]]; then
    log "nvkectl smoke: SKIP the write round trip on the ${BACKEND:-unset} backend"
    return 0
  fi

  nvkectl_smoke_write_spec | nvkectl_smoke_stage_spec write || return 1

  if ! out="$(nvkectl_exec create cluster -f "$(nvkectl_smoke_spec_path write)" 2>&1)"; then
    log "nvkectl smoke: 'create cluster' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'created (id: ' <<<"${out}"; then
    log "nvkectl smoke: 'create cluster' succeeded without reporting the id: ${out}" >&2
    nvkectl_smoke_write_cleanup
    return 1
  fi
  # A successful write prints its trace ID on stderr, which this captures with
  # stdout.
  if ! grep -Eq '^Trace ID: [0-9a-f]{32}$' <<<"${out}"; then
    log "nvkectl smoke: 'create cluster' succeeded without a Trace ID line: ${out}" >&2
    nvkectl_smoke_write_cleanup
    return 1
  fi
  log "nvkectl smoke: ${out}"

  # The cluster exists from here: delete it on any failure.
  nvkectl_smoke_write_assertions || rc=$?
  if ((rc != 0)); then
    nvkectl_smoke_write_cleanup
    return "${rc}"
  fi
  log "PASS: nvkectl created, listed, patched and deleted a cluster through the controller"
}

# Best-effort delete; never fails the caller.
nvkectl_smoke_write_cleanup() {
  nvkectl_exec delete cluster "${NVKECTL_SMOKE_WRITE_CLUSTER}" >/dev/null 2>&1 || true
}

nvkectl_smoke_write_assertions() {
  local out name="${NVKECTL_SMOKE_WRITE_CLUSTER}"

  # A row, not just a header.
  if ! out="$(nvkectl_exec get clusters 2>&1)"; then
    log "nvkectl smoke: 'get clusters' failed after the create: ${out}" >&2
    return 1
  fi
  if ! grep -qE "^${name}[[:space:]]" <<<"${out}"; then
    log "nvkectl smoke: 'get clusters' printed no row for ${name}: ${out}" >&2
    return 1
  fi

  # Exactly one element, ours.
  if ! out="$(nvkectl_exec get clusters "${name}" -o json 2>&1)"; then
    log "nvkectl smoke: 'get clusters ${name} -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e --arg n "${name}" 'length == 1 and .[0].name == $n' <<<"${out}" >/dev/null; then
    log "nvkectl smoke: 'get clusters ${name} -o json' did not return exactly our cluster: ${out}" >&2
    return 1
  fi

  # PATCH an ungated field. teleportEnabled has no provisioning-state gate, so
  # this exercises the patch path (FindClusterByName -> PatchCluster) even
  # though the write round trip never provisions a real node.
  if ! out="$(nvkectl_exec patch cluster "${name}" --patch 'teleportEnabled: true' 2>&1)"; then
    log "nvkectl smoke: 'patch cluster ${name}' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'patched (id: ' <<<"${out}"; then
    log "nvkectl smoke: 'patch cluster ${name}' succeeded without reporting the id: ${out}" >&2
    return 1
  fi

  if ! out="$(nvkectl_exec get clusters "${name}" -o json 2>&1)"; then
    log "nvkectl smoke: 'get clusters ${name} -o json' failed after patch: ${out}" >&2
    return 1
  fi
  if ! jq -e '.[0].teleportEnabled == true' <<<"${out}" >/dev/null; then
    log "nvkectl smoke: 'patch cluster' did not apply teleportEnabled=true: ${out}" >&2
    return 1
  fi

  # user-flow.md steps 9 and 11 document --patch-file beside --patch. It takes
  # a different branch: a file read rather than an inline document.
  nvkectl_smoke_stage_spec patch <<'EOF' || return 1
teleportEnabled: false
EOF
  if ! out="$(nvkectl_exec patch cluster "${name}" \
    --patch-file "$(nvkectl_smoke_spec_path patch)" 2>&1)"; then
    log "nvkectl smoke: 'patch cluster --patch-file' failed: ${out}" >&2
    return 1
  fi
  if ! out="$(nvkectl_exec get clusters "${name}" -o json 2>&1)"; then
    log "nvkectl smoke: 'get clusters ${name} -o json' failed after --patch-file: ${out}" >&2
    return 1
  fi
  if ! jq -e '.[0].teleportEnabled == false' <<<"${out}" >/dev/null; then
    log "nvkectl smoke: '--patch-file' did not apply teleportEnabled=false: ${out}" >&2
    return 1
  fi

  # A real cluster name, so this resolves further than the absent-name case. The
  # control plane is really provisioning, so the Secret appears once it is up:
  # either a kubeconfig or the missing-Secret message is correct, depending on
  # timing.
  if out="$(nvkectl_exec get kubeconfig "${name}" 2>&1)"; then
    if ! grep -q 'kind: Config' <<<"${out}"; then
      log "nvkectl smoke: 'get kubeconfig ${name}' succeeded without printing a kubeconfig" >&2
      return 1
    fi
    log "nvkectl smoke: ${name}'s control plane was already up; 'get kubeconfig' returned its kubeconfig"
  elif ! grep -Eq "(nvke|nke)-system/${name}-admin-kubeconfig" <<<"${out}"; then
    log "nvkectl smoke: 'get kubeconfig ${name}' named the wrong secret: ${out}" >&2
    return 1
  elif ! grep -q 'not be provisioned yet' <<<"${out}"; then
    log "nvkectl smoke: 'get kubeconfig ${name}' failed for some reason other than the missing Secret: ${out}" >&2
    return 1
  fi

  nvkectl_smoke_nodepool_round_trip || return 1

  # Only passes if the first create committed.
  if out="$(nvkectl_exec create cluster -f "$(nvkectl_smoke_spec_path write)" 2>&1)"; then
    log "nvkectl smoke: a second 'create cluster' of ${name} was accepted: ${out}" >&2
    return 1
  fi
  if ! grep -q 'already exists' <<<"${out}"; then
    log "nvkectl smoke: the duplicate create failed for a reason other than the name: ${out}" >&2
    return 1
  fi

  if ! out="$(nvkectl_exec delete cluster "${name}" 2>&1)"; then
    log "nvkectl smoke: 'delete cluster ${name}' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'deletion started' <<<"${out}"; then
    log "nvkectl smoke: 'delete cluster ${name}' succeeded without reporting it: ${out}" >&2
    return 1
  fi

  if out="$(nvkectl_exec delete cluster "${NVKECTL_SMOKE_ABSENT}" 2>&1)"; then
    log "nvkectl smoke: 'delete cluster ${NVKECTL_SMOKE_ABSENT}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nvkectl smoke: 'delete cluster ${NVKECTL_SMOKE_ABSENT}' failed without a not-found message: ${out}" >&2
    return 1
  fi

  # Teardown is asynchronous, and it removes a real control-plane node.
  local attempt
  for ((attempt = 1; attempt <= NVKECTL_SMOKE_CLUSTER_DELETE_POLLS; attempt++)); do
    if out="$(nvkectl_exec get clusters "${name}" 2>&1)"; then
      sleep "${NVKECTL_SMOKE_DELETE_POLL_SECONDS}"
      continue
    fi
    if grep -q 'not found' <<<"${out}"; then
      log "nvkectl smoke: ${name} was gone after ${attempt} polls"
      return 0
    fi
    log "nvkectl smoke: polling for ${name} failed for a reason other than not-found: ${out}" >&2
    return 1
  done
  log "nvkectl smoke: ${name} was still listed after ${NVKECTL_SMOKE_CLUSTER_DELETE_POLLS} polls" >&2
  return 1
}

# Create, list, filter, patch and delete one worker pool on the cluster the
# write round trip just created. Called with that cluster alive, and its
# failures unwind through the caller's cleanup.
nvkectl_smoke_nodepool_round_trip() {
  local out cluster_id cluster="${NVKECTL_SMOKE_WRITE_CLUSTER}" pool="${NVKECTL_SMOKE_WRITE_NODEPOOL}"

  # CreateNodePoolRequest carries clusterID, not a cluster name.
  if ! out="$(nvkectl_exec get clusters "${cluster}" -o json 2>&1)"; then
    log "nvkectl smoke: 'get clusters ${cluster} -o json' failed before the node pool round trip: ${out}" >&2
    return 1
  fi
  cluster_id="$(jq -r '.[0].id // empty' <<<"${out}")"
  if [[ -z "${cluster_id}" ]]; then
    log "nvkectl smoke: no cluster id for ${cluster}: ${out}" >&2
    return 1
  fi

  nvkectl_smoke_stage_spec pool <<EOF || return 1
clusterID: ${cluster_id}
name: ${pool}
nodeNamePrefix: pool
desiredNodeCount: 1
controlPlane: false
systemReservedMemory: "1Gi"
autoProvisionDisabled: true
autoModeDisabled: false
recycleBudget: 0
infra:
  machineTemplate:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: DockerMachineTemplate
    spec:
      template:
        spec:
          customImage: ${CAPD_NODE_IMAGE}
EOF

  if ! out="$(nvkectl_exec create nodepool -f "$(nvkectl_smoke_spec_path pool)" 2>&1)"; then
    log "nvkectl smoke: 'create nodepool' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'created (id: ' <<<"${out}"; then
    log "nvkectl smoke: 'create nodepool' succeeded without reporting the id: ${out}" >&2
    return 1
  fi

  # A row, not just a header.
  if ! out="$(nvkectl_exec get nodepools 2>&1)"; then
    log "nvkectl smoke: 'get nodepools' failed after the create: ${out}" >&2
    return 1
  fi
  if ! grep -q 'K8S VERSION' <<<"${out}"; then
    log "nvkectl smoke: 'get nodepools' printed no table header: ${out}" >&2
    return 1
  fi
  if ! grep -qE "^${pool}[[:space:]]" <<<"${out}"; then
    log "nvkectl smoke: 'get nodepools' printed no row for ${pool}: ${out}" >&2
    return 1
  fi

  # Filter by cluster: the cluster's own pools and nothing belonging to another
  # cluster. The control plane has no pool, so ours is the only one.
  if ! out="$(nvkectl_exec get nodepools --cluster "${cluster}" -o json 2>&1)"; then
    log "nvkectl smoke: 'get nodepools --cluster ${cluster} -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e --arg p "${pool}" --arg id "${cluster_id}" \
    'all(.clusterID == $id) and ([.[].name] == [$p])' \
    <<<"${out}" >/dev/null; then
    log "nvkectl smoke: 'get nodepools --cluster ${cluster}' did not return exactly that cluster's pools: ${out}" >&2
    return 1
  fi

  # Filter by name within the cluster.
  if ! out="$(nvkectl_exec get nodepools "${pool}" --cluster "${cluster}" -o json 2>&1)"; then
    log "nvkectl smoke: 'get nodepools ${pool} --cluster ${cluster} -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e --arg p "${pool}" 'length == 1 and .[0].name == $p' <<<"${out}" >/dev/null; then
    log "nvkectl smoke: 'get nodepools ${pool} --cluster ${cluster}' did not return exactly our pool: ${out}" >&2
    return 1
  fi

  # Filter by name alone: the ?name= query without ?clusterName=.
  if ! out="$(nvkectl_exec get nodepools "${pool}" -o json 2>&1)"; then
    log "nvkectl smoke: 'get nodepools ${pool} -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e --arg id "${cluster_id}" 'length == 1 and .[0].clusterID == $id' <<<"${out}" >/dev/null; then
    log "nvkectl smoke: 'get nodepools ${pool}' did not return exactly our pool: ${out}" >&2
    return 1
  fi

  nvkectl_smoke_nodepool_scale 2 || return 1
  # Scaling down is a desiredNodeCount change and nothing more: the node set
  # controller deletes surplus *pending* nodes and deliberately leaves running
  # ones alone (internal/controllers/nodeset/controller.go), so the field is the
  # only thing there is to assert.
  nvkectl_smoke_nodepool_scale 0 || return 1

  # Only passes if the first create committed.
  if out="$(nvkectl_exec create nodepool -f "$(nvkectl_smoke_spec_path pool)" 2>&1)"; then
    log "nvkectl smoke: a second 'create nodepool' of ${pool} was accepted: ${out}" >&2
    return 1
  fi
  if ! grep -q 'already exists' <<<"${out}"; then
    log "nvkectl smoke: the duplicate node pool create failed for a reason other than the name: ${out}" >&2
    return 1
  fi

  if ! out="$(nvkectl_exec delete nodepool "${pool}" --cluster "${cluster}" 2>&1)"; then
    log "nvkectl smoke: 'delete nodepool ${pool}' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'deletion started (id: ' <<<"${out}"; then
    log "nvkectl smoke: 'delete nodepool ${pool}' succeeded without reporting it: ${out}" >&2
    return 1
  fi

  # Teardown is asynchronous.
  local attempt
  for ((attempt = 1; attempt <= NVKECTL_SMOKE_DELETE_POLLS; attempt++)); do
    if out="$(nvkectl_exec get nodepools "${pool}" --cluster "${cluster}" 2>&1)"; then
      sleep "${NVKECTL_SMOKE_DELETE_POLL_SECONDS}"
      continue
    fi
    if grep -q 'not found' <<<"${out}"; then
      log "nvkectl smoke: node pool ${pool} was gone after ${attempt} polls"
      log "PASS: nvkectl created, listed, filtered, patched and deleted a node pool through the controller"
      return 0
    fi
    log "nvkectl smoke: polling for node pool ${pool} failed for a reason other than not-found: ${out}" >&2
    return 1
  done
  log "nvkectl smoke: node pool ${pool} was still listed after ${NVKECTL_SMOKE_DELETE_POLLS} polls" >&2
  return 1
}

# Patch desiredNodeCount to $1 and assert the controller stored it.
nvkectl_smoke_nodepool_scale() {
  local want="$1" out cluster="${NVKECTL_SMOKE_WRITE_CLUSTER}" pool="${NVKECTL_SMOKE_WRITE_NODEPOOL}"

  if ! out="$(nvkectl_exec patch nodepool "${pool}" --cluster "${cluster}" \
    --patch "desiredNodeCount: ${want}" 2>&1)"; then
    log "nvkectl smoke: 'patch nodepool ${pool}' to ${want} failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'patched (id: ' <<<"${out}"; then
    log "nvkectl smoke: 'patch nodepool ${pool}' succeeded without reporting the id: ${out}" >&2
    return 1
  fi

  if ! out="$(nvkectl_exec get nodepools "${pool}" --cluster "${cluster}" -o json 2>&1)"; then
    log "nvkectl smoke: 'get nodepools ${pool} -o json' failed after the patch: ${out}" >&2
    return 1
  fi
  if ! jq -e --argjson n "${want}" '.[0].desiredNodeCount == $n' <<<"${out}" >/dev/null; then
    log "nvkectl smoke: 'patch nodepool' did not apply desiredNodeCount=${want}: ${out}" >&2
    return 1
  fi
}
