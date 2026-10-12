#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Sourced by dev.sh: reuse its isolated kubeconfig, cluster checks, and credentials.
: "${dev_dir:?Source this helper through hack/dev.sh}"
declare -a host state clusters

cwe_environment() {
  export DEV_CWE=1 KIND_CLUSTER_NAME="$DEV_NAME-management"
  export CWE_HTTP_PORT="${CWE_HTTP_PORT:-18080}" CWE_TEMPORAL_UI_PORT="${CWE_TEMPORAL_UI_PORT:-18081}"
  export DEV_VALUES="$cwe_dir/values.yaml"
  export CWE_API_URL="http://127.0.0.1:$CWE_HTTP_PORT/api" CWE_TOKEN_FILE="$cwe_dir/token"
  export CWE_RESULTS_DIR="$cwe_dir/results"
  export NKE_AUTH_ENABLED=true NKE_PERMISSIVE_MODE=false
  export PROVISION_CONTROL_PLANE_WITH_CONTROL_PLANE_CONTROLLER=true
  export TENANT_ACCESS_ENABLED=false NKE_OIDC_AUTH_ENABLED=false
  export CWE_PROFILES_DIR="$PWD/../controller/test/fixtures/profiles"
  if [[ -z ${DOCKER_HOST:-} ]]; then
    DOCKER_HOST=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
    export DOCKER_HOST
  fi
  # These randomly generated credentials belong only to this disposable stack.
  set -a
  # shellcheck disable=SC1091
  source "$cwe_dir/credentials.env"
  set +a
}

cwe_setup() (
  trap 'rm -f "$cwe_dir/credentials.env.tmp"' EXIT
  check_tilt
  for tool in docker kubectl kind helm openssl go temporal; do
    command -v "$tool" >/dev/null || fail "$tool is required by the combined development environment."
  done
  if [[ ! -f "$cwe_dir/owned" ]]; then
    local existing cluster
    existing=$(kind get clusters)
    for cluster in "${clusters[@]}"; do
      if grep -Fxq -- "$cluster" <<<"$existing"; then
        fail "$cluster already exists outside this integration environment. Select a new DEV_NAME."
      fi
    done
  fi
  local target="$cwe_dir/tilt_config.json"
  if [[ -e "$cwe_config" || -L "$cwe_config" ]]; then
    [[ -L "$cwe_config" && $(readlink "$cwe_config") == "$target" ]] ||
      fail "$cwe_config already exists; preserve it and use a separate checkout for the combined environment."
  fi
  if [[ -e "$cwe_hook" ]]; then
    cmp -s "$cwe_hook" hack/cwe-hook.star || fail "$cwe_hook is not owned by this integration environment."
  fi
  mkdir -p "$cwe_dir" ../controller/tilt.d
  touch "$cwe_dir/owned"
  printf '%s\n' '{"providers":["capi_core","capd"]}' >"$target"
  [[ -L "$cwe_config" ]] || ln -s "$target" "$cwe_config"
  cp hack/cwe-hook.star "$cwe_hook"
  if [[ ! -f "$cwe_dir/credentials.env" ]]; then
    local variable value
    : >"$cwe_dir/credentials.env.tmp"
    for variable in DGXC_TENANT_USER DGXC_TENANT_PASSWORD CNPG_SUPERUSER_PASSWORD CNPG_CLOUD_PASSWORD CNPG_TEMPORAL_PASSWORD; do
      value=$(openssl rand -hex 24)
      printf '%s=%s\n' "$variable" "$value" >>"$cwe_dir/credentials.env.tmp"
    done
    mv "$cwe_dir/credentials.env.tmp" "$cwe_dir/credentials.env"
  fi
  sed -e 's/--namespace=project-a/--namespace=cwe-integration/' \
    -e 's|https://cwe.example.com/api|http://cloud-workflow-engine.cloud-workflow-engine.svc.cluster.local:8080/api|' \
    config/samples/cwe-values.yaml >"$cwe_dir/values.yaml"
  cwe_environment
  bash hack/dev.sh up
  connect
  "${state[@]}" create namespace cwe-integration --dry-run=client -o yaml | "${state[@]}" apply -f -
  "${host[@]}" -n "$HOST_NAMESPACE" create configmap management-cwe-bindings \
    --from-file=bindings.json=test/integration/testdata/bindings.json --dry-run=client -o yaml | "${host[@]}" apply -f -
  make -C ../controller clusterctl-install
  ../controller/.local/bin/clusterctl init --kubeconfig "$KUBECONFIG" --kubeconfig-context "$HOST_CONTEXT" \
    --core "cluster-api:$CAPI_VERSION" --bootstrap "kubeadm:$CAPI_VERSION" \
    --control-plane "kubeadm:$CAPI_VERSION" --infrastructure "docker:$CAPI_VERSION"
  "${host[@]}" -n capi-system rollout status deployment/capi-controller-manager --timeout=300s
  "${host[@]}" -n capd-system rollout status deployment/capd-controller-manager --timeout=300s
)

cwe_credentials() (
  local token_file
  token_file=$(mktemp "$cwe_dir/token.XXXXXX")
  trap 'rm -f "$token_file"' EXIT
  # Refresh the existing admitted development identity without logging its token.
  KUBE_CONTEXT="$HOST_CONTEXT" NKE_TOKEN_TTL=24h NKE_AUTH_TOKEN='' \
    bash ../controller/hack/nvke-token.sh >"$token_file"
  [[ -s "$token_file" ]] || fail "CWE must have authentication enabled."
  mv "$token_file" "$cwe_dir/token"
  "${host[@]}" -n "$HOST_NAMESPACE" create secret generic management-cwe-token \
    --from-file=token="$cwe_dir/token" --dry-run=client -o yaml |
    "${host[@]}" apply --server-side --field-manager=management-engine-dev -f -
)

cwe_down() (
  [[ -f "$cwe_dir/owned" ]] || fail "This environment was not created by make dev-cwe."
  local uid container proof scratch
  scratch=$(mktemp -d "$cwe_dir/cleanup.XXXXXX")
  trap 'rm -rf "$scratch"' EXIT
  touch "$scratch/uids"
  for proof in "$cwe_dir"/results/*.json; do
    [[ -f "$proof" ]] || continue
    uid=${proof##*/}
    uid=${uid%.json}
    [[ "$uid" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || continue
    printf '%s\n' "$uid" >>"$scratch/uids"
  done
  # Only remove CAPD load balancers belonging to this environment's CR UIDs.
  # The journal also permits cleanup after either Kind cluster has disappeared.
  if kind get clusters | grep -Fxq "$DEV_NAME-state"; then
    kind export kubeconfig --name "$DEV_NAME-state" --kubeconfig "$scratch/kubeconfig"
    "${state[@]}" --kubeconfig "$scratch/kubeconfig" -n cwe-integration get nvkeclusters \
      --ignore-not-found -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' >>"$scratch/uids" ||
      echo "State API unavailable; using recorded test UIDs for load-balancer cleanup." >&2
  fi
  sort -u "$scratch/uids" >"$scratch/unique-uids"
  # Stop CAPD before removing its containers so it cannot recreate them.
  bash hack/dev.sh down
  while IFS= read -r uid; do
    [[ -n "$uid" ]] || continue
    docker ps -aq --filter "label=io.x-k8s.kind.cluster=nke-$uid" \
      --filter label=io.x-k8s.kind.role=external-load-balancer >"$scratch/containers"
    while IFS= read -r container; do
      [[ -n "$container" ]] && docker rm -f "$container" >/dev/null
    done <"$scratch/containers"
  done <"$scratch/unique-uids"
  if [[ -L "$cwe_config" && $(readlink "$cwe_config") == "$cwe_dir/tilt_config.json" ]]; then
    rm "$cwe_config"
    if cmp -s "$cwe_hook" hack/cwe-hook.star; then rm "$cwe_hook"; fi
  fi
  rm -f "$cwe_dir/token" "$cwe_dir/token.tmp" "$cwe_dir/credentials.env" "$cwe_dir/owned"
  echo "CWE integration environment removed; diagnostics remain in $cwe_dir/results."
)

cwe_main() {
  cwe_dir="$dev_dir/cwe"
  cwe_config="$PWD/../controller/tilt_config.json"
  cwe_hook="$PWD/../controller/tilt.d/management-engine.star"
  case "$1" in
    cwe-up) cwe_setup ;;
    cwe-down) cwe_down ;;
    cwe-tilt|cwe-test|cwe-credentials)
      [[ -f "$cwe_dir/owned" ]] || fail "Run make dev-cwe-up first."
      cwe_environment
      connect
      case "$1" in
        cwe-tilt) start_tilt ../controller/Tiltfile "$TILT_PORT" ;;
        cwe-credentials) cwe_credentials ;;
        cwe-test)
          cwe_credentials
          mkdir -p "$CWE_RESULTS_DIR"
          exec go test -tags=integration_test -count=1 -timeout=12m -v ./test/integration \
            -ginkgo.v -ginkgo.junit-report="$CWE_RESULTS_DIR/junit.xml"
          ;;
      esac
      ;;
    *) fail "Unknown CWE development action: $1" ;;
  esac
}
