# Source releases

The OSS release deliverable is versioned source, instructions for building it,
and a validated software bill of materials (SBOM). Users build their own
binaries. Publishing binaries, Debian packages, container images or node images
is not an acceptance requirement for this source release.

This is the maintainer procedure for releasing NVKE itself. The separate
[artifact publication reference](docs/artifact-publication.md) describes the
existing build, registry and binary-release automation. A source tag does not
disable those workflows: review their tag triggers before pushing a tag.
Changing this document does not change publication behavior.

## Version and source identity

Use Semantic Versioning: `vMAJOR.MINOR.PATCH` for the repository and
`<module-directory>/vMAJOR.MINOR.PATCH` for an independently versioned Go
module. Component prefixes must follow the module directories in `go.work`.
The [CLI compatibility policy](cli/docs/compatibility.md) defines the CLI's
breaking-change surface. Use `-rc.N` for a release candidate, including the dot.

Nightly beta tags, and the release candidates automation cuts from a qualified
beta, are unsigned lightweight tags. They mark candidates for qualification,
not releases, so `git verify-tag` does not apply to them. The signed annotated
tag below is for the approved release; see
[pre-releases](docs/artifact-publication.md#pre-releases).

Record the full source commit, tag name and signed annotated tag object in the
release record. Tags are immutable once published. A beta or RC qualification
result can support a final release only when it covers that same source commit
and the declared component/platform scope. Changing source, dependency locks or
build inputs requires validation of the changed candidate; a new version name
does not transfer qualification to different bytes.
If retagging changes version strings or other build inputs, verify those
resulting builds separately; matching source commits do not prove identical
candidate and final binaries.

## Build from a released tag

Use a full Git clone so the tag object and build version metadata are present.
Replace `RELEASE_TAG` below with the tag named in the release notes:

```bash
git clone https://github.com/dsx-ai-factory/nvke.git
cd nvke
RELEASE_TAG=vMAJOR.MINOR.PATCH
git fetch origin tag "$RELEASE_TAG"
git verify-tag "$RELEASE_TAG"
git checkout --detach "$RELEASE_TAG"
git rev-parse HEAD
```

Verify the tag against a maintainer signing key obtained through a trusted
channel, and compare the full commit with the release record. A cryptographic
signature from an untrusted key is insufficient. A generated source archive
does not contain Git tag signatures or the history used for version stamping.

Follow [the development guide](docs/development.md#getting-the-tools) for tools
and [the contribution guide](CONTRIBUTING.md#prerequisites) for the Go workspace
version and GNU Make requirements. On macOS use `gmake` in place of `make`, or
activate the optional pinned Flox environment. From the checked-out tag:

```bash
make show-versions
make build
```

The root build writes binaries to `bin/`. Per-module build/test commands and
platform limitations are documented in [README.md](README.md#getting-started),
[development](docs/development.md) and [the workspace guide](docs/go-workspace.md).
Building a CLI on macOS does not establish that Linux-only agents, node-image
builds or hardware qualification run natively on macOS. Reference node-image
builds have their own [OS-image guide](os-image/README.md).

## Select and approve a candidate

The [maintainers](MAINTAINERS.md) own the release decision under
[project governance](GOVERNANCE.md). Name a release owner and record maintainer
approval and the RelEng evidence review in the release record before publication.
This procedure does not grant repository access or replace live branch rules.

Before tagging, the owner must:

1. Freeze the candidate commit and version. Finish release-note and dependency
   changes before collecting final evidence. Use the existing
   [changelog procedure](docs/artifact-publication.md#1-update-changelogmd);
   do not hand-edit generated changelog entries.
2. Check that the candidate has the required repository checks and reviews.
   Record actual check names, result links and the full tested commit. Missing,
   skipped or cancelled required checks are not passes.
3. Record a clean-checkout build result using the tools and commands above,
   with the host OS/architecture and outputs. Link tests for each claimed
   platform; do not turn an untested platform into a support claim.
4. Identify the qualification scope and link its results using the
   [qualification guide](docs/qualification/overview.md) and
   [release tooling guide](release/README.md). State coverage exclusions and
   any explicitly approved N/A scope. Unit tests, build success and hardware
   acceptance are separate evidence. Incomplete required qualification blocks
   release; future GA features are not silently added to OSS acceptance.
5. Complete the SBOM validation below for this same candidate, then record the
   release decision. An unresolved validation or approval blocks publication.

## Validate the source-release SBOM

Publish a machine-readable SBOM for the released source and its declared
dependencies. SPDX 2.3 JSON is the format already used by NVKE's artifact
tooling. A source/dependency inventory and a scan of a built binary describe
different subjects; label them accordingly. A reference build may support
validation without distributing the resulting binaries.

The release owner records the following evidence:

| Check | Required evidence |
|---|---|
| Subject | Repository, full source commit, release tag, SBOM filename and SHA-256; identify any reference-build subjects separately |
| Generation | Generator version and identity, exact command/configuration, scope and exclusions; use the reviewed Syft version in `make/tool-versions.mk` if selecting Syft |
| Format and semantics | Successful validation using a maintained validator for the declared SPDX version; record validator version, invocation, log and exit status |
| Dependency coverage | Compare the inventory with the release's `go.work`, module manifests and locks, plus declared non-Go build inputs; account for local replacements and external sources, and document exclusions |
| Review | Maintainer review of inventory gaps and unresolved validation findings, with the scope accepted for the release |
| Consumer verification | An authenticated release record binding the source commit, SBOM digest and validation-report digest, with instructions for checking its signer and downloaded bytes |

Use established tools such as [Syft's directory scanning](https://oss.anchore.com/docs/guides/sbom/scan-targets/)
and [the SPDX validation tools](https://github.com/spdx/tools-python); do not
substitute an ad hoc JSON-field check for SPDX validation. Select and record
reviewed tool versions before generating the release evidence. Keep generated
files outside the source being scanned so the inventory does not include its
own outputs or local build caches.

Schema/semantic validation and dependency-coverage review are both required.
A valid signature or matching checksum proves neither inventory completeness
nor absence of vulnerabilities. Record vulnerability scan results separately,
including unresolved findings and their disposition. Do not describe an
uncompleted scan as clean.

The artifact workflows documented in the publication reference generate
artifact SBOMs and integrity evidence. They do not, by themselves, establish
that this source-release SBOM validation and coverage review occurred. Until
the required reports are available, mark this release requirement incomplete.

## Publish and verify

After approval, the release owner creates a signed annotated tag targeting the
approved full commit. Verify the local tag target before pushing the exact tag
to the canonical repository. Review
[the existing publication triggers](docs/artifact-publication.md#2-tag-the-release)
first: component and beta tags can launch artifact publication. A tag must not
be used as a supposedly source-only switch for workflows that publish binaries.

The release notes must link the tagged build instructions, the validated SBOM,
its validation report and the authenticated evidence record. Record the exact
qualification subjects and coverage, accepted limitations, approval and
correction contact. Internal credentials, host addresses and provider logs must
stay out of public evidence; publish a sanitized summary instead.

Read the published record back from the consumer's access path. Confirm tag
signature and commit, access to tagged build instructions, and that the SBOM
and validation report match their authenticated digests. Verify any reference
build independently of whether its output is distributed. Leave the release
incomplete if a required link, report or subject binding is missing. This
document does not claim an automated source-only publication gate exists.

## Hotfixes and recovery

Branch a hotfix from the affected release tag, review the fix and backport to
`main` when applicable. Increment the patch version and repeat candidate checks,
build proof, qualification for the affected scope and SBOM validation on the
new commit. Follow [SECURITY.md](SECURITY.md) for security-sensitive reports.

For a failed or partial publication, preserve the tag, source commit, logs and
uploaded evidence. Mark the release incomplete and identify the failed step.
Retry only against the same approved commit and exact evidence bytes; do not
replace a mismatched asset or move a published tag. If source or evidence must
change, issue a new candidate/version with new validation and approval.

For an already published bad release, mark it withdrawn in its release notes,
link the replacement and notify consumers through the project's support
channel. Preserve the original source identity and evidence. The release owner
records the correction and verifies the replacement from the consumer path.
