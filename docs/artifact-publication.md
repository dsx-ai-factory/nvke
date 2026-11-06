# Artifact publication reference

This document describes NVKE's existing artifact build and publication
workflows. The source-only OSS release procedure and its validated SBOM
requirements are in [RELEASE.md](../RELEASE.md). Artifact publication is not a
requirement of that source-release deliverable. Paths and shell commands below
are relative to the repository root. The workflow behavior described here is
not disabled by the source-only documentation scope.

## Versioning

NVKE uses [Semantic Versioning](https://semver.org/):

```
v<MAJOR>.<MINOR>.<PATCH>
```

| Increment | When |
|---|---|
| `PATCH` | Backwards-compatible bug fixes |
| `MINOR` | New backwards-compatible features |
| `MAJOR` | Breaking API or behavioral changes |

What counts as breaking differs per component, so each states its own surface.
For `nvkectl` that is [the CLI compatibility policy](../cli/docs/compatibility.md),
which lists the commands, flags, exit codes and output formats these increments
apply to, and the deprecation sequence a removal has to go through first.

## Release cadence

There is no fixed release schedule. Releases are made when a meaningful
set of features or fixes has accumulated on `main`, or when a security
patch requires an immediate out-of-band release.

## Creating a release

### 1. Tag the release

A bare tag releases the whole repository:

```bash
git tag -s v0.2.0 -m "Release v0.2.0"
git push upstream v0.2.0
```

A single component is tagged with its own directory in front:

```bash
git tag -s cli/v0.2.0 -m "Release cli/v0.2.0"
git push upstream cli/v0.2.0
```

The prefix is the module's directory because Go requires it: the module
`github.com/dsx-ai-factory/nvke/cli` can only be resolved by a tag named `cli/vX.Y.Z`.

No workflow creates a GitHub Release. A tag publishes artifacts through the
workflows below:

| Tag | Workflow | What ships |
|---|---|---|
| `agent/vX.Y.Z` | `agent-deb-publish.yml` (+ `agent-artifact.yml`) | `.deb`s to the apt intake, GHCR binary and `.deb`, NVCR binary + `.deb`, S3/CloudFront `.deb` |
| `os-image/vX.Y.Z` | `os-image-artifact.yml` | qcow2s on NVCR; qcow2s with their SBOMs and checksums, and `nvke-os-image-check`, on S3/CloudFront; version-tagged qcow2s on GHCR only when `NVKE_GITHUB_PUBLISH` names `os-image` |
| `nktp/vX.Y.Z` | `nktp-chart-publish.yml` | Helm chart on GHCR |
| `cli/vX.Y.Z` | `tag-publish.yml` (+ `cli-artifact.yml`, `cli-artifact-nvcr.yml`, `cli-s3-publish.yml`) | binaries on GHCR, NVCR, S3/CloudFront |
| `controller/vX.Y.Z` | `image-nvcr.yml`, `chart-publish.yml`, `chart-publish-nvcr.yml` | image and chart; see below |

A bare `vX.Y.Z` tag publishes nothing; `tag-publish.yml` runs only the license
checks for it.

### Switching GHCR publishing off

The Actions variable `NVKE_GITHUB_PUBLISH` lists the components whose
release-tagged artifacts go to GHCR. Unset, it means
`root,controller,cli,agent,cluster-api-provider-provisioned,nktp`, so every
component but `os-image`, whose qcow2s go to NVCR and S3 only. Set it to `none`
to stop every GHCR release publication, or list the components you want. NVCR, S3 and
CloudFront are not affected, and neither is the CI hand-off on GHCR: the `sha-`
and `edge` aliases of `agent` and `cli` and the `sha-` and `main-sha-` tagged
qcow2s of `os-image` keep flowing, because `agent/Makefile`, the NVCR and S3
publishers and the NICo VMs lane read them. Only the version-tagged aliases
stop. The variable must be a repository or organization variable; an
environment variable is not visible to the job conditions.

Set it under the repository's Settings, Actions, Variables, or with
`gh variable set NVKE_GITHUB_PUBLISH --body 'controller,cli'`. An empty value
counts as unset and gives the default list; only `none` means nothing. The
names, comma-separated with no spaces:

| Name | Tag | What GHCR gets when the name is listed |
|---|---|---|
| `root` | bare `vX.Y.Z` | nothing; accepted so existing values keep working |
| `controller` | `controller/v*` | the `nvke-controller` image and the `charts/nvke-controller` chart |
| `cli` | `cli/v*` | the `cli` binaries |
| `agent` | `agent/v*` | `agent` and `agent-deb` |
| `cluster-api-provider-provisioned` | `cluster-api-provider-provisioned/v*` | the provider image |
| `nktp` | `nktp/v*` | the chart |
| `os-image` | `os-image/v*` | the version-tagged qcow2s |

An `os-image/` tag that fails Packer or the boot-smoke test never publishes the
OCI artifact.

An `nktp/` tag releases the NVKE telemetry pipeline (NKTP) Helm chart. The chart version is the tag's:
`nktp/vX.Y.Z` packages the chart as `X.Y.Z` and `nktp/vX.Y.Z-rc.1` as
`X.Y.Z-rc.1`, the way the NVKE controller chart takes its version from
`controller/v*`. `version:` in `nktp/deploy/charts/nktp/Chart.yaml` is a
placeholder, so nothing needs editing before tagging. The workflow pushes
`oci://ghcr.io/dsx-ai-factory/nvke/charts/nktp:<version>`.

A `controller/vX.Y.Z` tag publishes the controller image and Helm chart:

| Workflow | What it publishes |
|---|---|
| `image-nvcr.yml` | multi-arch image to `nvcr.io/<org>/nvke-controller`, promotes that released image to DSX as `nvcr.io/<dsx-org-team>/nvke-controller`, and promotes release tags to `ghcr.io/dsx-ai-factory/nvke/nvke-controller` |
| `chart-publish.yml` | chart to `ghcr.io/dsx-ai-factory/nvke/charts/nvke-controller` |
| `chart-publish-nvcr.yml` | chart to `nvcr.io/<org>/nvke-controller-chart` and `nvcr.io/<dsx-org-team>/nvke-controller-chart` |

Every `main` commit already publishes the image and chart to NVCR. GHCR receives
released versions only, which is why a chart built from a tag can safely point at
`ghcr.io/dsx-ai-factory/nvke/nvke-controller:<version>` while a chart built from `main` could not.
Releases before the repository moved to `dsx-ai-factory` on 2026-09-12 were promoted to
`ghcr.io/nvidia/nke/controller` instead: `controller/v0.1.1-rc.1` to `rc.3` are there,
`controller/v0.1.0` predates the promotion job, and the `rc.4` promotion failed, so that
image exists nowhere. Later releases land under `ghcr.io/dsx-ai-factory/nvke`.

### Image repository and pull secret names

The controller image is `nvke-controller` on every registry. The chart is `nvke-controller`
on GHCR and `nvke-controller-chart` on NVCR. The GHCR packages for releases cut before
this rename no longer exist: the images `nke-controller` and `controller` and the charts
`charts/cloud-workflow-engine`, `charts/controller` and `charts/nke-controller`. Nothing
was republished under the new name. GHCR holds the controller from `controller/v0.3.0-rc.1`
on, so a pin on an earlier tag or on `nke-controller` does not resolve there, and the
hardware lane cannot qualify an earlier release from GHCR.

The pull secret `svc-nke-ngc-imagepull` keeps its name. It is an NGC service account
provisioned outside this repository, so a rename means re-provisioning it on every site in
step with the chart reference. `chart-publish-nvcr.yml` adds it only to the NVCR-published
chart; the GHCR chart defaults to no pull secret.

A new GHCR package is created `internal`. Make it public in the package settings after the
first release that publishes it.

DSX receives the controller image from `image-nvcr.yml`, after the NVKE multi-arch
manifest exists, and the controller chart from `chart-publish-nvcr.yml`. It
does not receive the site validation image (`controller/test/e2e/Dockerfile`),
which is published nowhere: a site builds it from its controller's release tag
(see "Building the image yourself" in `controller/test/e2e/README.md`). Both
workflows use separate credentials from the same protected GitHub environment:

| Name | Type | Purpose |
|---|---|---|
| `NVKE_NGC_ORG` | variable | Source NGC org for `nvcr.io/<org>/nvke-controller` |
| `NGC_REGISTRY_TOKEN_NVKE` | secret | Read/write token for the source NVKE NGC org |
| `DSX_NGC_ORG_TEAM` | variable | Destination NGC org/team, for example `<org>/components-dev` |
| `NGC_REGISTRY_TOKEN_DSX` | secret | Write token for the DSX NGC org/team |

The source and destination both live on `nvcr.io`, but they may require different
NGC tokens. Docker auth stores one credential per registry host, so the DSX
promotion pulls the source image into a local OCI layout with the NVKE token, then
logs in with the DSX token and pushes that same index to
`nvcr.io/<dsx-org-team>/nvke-controller`. Immutable tags
(`sha-<commit>` and the git-described version) are treated as idempotent only
when an existing destination digest matches the source digest; a mismatch fails
the run. The `edge` tag is mutable and moves only on `main`.

The DSX chart is packaged from the controller chart as `nvke-controller-chart`,
the same chart artifact name used for the NVKE NVCR chart, and pushed to
`oci://nvcr.io/<dsx-org-team>`. Its chart version is the controller git describe
string without the leading `v`, while `appVersion` keeps the leading `v` so the
rendered Deployment pulls the same image tag promoted by `image-nvcr.yml`.

Push tags to `upstream`. A tag pushed to a fork still starts `tag-publish.yml`
there, which runs the license checks (and, for a `cli/` tag, builds nvkectl),
but publishes nothing: every publisher guards its publish jobs on this
repository's ID.

### 2. Verify artifacts

After the tag push, confirm that any CI release pipelines complete
successfully (node OS-image bake, chart publish, etc.). For `os-image/v*`, that is
the **OS Image Artifact** check: Packer, boot-smoke, then the NVCR and S3
publishers (GHCR only when `NVKE_GITHUB_PUBLISH` names `os-image`). For `controller/v*`, confirm the **Image
NVCR** check, and, when `NVKE_GITHUB_PUBLISH` names `controller`, the **Chart
Publish** check and the promoted image at
`ghcr.io/dsx-ai-factory/nvke/nvke-controller:<version>`. For `nktp/v*`, confirm the
**NKTP Chart Publish** check.

## Verifying a published image

Container images carry a
[build-provenance attestation](https://docs.github.com/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds)
over their index digest. It records the workflow, the ref and the commit that
built the image.

```bash
gh attestation verify \
  oci://ghcr.io/dsx-ai-factory/nvke/nvke-controller@sha256:<digest> \
  --repo dsx-ai-factory/nvke \
  --bundle-from-oci
```

`--bundle-from-oci` reads the attestation attached to the GHCR image. Without
it, the command fetches attestations from GitHub instead.

## Software bills of materials

The artifacts listed below have SBOMs; Helm charts have no SBOM of their own.
The format is **SPDX 2.3, as JSON**,
generated by [syft](https://github.com/anchore/syft) at the version pinned in
`make/tool-versions.mk` — `make print-SYFT_VERSION`. One document describes one
artifact, and every document is generated from the built artifact rather than
from this source tree, so it records what was actually built and not what a
build of this commit ought to select.

| Artifact | Document | Subject granularity | Where |
|---|---|---|---|
| `nvke-node-{fde,non-fde}-ubuntu-<rel>-<arch>.qcow2` | `nvke-node-…-<arch>.spdx.json` | dpkg packages, from the image's own package database | S3, beside the disk |
| `nvke-controller` image | `controller-sbom-linux-<arch>.spdx.json` | image contents, scanned from the pushed image | workflow artifact on the **Image NVCR** run |
| `cluster-api-provider-provisioned` image | `cluster-api-provider-provisioned-sbom-linux-<arch>.spdx.json` | image contents, scanned from the pushed image | workflow artifact on the **Image Provisioned NVCR** run |

The Helm charts — `nvke-controller` and `nktp` — have no SBOM of their own. A
chart carries no components; it carries a reference to an image that does, and
that image's SBOM is the row above. An SBOM of the chart archive would list the
archive.

The granularity differs by artifact because the useful question differs. For a
node image it is "does this image carry <component>", and everything that
reaches the image — archive packages and our own `.deb`s alike — is a dpkg
entry, so the document is the image's package set. For a container image it is
the image's contents.

`nvkectl` and the agent `.deb`s have no published SBOM.

### Retrieving one

A node-image SBOM sits on S3 beside its disk, with the same name as the disk
and `.spdx.json` in place of `.qcow2`.

Answering "which of our artifacts contains `<component>`" is then a query over
the downloaded documents, for example with `jq`:

```bash
jq -r --arg c openssl \
  '.packages[] | select(.name | test($c)) | "\(.name) \(.versionInfo)"' \
  *.spdx.json
```

The two container images are scanned by their publish workflows, so their
documents are build artifacts on the run —
`gh run download <run-id> --name controller-sbom-linux-amd64.spdx.json`.

Verify images **by digest, not by tag**. A tag is a mutable pointer; the
attestation is over the digest, so a tag that moves no longer names the thing
that was attested. `crane digest <image>:<tag>` resolves the current one.

`--repo` is not optional decoration. It is what binds the attestation to *this*
repository; without an identity constraint you would only be establishing that
the artifact was built by some GitHub workflow somewhere. Use
`--signer-workflow` to pin it tighter still.

### What is attested

| Artifact | Attested subject | Where the attestation lives |
|---|---|---|
| `nvke-controller`, `cluster-api-provider-provisioned` on GHCR | the multi-arch index digest | attached to the image in GHCR as an OCI referrer |
| the same two images on NVCR | the same index digest | GitHub's attestation store only |

Attestations are made where the artifact lands. `crane copy` preserves an image
digest but does not carry its referrers, so each GHCR promotion attests the
image it just pushed rather than relying on the source.

### Verification before promotion

Promotion is the moment a candidate becomes authoritative, so the promotion
jobs verify rather than assume. `.github/actions/verify-attestation` wraps
`gh attestation verify` and fails the job on missing, unverifiable or
mismatched evidence; `image-nvcr.yml` and `image-provisioned-nvcr.yml` call it
in three places:

1. **Before the GHCR copy** and **before the DSX promotion**, over the NVCR
   index digest the `manifest` job published and attested. Nothing is copied
   until that passes.
2. **After the GHCR copy**, with `--bundle-from-oci`, so the check reads the
   referrer GHCR serves — the same path a consumer takes — rather than the
   GitHub API, which would have answered from the NVCR attestation over the
   same digest and proved nothing about what landed.

Each call pins the repository, the signer workflow, the source commit and the
source ref, and re-reads the attested subject digest out of the verified
statement. Mutable aliases — `sha-<commit>` on GHCR, `edge` on DSX — move only
after every gate above them has passed, so an alias never points at an image
the run has not finished vouching for. Nothing in either path rebuilds the
artifact it promotes: `crane copy` and `crane push` move the index the
`manifest` job created, and the digest is asserted equal on the far side.

It fails closed.

### Why GitHub attestations rather than cosign signatures

Both are keyless Sigstore under the hood — a Fulcio certificate issued against
the GitHub Actions OIDC token, recorded in a public transparency log, with no
signing key for this project to hold, rotate or leak. The differences that
decided it:

- **A build-provenance attestation is strictly more than a signature.** A
  cosign `sign` establishes that an identity vouched for some bytes. An
  attestation establishes what workflow built them, from which source revision.
  That is the same artifact the provenance work needs, so it is produced once.
- **`gh attestation verify --repo` is identity-safe by construction.** A bare
  `cosign verify` with no `--certificate-identity` accepts any valid Sigstore
  signature from anyone, which proves only that *something* signed it. The
  common command should not be the unsafe one.
- **GHCR is the public distribution point**, and it serves the OCI referrers
  API, so `push-to-registry` puts the attestation beside the image and
  verification needs the registry alone.

**No SLSA level is claimed.** Doing so requires evaluating every requirement for
a specific version and track, which has not been done.

### Not yet covered

The NVCR and DSX copies of the two images carry **no attestation in the
registry**. NVCR does not implement the OCI referrers API —
`/v2/<repo>/referrers/<digest>` returns a route-level 404 rather than an OCI
error — so `push-to-registry` has nowhere to put the bundle. The provenance for
the NVCR index exists in GitHub's attestation store, which is what the
promotion gates above read, and it can be verified by hand the same way:

```bash
gh attestation verify \
  oci://nvcr.io/<org>/nvke-controller@sha256:<digest> \
  --repo dsx-ai-factory/nvke \
  --signer-workflow dsx-ai-factory/nvke/.github/workflows/image-nvcr.yml
```

That needs network access to GitHub, so it is not a substitute for a bundle
beside the image: an air-gapped or offline consumer of the NVCR or DSX copies
still has nothing local to check. The DSX copy has no attestation of its own at
all — it shares the index digest, so the command above covers it, but only
because the digests match. Closing this properly needs either referrers support
from NGC or a second mechanism for those registries.

The other artifacts NVKE publishes — the Helm charts, the `agent` and `cli`
OCI artifacts, `agent-deb`, the `os-image` disks and their S3 copies — are not
attested. That is follow-up work along the pattern in `image-nvcr.yml`.

## Hotfix releases

For security patches that cannot wait for the next regular release:

1. Branch from the tag that needs patching: `git checkout -b fix/v0.1.1 v0.1.0`
2. Cherry-pick or apply the fix.
3. Follow steps 1–2 above with the patch version incremented.
4. Backport to `main` if the fix applies there too.

## Pre-releases

For release candidates, append `-rc.N` to the version: `v0.2.0-rc.1`, or
`cli/v0.2.0-rc.1` for one component. Tag and push the same way.

A root release candidate can also be cut without anyone pushing it. When the
Qualify e2e run a nightly beta tag started passes every stream at scope
`golden-path` and stage `full`, `.github/workflows/rc-promotion.yml` tags
the beta's commit as the next `vX.Y.Z-rc.N` of the beta's version. A dispatched
or pull request run of Qualify e2e never promotes, and a re-run does not tag a
beta that an RC already names. It pushes only while the `NVKE_AUTO_RC`
repository variable is `true`; otherwise the run summary says what it would
have tagged.

Unlike an RC a person pushes with `git tag -s`, the automatic RC is an unsigned
lightweight tag, as the nightly betas are: the workflow has no key to sign
with. It marks a candidate that qualified, not a release, so `git verify-tag`
does not apply to it. The signed annotated tag
[RELEASE.md](../RELEASE.md#publish-and-verify) requires is for the approved
release, which the release owner creates.

**The dot in `-rc.N` is load-bearing.** The publishing workflows reject
`v0.2.0-rc1`. The dot also gives the right ordering: `rc.2` sorts before
`rc.10`, while `rc2` sorts after `rc10`, because SemVer compares an all-digit
identifier as a number and anything else as text.
