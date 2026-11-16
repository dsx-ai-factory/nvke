# Publishing Cluster Service packages

The Cluster Service publisher builds the manager, tenant API and operator API
from one source commit, then packages the chart with those images pinned by
digest. The chart includes all six state CRDs and its RBAC. State-only installs
use the same package with runtime components disabled.

## Artifacts and versions

The workflow is [Cluster Service Publish](../../.github/workflows/cluster-service-publish.yml).
It uses the existing protected `nvcr` environment and registry credentials,
provenance verification, SPDX generation and digest inventory described in the
[NVKE publication reference](../../docs/artifact-publication.md).

Each destination receives:

| Artifact | Repository under `nvcr.io/<org-or-org/team>` | Platforms |
| --- | --- | --- |
| Manager image | `cluster-service-manager` | linux/amd64, linux/arm64 |
| Tenant API image | `cluster-service-tenant-api` | linux/amd64, linux/arm64 |
| Operator API image | `cluster-service-operator-api` | linux/amd64, linux/arm64 |
| Helm chart, including state | `cluster-service` | Architecture independent |

The NVKE destination comes from `NVKE_NGC_ORG`, and the DSX destination from
`DSX_NGC_ORG_TEAM`. Their existing `NGC_REGISTRY_TOKEN_NVKE` and
`NGC_REGISTRY_TOKEN_DSX` credentials stay in that environment. Registry owners
must provision these repositories and permit the corresponding service accounts
to list, pull and push them before the first publication. No registry identifiers
or credentials belong in source control.

The workflow runs for relevant changes on `main`, component release tags, or a
manual dispatch on one of those refs. It rejects arbitrary branch dispatches and
malformed component tags before building. Fork and PR events cannot publish.

| Source | Image version | Chart version |
| --- | --- | --- |
| Main commit | `v0.0.0-sha.g<full-commit>` | `0.0.0-sha.g<full-commit>` |
| `management-engine/v0.3.0-rc.1` | `v0.3.0-rc.1` | `0.3.0-rc.1` |
| `management-engine/v0.3.0` | `v0.3.0` | `0.3.0` |

The tag prefix retains the existing Go module path. Published names use Cluster
Service. Root tags and other components' tags do not publish these packages.
This publisher targets NVCR and DSX; it adds no GHCR publication or automatic
release qualification. An RC is a package candidate, not proof of deployment or
qualification. Coordinate its version and creation with the release owners.

## What a successful run establishes

1. Native builders push each runtime's two architectures by digest, with no
   installation tag pointing at an incomplete image.
2. Each index is assembled from those exact digests and checked for both Linux
   platforms. GitHub provenance is recorded for its digest before a version tag
   is published. Per-architecture SPDX documents describe the built images.
3. The DSX promotion verifies the index's signer workflow, source ref and commit,
   copies all platforms without rebuilding, and checks the destination digest.
4. The chart packager requires all three receipts to agree on version, commit,
   destination and architectures. It pins the images by digest and records the
   source revision in the chart. The packaged `release.json` carries those
   references alongside the state schemas.
5. The existing `record-digest` action records each published image and chart.
   The run retains image receipts for 90 days. These records identify packages;
   they do not add Cluster Service to the existing workload qualification matrix.

Builds normalize timestamps to the source commit using
[Docker’s reproducible-build inputs](https://docs.docker.com/build/ci/github-actions/reproducible-builds/).
Version tags are not overwritten. A retry accepts an existing image only when
its digest matches. An existing chart must have identical archive contents;
archive timestamps may differ. Lookup errors fail closed. The workflow does not
publish `latest` or move an `edge` alias.

The existing NVCR attestation limitation also applies here: image provenance is
stored in GitHub, rather than as NVCR OCI referrers. Charts have no separate
attestation; image provenance and the package's source/image record are distinct
from a chart signature.

## DSX handoff

Use the DSX chart repository and exact chart version from the completed run.
The DSX chart defaults reference the DSX copies of all three images by digest,
with `ngc-dsx-registry-creds` as the existing pull Secret. The NVKE package uses
`svc-nke-ngc-imagepull`. Provision that Secret in each workload namespace or
supply the appropriate chart overrides; packaging does not issue credentials.

For example, after replacing the placeholders with the run's actual coordinates:

```sh
helm pull oci://nvcr.io/ORG/TEAM/cluster-service --version 0.3.0-rc.1
helm show values cluster-service-0.3.0-rc.1.tgz
helm template cluster-service cluster-service-0.3.0-rc.1.tgz --namespace cluster-service-system -f site-values.yaml
```

`site-values.yaml` supplies the NVKE controller endpoint, infrastructure-template
ConfigMap and the selected runtime/identity/network settings described in the
[installation guide](helm-installation.md). APIs remain disabled by default.
The workflow's `.invalid` backend is a render-test input, not a packaged default.

The SBOM integration must select the published Helm package rather than a Git
chart path. Cluster enablement, Argo CD repository access, site configuration,
live validation and rollback rehearsal follow in their deployment workstreams.

## Local validation

`make test-unit` includes packaging and publication-boundary tests. The chart
CI job also runs the packager's real Helm render test with the pinned Helm.
Tests reject missing/mismatched receipts, unsupported architecture sets,
conflicting versions and registry lookup errors without publishing anything.

To stage a package from receipts downloaded from a successful run, from
`management-engine/`:

```sh
make release-package RECEIPTS=/path/to/receipts IMAGE_REPOSITORY=nvcr.io/ORG/TEAM VERSION=v0.3.0-rc.1 REVISION=FULL_SOURCE_COMMIT OUTPUT=/path/to/new-staging-directory IMAGE_PULL_SECRET=ngc-dsx-registry-creds
helm package /path/to/new-staging-directory
```

The output directory must be new, so a previous run cannot contribute stale
files. The source checkout and generated chart remain unchanged.

Created with AI assistance
