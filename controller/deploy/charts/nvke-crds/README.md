# Shared NVKE catalog CRDs

This chart installs MachineImage, ClusterRelease and NodePoolRelease. These are
catalog APIs used by both NVKE controller and Cluster Service. It runs no workloads.
Cluster Service's NVKECluster, ControlPlaneClass and ClusterReleaseChannel APIs
remain with Management Engine.

## Dependencies and ownership

Choose one Helm release to own the catalog definitions in each cluster. A chart
dependency packages resources into its parent's release; Helm does not deduplicate
that dependency across independently installed releases.

For a new deployment, install one dedicated catalog release before either consumer:

```sh
helm upgrade --install nvke-catalog controller/deploy/charts/nvke-crds \
  --namespace nvke-system --create-namespace
```

Disable catalog installation in each consumer: `releaseCRDs.install=false` on the
NVKE controller and `nvke-crds.enabled=false` on Management Engine. The latter is
Management Engine's co-install default. Both still require the compatible catalog
APIs on their datastore cluster. Deployment tooling must order the catalog release
before consumers and catalog records, and wait for CRD establishment.

Existing controller installations keep their release as the owner through the
conditional dependency, preserving the original three-schema default. Management
Engine can reuse those installed APIs without an ownership transfer. Alternatively,
exactly one consumer can explicitly enable the dependency on a fresh installation.
`enabled=false` suppresses all definitions in the shared chart.

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
