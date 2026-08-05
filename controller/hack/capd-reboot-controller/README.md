# CAPD reboot controller (dev)

Small **dev-only** controller that reconciles CAPD `DockerMachine` objects and watches their owner CAPI `Machine` objects for the reboot annotation (`nke.nvidia.com/reboot`). When it sees a new reboot request on the `Machine`, it simulates a reboot by waiting for a configured duration and then signals completion to CWE via the `nke.nvidia.com/reboot-complete-timestamp` annotation on that same CAPI `Machine`.

## Enable in Tilt

This provider now loads by default as a path-based provider from `./hack/capd-reboot-controller`.

If you override `providers` in `tilt_config.json`, include the provider entry explicitly to keep it enabled:

```json
{
  "providers": [
    "capi_core",
    "capd",
    {
      "capd-reboot-controller": "./hack/capd-reboot-controller"
    }
  ]
}
```

The provider deploys the dev controller and makes CWE wait for it before becoming ready.

## Prerequisites

- `kubectl` with a kubeconfig for the **management** cluster (where CAPD runs).

## Configure the reboot delay

Tilt declaratively manages the `capd-reboot-controller-config` `ConfigMap`. By default it sets `CAPD_REBOOT_CONTROLLER_SLEEP_DURATION=10s`.

Configure this in the main CWE repo root `tilt_config.json`, not in `hack/capd-reboot-controller/`.

To override it, set `capd_reboot_controller_sleep_duration` in `tilt_config.json`:

```json
{
  "capd_reboot_controller_sleep_duration": "30s"
}
```

When that value changes, Tilt updates the `ConfigMap`, but the running controller does not reload the value automatically. Re-deploy the `capd-reboot-controller` resource in Tilt, or restart the `capd-reboot-controller` Deployment, to pick up the new value.

## Run the controller

```bash
cd hack/capd-reboot-controller
go run ./cmd/capd-reboot-controller
```

Uses `KUBECONFIG` if set; otherwise `~/.kube/config`.

## Behavior

- Watches `DockerMachine` (`infrastructure.cluster.x-k8s.io/v1beta2`) in all namespaces.
- For each object, loads the owning CAPI **Machine**.
- If the owning `Machine` has `metadata.annotations["nke.nvidia.com/reboot"]`, waits for the configured sleep duration and then server-side applies `nke.nvidia.com/reboot-complete-timestamp` to that `Machine`.
- If the completion annotation is already at or after the requested reboot timestamp, skips the duplicate reconcile for that reboot request.

This module is **not** part of the main CWE `go.mod`; it is a standalone helper under `hack/`.
