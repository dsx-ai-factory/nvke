# NVSentinel fault remediation on simulated GPUs

`run.sh` shows [NVSentinel](https://github.com/NVIDIA/nvsentinel) detect a GPU fault on an NVKE workload cluster node, cordon and drain the node, and uncordon it once the fault clears, with no GPUs. It adapts Mokka's [NVSentinel guide](https://github.com/NVIDIA/k8s-test-infra/tree/main/docs/guides/nv-sentinel), which runs on Kind, to NVKE.

The fault is a thermal-margin violation. Mokka pins one GPU above its slowdown threshold, which drives the GPU's T.Limit margin below zero. NVSentinel's GPU Health Monitor reads the margin from DCGM, and its fault-quarantine and node-drainer modules act on the event. Resetting the temperature reopens the margin, and NVSentinel uncordons the node without a DCGM restart.

The run:

1. Runs [`../demo.sh`](../README.md) for a pool of two Mokka nodes.
2. Turns on GPU Operator's standalone DCGM, which the GPU Health Monitor polls.
3. Installs cert-manager, which issues NVSentinel's MongoDB certificates.
4. Installs NVSentinel with [`nvsentinel-values.yaml`](../../../controller/test/e2e/testdata/nvsentinel-values.yaml), Mokka's guide values: MongoDB through the Percona operator, here in an `emptyDir` so the cluster needs no StorageClass, the thermal-margin watch acting rather than dry-run, and the circuit breaker off for a two-node pool.
5. Runs a Deployment that requests one GPU on the pool.
6. Heats one GPU on the workload's node, then resets it.

It prints `PASS` or `FAIL` for each check and exits non-zero if any fails:

- everything `demo.sh` checks,
- DCGM's service exists, and NVSentinel's pods are Ready,
- with the GPU hot, NVSentinel cordons the node, and the workload moves to the other node,
- with the GPU reset, NVSentinel uncordons the node.

## Run it

Prerequisites are those of [`demo.sh`](../README.md#before-you-begin), plus network access to `ghcr.io` for the NVSentinel chart, `github.com` for cert-manager, and Docker Hub for the Percona images. NVSentinel's core modules run on the control-plane node, so a drain never evicts them.

```bash
CLUSTER=<WORKLOAD_CLUSTER> hack/mokka-demo/nvsentinel/run.sh
```

| Variable | Default | Meaning |
|---|---|---|
| `CLUSTER` | required | Workload cluster name |
| `POOL` / `POOL_PREFIX` | `mokka-sentinel` / `ms` | The two-node pool `demo.sh` creates |
| `LABEL_KEY` | `mokka.example/nvsentinel-gpu` | Node label that selects Mokka |
| `GPU_PROFILE` | `h100` | Mokka GPU profile |
| `MOKKA_RELEASE` | `nvml-mock-sentinel` | Helm release name for Mokka |
| `TARGET_GPU` / `HOT_TEMP_C` | `0` / `142` | GPU index to heat, and its temperature, above every profile's slowdown threshold |
| `NVSENTINEL_VERSION` / `CERT_MANAGER_VERSION` / `GPU_OPERATOR_VERSION` | `v1.21.0` / `v1.19.1` / `v26.3.3` | Versions |
| `TENANT_KUBECONFIG` | from `nvkectl get kubeconfig` | The workload cluster's kubeconfig |
| `TIMEOUT` | `20m` | Helm and readiness timeout |

## In CI

The e2e suite's `CAPD NVSentinel` entry runs the same scenario in Go on a CAPD cluster with two `h100` Mokka nodes. Its own lane, `capd-nvsentinel-e2e.yml`, runs it twice a day, and on pull requests that change the scenario or the Mokka setup. To run it against a local stack:

```bash
cd controller
LOCAL_NKE_E2E_LABEL_FILTER=nvsentinel hack/local-backend-e2e.sh
```

## What it does not prove

- **Latched faults.** XID and ECC faults stay latched in DCGM until its hostengine restarts; this run uses a thermal fault because it clears on its own.
- **Remediation beyond drain.** Fault remediation and the janitor, which reboot or replace a node, are off: NVKE's node lifecycle has no simulated path for them yet.
- **The circuit breaker.** It is off, since one cordoned node is half of a two-node pool. Leave it on in real clusters.
- Everything `demo.sh` does not prove: see [What it does not prove](../README.md#what-it-does-not-prove).

## Clean up

Point `kubectl` and Helm at the workload cluster first, as in [`demo.sh`'s clean-up](../README.md#clean-up).

```bash
nvkectl get kubeconfig <WORKLOAD_CLUSTER> --output-file workload.kubeconfig
export KUBECONFIG=$PWD/workload.kubeconfig
helm uninstall nvsentinel --namespace nvsentinel
kubectl delete -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
helm uninstall nvml-mock-sentinel --namespace mokka
nvkectl delete nodepool mokka-sentinel --cluster <WORKLOAD_CLUSTER>
```

Then GPU Operator, as in [`demo.sh`'s clean-up](../README.md#clean-up).
