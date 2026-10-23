#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Sourced by dev.sh: reuse its isolated kubeconfig, cluster checks, and credentials.
: "${dev_dir:?Source this helper through hack/dev.sh}"
declare -a host state clusters

nvke_controller_environment() {
  export DEV_NVKE_CONTROLLER=1 KIND_CLUSTER_NAME="$DEV_NAME-management"
  # NKE_CONTROLLER_HTTP_PORT and NKE_CONTROLLER_TEMPORAL_UI_PORT are deprecated aliases; #1399 removes them.
  export NVKE_CONTROLLER_HTTP_PORT="${NVKE_CONTROLLER_HTTP_PORT:-${NKE_CONTROLLER_HTTP_PORT:-18080}}" NVKE_CONTROLLER_TEMPORAL_UI_PORT="${NVKE_CONTROLLER_TEMPORAL_UI_PORT:-${NKE_CONTROLLER_TEMPORAL_UI_PORT:-18081}}"
  export DEV_VALUES="$nvke_controller_dir/values.yaml"
  export NVKE_CONTROLLER_API_URL="http://127.0.0.1:$NVKE_CONTROLLER_HTTP_PORT/api" NVKE_CONTROLLER_TOKEN_FILE="$nvke_controller_dir/token"
  export NVKE_CONTROLLER_RESULTS_DIR="$nvke_controller_dir/results"
  export NKE_AUTH_ENABLED=true NKE_PERMISSIVE_MODE=false
  export TENANT_ACCESS_ENABLED=false NKE_OIDC_AUTH_ENABLED=false
  export NVKE_CONTROLLER_PROFILES_DIR="$PWD/../controller/test/fixtures/profiles"
  if [[ -z ${DOCKER_HOST:-} ]]; then
    DOCKER_HOST=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
    export DOCKER_HOST
  fi
  # These randomly generated credentials belong only to this disposable stack.
  set -a
  # shellcheck disable=SC1091
  source "$nvke_controller_dir/credentials.env"
  set +a
}

nvke_controller_setup() (
  trap 'rm -f "$nvke_controller_dir/credentials.env.tmp"' EXIT
  check_tilt
  for tool in docker kubectl kind helm openssl go temporal; do
    command -v "$tool" >/dev/null || fail "$tool is required by the combined development environment."
  done
  if [[ ! -f "$nvke_controller_dir/owned" ]]; then
    local existing cluster
    existing=$(kind get clusters)
    for cluster in "${clusters[@]}"; do
      if grep -Fxq -- "$cluster" <<<"$existing"; then
        fail "$cluster already exists outside this integration environment. Select a new DEV_NAME."
      fi
    done
  fi
  local target="$nvke_controller_dir/tilt_config.json"
  if [[ -e "$nvke_controller_config" || -L "$nvke_controller_config" ]]; then
    [[ -L "$nvke_controller_config" && $(readlink "$nvke_controller_config") == "$target" ]] ||
      fail "$nvke_controller_config already exists; preserve it and use a separate checkout for the combined environment."
  fi
  if [[ -e "$nvke_controller_hook" ]]; then
    cmp -s "$nvke_controller_hook" hack/nvke-controller-hook.star || fail "$nvke_controller_hook is not owned by this integration environment."
  fi
  mkdir -p "$nvke_controller_dir" ../controller/tilt.d
  touch "$nvke_controller_dir/owned"
  printf '%s\n' '{"providers":["capi_core","capd"]}' >"$target"
  [[ -L "$nvke_controller_config" ]] || ln -s "$target" "$nvke_controller_config"
  cp hack/nvke-controller-hook.star "$nvke_controller_hook"
  if [[ ! -f "$nvke_controller_dir/credentials.env" ]]; then
    local variable value
    : >"$nvke_controller_dir/credentials.env.tmp"
    for variable in DGXC_TENANT_USER DGXC_TENANT_PASSWORD CNPG_SUPERUSER_PASSWORD CNPG_CLOUD_PASSWORD CNPG_TEMPORAL_PASSWORD; do
      value=$(openssl rand -hex 24)
      printf '%s=%s\n' "$variable" "$value" >>"$nvke_controller_dir/credentials.env.tmp"
    done
    mv "$nvke_controller_dir/credentials.env.tmp" "$nvke_controller_dir/credentials.env"
  fi
  sed -e 's/--namespace=project-a/--namespace=nvke-controller-integration/' \
    -e 's|https://controller.example.com/api|http://nvke-controller.nvke-controller.svc.cluster.local:8080/api|' \
    config/samples/nvke-controller-values.yaml >"$nvke_controller_dir/values.yaml"
  nvke_controller_environment
  bash hack/dev.sh up
  connect
  "${state[@]}" create namespace nvke-controller-integration --dry-run=client -o yaml | "${state[@]}" apply -f -
  "${host[@]}" -n "$HOST_NAMESPACE" create configmap management-nvke-controller-bindings \
    --from-file=bindings.json=test/integration/testdata/bindings.json --dry-run=client -o yaml | "${host[@]}" apply -f -
  make -C ../controller clusterctl-install
  ../controller/.local/bin/clusterctl init --kubeconfig "$KUBECONFIG" --kubeconfig-context "$HOST_CONTEXT" \
    --core "cluster-api:$CAPI_VERSION" --bootstrap "kubeadm:$CAPI_VERSION" \
    --control-plane "kubeadm:$CAPI_VERSION" --infrastructure "docker:$CAPI_VERSION"
  "${host[@]}" -n capi-system rollout status deployment/capi-controller-manager --timeout=300s
  "${host[@]}" -n capd-system rollout status deployment/capd-controller-manager --timeout=300s
)

nvke_controller_credentials() (
  local token_file
  token_file=$(mktemp "$nvke_controller_dir/token.XXXXXX")
  trap 'rm -f "$token_file"' EXIT
  # Refresh the existing admitted development identity without logging its token.
  KUBE_CONTEXT="$HOST_CONTEXT" NKE_TOKEN_TTL=24h NKE_AUTH_TOKEN='' \
    bash ../controller/hack/nvke-token.sh >"$token_file"
  [[ -s "$token_file" ]] || fail "The NVKE controller must have authentication enabled."
  mv "$token_file" "$nvke_controller_dir/token"
  "${host[@]}" -n "$HOST_NAMESPACE" create secret generic management-nvke-controller-token \
    --from-file=token="$nvke_controller_dir/token" --dry-run=client -o yaml |
    "${host[@]}" apply --server-side --field-manager=management-engine-dev -f -
)

nvke_controller_down() (
  [[ -f "$nvke_controller_dir/owned" ]] || fail "This environment was not created by make dev-nvke-controller."
  local uid container proof scratch
  scratch=$(mktemp -d "$nvke_controller_dir/cleanup.XXXXXX")
  trap 'rm -rf "$scratch"' EXIT
  touch "$scratch/uids"
  for proof in "$nvke_controller_dir"/results/*.json; do
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
    "${state[@]}" --kubeconfig "$scratch/kubeconfig" -n nvke-controller-integration get nvkeclusters \
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
  if [[ -L "$nvke_controller_config" && $(readlink "$nvke_controller_config") == "$nvke_controller_dir/tilt_config.json" ]]; then
    rm "$nvke_controller_config"
    if cmp -s "$nvke_controller_hook" hack/nvke-controller-hook.star; then rm "$nvke_controller_hook"; fi
  fi
  rm -f "$nvke_controller_dir/token" "$nvke_controller_dir/token.tmp" "$nvke_controller_dir/credentials.env" "$nvke_controller_dir/owned"
  echo "NVKE controller integration environment removed; diagnostics remain in $nvke_controller_dir/results."
)

nvke_controller_main() {
  nvke_controller_dir="$dev_dir/nvke-controller"
  nvke_controller_config="$PWD/../controller/tilt_config.json"
  nvke_controller_hook="$PWD/../controller/tilt.d/management-engine.star"
  case "$1" in
    nvke-controller-up) nvke_controller_setup ;;
    nvke-controller-down) nvke_controller_down ;;
    nvke-controller-tilt|nvke-controller-test|nvke-controller-credentials)
      [[ -f "$nvke_controller_dir/owned" ]] || fail "Run make dev-nvke-controller-up first."
      nvke_controller_environment
      connect
      case "$1" in
        nvke-controller-tilt) start_tilt ../controller/Tiltfile "$TILT_PORT" ;;
        nvke-controller-credentials) nvke_controller_credentials ;;
        nvke-controller-test)
          nvke_controller_credentials
          mkdir -p "$NVKE_CONTROLLER_RESULTS_DIR"
          exec go test -tags=integration_test -count=1 -timeout=12m -v ./test/integration \
            -ginkgo.v -ginkgo.junit-report="$NVKE_CONTROLLER_RESULTS_DIR/junit.xml"
          ;;
      esac
      ;;
    *) fail "Unknown NVKE controller development action: $1" ;;
  esac
}
