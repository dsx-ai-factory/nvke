# Shared NVKE CRDs

This chart installs NVKE's Kubernetes API definitions. It runs no controllers.
The generated YAML under `bases/` is the canonical schema copy: Helm renders
those files and Management Engine's Kustomize base references the same directory.

| Schema set | Types |
| --- | --- |
| Always installed | MachineImage, ClusterRelease, NodePoolRelease |
| `clusterServiceCRDs.enabled=true` (default) | NVKECluster, ControlPlaneClass, ClusterReleaseChannel |

## Installers and ownership

Choose one installer for each CRD in a cluster. Existing controller installations
keep their release as the owner: the controller chart includes this chart as a
dependency with `clusterServiceCRDs.enabled=false`, preserving its three-schema
default. Existing release names and schema specifications are unchanged.

Before adding Cluster Service on that cluster, enable
`nvke-crds.clusterServiceCRDs.enabled=true` in the controller's values, after
checking ownership of the additional definitions. The Management Engine chart
then consumes the installed APIs. For a separate state cluster, install this
chart independently there.

A new installation can instead give schema ownership to a dedicated release:

```sh
helm upgrade --install nvke-crds controller/deploy/charts/nvke-crds \
  --namespace nvke-system --create-namespace
```

Configure `releaseCRDs.install=false` on the controller when using this model.
Both applications then reuse the schema release; installing an enabled copy as
a dependency of each independent application release would create competing owners.

## Move an existing owner

Ownership transfer is an operator-controlled migration:

1. Identify the installed schemas, their current Helm release metadata and all
   installers that might update them. Back up existing custom resources and
   select compatible chart and workload versions.
2. Verify the existing schemas have `helm.sh/resource-policy: keep`. Upgrade the
   current controller release with CRD installation disabled, keeping its other
   site values. This retains the definitions and records while removing them
   from that release's desired manifests.
3. After every former installer is disabled, install the dedicated schema release
   with Helm's `--take-ownership` option. Audit its rendered object set first;
   this option deliberately changes the ownership of existing objects.
4. Confirm all schemas and records remain available before upgrading consumers.
   Keep the controller's CRD dependency disabled on subsequent upgrades and rollbacks.

No CRD deletion or recreation is required. This refactor performs no live migration.

## Versions, upgrades and retention

Bump this chart's version when schemas or chart behaviour change, and update the
controller's dependency version and lock file together. Pin compatible schema
and workload versions for deployment. The chart is independent of an image tag.

Definitions are ordinary Helm resources with `helm.sh/resource-policy: keep`;
Helm can apply schema updates during an upgrade and retains definitions when a
release or schema set is removed. Existing custom resources therefore survive.
Argo CD retention and CRD-first ordering annotations are also included. Inspect
schema compatibility before upgrades; a rollback does not undo data migrations.

CRDs intentionally live outside Helm's special `crds/` install-only directory,
which does not perform schema upgrades. Registration must complete before
installing resources that use these APIs. Deleting a CRD deletes its records;
retain it when retiring an application.

## Development

Generate from `management-engine/apis` through its pinned controller-gen:

```sh
make -C management-engine manifests-state
make chart-crds-check
make chart-check
```

`make chart-crds` renders the controller's release-only schema set into ignored
`dist/release-crds.yaml` for tooling such as Tilt. No generated bundle or vendored
dependency archive is tracked. `make chart-deps` stages the local dependency for
Helm source operations; package jobs run dependency build before packaging.
The CLI embeds canonical chart sources and assembles the dependency on extraction.
