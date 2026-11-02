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

### 1. Update CHANGELOG.md

`CHANGELOG.md` is generated from commit subjects by
[git-cliff](https://git-cliff.org), configured in `cliff.toml`. **Do not edit it
by hand.**

```bash
# 1. Drop the pending [Unreleased] section. Without this, --prepend leaves it
#    in place holding the same entries the new section just claimed, so the
#    file lists everything twice and keeps a stale [Unreleased] forever.
awk '/^## \[Unreleased\]/{s=1} /^## \[[0-9]/{s=0} !s' CHANGELOG.md > /tmp/cl && mv /tmp/cl CHANGELOG.md

# 2. Prepend the new section. --prepend leaves published sections byte-identical;
#    regenerating the whole file re-derives their dates and rewrites history.
git-cliff --unreleased --tag v0.2.0 --prepend CHANGELOG.md
```

Run this in a **full clone**. On a shallow one git-cliff emits a near-empty
section and exits 0, so nothing fails.

```bash
git commit -s -m "chore: update CHANGELOG.md for v0.2.0"
```

**The heading carries no `v` prefix.** `.github/workflows/release.yml` reads the
section back by that heading and does not fail when it finds nothing — it emits
a fallback line — so a mismatch ships a release with no notes rather than an
error.

A component tag never matches. `cliff.toml` sets `tag_pattern = "^v[0-9]"`, so
git-cliff writes sections for bare tags only, and a `cli/v0.2.0` release falls
back to the one-line body. Write the notes by hand afterwards if the release
needs them.

### 2. Tag the release

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
`.github/workflows/release.yml` reads the directory back off the tag and checks
it against `go.work`, so a prefix naming no module fails the run.

Some prefixes skip that workflow's GitHub Release job and cut the release
themselves, after they have built and verified the real artifacts:

| Tag | Workflow | What ships |
|---|---|---|
| `agent/vX.Y.Z` | `agent-deb-publish.yml` (+ `agent-artifact.yml`) | `.deb`s, GitHub Release assets, GHCR binary, NVCR binary + `.deb`, S3/CloudFront `.deb` |
| `os-image/vX.Y.Z` | `os-image-artifact.yml` | qcow2 on NVCR and S3/CloudFront; on GHCR, with `nvke-os-image-check` on the GitHub Release, only when `NVKE_GITHUB_PUBLISH` names `os-image` |
| `nktp/vX.Y.Z` | `nktp-chart-publish.yml` | Helm chart on GHCR and the GitHub Release |
| `cli/vX.Y.Z` | `release.yml` (+ `cli-artifact.yml`, `cli-artifact-nvcr.yml`, `cli-s3-publish.yml`) | binaries on GitHub Release, GHCR, NVCR, S3/CloudFront |

### Switching GitHub publishing off

The Actions variable `NVKE_GITHUB_PUBLISH` lists the components whose releases
go to GitHub: the GitHub Release record and assets, and the release-tagged
artifacts on GHCR. Unset, it means
`root,controller,cli,agent,cluster-api-provider-provisioned,nktp`, so every
component but `os-image`, whose qcow2s go to NVCR and S3 only. Set it to `none`
to stop every GitHub publication, or list the components you want. NVCR, S3 and
CloudFront are not affected, and neither is the CI hand-off on GHCR: the `sha-`
and `edge` aliases of `agent` and `cli` and the `sha-` and `main-sha-` tagged
qcow2s of `os-image` keep flowing, because `agent/Makefile`, the NVCR and S3
publishers and the NICo VMs lane read them. Only the version-tagged aliases, the GitHub
Releases and their attestations stop. The variable must be a repository or
organization variable; an environment variable is not visible to the job
conditions. With `os-image` off, each leg's SBOM, checksums file and the
checker binary are published to S3 beside the disk instead.

Set it under the repository's Settings, Actions, Variables, or with
`gh variable set NVKE_GITHUB_PUBLISH --body 'controller,cli'`. An empty value
counts as unset and gives the default list; only `none` means nothing. The
names, comma-separated with no spaces:

| Name | Tag | What GitHub gets when the name is listed |
|---|---|---|
| `root` | bare `vX.Y.Z` | the GitHub Release with the product manifest and SBOM |
| `controller` | `controller/v*` | the GitHub Release, the `nvke-controller` image and the `charts/nvke-controller` chart on GHCR |
| `cli` | `cli/v*` | the GitHub Release with the binaries, `cli` binaries on GHCR |
| `agent` | `agent/v*` | the GitHub Release with the `.deb`s, `agent` and `agent-deb` on GHCR |
| `cluster-api-provider-provisioned` | `cluster-api-provider-provisioned/v*` | the provider image on GHCR |
| `nktp` | `nktp/v*` | the GitHub Release and the chart on GHCR |
| `os-image` | `os-image/v*` | the GitHub Release and the version-tagged qcow2s on GHCR |

An `os-image/` tag that fails Packer or the boot-smoke test never publishes the
OCI artifact and never promotes the GitHub Release out of draft.

An `nktp/` tag releases the NVKE telemetry pipeline (NKTP) Helm chart. The chart version is the tag's:
`nktp/vX.Y.Z` packages the chart as `X.Y.Z` and `nktp/vX.Y.Z-rc.1` as
`X.Y.Z-rc.1`, the way the NVKE controller chart takes its version from
`controller/v*`. `version:` in `nktp/deploy/charts/nktp/Chart.yaml` is a
placeholder, so nothing needs editing before tagging. The workflow pushes
`oci://ghcr.io/dsx-ai-factory/nvke/charts/nktp:<version>` and writes the release
body itself, because git-cliff has no section for a component tag.

A `controller/vX.Y.Z` tag goes through `release.yml` like any other component and
additionally publishes the controller image and Helm chart:

| Workflow | What it publishes |
|---|---|
| `image-nvcr.yml` | multi-arch image to `nvcr.io/<org>/nvke-controller`, promotes that released image to DSX as `nvcr.io/<dsx-org-team>/nvke-controller`, and promotes release tags to `ghcr.io/dsx-ai-factory/nvke/nvke-controller` |
| `chart-publish.yml` | chart to `ghcr.io/dsx-ai-factory/nvke/charts/nvke-controller` |
| `chart-publish-nvcr.yml` | chart to `nvcr.io/<org>/nvke-controller-chart` and `nvcr.io/<dsx-org-team>/nvke-controller-chart` |
| `image-validate-nvcr.yml` | multi-arch site validation image (`controller/test/e2e/Dockerfile`) pushed straight to DSX as `nvcr.io/<dsx-org-team>/nvke-validate`, under the controller image's version |

Every `main` commit already publishes the image and chart to NVCR. GHCR receives
released versions only, which is why a chart built from a tag can safely point at
`ghcr.io/dsx-ai-factory/nvke/nvke-controller:<version>` while a chart built from `main` could not.
Releases before the repository moved to `dsx-ai-factory` on 2026-09-12 were promoted to
`ghcr.io/nvidia/nke/controller` instead: `controller/v0.1.1-rc.1` to `rc.3` are there,
`controller/v0.1.0` predates the promotion job, and the `rc.4` promotion failed, so that
image exists nowhere. Later releases land under `ghcr.io/dsx-ai-factory/nvke`.

### Image repository and pull secret names

The controller image is `nvke-controller` on every registry. The chart is `nvke-controller`
on GHCR and `nvke-controller-chart` on NVCR. Releases
cut before this rename were promoted to `ghcr.io/dsx-ai-factory/nvke/nke-controller`,
which stays in place so existing pins keep resolving; nothing is republished under the new
name. A pin moves to `nvke-controller` only once its tag exists there.
`qualify-e2e.yml` probes GHCR for `nvke-controller` first and falls back to `nke-controller`, so
it qualifies releases from either side of the rename.

The pull secret `svc-nke-ngc-imagepull` keeps its name. It is an NGC service account
provisioned outside this repository, so a rename means re-provisioning it on every site in
step with the chart reference. `chart-publish-nvcr.yml` adds it only to the NVCR-published
chart; the GHCR chart defaults to no pull secret.

A new GHCR package is created `internal`. Make it public in the package settings after the
first release that publishes it.

DSX receives the controller image from `image-nvcr.yml`, after the NVKE multi-arch
manifest exists. It receives the controller chart from `chart-publish-nvcr.yml`,
and the validation image that tests that controller from
`image-validate-nvcr.yml`, tagged with the same version so a site that pins a
controller can run the tests built from the same commit. All three use separate
credentials from the same protected GitHub environment:

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

Push tags to `upstream`. The release workflow has no repository guard, so a tag
pushed to a fork cuts a real release there.

### 3. Wait for the release workflow

The tag push creates the GitHub release. `.github/workflows/release.yml` builds
the component's artifacts, attaches them with a `checksums.txt`, and takes the
body from `CHANGELOG.md`. Nothing is created by hand.

### 4. Verify artifacts

After the tag push, confirm that any CI release pipelines complete
successfully (node OS-image bake, chart publish, etc.). For `os-image/v*`, that is
the **OS Image Artifact** check: Packer, boot-smoke, then the NVCR and S3
publishers (GHCR and the GitHub Release only when `NVKE_GITHUB_PUBLISH` names
`os-image`). For `controller/v*`, confirm the **Image
NVCR** check, and, when `NVKE_GITHUB_PUBLISH` names `controller`, the **Chart
Publish** check and the promoted image at
`ghcr.io/dsx-ai-factory/nvke/nvke-controller:<version>`. For `nktp/v*`, confirm the
**NKTP Chart Publish** check.

## Verifying a release

Every release attaches a `checksums.txt` and a `checksums.txt.sigstore.json`
beside it. The checksums file binds every artifact by SHA-256; the bundle is a
[build-provenance attestation](https://docs.github.com/actions/security-guides/using-artifact-attestations-to-establish-provenance-for-builds)
over that file. Verifying the attestation and then running `sha256sum -c`
covers everything the release ships, including the `os-image` qcow2s that
are distributed as OCI artifacts rather than attached. An `os-image` release
exists only when `NVKE_GITHUB_PUBLISH` names `os-image`; otherwise each leg's
`checksums-<arch>[-ubuntu24.04]-<variant>.txt` sits on S3 beside its disk,
without an attestation.

A checksums file on its own proves a download arrived intact. It does not prove
who produced it: whoever can replace the archive can replace the checksums file
next to it. The attestation is what makes the claim attributable — and it says
more than "someone vouched for these bytes", because it records the workflow,
the ref and the commit that built them.

### Release assets

```bash
gh attestation verify checksums.txt --repo dsx-ai-factory/nvke
sha256sum -c checksums.txt
```

Before disconnecting, obtain the trusted roots on an online machine:

```bash
gh attestation trusted-root > trusted_root.jsonl
```

Transfer that file, the attached bundle, `checksums.txt`, the artifacts to
verify, and the GitHub CLI into the offline environment. Refresh the trusted
roots when importing new signed material; an offline copy cannot report later
key revocations. Follow [GitHub's offline verification guide](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/verify-attestations-offline)
for repository identity checks when using an attached bundle.

Verify offline using the local bundle and trusted roots:

```bash
gh attestation verify checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --custom-trusted-root trusted_root.jsonl \
  --repo dsx-ai-factory/nvke
```

### Published images

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
from this source tree, so it records what the release actually contains and not
what a build of this commit ought to select.

| Artifact | Document | Subject granularity | Where |
|---|---|---|---|
| `nvkectl-<os>-<arch>` | `nvkectl-<os>-<arch>.spdx.json` | Go modules, from the binary's embedded build info | GitHub Release asset |
| `nvke-agent_<version>_<arch>.deb` | `nvke-agent_<version>_<arch>.spdx.json` | Go modules, from the binary inside the package | GitHub Release asset |
| `nvke-node-{fde,non-fde}-ubuntu-<rel>-<arch>.qcow2` | `nvke-node-…-<arch>.spdx.json` | dpkg packages, from the image's own package database | S3, beside the disk; a GitHub Release asset too when `NVKE_GITHUB_PUBLISH` names `os-image` |
| `nvke-controller` image | `controller-sbom-linux-<arch>.spdx.json` | image contents, scanned from the pushed image | workflow artifact on the **Image NVCR** run |
| `cluster-api-provider-provisioned` image | `cluster-api-provider-provisioned-sbom-linux-<arch>.spdx.json` | image contents, scanned from the pushed image | workflow artifact on the **Image Provisioned NVCR** run |

The Helm charts — `nvke-controller` and `nktp` — have no SBOM of their own. A
chart carries no components; it carries a reference to an image that does, and
that image's SBOM is the row above. An SBOM of the chart archive would list the
archive.

The granularity differs by artifact because the useful question differs. For a
Go binary it is "which module versions are linked into this", so the document
is a module list read back out of the binary. For a node image it is "does this
image carry <component>", and everything that reaches the image — archive
packages and our own `.deb`s alike — is a dpkg entry, so the document is the
image's package set. The agent's own module graph is not repeated in the image
SBOM; it is the SBOM attached to the agent `.deb`.

### Retrieving one

The binary, `.deb` and node-image SBOMs are release assets, so `gh release
download` fetches them by name:

```bash
gh release download cli/v0.2.0 --repo dsx-ai-factory/nvke \
  --pattern 'nvkectl-linux-amd64.spdx.json'

gh release download agent/v0.2.0 --repo dsx-ai-factory/nvke \
  --pattern '*.spdx.json'

gh release download os-image/v0.2.0 --repo dsx-ai-factory/nvke \
  --pattern 'nvke-node-fde-ubuntu-26.04.1-amd64.spdx.json'
```

Each is named in that release's `checksums.txt`, and `checksums.txt` carries a
build-provenance attestation, so the same two commands under "Verifying a
release" above establish that an SBOM is the one this repository published:

```bash
gh attestation verify checksums.txt --repo dsx-ai-factory/nvke
sha256sum -c checksums.txt --ignore-missing
```

Answering "which of our artifacts contains `<component>`" is then a query over
the downloaded documents, for example with `jq`:

```bash
jq -r --arg c openssl \
  '.packages[] | select(.name | test($c)) | "\(.name) \(.versionInfo)"' \
  *.spdx.json
```

The two container images are scanned by their publish workflows rather than by
a release job, so their documents are build artifacts on the run, not release
assets — `gh run download <run-id> --name controller-sbom-linux-amd64.spdx.json`.
They are covered by neither `checksums.txt` nor its attestation.

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
| Release assets (`nvkectl`, agent `.deb`s, `nvke-os-image-check`, the NKTP chart archive) | `checksums.txt`, which names each by digest | GitHub, plus the attached bundle |
| `os-image` qcow2 OCI artifacts | the same `checksums.txt` | GitHub, plus the attached bundle, when `NVKE_GITHUB_PUBLISH` names `os-image`; otherwise unattested |
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

This is the image equivalent of the asset re-hash `release.yml` runs before it
publishes a release, and it fails the same way: closed.

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

The other artifacts NVKE publishes to GHCR — the Helm charts, the `agent` and
`cli` OCI artifacts, `agent-deb`, and the `os-image-*` repositories — are not
attested individually yet. The `os-image` disks and the NKTP chart are covered
transitively through `checksums.txt`, which names the chart archive whose
digest is the chart layer on GHCR; the rest are follow-up work along the pattern in
`image-nvcr.yml`.

## Hotfix releases

For security patches that cannot wait for the next regular release:

1. Branch from the tag that needs patching: `git checkout -b fix/v0.1.1 v0.1.0`
2. Cherry-pick or apply the fix.
3. Follow steps 1–4 above with the patch version incremented.
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

**The dot in `-rc.N` is load-bearing.** `.github/workflows/release.yml` marks
the release as a pre-release by looking for `-rc.`, so `v0.2.0-rc1` without it
would publish as a full release and take the repository's "Latest release"
badge. The workflow rejects that spelling rather than releasing it. The dot also
gives the right ordering: `rc.2` sorts before `rc.10`, while `rc2` sorts after
`rc10`, because SemVer compares an all-digit identifier as a number and anything
else as text.

GitHub does not infer any of this from the tag name. Releases created through
the API are full releases unless the caller says otherwise, and the workflow is
the only thing that says otherwise.
