# Publishing Cluster Service packages

Cluster Service uses the existing NVKE publication conventions: GitHub Actions
builds the manager, tenant API and operator API images, then runs Helm to package
and push their chart. The chart includes the six state CRDs and RBAC; state-only
installs disable the runtime components.

## Artifacts and destinations

[Cluster Service Publish](../../.github/workflows/cluster-service-publish.yml)
uses the protected `nvcr` environment, native amd64/arm64 runners and the existing
registry credentials, provenance verification, SPDX generation and digest inventory.
See the [NVKE publication reference](../../docs/artifact-publication.md).

| Artifact | Repository under `nvcr.io/<org-or-org/team>` |
| --- | --- |
| Manager image | `cluster-service-manager` |
| Tenant API image | `cluster-service-tenant-api` |
| Operator API image | `cluster-service-operator-api` |
| Helm chart, including state | `cluster-service` |

Images and charts publish only to NVCR. This workflow adds no GHCR publication
or GitHub Release assets. The destinations come from `NVKE_NGC_ORG` and
`DSX_NGC_ORG_TEAM`; credentials come from `NGC_REGISTRY_TOKEN_NVKE` and
`NGC_REGISTRY_TOKEN_DSX`. Before first publication, registry owners must confirm
repository visibility and provision the service accounts' list/pull/push access.
NVCR hosting alone does not establish private access.

SBOMs, digest records and image provenance remain in GitHub Actions, following
the existing publishers. This source repository is public, so that evidence
must not contain confidential information. Registry credentials stay in the
protected environment and must never enter these records.

## Versions and publication flow

Relevant changes on `main`, repository release tags (`vX.Y.Z`) and nightly beta
tags (`vX.Y.Z-beta.N`) trigger publication. Manual dispatch accepts main or a
repository version tag. Forks, PRs and arbitrary branch dispatches cannot publish.
Component-specific `management-engine/v*` tags do not trigger this workflow.

Versions come from the shared `release/hack/describe-version.sh`, using full Git
history. Tagged builds use their repository tag; main builds use the repository's
`git describe` version, such as `v0.3.0-4-gabcdef`. Helm chart versions drop the
leading `v`; `appVersion` and image tags retain it. Beta artifacts stay in the
NVKE destination. Main and release builds also publish to DSX, matching the
existing NVCR publishers. A release candidate tag does not independently cut a
new Cluster Service release; follow the repository pre-release procedure.

Each successful run:

1. Builds each image for linux/amd64 and linux/arm64 and assembles the exact
   architecture digests into a multi-architecture index.
2. Records image provenance and generates per-architecture SPDX documents.
   Before DSX promotion, verifies the index's source commit, ref and signer,
   then copies it without rebuilding.
3. Checks the destination image receipts match the source revision, version and
   architecture set. Updates the chart's three image defaults to those digests
   and supplies the destination's existing pull Secret. APIs stay disabled.
4. Runs `helm dependency build`, `helm lint`, `helm package` and a render check
   against the packaged archive, then `helm push`. The existing `record-digest`
   action records the chart and image references for consumers.

Image tags reject conflicting digests and registry lookup errors. Chart upload
uses the same Helm push flow as the controller chart; this workflow adds no
separate chart archive comparison or release-packaging program. Image builds
normalize timestamps to the source commit using
[Docker's reproducible-build inputs](https://docs.docker.com/build/ci/github-actions/reproducible-builds/).
The workflow does not publish `latest` or move an `edge` alias.

## DSX handoff

Select the DSX chart repository and exact version from a completed run. Its
image defaults point to the DSX image digests and use `ngc-dsx-registry-creds`.
The NVKE chart uses `svc-nke-ngc-imagepull`. Supply the selected Secret in each
workload namespace, or override the chart's pull-Secret values.

After replacing placeholders with the published coordinates:

```sh
helm pull oci://nvcr.io/ORG/TEAM/cluster-service --version 0.3.0-4-gabcdef
helm show values cluster-service-0.3.0-4-gabcdef.tgz
helm template cluster-service cluster-service-0.3.0-4-gabcdef.tgz --namespace cluster-service-system -f site-values.yaml
```

`site-values.yaml` supplies the backend endpoint, infrastructure-template
ConfigMap and the identity/network settings in the
[installation guide](helm-installation.md). The workflow's `.invalid` backend is
only a render-test input. Site enablement, API registration, live validation and
rollback rehearsal remain deployment work; publication does not qualify a release.

## Validation

`make test-unit` checks the publication ref boundary, shared version resolution
and image-tag conflict handling without registry access. The publication job
validates image receipts and renders the packaged chart with all three workloads
to check their digest references before upload. Actual publication requires the
protected credentials and destination access described above.

Created with AI assistance
