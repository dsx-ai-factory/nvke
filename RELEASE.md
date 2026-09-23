# Release process

This document describes how nke releases are created and published.

## Versioning

nke uses [Semantic Versioning](https://semver.org/):

```
v<MAJOR>.<MINOR>.<PATCH>
```

| Increment | When |
|---|---|
| `PATCH` | Backwards-compatible bug fixes |
| `MINOR` | New backwards-compatible features |
| `MAJOR` | Breaking API or behavioral changes |

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
`github.com/dsx-ai-factory/nke/cli` can only be resolved by a tag named `cli/vX.Y.Z`.
`.github/workflows/release.yml` reads the directory back off the tag and checks
it against `go.work`, so a prefix naming no module fails the run.

Two prefixes skip that workflow's GitHub Release job and cut the release
themselves, after they have built and verified the real artifacts:

| Tag | Workflow | What ships |
|---|---|---|
| `agent/vX.Y.Z` | `agent-deb-publish.yml` (+ `agent-artifact.yml`) | `.deb`s, GitHub Release assets, GHCR binary, NVCR binary + `.deb`, S3/CloudFront `.deb` |
| `os-image/vX.Y.Z` | `os-image-artifact.yml` | qcow2 on GHCR, NVCR, and S3/CloudFront; `nke-os-image-check` on the GitHub Release |
| `cli/vX.Y.Z` | `release.yml` (+ `cli-artifact.yml`, `cli-artifact-nvcr.yml`, `cli-s3-publish.yml`) | binaries on GitHub Release, GHCR, NVCR, S3/CloudFront |

An `os-image/` tag that fails Packer or the boot-smoke test never publishes the
OCI artifact and never promotes the GitHub Release out of draft.

A `controller/vX.Y.Z` tag goes through `release.yml` like any other component and
additionally publishes the controller image and Helm chart:

| Workflow | What it publishes |
|---|---|
| `image-nvcr.yml` | multi-arch image to `nvcr.io/<org>/nke-controller`, promotes that released image to DSX as `nvcr.io/<dsx-org-team>/nke-controller`, and promotes release tags to `ghcr.io/dsx-ai-factory/nke/nke-controller` |
| `chart-publish.yml` | chart to `ghcr.io/dsx-ai-factory/nke/charts/nke-controller` |
| `chart-publish-nvcr.yml` | chart to `nvcr.io/<org>/nke-controller-chart` and `nvcr.io/<dsx-org-team>/nke-controller-chart` |

Every `main` commit already publishes the image and chart to NVCR. GHCR receives
released versions only, which is why a chart built from a tag can safely point at
`ghcr.io/dsx-ai-factory/nke/nke-controller:<version>` while a chart built from `main` could not.
Releases before the repository moved to `dsx-ai-factory` on 2026-09-12 were promoted to
`ghcr.io/nvidia/nke/controller` instead: `controller/v0.1.1-rc.1` to `rc.3` are there,
`controller/v0.1.0` predates the promotion job, and the `rc.4` promotion failed, so that
image exists nowhere. Later releases land under `ghcr.io/dsx-ai-factory/nke`.

DSX receives the controller image from `image-nvcr.yml`, after the NKE multi-arch
manifest exists. It receives the controller chart from `chart-publish-nvcr.yml`.
Both use separate credentials from the same protected GitHub environment:

| Name | Type | Purpose |
|---|---|---|
| `NKE_NGC_ORG` | variable | Source NGC org for `nvcr.io/<org>/nke-controller` |
| `NGC_REGISTRY_TOKEN_NKE` | secret | Read/write token for the source NKE NGC org |
| `DSX_NGC_ORG_TEAM` | variable | Destination NGC org/team, for example `<org>/components-dev` |
| `NGC_REGISTRY_TOKEN_DSX` | secret | Write token for the DSX NGC org/team |

The source and destination both live on `nvcr.io`, but they may require different
NGC tokens. Docker auth stores one credential per registry host, so the DSX
promotion pulls the source image into a local OCI layout with the NKE token, then
logs in with the DSX token and pushes that same index to
`nvcr.io/<dsx-org-team>/nke-controller`. Immutable tags
(`sha-<commit>` and the git-described version) are treated as idempotent only
when an existing destination digest matches the source digest; a mismatch fails
the run. The `edge` tag is mutable and moves only on `main`.

The DSX chart is packaged from the controller chart as `nke-controller-chart`,
the same chart artifact name used for the NKE NVCR chart, and pushed to
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
the **OS Image Artifact** check: Packer, boot-smoke, then `oras` to
`ghcr.io/dsx-ai-factory/nke/os-image`. For `controller/v*`, confirm the **Image
NVCR** and **Chart Publish** checks, then that the promoted image is present at
`ghcr.io/dsx-ai-factory/nke/nke-controller:<version>`.

## Hotfix releases

For security patches that cannot wait for the next regular release:

1. Branch from the tag that needs patching: `git checkout -b fix/v0.1.1 v0.1.0`
2. Cherry-pick or apply the fix.
3. Follow steps 1–4 above with the patch version incremented.
4. Backport to `main` if the fix applies there too.

## Pre-releases

For release candidates, append `-rc.N` to the version: `v0.2.0-rc.1`, or
`cli/v0.2.0-rc.1` for one component. Tag and push the same way.

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
