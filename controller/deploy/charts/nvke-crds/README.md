# Shared NVKE catalog CRDs

This chart installs MachineImage, ClusterRelease and NodePoolRelease, the catalog
APIs shared by NVKE components. It contains their canonical generated definitions
and runs no workloads.

## Installation and ownership

Install this chart once per cluster, either as a dedicated release or through one
application's chart dependency. Other consumers should reuse those definitions.
Helm dependencies are part of their parent's release; Helm does not deduplicate
them across separately installed releases.

To install a dedicated release from a repository checkout:

```sh
helm upgrade --install nvke-catalog controller/deploy/charts/nvke-crds \
  --namespace nvke-system --create-namespace
```

The definitions are cluster-scoped. Wait for all three CRDs to reach the
`Established` condition before creating catalog resources. Consumers must use
compatible schema versions. See each application's installation guide for how
to enable its dependency or use an existing owner; the [NVKE controller chart](../controller/README.md#catalog-crds)
documents its configuration. This chart's `enabled=false` suppresses all three
definitions when used as a dependency.

## Transferring ownership

To move existing definitions to a dedicated release:

1. Identify their current Helm release metadata and every installer that manages
   them. Back up catalog records and select compatible chart and workload versions.
2. Verify the definitions have `helm.sh/resource-policy: keep`. Disable catalog
   installation in the current owner's configuration and upgrade that release,
   preserving its other values. Verify the CRDs and records remain available.
3. After the former installers are disabled, install this chart with Helm's
   `--take-ownership` option. Inspect its rendered resources first: the option
   deliberately adopts existing objects into the new release.
4. Verify the new owner and existing records. Keep catalog installation disabled
   in former owners on subsequent upgrades and rollbacks.

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

Generate from `management-engine/apis` using its pinned controller-gen:

```sh
make -C management-engine manifests-state
make chart-crds-check
make chart-check
```

`make chart-crds` renders the catalog definitions into ignored
`dist/release-crds.yaml` for tools such as Tilt. Helm and Kustomize read the
canonical files under `bases/`; generated bundles and dependency archives are
not tracked. The CLI embeds the same chart sources.
