#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Drive nkectl against the CWE a backend E2E lane already deployed. Source it
# from ci/backend-e2e.sh; do not run it.
#
# The Ginkgo suite proves the server. This proves the CLI can talk to it: read
# commands and their negatives, provider inspection, rejected creates, and one
# create, list, patch, delete round trip over a cluster and a node pool.
#
# Needs from the caller: log, runner_exec, RUNNER_NAME, NKECTL_BIN, TMP_DIR,
# BACKEND, CAPD_NODE_IMAGE.

# The CWE service the suite deploys.
NKECTL_SMOKE_CWE_NAMESPACE=cloud-workflow-engine
NKECTL_SMOKE_CWE_SERVICE=cloud-workflow-engine
NKECTL_SMOKE_CWE_PORT=8080
# A name no lane creates. Used by every negative assertion, so a rename here
# cannot leave one of them silently matching a real object.
NKECTL_SMOKE_ABSENT=nkectl-smoke-absent
# Name the rejection bodies carry; asserted absent afterwards.
NKECTL_SMOKE_REJECT_CLUSTER=nkectl-smoke-reject
# The suite's default; create validation checks only the semver shape.
NKECTL_SMOKE_KUBERNETES_VERSION="${NKECTL_SMOKE_KUBERNETES_VERSION:-1.34.2}"
# Name the round trip creates and deletes.
NKECTL_SMOKE_WRITE_CLUSTER=nkectl-smoke-write
# The worker pool the round trip adds to that cluster, then removes.
NKECTL_SMOKE_WRITE_NODEPOOL=nkectl-smoke-pool
# Delete wait: this many polls, this many seconds apart.
NKECTL_SMOKE_DELETE_POLLS="${NKECTL_SMOKE_DELETE_POLLS:-60}"
NKECTL_SMOKE_DELETE_POLL_SECONDS="${NKECTL_SMOKE_DELETE_POLL_SECONDS:-5}"

nkectl_smoke_build() {
  log "compiling nkectl for linux/${1}"
  (cd ../cli && CGO_ENABLED=0 GOOS=linux GOARCH="${1}" go build -o "${NKECTL_BIN}" ./cmd/nkectl)
}

nkectl_smoke_stage() {
  docker cp "${NKECTL_BIN}" "${RUNNER_NAME}:/usr/local/bin/nkectl"
}

# Every call is bounded. Nothing here had a timeout, and a hang does not just
# stall: the job cap sends SIGTERM, handle_signal sets SKIP_DIAGNOSTICS, and the
# lane loses its whole diagnostics artifact to one stuck CLI call.
NKECTL_SMOKE_TIMEOUT="${NKECTL_SMOKE_TIMEOUT:-60}"

nkectl_exec() {
  timeout "${NKECTL_SMOKE_TIMEOUT}" docker exec \
    -e KUBECONFIG=/state/management.kubeconfig \
    "${RUNNER_NAME}" nkectl "$@"
}

nkectl_no_auth_exec() {
  timeout "${NKECTL_SMOKE_TIMEOUT}" docker exec \
    -e XDG_CONFIG_HOME=/tmp/nkectl-no-auth \
    "${RUNNER_NAME}" nkectl "$@"
}

# Port-forward rather than the apiserver service proxy: the proxy does not
# forward a bearer token to CWE, which is why wait_for_zero_tenant_clusters
# counts CAPI objects instead of calling the API.
# Called bare, not as `nkectl_smoke || exit 1`: bash disables errexit for a
# whole function body when the call is the left side of ||, which made every
# unguarded command here unable to fail the lane. Keep the call sites in
# ci/backend-e2e.sh bare, and keep each assertion an explicit `return 1`.
nkectl_smoke() {
  local no_auth_config out rc

  docker exec -d -e KUBECONFIG=/state/management.kubeconfig "${RUNNER_NAME}" \
    kubectl -n "${NKECTL_SMOKE_CWE_NAMESPACE}" port-forward \
    "svc/${NKECTL_SMOKE_CWE_SERVICE}" "${NKECTL_SMOKE_CWE_PORT}:${NKECTL_SMOKE_CWE_PORT}"

  runner_exec nkectl config set-site --name e2e \
    --url "http://127.0.0.1:${NKECTL_SMOKE_CWE_PORT}" \
    --kubeconfig /state/management.kubeconfig >/dev/null

  # A separate site with no kubeconfig exercises the no-credential path. The
  # normal site above exercises token minting through the management cluster.
  no_auth_config="${TMP_DIR}/nkectl-no-auth-config.yaml"
  cat >"${no_auth_config}" <<EOF
sites:
  e2e:
    url: http://127.0.0.1:${NKECTL_SMOKE_CWE_PORT}
    kubeconfig: ""
current-site: e2e
EOF
  runner_exec mkdir -p /tmp/nkectl-no-auth/nkectl
  docker cp "${no_auth_config}" \
    "${RUNNER_NAME}:/tmp/nkectl-no-auth/nkectl/config.yaml" >/dev/null

  # Doubles as the readiness wait for the forward.
  rc=1
  for _ in $(seq 1 15); do
    if out="$(nkectl_exec get clusters 2>&1)"; then
      rc=0
      break
    fi
    sleep 2
  done
  if ((rc != 0)); then
    log "nkectl smoke: 'get clusters' never succeeded: ${out}" >&2
    return 1
  fi
  if ! grep -q 'KUBERNETES VERSION' <<<"${out}"; then
    log "nkectl smoke: 'get clusters' printed no table header: ${out}" >&2
    return 1
  fi

  # Control: without this, a CWE that stopped enforcing auth would leave the
  # check above green and prove nothing about the token.
  if [[ "${NKE_E2E_REQUIRE_AUTHN:-}" == "true" ]]; then
    if out="$(nkectl_no_auth_exec get clusters 2>&1)"; then
      log "nkectl smoke: 'get clusters' succeeded with no token, so the run above proves nothing: ${out}" >&2
      return 1
    fi
  else
    log "nkectl smoke: SKIP the no-token control, NKE_E2E_REQUIRE_AUTHN is not true"
  fi

  if ! out="$(nkectl_exec get clusters -o json 2>&1)"; then
    log "nkectl smoke: 'get clusters -o json' failed: ${out}" >&2
    return 1
  fi
  case "${out}" in
    '['*|'{'*|null*) ;;
    *) log "nkectl smoke: -o json did not emit JSON: ${out}" >&2; return 1 ;;
  esac

  # Read-path negatives. Each asserts the message, not just a non-zero exit: an
  # exit code alone cannot tell "the lookup ran and found nothing" apart from
  # "the request never left the client".

  # Exercises the ?name= filter, which the calls above never hit.
  if out="$(nkectl_exec get clusters "${NKECTL_SMOKE_ABSENT}" 2>&1)"; then
    log "nkectl smoke: 'get clusters ${NKECTL_SMOKE_ABSENT}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nkectl smoke: 'get clusters <name>' failed without a not-found message" >&2
    return 1
  fi

  # The same FindClusterByName lookup patch cluster shares with delete.
  if out="$(nkectl_exec patch cluster "${NKECTL_SMOKE_ABSENT}" --patch 'teleportEnabled: true' 2>&1)"; then
    log "nkectl smoke: 'patch cluster ${NKECTL_SMOKE_ABSENT}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nkectl smoke: 'patch cluster' failed without a not-found message" >&2
    return 1
  fi

  # Rejected by the printer before any request is issued.
  if out="$(nkectl_exec get clusters -o bogus 2>&1)"; then
    log "nkectl smoke: 'get clusters -o bogus' was accepted" >&2
    return 1
  fi
  if ! grep -qi 'output format' <<<"${out}"; then
    log "nkectl smoke: '-o bogus' failed for some reason other than the format" >&2
    return 1
  fi

  nkectl_smoke_nodepool_negatives || return 1
  nkectl_smoke_providers || return 1
  nkectl_smoke_create_rejections || return 1
  nkectl_smoke_write_round_trip || return 1

  log "PASS: nkectl reached CWE, authenticated, listed clusters and node pools, read providers, and had malformed creates rejected"
}

# Node pool read-path negatives, asserting the message like the cluster ones
# above. A node pool command resolves a name through CWE before it acts, so an
# exit code alone cannot tell a lookup that found nothing apart from a flag the
# CLI rejected on its own.
nkectl_smoke_nodepool_negatives() {
  local out absent="${NKECTL_SMOKE_ABSENT}"

  if out="$(nkectl_exec get nodepools "${absent}" 2>&1)"; then
    log "nkectl smoke: 'get nodepools ${absent}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nkectl smoke: 'get nodepools <name>' failed without a not-found message: ${out}" >&2
    return 1
  fi

  # --cluster is resolved through FindClusterByName before any pool is listed.
  if out="$(nkectl_exec get nodepools --cluster "${absent}" 2>&1)"; then
    log "nkectl smoke: 'get nodepools --cluster ${absent}' succeeded against a cluster that does not exist" >&2
    return 1
  fi
  if ! grep -q "cluster \"${absent}\" not found" <<<"${out}"; then
    log "nkectl smoke: 'get nodepools --cluster <name>' failed without naming the missing cluster: ${out}" >&2
    return 1
  fi

  # The FindNodePoolByName lookup patch and delete share.
  if out="$(nkectl_exec patch nodepool "${absent}" --cluster "${absent}" --patch 'desiredNodeCount: 1' 2>&1)"; then
    log "nkectl smoke: 'patch nodepool ${absent}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q "node pool \"${absent}\" not found in cluster \"${absent}\"" <<<"${out}"; then
    log "nkectl smoke: 'patch nodepool' failed without a not-found message: ${out}" >&2
    return 1
  fi

  if out="$(nkectl_exec delete nodepool "${absent}" --cluster "${absent}" 2>&1)"; then
    log "nkectl smoke: 'delete nodepool ${absent}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q "node pool \"${absent}\" not found in cluster \"${absent}\"" <<<"${out}"; then
    log "nkectl smoke: 'delete nodepool' failed without a not-found message: ${out}" >&2
    return 1
  fi

  # Pool names are unique per cluster, not per site, so --cluster is required.
  if out="$(nkectl_exec delete nodepool "${absent}" 2>&1)"; then
    log "nkectl smoke: 'delete nodepool ${absent}' was accepted with no --cluster" >&2
    return 1
  fi
  if ! grep -q 'required flag(s) "cluster" not set' <<<"${out}"; then
    log "nkectl smoke: 'delete nodepool' with no --cluster failed for some other reason: ${out}" >&2
    return 1
  fi

  # Rejected by the patch validator before any request is issued: teleportEnabled
  # is a real UpdateNodePoolRequest field that nkectl does not expose.
  if out="$(nkectl_exec patch nodepool "${absent}" --cluster "${absent}" --patch 'teleportEnabled: true' 2>&1)"; then
    log "nkectl smoke: 'patch nodepool --patch teleportEnabled' was accepted" >&2
    return 1
  fi
  if ! grep -q 'nkectl can set desiredNodeCount' <<<"${out}"; then
    log "nkectl smoke: an unsupported patch field failed for some reason other than the field: ${out}" >&2
    return 1
  fi
}

# `get providers` reads the site kubeconfig through kubectl rather than the CWE
# REST client, so it is the only smoke assertion covering that subsystem. Both
# lanes stage CAPNICo and wait for its CRDs (ci/backend-e2e.sh, needs-capnico-source),
# so crdsEstablished holds on CAPD too; only the NICo lane deploys the manager
# and the credentials Secret.
nkectl_smoke_providers() {
  local out types

  if ! out="$(nkectl_exec get providers -o json 2>&1)"; then
    log "nkectl smoke: 'get providers -o json' failed" >&2
    return 1
  fi

  types="$(jq -r '[.[].type] | join(",")' <<<"${out}" 2>/dev/null || true)"
  if [[ "${types}" != "nico,launchlayer" ]]; then
    log "nkectl smoke: 'get providers' listed [${types}], want [nico,launchlayer]" >&2
    return 1
  fi

  # The load-bearing assertion: it fails if the CRD names drift from what the
  # provider ships, or if the kubeconfig plumbing breaks. Deliberately NOT a
  # check for state == CONFIGURED -- managerDeployments selects on
  # app.kubernetes.io/name, a label owned by the provider repository.
  if [[ "$(jq -r '.[] | select(.type=="nico") | .crdsEstablished' <<<"${out}")" != "true" ]]; then
    log "nkectl smoke: nico provider reports crdsEstablished=false" >&2
    return 1
  fi

  # CAPLL is deployed by neither lane, so this pins the not-installed rendering.
  if [[ "$(jq -r '.[] | select(.type=="launchlayer") | .state' <<<"${out}")" != "NOT-INSTALLED" ]]; then
    log "nkectl smoke: launchlayer is not NOT-INSTALLED, so this lane changed shape" >&2
    return 1
  fi

  if [[ "${NKE_E2E_BACKEND:-}" == "nico" ]]; then
    if [[ "$(jq -r '.[] | select(.type=="nico") | .secretPresent' <<<"${out}")" != "true" ]]; then
      log "nkectl smoke: nico provider reports secretPresent=false on the NICo lane" >&2
      return 1
    fi
  fi

  if ! out="$(nkectl_exec get providers 2>&1)"; then
    log "nkectl smoke: 'get providers' table mode failed" >&2
    return 1
  fi
  if ! grep -q 'PROVIDER' <<<"${out}"; then
    log "nkectl smoke: 'get providers' printed no table header" >&2
    return 1
  fi

  # A site that is not configured must fail before any cluster contact.
  if out="$(nkectl_exec --site "${NKECTL_SMOKE_ABSENT}" get providers 2>&1)"; then
    log "nkectl smoke: '--site ${NKECTL_SMOKE_ABSENT}' was accepted" >&2
    return 1
  fi
}

# Where a staged spec lands in the runner container.
nkectl_smoke_spec_path() {
  echo "/tmp/nkectl-smoke-$1.yaml"
}

# Stage the YAML on stdin into the runner. nkectl_exec has no -i, so
# `create cluster -f -` cannot read stdin.
nkectl_smoke_stage_spec() {
  local name="$1"
  local host_path="${TMP_DIR}/nkectl-smoke-${name}.yaml"
  cat >"${host_path}" || return 1
  docker cp "${host_path}" "${RUNNER_NAME}:$(nkectl_smoke_spec_path "${name}")" >/dev/null
}

# Bodies the OpenAPI validator rejects before any handler runs: no row, no
# workflow. Each is valid except for the one fault it names.
nkectl_smoke_create_rejections() {
  local out

  nkectl_smoke_stage_spec reject-name <<EOF || return 1
name: Nkectl_Smoke
controlPlaneKubernetesVersion: ${NKECTL_SMOKE_KUBERNETES_VERSION}
cniPlugin: calico
infra:
  cluster:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: DockerCluster
    metadata:
      name: ${NKECTL_SMOKE_REJECT_CLUSTER}
    spec: {}
EOF
  nkectl_smoke_expect_create_rejected reject-name "a name that violates the DNS-label pattern" || return 1

  nkectl_smoke_stage_spec reject-version <<EOF || return 1
name: ${NKECTL_SMOKE_REJECT_CLUSTER}
cniPlugin: calico
infra:
  cluster:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: DockerCluster
    metadata:
      name: ${NKECTL_SMOKE_REJECT_CLUSTER}
    spec: {}
EOF
  nkectl_smoke_expect_create_rejected reject-version "no controlPlaneKubernetesVersion" || return 1

  nkectl_smoke_stage_spec reject-provider <<EOF || return 1
name: ${NKECTL_SMOKE_REJECT_CLUSTER}
controlPlaneKubernetesVersion: ${NKECTL_SMOKE_KUBERNETES_VERSION}
cniPlugin: calico
EOF
  nkectl_smoke_expect_create_rejected reject-provider "neither nico nor infra" || return 1

  # Zero state: a rejected create must leave nothing to find by name.
  if out="$(nkectl_exec get clusters "${NKECTL_SMOKE_REJECT_CLUSTER}" 2>&1)"; then
    log "nkectl smoke: a rejected create left cluster ${NKECTL_SMOKE_REJECT_CLUSTER} behind: ${out}" >&2
    return 1
  fi
}

# Assert the status, not the message: the text is the middleware's, and a 400
# is the only proof the request reached CWE rather than dying in the CLI.
nkectl_smoke_expect_create_rejected() {
  local spec="$1" what="$2" out
  if out="$(nkectl_exec create cluster -f "$(nkectl_smoke_spec_path "${spec}")" 2>&1)"; then
    log "nkectl smoke: 'create cluster' with ${what} was accepted: ${out}" >&2
    return 1
  fi
  if ! grep -q '400' <<<"${out}"; then
    log "nkectl smoke: 'create cluster' with ${what} failed, but not with a 400 from CWE: ${out}" >&2
    return 1
  fi
}

# Create, list, duplicate-reject, delete, with the node pool round trip nested
# inside it. One control-plane pool with autoProvisionDisabled passes create
# validation and schedules no node, so CWE writes only the CAPI Cluster and
# DockerCluster. Runs after the last leg: the gate between legs counts tenant
# clusters.
nkectl_smoke_write_round_trip() {
  local out rc=0

  if [[ "${NKE_E2E_NKECTL_SMOKE_WRITE:-true}" != "true" ]]; then
    log "nkectl smoke: SKIP the write round trip, NKE_E2E_NKECTL_SMOKE_WRITE is not true"
    return 0
  fi
  # Only CAPD can take a DockerCluster spec.
  if [[ "${BACKEND:-}" != "capd" ]]; then
    log "nkectl smoke: SKIP the write round trip on the ${BACKEND:-unset} backend"
    return 0
  fi

  nkectl_smoke_stage_spec write <<EOF || return 1
name: ${NKECTL_SMOKE_WRITE_CLUSTER}
controlPlaneKubernetesVersion: ${NKECTL_SMOKE_KUBERNETES_VERSION}
cniPlugin: calico
etcdEncrypted: false
nodePools:
  - name: control-plane
    nodeNamePrefix: cp
    desiredNodeCount: 1
    controlPlane: true
    systemReservedMemory: "1Gi"
    autoProvisionDisabled: true
    autoModeDisabled: false
    recycleBudget: 0
    infra:
      spireAttestationMethod: joinToken
      machineTemplate:
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
        kind: DockerMachineTemplate
        spec:
          template:
            spec:
              customImage: ${CAPD_NODE_IMAGE}
infra:
  cluster:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: DockerCluster
    metadata:
      name: ${NKECTL_SMOKE_WRITE_CLUSTER}
    spec: {}
EOF

  if ! out="$(nkectl_exec create cluster -f "$(nkectl_smoke_spec_path write)" 2>&1)"; then
    log "nkectl smoke: 'create cluster' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'created (id: ' <<<"${out}"; then
    log "nkectl smoke: 'create cluster' succeeded without reporting the id: ${out}" >&2
    nkectl_smoke_write_cleanup
    return 1
  fi
  log "nkectl smoke: ${out}"

  # The cluster exists from here: delete it on any failure.
  nkectl_smoke_write_assertions || rc=$?
  if ((rc != 0)); then
    nkectl_smoke_write_cleanup
    return "${rc}"
  fi
  log "PASS: nkectl created, listed, patched and deleted a cluster through CWE"
}

# Best-effort delete; never fails the caller.
nkectl_smoke_write_cleanup() {
  nkectl_exec delete cluster "${NKECTL_SMOKE_WRITE_CLUSTER}" >/dev/null 2>&1 || true
}

nkectl_smoke_write_assertions() {
  local out name="${NKECTL_SMOKE_WRITE_CLUSTER}"

  # A row, not just a header.
  if ! out="$(nkectl_exec get clusters 2>&1)"; then
    log "nkectl smoke: 'get clusters' failed after the create: ${out}" >&2
    return 1
  fi
  if ! grep -qE "^${name}[[:space:]]" <<<"${out}"; then
    log "nkectl smoke: 'get clusters' printed no row for ${name}: ${out}" >&2
    return 1
  fi

  # Exactly one element, ours.
  if ! out="$(nkectl_exec get clusters "${name}" -o json 2>&1)"; then
    log "nkectl smoke: 'get clusters ${name} -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e --arg n "${name}" 'length == 1 and .[0].name == $n' <<<"${out}" >/dev/null; then
    log "nkectl smoke: 'get clusters ${name} -o json' did not return exactly our cluster: ${out}" >&2
    return 1
  fi

  # PATCH an ungated field. teleportEnabled has no provisioning-state gate, so
  # this exercises the patch path (FindClusterByName -> PatchCluster) even
  # though the write round trip never provisions a real node.
  if ! out="$(nkectl_exec patch cluster "${name}" --patch 'teleportEnabled: true' 2>&1)"; then
    log "nkectl smoke: 'patch cluster ${name}' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'patched (id: ' <<<"${out}"; then
    log "nkectl smoke: 'patch cluster ${name}' succeeded without reporting the id: ${out}" >&2
    return 1
  fi

  if ! out="$(nkectl_exec get clusters "${name}" -o json 2>&1)"; then
    log "nkectl smoke: 'get clusters ${name} -o json' failed after patch: ${out}" >&2
    return 1
  fi
  if ! jq -e '.[0].teleportEnabled == true' <<<"${out}" >/dev/null; then
    log "nkectl smoke: 'patch cluster' did not apply teleportEnabled=true: ${out}" >&2
    return 1
  fi

  nkectl_smoke_nodepool_round_trip || return 1

  # Only passes if the first create committed.
  if out="$(nkectl_exec create cluster -f "$(nkectl_smoke_spec_path write)" 2>&1)"; then
    log "nkectl smoke: a second 'create cluster' of ${name} was accepted: ${out}" >&2
    return 1
  fi
  if ! grep -q 'already exists' <<<"${out}"; then
    log "nkectl smoke: the duplicate create failed for a reason other than the name: ${out}" >&2
    return 1
  fi

  if ! out="$(nkectl_exec delete cluster "${name}" 2>&1)"; then
    log "nkectl smoke: 'delete cluster ${name}' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'deletion started' <<<"${out}"; then
    log "nkectl smoke: 'delete cluster ${name}' succeeded without reporting it: ${out}" >&2
    return 1
  fi

  if out="$(nkectl_exec delete cluster "${NKECTL_SMOKE_ABSENT}" 2>&1)"; then
    log "nkectl smoke: 'delete cluster ${NKECTL_SMOKE_ABSENT}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nkectl smoke: 'delete cluster ${NKECTL_SMOKE_ABSENT}' failed without a not-found message: ${out}" >&2
    return 1
  fi

  # Teardown is asynchronous.
  local attempt
  for ((attempt = 1; attempt <= NKECTL_SMOKE_DELETE_POLLS; attempt++)); do
    if out="$(nkectl_exec get clusters "${name}" 2>&1)"; then
      sleep "${NKECTL_SMOKE_DELETE_POLL_SECONDS}"
      continue
    fi
    if grep -q 'not found' <<<"${out}"; then
      log "nkectl smoke: ${name} was gone after ${attempt} polls"
      return 0
    fi
    log "nkectl smoke: polling for ${name} failed for a reason other than not-found: ${out}" >&2
    return 1
  done
  log "nkectl smoke: ${name} was still listed after ${NKECTL_SMOKE_DELETE_POLLS} polls" >&2
  return 1
}

# Create, list, filter, patch and delete one worker pool on the cluster the
# write round trip just created. Called with that cluster alive, and its
# failures unwind through the caller's cleanup.
nkectl_smoke_nodepool_round_trip() {
  local out cluster_id cluster="${NKECTL_SMOKE_WRITE_CLUSTER}" pool="${NKECTL_SMOKE_WRITE_NODEPOOL}"

  # CreateNodePoolRequest carries clusterID, not a cluster name.
  if ! out="$(nkectl_exec get clusters "${cluster}" -o json 2>&1)"; then
    log "nkectl smoke: 'get clusters ${cluster} -o json' failed before the node pool round trip: ${out}" >&2
    return 1
  fi
  cluster_id="$(jq -r '.[0].id // empty' <<<"${out}")"
  if [[ -z "${cluster_id}" ]]; then
    log "nkectl smoke: no cluster id for ${cluster}: ${out}" >&2
    return 1
  fi

  nkectl_smoke_stage_spec pool <<EOF || return 1
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
  spireAttestationMethod: joinToken
  machineTemplate:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: DockerMachineTemplate
    spec:
      template:
        spec:
          customImage: ${CAPD_NODE_IMAGE}
EOF

  if ! out="$(nkectl_exec create nodepool -f "$(nkectl_smoke_spec_path pool)" 2>&1)"; then
    log "nkectl smoke: 'create nodepool' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'created (id: ' <<<"${out}"; then
    log "nkectl smoke: 'create nodepool' succeeded without reporting the id: ${out}" >&2
    return 1
  fi

  # A row, not just a header.
  if ! out="$(nkectl_exec get nodepools 2>&1)"; then
    log "nkectl smoke: 'get nodepools' failed after the create: ${out}" >&2
    return 1
  fi
  if ! grep -q 'K8S VERSION' <<<"${out}"; then
    log "nkectl smoke: 'get nodepools' printed no table header: ${out}" >&2
    return 1
  fi
  if ! grep -qE "^${pool}[[:space:]]" <<<"${out}"; then
    log "nkectl smoke: 'get nodepools' printed no row for ${pool}: ${out}" >&2
    return 1
  fi

  # Filter by cluster: the cluster's own control-plane pool and ours, and
  # nothing belonging to another cluster.
  if ! out="$(nkectl_exec get nodepools --cluster "${cluster}" -o json 2>&1)"; then
    log "nkectl smoke: 'get nodepools --cluster ${cluster} -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e --arg p "${pool}" --arg id "${cluster_id}" \
    'all(.clusterID == $id) and ([.[].name] | index($p) != null and index("control-plane") != null)' \
    <<<"${out}" >/dev/null; then
    log "nkectl smoke: 'get nodepools --cluster ${cluster}' did not return exactly that cluster's pools: ${out}" >&2
    return 1
  fi

  # Filter by name within the cluster.
  if ! out="$(nkectl_exec get nodepools "${pool}" --cluster "${cluster}" -o json 2>&1)"; then
    log "nkectl smoke: 'get nodepools ${pool} --cluster ${cluster} -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e --arg p "${pool}" 'length == 1 and .[0].name == $p' <<<"${out}" >/dev/null; then
    log "nkectl smoke: 'get nodepools ${pool} --cluster ${cluster}' did not return exactly our pool: ${out}" >&2
    return 1
  fi

  # Filter by name alone: the ?name= query without ?clusterName=.
  if ! out="$(nkectl_exec get nodepools "${pool}" -o json 2>&1)"; then
    log "nkectl smoke: 'get nodepools ${pool} -o json' failed: ${out}" >&2
    return 1
  fi
  if ! jq -e --arg id "${cluster_id}" 'length == 1 and .[0].clusterID == $id' <<<"${out}" >/dev/null; then
    log "nkectl smoke: 'get nodepools ${pool}' did not return exactly our pool: ${out}" >&2
    return 1
  fi

  nkectl_smoke_nodepool_scale 2 || return 1
  # Scaling down is a desiredNodeCount change and nothing more: the node set
  # controller deletes surplus *pending* nodes and deliberately leaves running
  # ones alone (internal/controllers/nodeset/controller.go), so the field is the
  # only thing there is to assert.
  nkectl_smoke_nodepool_scale 0 || return 1

  # Only passes if the first create committed.
  if out="$(nkectl_exec create nodepool -f "$(nkectl_smoke_spec_path pool)" 2>&1)"; then
    log "nkectl smoke: a second 'create nodepool' of ${pool} was accepted: ${out}" >&2
    return 1
  fi
  if ! grep -q 'already exists' <<<"${out}"; then
    log "nkectl smoke: the duplicate node pool create failed for a reason other than the name: ${out}" >&2
    return 1
  fi

  if ! out="$(nkectl_exec delete nodepool "${pool}" --cluster "${cluster}" 2>&1)"; then
    log "nkectl smoke: 'delete nodepool ${pool}' failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'deletion started (id: ' <<<"${out}"; then
    log "nkectl smoke: 'delete nodepool ${pool}' succeeded without reporting it: ${out}" >&2
    return 1
  fi

  # Teardown is asynchronous.
  local attempt
  for ((attempt = 1; attempt <= NKECTL_SMOKE_DELETE_POLLS; attempt++)); do
    if out="$(nkectl_exec get nodepools "${pool}" --cluster "${cluster}" 2>&1)"; then
      sleep "${NKECTL_SMOKE_DELETE_POLL_SECONDS}"
      continue
    fi
    if grep -q 'not found' <<<"${out}"; then
      log "nkectl smoke: node pool ${pool} was gone after ${attempt} polls"
      log "PASS: nkectl created, listed, filtered, patched and deleted a node pool through CWE"
      return 0
    fi
    log "nkectl smoke: polling for node pool ${pool} failed for a reason other than not-found: ${out}" >&2
    return 1
  done
  log "nkectl smoke: node pool ${pool} was still listed after ${NKECTL_SMOKE_DELETE_POLLS} polls" >&2
  return 1
}

# Patch desiredNodeCount to $1 and assert CWE stored it.
nkectl_smoke_nodepool_scale() {
  local want="$1" out cluster="${NKECTL_SMOKE_WRITE_CLUSTER}" pool="${NKECTL_SMOKE_WRITE_NODEPOOL}"

  if ! out="$(nkectl_exec patch nodepool "${pool}" --cluster "${cluster}" \
    --patch "desiredNodeCount: ${want}" 2>&1)"; then
    log "nkectl smoke: 'patch nodepool ${pool}' to ${want} failed: ${out}" >&2
    return 1
  fi
  if ! grep -q 'patched (id: ' <<<"${out}"; then
    log "nkectl smoke: 'patch nodepool ${pool}' succeeded without reporting the id: ${out}" >&2
    return 1
  fi

  if ! out="$(nkectl_exec get nodepools "${pool}" --cluster "${cluster}" -o json 2>&1)"; then
    log "nkectl smoke: 'get nodepools ${pool} -o json' failed after the patch: ${out}" >&2
    return 1
  fi
  if ! jq -e --argjson n "${want}" '.[0].desiredNodeCount == $n' <<<"${out}" >/dev/null; then
    log "nkectl smoke: 'patch nodepool' did not apply desiredNodeCount=${want}: ${out}" >&2
    return 1
  fi
}
