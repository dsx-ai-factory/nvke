#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Drive nkectl against the CWE a backend E2E lane already deployed. Source it
# from ci/backend-e2e.sh; do not run it.
#
# The Ginkgo suite proves the server. This proves the CLI can talk to it: read
# commands and their negatives, provider inspection, rejected creates.
#
# Needs from the caller: log, runner_exec, RUNNER_NAME, NKECTL_BIN, TMP_DIR.

# The CWE service the suite deploys, and the ServiceAccount it mints its token
# from (controller/test/e2e/internal/authtoken).
NKECTL_SMOKE_CWE_NAMESPACE=cloud-workflow-engine
NKECTL_SMOKE_CWE_SERVICE=cloud-workflow-engine
NKECTL_SMOKE_CWE_PORT=8080
NKECTL_SMOKE_TOKEN_NAMESPACE=nke-clients
NKECTL_SMOKE_TOKEN_SERVICE_ACCOUNT=nke-client
NKECTL_SMOKE_AUTH_CONFIGMAP=nke-auth-config
# A name no lane creates. Used by every negative assertion, so a rename here
# cannot leave one of them silently matching a real object.
NKECTL_SMOKE_ABSENT=nkectl-smoke-absent
# Name the rejection bodies carry; asserted absent afterwards.
NKECTL_SMOKE_REJECT_CLUSTER=nkectl-smoke-reject
# The suite's default; create validation checks only the semver shape.
NKECTL_SMOKE_KUBERNETES_VERSION="${NKECTL_SMOKE_KUBERNETES_VERSION:-1.34.2}"

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
  local token="$1"
  shift
  timeout "${NKECTL_SMOKE_TIMEOUT}" docker exec \
    -e KUBECONFIG=/state/management.kubeconfig \
    -e "NKECTL_DEV_TOKEN=${token}" \
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
  local token out rc audience

  # CWE checks the token's audience, so an unscoped `kubectl create token` is
  # rejected with audience_mismatch. Read the expected value rather than
  # hardcoding it, the way test/e2e/internal/authtoken does.
  audience="${NKE_E2E_AUDIENCE:-}"
  if [[ -z "${audience}" ]]; then
    audience="$(runner_exec kubectl -n "${NKECTL_SMOKE_CWE_NAMESPACE}" get configmap \
      "${NKECTL_SMOKE_AUTH_CONFIGMAP}" -o "jsonpath={.data.auth\\.yaml}" 2>/dev/null |
      sed -n 's/^audiences:[^"]*"\([^"]*\)".*/\1/p' | head -1)"
  fi
  if [[ -z "${audience}" ]]; then
    log "nkectl smoke: no audience in ${NKECTL_SMOKE_CWE_NAMESPACE}/${NKECTL_SMOKE_AUTH_CONFIGMAP}; set NKE_E2E_AUDIENCE" >&2
    return 1
  fi

  if ! token="$(runner_exec kubectl create token "${NKECTL_SMOKE_TOKEN_SERVICE_ACCOUNT}" \
    -n "${NKECTL_SMOKE_TOKEN_NAMESPACE}" --audience "${audience}" --duration=1h 2>&1)"; then
    log "nkectl smoke: could not mint a token for ${NKECTL_SMOKE_TOKEN_NAMESPACE}/${NKECTL_SMOKE_TOKEN_SERVICE_ACCOUNT}: ${token}" >&2
    return 1
  fi

  docker exec -d -e KUBECONFIG=/state/management.kubeconfig "${RUNNER_NAME}" \
    kubectl -n "${NKECTL_SMOKE_CWE_NAMESPACE}" port-forward \
    "svc/${NKECTL_SMOKE_CWE_SERVICE}" "${NKECTL_SMOKE_CWE_PORT}:${NKECTL_SMOKE_CWE_PORT}"

  runner_exec nkectl config set-site --name e2e \
    --url "http://127.0.0.1:${NKECTL_SMOKE_CWE_PORT}" \
    --kubeconfig /state/management.kubeconfig >/dev/null

  # Doubles as the readiness wait for the forward.
  rc=1
  for _ in $(seq 1 15); do
    if out="$(nkectl_exec "${token}" get clusters 2>&1)"; then
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
    if out="$(nkectl_exec "" get clusters 2>&1)"; then
      log "nkectl smoke: 'get clusters' succeeded with no token, so the run above proves nothing: ${out}" >&2
      return 1
    fi
  else
    log "nkectl smoke: SKIP the no-token control, NKE_E2E_REQUIRE_AUTHN is not true"
  fi

  if ! out="$(nkectl_exec "${token}" get clusters -o json 2>&1)"; then
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

  # Exercises FindClusterByName's ?name= filter, which the calls above never hit.
  if out="$(nkectl_exec "${token}" get clusters -c "${NKECTL_SMOKE_ABSENT}" 2>&1)"; then
    log "nkectl smoke: 'get clusters -c ${NKECTL_SMOKE_ABSENT}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nkectl smoke: 'get clusters -c' failed without a not-found message" >&2
    return 1
  fi

  # The two-hop FindClusterByName -> GetCluster path.
  if out="$(nkectl_exec "${token}" describe cluster "${NKECTL_SMOKE_ABSENT}" 2>&1)"; then
    log "nkectl smoke: 'describe cluster ${NKECTL_SMOKE_ABSENT}' succeeded against a name that does not exist" >&2
    return 1
  fi
  if ! grep -q 'not found' <<<"${out}"; then
    log "nkectl smoke: 'describe cluster' failed without a not-found message" >&2
    return 1
  fi

  # Rejected by the printer before any request is issued.
  if out="$(nkectl_exec "${token}" get clusters -o bogus 2>&1)"; then
    log "nkectl smoke: 'get clusters -o bogus' was accepted" >&2
    return 1
  fi
  if ! grep -qi 'output format' <<<"${out}"; then
    log "nkectl smoke: '-o bogus' failed for some reason other than the format" >&2
    return 1
  fi

  nkectl_smoke_providers "${token}" || return 1
  nkectl_smoke_create_rejections "${token}" || return 1

  log "PASS: nkectl reached CWE, authenticated, listed clusters, read providers, and had malformed creates rejected"
}

# `get providers` reads the site kubeconfig through kubectl rather than the CWE
# REST client, so it is the only smoke assertion covering that subsystem. Both
# lanes stage CAPNICo and wait for its CRDs (ci/backend-e2e.sh, needs-nico-provider),
# so crdsEstablished holds on CAPD too; only the NICo lane deploys the manager
# and the credentials Secret.
nkectl_smoke_providers() {
  local token="$1" out types

  if ! out="$(nkectl_exec "${token}" get providers -o json 2>&1)"; then
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

  if ! out="$(nkectl_exec "${token}" get providers 2>&1)"; then
    log "nkectl smoke: 'get providers' table mode failed" >&2
    return 1
  fi
  if ! grep -q 'PROVIDER' <<<"${out}"; then
    log "nkectl smoke: 'get providers' printed no table header" >&2
    return 1
  fi

  # A site that is not configured must fail before any cluster contact.
  if out="$(nkectl_exec "${token}" --site "${NKECTL_SMOKE_ABSENT}" get providers 2>&1)"; then
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
  local token="$1" out

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
  nkectl_smoke_expect_create_rejected "${token}" reject-name "a name that violates the DNS-label pattern" || return 1

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
  nkectl_smoke_expect_create_rejected "${token}" reject-version "no controlPlaneKubernetesVersion" || return 1

  nkectl_smoke_stage_spec reject-provider <<EOF || return 1
name: ${NKECTL_SMOKE_REJECT_CLUSTER}
controlPlaneKubernetesVersion: ${NKECTL_SMOKE_KUBERNETES_VERSION}
cniPlugin: calico
EOF
  nkectl_smoke_expect_create_rejected "${token}" reject-provider "neither nico nor infra" || return 1

  # Zero state: a rejected create must leave nothing to find by name.
  if out="$(nkectl_exec "${token}" get clusters -c "${NKECTL_SMOKE_REJECT_CLUSTER}" 2>&1)"; then
    log "nkectl smoke: a rejected create left cluster ${NKECTL_SMOKE_REJECT_CLUSTER} behind: ${out}" >&2
    return 1
  fi
}

# Assert the status, not the message: the text is the middleware's, and a 400
# is the only proof the request reached CWE rather than dying in the CLI.
nkectl_smoke_expect_create_rejected() {
  local token="$1" spec="$2" what="$3" out
  if out="$(nkectl_exec "${token}" create cluster -f "$(nkectl_smoke_spec_path "${spec}")" 2>&1)"; then
    log "nkectl smoke: 'create cluster' with ${what} was accepted: ${out}" >&2
    return 1
  fi
  if ! grep -q '400' <<<"${out}"; then
    log "nkectl smoke: 'create cluster' with ${what} failed, but not with a 400 from CWE: ${out}" >&2
    return 1
  fi
}
