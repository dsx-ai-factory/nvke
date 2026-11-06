# storage

A default StorageClass for a cluster that has none: Rancher's
local-path-provisioner, vendored.

PostgreSQL takes a PVC in the [`nvke`](../nvke/) chart and again in
[`nico`](../nico/). Without a default StorageClass both sit `Pending` forever,
and nothing in the stack says why.

```bash
helm dependency build deploy/charts/storage
helm upgrade --install nvke-storage deploy/charts/storage \
  --namespace nvke-storage --create-namespace
```

## Local storage is a lab default

A local-path volume is a directory on one node. It does not move, does not
replicate, and does not survive the node. Good enough for a development
cluster or a single-node site; not what a cluster holding the NVKE controller's
database should run on.

A cluster with real storage installs the CSI driver for that storage and
leaves this chart out. The deploy path makes the same call for the same
reason: it installs local-path for the local site only, and treats the
StorageClass as a property of the hardware rather than of NVKE.

## Two defaults is not a configuration

Kubernetes has no rule for choosing between two default StorageClasses, and a
PVC that names no class may be bound by either. `check-site.sh` fails a cluster
that has more than one.

`templates/validate.yaml` looks for an existing default and refuses to add a
second:

```
the cluster already has a default StorageClass, "standard", and this chart
would add a second ("local-path").
```

Three ways out, in the message: leave the chart out, set
`local-path-provisioner.storageClass.defaultClass=false` to install the class
without making it default, or set `refuseSecondDefaultClass=false` to replace
the existing default deliberately.

The check reads the live cluster, so it finds nothing during `helm template`
or `--dry-run`. It guards an install, not a render.

## On kind

kind already ships this: Rancher's provisioner in `local-path-storage`, with
the class named `standard`. The chart is off in the umbrella for that reason,
and the guard above refuses it if you try. Releasing into `local-path-storage`
also fails on its own, because kind's objects there carry no Helm ownership
metadata. See [`values-kind.yaml`](values-kind.yaml) for the commands to use it
on kind anyway.

## The chart is vendored

Rancher publishes no Helm repository. The chart lives inside the provisioner's
own git repository under `deploy/chart`, and a Helm dependency cannot name a
git reference. Third-party republishes of it exist on public chart
repositories; this chart does not depend on one.

So it is vendored verbatim into [`vendor/`](vendor/), Apache-2.0, with its
LICENSE. Refresh it with:

```bash
deploy/charts/storage/hack/sync-chart.sh [version]
```

The default version is `LOCAL_PATH_PROVISIONER_VERSION` from
`make/tool-versions.mk` — currently `v0.0.31` — so this chart and
`deploy/nvke-deploy-local.sh` install the same provisioner. Bump both together.

## Namespace

The vendored chart takes its namespace from the release namespace, with no
override, so the provisioner runs in whichever namespace this chart is
released into. That is harmless: the StorageClass is cluster-scoped and the
provisioner does not care where it runs.

## Values worth knowing

| Value | Default | Purpose |
|---|---|---|
| `refuseSecondDefaultClass` | `true` | Refuse to add a second default StorageClass |
| `local-path-provisioner.storageClass.name` | `local-path` | Matches the deploy path's local site |
| `local-path-provisioner.storageClass.defaultClass` | `true` | The point of the chart |
| `local-path-provisioner.storageClass.volumeBindingMode` | `WaitForFirstConsumer` | Places the volume on the node the pod lands on |
| `local-path-provisioner.storageClass.reclaimPolicy` | `Delete` | `Retain` to keep data after the PVC goes |
| `local-path-provisioner.nodePathMap` | `/opt/local-path-provisioner` | Must have room for every PVC the cluster creates |
