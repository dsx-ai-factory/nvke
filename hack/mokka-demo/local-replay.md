# Replay Mokka on local CAPD nodes

Use the existing local E2E runner to provision a tenant, install Mokka and one
GPU Operator, validate GPU discovery and placement, and delete the tenant.
This is a CPU simulation: driver/toolkit installation and the CUDA workload are
disabled. It does not qualify physical GPUs, AICR, recycling or performance.

## Prepare a separate checkout

Start from a clean checkout containing the local runner fixes. Until [#2109](https://github.com/dsx-ai-factory/nvke/pull/2109)
is merged, include its mixed-model E2E case in a disposable worktree. The pinned
revision below contains that case and its shared request/cleanup helpers; this
recipe does not copy them or implement another API client.

```sh
git worktree add --detach ../nvke-mokka HEAD
git -C ../nvke-mokka fetch https://github.com/dsx-ai-factory/nvke.git 4f4e2527f05050f50306ed23985eb568b8f0bccc
git -C ../nvke-mokka merge --no-commit --no-ff FETCH_HEAD
cd ../nvke-mokka
```

Stop if the merge reports a conflict. After #2109 merges, the fetch/merge is
unnecessary. Record `git rev-parse HEAD` and the scenario revision with results.

## Run

Use the [development prerequisites](../../CONTRIBUTING.md#prerequisites), plus
Bash 4+, kind, kubectl, Helm, Tilt, jq, dpkg-deb, curl and OpenSSL. GNU Make
must resolve as `make` on PATH, including in subprocesses. Start a local Linux
Docker daemon or Colima VM with at least 8 CPUs, 16 GiB RAM and 100 GiB free
space; a 150 GiB disk provides headroom. On macOS keep the checkout in a
Colima-shared directory. Run serially: CAPD tenants share BGP and VIP routes.

In the worktree shell, create fresh local credentials and name the resources
so they can be inspected and removed without touching another setup:

```sh
umask 077
export DGXC_TENANT_USER=mokka-local
for name in DGXC_TENANT_PASSWORD CNPG_SUPERUSER_PASSWORD CNPG_CLOUD_PASSWORD CNPG_TEMPORAL_PASSWORD; do
  export "$name=$(openssl rand -hex 18)"
done
export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
export COLIMA_DOCKER_HOST="$DOCKER_HOST"
export MOKKA_RUN_ID="$(date +%s)"
export KIND_CLUSTER_NAME="nvke-mokka-$MOKKA_RUN_ID"
export BGP_CONTAINER_NAME="nvke-mokka-bgp-$MOKKA_RUN_ID"
export BUNDLE_MIRROR_NAME="nvke-mokka-bundles-$MOKKA_RUN_ID"
export BUNDLE_MIRROR_VOLUME="$BUNDLE_MIRROR_NAME"
export LOCAL_NKE_E2E_STATE_DIR="$PWD/.local/mokka"
export LOCAL_NKE_E2E_KUBECONFIG="$LOCAL_NKE_E2E_STATE_DIR/management.kubeconfig"
LOCAL_NKE_E2E_LABEL_FILTER=mokka LOCAL_NVKE_E2E_REUSE_DOCKER_HOST=true CAPD_SPIRE_ATTESTATION_METHOD=joinToken INSTALL_CNI_AFTER_INIT=false make -C controller test-e2e-capd-local
```

The runner uses the checkout's toolchain and chart pins. Its Go mixed-model
case is unaffected by the shell's `MOKKA_VERSION`/`GPU_OPERATOR_VERSION` values.
It exercises GB300 x4 and H100 x8, both mock validators, and a one-GPU pod
selecting each model. Inspect the JUnit report under `$LOCAL_NKE_E2E_STATE_DIR`;
a nonzero exit or partial run is not a pass. Keep that directory private: logs
and kubeconfigs may contain the generated credentials. Join-token attestation
and the public-bundle CNI path are local fixture choices, not production setup.

The public bundle downloader permits 600 seconds per artifact and a 1,200-second
retry budget. Override `CAPD_BUNDLE_DOWNLOAD_TIMEOUT` and
`CAPD_BUNDLE_RETRY_MAX_TIME` in seconds if needed; checksum verification remains
mandatory.

## Cleanup

Successful E2E runs delete their tenant through the shared lifecycle helpers.
Failures preserve it for diagnosis. If tenant resources remain, use the normal
NVKE deletion path described in the [E2E guide](../../controller/test/e2e/README.md)
before removing management. In the same shell, the guard below preserves the
management cluster if tenant CAPI objects remain or its API cannot be read:

```sh
set -o pipefail
if kubectl --kubeconfig "$LOCAL_NKE_E2E_KUBECONFIG" --context "kind-$KIND_CLUSTER_NAME" get clusters,machines -A -o json | jq -e '.items | length == 0' >/dev/null; then
  KUBECONFIG="$LOCAL_NKE_E2E_KUBECONFIG" kind delete cluster --name "$KIND_CLUSTER_NAME" &&
    docker rm -f "$BGP_CONTAINER_NAME" "$BUNDLE_MIRROR_NAME" &&
    docker volume rm "$BUNDLE_MIRROR_VOLUME"
else
  printf '%s\n' 'Management kept: tenant resources remain or the API is unavailable.' >&2
fi
```

Images, caches and private diagnostics are retained. This recipe introduces no
separate provisioning client, patch series or version-lock file. To add AICR,
extend the shared experiment before its tenant-deletion phase rather than
creating another provisioning path.

## Verified baseline

On 2026-11-16, runtime changes at `3e1402fc` combined with scenario revision
`4f4e2527f05050f50306ed23985eb568b8f0bccc` passed all nine selected lifecycle and
mixed-model specs on macOS arm64 / Colima (8 CPUs, 16 GiB RAM) in about 22 minutes.
The checks covered GB300 x4, H100 x8, both validators, model-selected one-GPU pods,
tenant deletion and tenant-access secret cleanup.
Management/BGP/bundle cleanup was verified separately with the private kubeconfig;
the global kubeconfig and an older management cluster were preserved.

Created with AI assistance
