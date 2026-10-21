# Developing nais-dpu-sim

See the [README](README.md) for architecture, provisioning, and packet flows.

## Prepare the host

Use the [shared controller setup](../../README.md#capi--tilt-development-recommended) for Docker and the local development tools.
Use its [CAPNICo source setup](../../README.md#building-capnico-from-a-local-checkout) for the local provider image and generated Helm chart.

The simulator needs an arm64 Docker daemon with kernel Open vSwitch support and these inputs:

| Input | Requirement |
|---|---|
| `cwe-capd-kindest-node:local` | The shared CAPD node image, built for arm64 |
| `cluster-api-provider-nico:local` | The local CAPNICo image, built for arm64 |
| `CAPNICO_CHART` | An absolute path to the matching CAPNICo Helm chart with generated resources |
| `kindest/node:v1.34.8` | The driver image for host namespace tools |

Run one simulation per Docker daemon because the fixtures use fixed subnets from `172.28.234.0/24` through `172.28.237.0/24`.
The simulator creates its own management cluster through the shared `kind-up` target.
It keeps your kubeconfig and current context unchanged.

## Build and create

From the repository root, enter the [shared Flox environment](../../README.md#tool-dependencies):

```sh
flox activate
```

Use this shell for the remaining commands:

```sh
export DOCKER_CONTEXT=colima
unset DOCKER_HOST
export CAPNICO_CHART=/absolute/path/to/capi-provider-nico
cd controller/hack/nais-dpu-sim
make test build
make create CAPNICO_CHART="$CAPNICO_CHART"
```

The build downloads the pinned inputs, compares their checksums, builds the local images and packages, and renders the three OVN roles.
Use `make help` for individual build targets.

Creation provisions one control-plane node and one worker in each tenant cluster, then tests provisioning and networking.
It prints the run directory, which contains access files and reports.
Set that directory before later operations:

```sh
export STATE_DIR=/absolute/path/printed/by/create
```

## Operate a retained run

Use these commands from the simulator directory with `STATE_DIR` set:

| Command | Purpose |
|---|---|
| `make verify` | Repeat provisioning and network assertions |
| `make scale WORKERS=2` | Add workers to both tenant clusters, up to three workers each |
| `make restart` | Restart the three OVN DaemonSets and repeat network assertions |
| `make resume CAPNICO_CHART="$CAPNICO_CHART"` | Retry a failed run with its saved cluster IDs and setup progress |
| `make cleanup` | Delete both tenant clusters through NVKE, then remove the run's management cluster and Docker fixtures |

Scale preserves existing host-to-DPU pairs and tests labels on each new worker's initial Kubernetes Node event.
Resume avoids provisioning both clusters again after a setup failure.
Cleanup retains the local reports.

To inspect workload nodes, run `kubectl --kubeconfig "$STATE_DIR/workload.kubeconfig" get nodes`.
Use `dpu.kubeconfig` or `management.kubeconfig` to inspect the other clusters.

## Read the results

| Report | Assertion |
|---|---|
| `management-validation.json` | Both tenant clusters and their provisioning workflows completed |
| `host-provisioning-validation.json` | Bootstrap labels, host routes, IPv6, scheduling, and absence of host OVS |
| `dpu-primary-validation.json` | Isolation of the primary and workload networks on each DPU |
| `ovn-runtime-validation.json` | Pinned OVN image and ready host, workload-DPU, and primary-DPU components |
| `network-matrix.json` | Traffic in both tenant clusters, plus workload BGP sessions and fabric packet capture |
| `worker-registration-validation.json` | Required labels on initial Node events after scaling |

Do not commit `.state` because it contains kubeconfigs, runtime credentials, and logs.
