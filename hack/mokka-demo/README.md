# Simulated GPU nodes with Mokka

`demo.sh` gives an NVKE workload cluster GPU nodes without GPUs, and checks that the GPU stack works on them. It uses [Mokka](https://github.com/NVIDIA/k8s-test-infra) (`nvml-mock`), which simulates what an NVIDIA driver exposes on a node, so software that reads GPU state behaves as it would on hardware.

The demo:

1. Creates a node pool whose `nodeLabels` select Mokka. It copies the instance type, release and network of an existing worker pool, so it needs no provider-specific input.
2. Installs Mokka on the nodes with that label, and only on them.
3. Installs GPU Operator with [`gpu-operator-values.yaml`](gpu-operator-values.yaml): Mokka stands in for the driver and container toolkit.
4. Runs a pod that requests one GPU on a labeled node.

It prints `PASS` or `FAIL` for each check and exits non-zero if any fails:

- the pool's nodes are Ready and carry the label,
- `nvidia-smi -L` on each node lists the simulated GPUs,
- the ClusterPolicy is ready, each node advertises `nvidia.com/gpu`, and the GPU Operator validator completes,
- the pod sees exactly one GPU.

## Before you begin

- `nvkectl` pointed at the NVKE site (`nvkectl config set-site`), and a Provisioned workload cluster with at least one worker pool.
- `kubectl`, Helm 3.8 or later and `jq` on `PATH`.
- The workload cluster's nodes can pull from `ghcr.io`, `nvcr.io`, `registry.k8s.io` and Docker Hub. Mokka and GPU Operator publish amd64 and arm64 images.

## Run the demo

```bash
CLUSTER=<WORKLOAD_CLUSTER> hack/mokka-demo/demo.sh
```

The demo reads the workload cluster's kubeconfig through `nvkectl get kubeconfig`. Set `TENANT_KUBECONFIG` to use a file you already have; it does not use an inherited `KUBECONFIG`, which usually points at the management cluster.

| Variable | Default | Meaning |
|---|---|---|
| `CLUSTER` | required | Workload cluster name |
| `POOL` / `POOL_PREFIX` | `mokka-gpu` / `mg` | Name and node name prefix of the pool the demo creates. An existing pool of that name is reused. |
| `NODE_COUNT` | `1` | Nodes in the pool |
| `BASE_POOL` | the first worker pool | Pool whose infrastructure settings the new pool copies |
| `LABEL_KEY` | `mokka.example/simulated-gpu` | Node label that selects Mokka |
| `GPU_PROFILE` | `gb300` | Mokka GPU profile: `a100`, `b200`, `gb200`, `gb300`, `h100`, `l40s` or `t4` |
| `MOKKA_VERSION` / `GPU_OPERATOR_VERSION` | `0.6.0` / `v26.3.3` | Chart versions |
| `TIMEOUT` | `20m` | Helm and readiness timeout |

It runs against any NVKE site, for example a DSX OS Simulator site or the [local site](../../docs/get-started/local-site.md). On the local site, run it from the workspace shell after `source ~/nvke-local-site/env.sh`.

## What it does not prove

- **CUDA, NCCL and RDMA.** Mokka simulates the driver's interfaces, not GPU execution. The CUDA validator passes because the values skip its CUDA workload (`WITH_WORKLOAD=false`).
- **Distinct GPU identities across nodes.** Every Mokka node with the same profile reports the same GPU UUIDs, which matters for consumers that track GPUs across a cluster.
- **Performance and thermals.** Telemetry values come from the profile, not from load.

## Clean up

```bash
helm uninstall gpu-operator --namespace gpu-operator
helm uninstall nvml-mock --namespace mokka
nvkectl delete nodepool <POOL> --cluster <WORKLOAD_CLUSTER>
```

A node that loses Mokka keeps the `nvidia.com/gpu.product` label GPU Feature Discovery set, while its allocatable GPUs drop to 0.
