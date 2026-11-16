# NVKE controller release CRDs

This chart installs MachineImage, ClusterRelease and NodePoolRelease, the release
APIs the NVKE controller reads to resolve releases. The NVKE controller owns these APIs. The definitions are generated from the Go
types in [`controller/k8s-apis/v1`](../../../k8s-apis/v1)


## Installation

The [NVKE controller chart](../controller/README.md#release-crds) installs this
chart as a dependency by default (`releaseCRDs.install=true`), so a standard
controller install needs nothing else.

To manage the definitions in their own Helm release, disable the dependency in
the controller chart and install this chart directly:

```sh
helm upgrade --install nvke-crds controller/deploy/charts/nvke-crds \
  --namespace nvke-system --create-namespace
```

The definitions are cluster-scoped. Wait for all three CRDs to reach the
`Established` condition before creating release resources. Consumers must use
compatible schema versions. See each application's installation guide for how
to enable its dependency or use an existing owner; the [NVKE controller chart](../controller/README.md#release-crds)
documents its configuration. This chart's `enabled=false` suppresses all three
definitions when used as a dependency.

## Versions, upgrades and retention

Bump this chart's version when schemas or chart behaviour change. Consumers must
update their dependency versions and lock files together. The chart is versioned
independently of workload images.

Definitions are templated Helm resources with `helm.sh/resource-policy: keep`.
Helm can update their schemas during an upgrade and retains them on uninstall.
Argo CD retention and CRD-first sync annotations are also included. Review schema
compatibility and back up records before upgrades; rollback does not undo data
migrations. Deleting a CRD deletes its records.

CRDs live outside Helm's special `crds/` install-only directory so schema upgrades
can be managed by the owning release.

## Development

Generate from [`controller/k8s-apis`](../../../k8s-apis) using its pinned controller-gen:

```sh
make -C controller/k8s-apis generate manifests
make chart-crds-check
make chart-check
```

`make chart-crds` renders the release definitions into ignored
`dist/release-crds.yaml` for tools such as Tilt. Helm and Kustomize read the
canonical files under `bases/`; generated bundles and dependency archives are
not tracked. The CLI embeds the same chart sources.
