# Mixed simulated GPU models in one cluster

`run.sh` gives one NVKE workload cluster node pools that present different GPU models, and checks that workloads can tell them apart. It runs [`../demo.sh`](../README.md) once per Mokka GPU profile, each time with its own node pool, node label and Mokka release, so each pool's nodes report a different GPU.

It prints `PASS` or `FAIL` for each check and exits non-zero if any fails:

- everything `demo.sh` checks, for each pool,
- each node's `nvidia.com/gpu.product` label is the GPU name `nvidia-smi` reports on it, and its allocatable `nvidia.com/gpu` is the number of GPUs `nvidia-smi` lists,
- the pools advertise as many different models as there are profiles,
- a pod with a `nodeSelector` on one model's `nvidia.com/gpu.product` runs on that model's pool and sees that model.

## Run it

Prerequisites are those of [`demo.sh`](../README.md#before-you-begin).

```bash
CLUSTER=<WORKLOAD_CLUSTER> hack/mokka-demo/mixed-gpus/run.sh
```

| Variable | Default | Meaning |
|---|---|---|
| `CLUSTER` | required | Workload cluster name |
| `PROFILES` | `gb300 h100` | Two or more Mokka GPU profiles; see `GPU_PROFILE` in [`demo.sh`](../README.md#run-the-demo) |
| `TENANT_KUBECONFIG` | from `nvkectl get kubeconfig` | The workload cluster's kubeconfig |
| `TIMEOUT` | `20m` | Helm and readiness timeout, per `demo.sh` run |

For each profile `<P>`, the run creates node pool `mokka-<P>` with node name prefix `m<P>`, node label `mokka.example/gpu-<P>=true`, and Mokka release `nvml-mock-<P>` in namespace `mokka`. GPU Operator is shared.

## What it does not prove

- **Mixed models on one node.** Each node presents one profile, as each Mokka release serves one.
- **Scheduling by GPU memory or compute capability.** The workloads select on `nvidia.com/gpu.product` alone, though GPU Feature Discovery publishes the other labels as well.
- Everything `demo.sh` does not prove: see [What it does not prove](../README.md#what-it-does-not-prove).

## Clean up

For each profile `<P>`:

```bash
helm uninstall nvml-mock-<P> --namespace mokka
nvkectl delete nodepool mokka-<P> --cluster <WORKLOAD_CLUSTER>
```

Then GPU Operator, as in [`demo.sh`'s clean-up](../README.md#clean-up).
