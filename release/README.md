# release/

This directory holds release and qualification tooling: the orchestration that
takes a candidate set of already-built artifacts and resolves what a
qualification run would cover.

> `qualify plan` resolves a real plan from the repository and places its
> `plan.json`, and `qualify verify` checks that everything the plan names is
> published. Each stream's leg then records its outcome with `qualify result`
> and ships it to TestGrid with `qualify publish`. The
> [`qualify-e2e` workflow](../.github/workflows/qualify-e2e.yml) runs all four
> and qualifies one job per stream.

## Usage

`qualify plan` resolves what to qualify and writes it as JSON. Run it from the
repository root.

`registry.yaml` names the artifact registry as `oci://nvcr.io/${QUALIFY_E2E_NGC_ORG}`,
so every `plan` needs that variable. It is read when the file is loaded, and an
unset one fails the command before anything resolves.

```console
$ export QUALIFY_E2E_NGC_ORG=<the NGC org the artifacts are published under>

# The highest version of nvke, over every stream
$ go run ./release/cmd/qualify plan --subject nvke

# A specific tag, narrowed to the golden-path scope
$ go run ./release/cmd/qualify plan --subject nvke --version v0.22.0-beta.1 --scope golden-path

# Use a version you choose in place of what resolves
$ go run ./release/cmd/qualify plan --subject nvke --version v0.22.0-beta.1 --overrides controller=v0.1.1-rc.11,kube=1.36.3

# Write the plan to a file instead of stdout
$ go run ./release/cmd/qualify plan --subject nvke --output plan.json

# Qualify a beta: describe every component at the commit the tag marks
$ go run ./release/cmd/qualify plan --subject nvke --version v0.22.0-beta.1 --resolution commitDescribe

# Check that everything the plan names is published, waiting for any still building
$ NGC_API_KEY=<an NGC key that can read that org> \
    go run ./release/cmd/qualify verify --plan plan.json --wait 90m
```

`verify` reads the registries anonymously unless it is given credentials, and
both hold private repositories, so without them every OCI reference answers 401.
With `--wait` it then retries each one for the whole budget before failing.

| Variable | For |
|---|---|
| `NGC_API_KEY` | `nvcr.io`, with the literal username `$oauthtoken` |
| `GITHUB_ACTOR`, `GITHUB_TOKEN` | `ghcr.io` |

| Flag | Default | Meaning |
|---|---|---|
| `--subject` | required | What to qualify. `nvke` is NVKE as a whole |
| `--version` | the highest version | The subject's version, which must be an existing tag |
| `--overrides` | none | Values to use in place of what resolves, as `name=value` pairs separated by commas |
| `--scope` | `full` | How much of the matrix to cover, a scope from `policy.yaml` |
| `--output` | `-` | Where to write the plan. `-` is stdout |
| `--resolution` | `commitDescribe` | How to resolve the components, named for the strategy that answers. `commitDescribe` describes each at the commit the subject's version points at, which is how a beta is qualified. `gitTag` takes each from its own release tags, and a release qualification has to select it: left on the default a release verifies the describe-named artifacts, which exist, so it passes while qualifying builds other than the release-tagged ones |
| `--check-exists` | off | Ask for each location this resolves and require it to be published. Off leaves the question to `qualify verify` |
| `--root`, `--policy`, `--registry` | the enclosing checkout and the files under `release/` | Where to read the repository and the two files from |

Without `--version` the subject resolves the way every component does, to the
highest version of any kind. Pass `--version` to name a tag that exists.

A value given with `--overrides` replaces what that component or dependency
would have resolved to, and is checked the way a resolved one is: a tag has to
exist, and a Kubernetes or OVN version has to be one the image locks. An image or
a bootloader such as `os-image-amd64` is a dependency whose override is a URL, so
it is overridden by name too. The names that were overridden are listed under
`overrides` in the plan.

A source's base can name a `${VARIABLE}`, which is substituted from the
environment when the file is read. The registry under which artifacts sit names
one organization's account rather than anything about NVKE, so the file states
the shape and a run supplies the value. An unset or empty variable fails on the
file that asked for it, with every missing name at once, rather than producing a
plan that names an artifact under no org.

Planning reaches nothing but git. Whether each artifact is published is asked
once, by `qualify verify`, for everything the plan names. `--check-exists` asks
during resolution instead, which needs a network that can reach the bucket: it is
private, so a missing artifact and a network that is not allowed both answer 403,
and the check can confirm that an artifact exists but not that it does not.

A release is qualified with `--resolution gitTag`, which takes each component from
its own release tags. A beta is qualified with `--resolution commitDescribe`: a
beta tag names a commit and carries no component tags, so the tags would answer
with the last release rather than what was built there. The commit is derived from
the version being qualified rather than given. The plan looks like this:

```console
$ go run ./release/cmd/qualify plan --subject nvke --version v0.21.0 --scope full
{
  "subject": "nvke",
  "version": "v0.21.0",
  "scope": "full",
  "combinations": [
    {
      "id": "capi-provisioned-v0.1.0_cli-v0.1.0_controller-v0.1.1_os-image-v0.1.0",
      "components": [
        { "name": "controller",       "version": "v0.1.1", "url": "oci://nvcr.io/<org>/nvke-controller:v0.1.1", "versionStrategy": "gitTag", "locationStrategy": "ociArtifact" },
        { "name": "cli",              "version": "v0.1.0", "versionStrategy": "gitTag" },
        { "name": "os-image",         "version": "v0.1.0", "versionStrategy": "gitTag" },
        { "name": "capi-provisioned", "version": "v0.1.0", "url": "oci://nvcr.io/<org>/cluster-api-provider-provisioned:v0.1.0", "versionStrategy": "gitTag", "locationStrategy": "ociArtifact" }
      ],
      "dependencies": [
        { "name": "cni-ovn",               "version": "2.4.2", "versionStrategy": "lockedPackage" },
        { "name": "os-image-bootloader-amd64", "url": "https://cdn.example/nke-isos/qcow/efi/x86_64/qcow-imager.efi", "locationStrategy": "url" },
        { "name": "os-image-bootloader-arm64", "url": "https://cdn.example/nke-isos/qcow/efi/aarch64/qcow-imager.efi", "locationStrategy": "url" },
        { "name": "cli-amd64",                 "version": "v0.1.0", "url": "oci://nvcr.io/<org>/nvkectl:v0.1.0-linux-amd64", "locationStrategy": "ociArtifact" },
        { "name": "cli-arm64",                 "version": "v0.1.0", "url": "oci://nvcr.io/<org>/nvkectl:v0.1.0-linux-arm64", "locationStrategy": "ociArtifact" },
        { "name": "kind",                      "version": "v0.32.0", "versionStrategy": "makeVar" },
        { "name": "clusterctl",                "version": "v1.12.3", "versionStrategy": "makeVar" },
        { "name": "nerdctl",                   "version": "2.3.5", "versionStrategy": "makeVar" }
      ],
      "streams": [
        {
          "name": "forge-gb200-amd64-ubuntu-24.04-kube-1.36",
          "capabilities": { "arch": "amd64", "gpu": "gb200", "kube": "1.36", "provider": "forge", "ubuntu": "24.04" },
          "dependencies": [
            { "name": "kube",           "version": "1.36.3", "versionStrategy": "lockedPackage" },
            { "name": "os-image-amd64", "version": "v0.1.0", "url": "https://cdn.example/nvke-packages/os-image/releases/v0.1.0/amd64/nvke-node-non-fde-ubuntu-24.04.5-amd64.qcow2", "locationStrategy": "osImage" },
            { "name": "os-image-arm64", "version": "v0.1.0", "url": "https://cdn.example/nvke-packages/os-image/releases/v0.1.0/arm64/nvke-node-non-fde-ubuntu-24.04.5-arm64.qcow2", "locationStrategy": "osImage" }
          ]
        },
        {
          "name": "forge-gb200-arm64-ubuntu-24.04-kube-1.36",
          "capabilities": { "arch": "arm64", "gpu": "gb200", "kube": "1.36", "provider": "forge", "ubuntu": "24.04" },
          "dependencies": [
            { "name": "kube",           "version": "1.36.3", "versionStrategy": "lockedPackage" },
            { "name": "os-image-amd64", "version": "v0.1.0", "url": "https://cdn.example/nvke-packages/os-image/releases/v0.1.0/amd64/nvke-node-non-fde-ubuntu-24.04.5-amd64.qcow2", "locationStrategy": "osImage" },
            { "name": "os-image-arm64", "version": "v0.1.0", "url": "https://cdn.example/nvke-packages/os-image/releases/v0.1.0/arm64/nvke-node-non-fde-ubuntu-24.04.5-arm64.qcow2", "locationStrategy": "osImage" }
          ]
        },
        {
          "name": "forge-gb200-amd64-ubuntu-26.04-kube-1.36",
          "capabilities": { "arch": "amd64", "gpu": "gb200", "kube": "1.36", "provider": "forge", "ubuntu": "26.04" },
          "dependencies": [
            { "name": "kube",           "version": "1.36.3", "versionStrategy": "lockedPackage" },
            { "name": "os-image-amd64", "version": "v0.1.0", "url": "https://cdn.example/nvke-packages/os-image/releases/v0.1.0/amd64/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2", "locationStrategy": "osImage" },
            { "name": "os-image-arm64", "version": "v0.1.0", "url": "https://cdn.example/nvke-packages/os-image/releases/v0.1.0/arm64/nvke-node-non-fde-ubuntu-26.04.1-arm64.qcow2", "locationStrategy": "osImage" }
          ]
        },
        {
          "name": "forge-gb200-arm64-ubuntu-26.04-kube-1.36",
          "capabilities": { "arch": "arm64", "gpu": "gb200", "kube": "1.36", "provider": "forge", "ubuntu": "26.04" },
          "dependencies": [
            { "name": "kube",           "version": "1.36.3", "versionStrategy": "lockedPackage" },
            { "name": "os-image-amd64", "version": "v0.1.0", "url": "https://cdn.example/nvke-packages/os-image/releases/v0.1.0/amd64/nvke-node-non-fde-ubuntu-26.04.1-amd64.qcow2", "locationStrategy": "osImage" },
            { "name": "os-image-arm64", "version": "v0.1.0", "url": "https://cdn.example/nvke-packages/os-image/releases/v0.1.0/arm64/nvke-node-non-fde-ubuntu-26.04.1-arm64.qcow2", "locationStrategy": "osImage" }
          ]
        }
      ]
    }
  ]
}
```

## How the plan is used

`plan.json` is the input to a qualification run. In the
[`qualify-e2e` workflow](../.github/workflows/qualify-e2e.yml):

- The `plan` job runs `qualify plan --subject nvke`. The dispatch inputs and
  `QUALIFY_E2E_*` repository variables that name a version are passed as
  `--overrides`, so the plan records them and a job never has to guess which of
  the two it was handed. `--resolution` is `commitDescribe` unless `gitTag` is
  selected by input or by `QUALIFY_E2E_RESOLUTION`, and a beta tag push takes
  `commitDescribe` whatever the variable says.
  The plan is uploaded as an artifact.
- `qualify-run` runs once for each stream of each combination, gated behind a
  GitHub environment from its first step. It runs `qualify verify` on that
  plan before any machine is taken, so an artifact that was never published
  fails there rather than an hour into a run -- waiting when the plan
  resolved at a commit, because the publishers may still be running -- then
  resolves versions and capabilities and builds the site inputs. Its
  versions, architecture and GPU model are that stream's entry in the plan,
  so every stream qualifies exactly what the plan resolved. Each job is
  named by its stream.
- After the suite runs (or fails to), `qualify result` writes
  `qualify-result.json` from that stream's plan entry and its Ginkgo report,
  on every non-dry-run leg -- including a PR leg and an infrastructure
  failure, so the record exists even when nothing gets published. `qualify
  publish` then ships it, with the JUnit report, to TestGrid -- but only on a
  trusted ref (`main`, or a release/beta tag), since that is the only
  identity the testgrid repo's WIF provider will exchange a token for. A
  labelled PR leg gets a result record as a workflow artifact and nothing on
  TestGrid.

The plan carries what a job needs, the versions to test and the capabilities of
the stream to run on. The e2e suite requires every version a Forge run uses
(`controller`, `capi-provisioned`, `kube`, `cni-ovn`, `kind`, `clusterctl`,
`nerdctl` and the image and bootloader URLs) and fails before a machine exists
when one is missing. It carries no default for them, so the plan is the one
place the versions come from.

## What this is not

`build/` produces the artifacts for one component. `release/` never builds
anything. It consumes artifacts that already exist, and it is an error for
anything under this directory to compile source into a shipped artifact. That
separation is what makes the qualification meaningful: a release promotes the
exact bytes that were tested, and tooling that can rebuild is tooling that can
substitute.

Per-component publication stays where it is. `agent/v*`, `os-image/v*`,
`controller/v*` and `cli/v*` tags continue to drive their own workflows exactly
as [`RELEASE.md`](../RELEASE.md) describes. `release/` orchestrates *over* those
publications; it does not replace them.

## Layout

| Path | Contents |
|---|---|
| [`registry.yaml`](registry.yaml) | How each component resolves to a version, and the named locations artifacts are published under. It holds the rule that finds a version, never a version |
| [`policy.yaml`](policy.yaml) | The streams that exist and the scopes over them |
| [`qualification.yaml`](qualification.yaml) | What is qualified when NVKE is qualified as a whole: the components tested together, and the other versions a run needs |
| [`cmd/qualify/`](cmd/qualify/) | The binary. `plan` resolves what to qualify, `verify` checks that it is published |
| [`internal/qualification/`](internal/qualification/) | Reading and checking the three files, and turning them into a plan |
| [`internal/resolve/`](internal/resolve/) | The resolution strategies. Of this repository it takes only `internal/artifact` |
| [`internal/artifact/`](internal/artifact/) | Asking whether a location holds something, over HTTPS or a registry, and waiting for one that is still publishing |
| [`hack/next-tag.sh`](hack/next-tag.sh) | The version the next beta or release candidate tag gets, from the commits since the latest GA tag. [`test_next_tag.py`](hack/test_next_tag.py) tests it against fixture repositories |

Each file has one concern. A component that is qualified on its own would carry
its own `<component>/qualification.yaml` next to its code, using the same
schema, and share the registry and the policy.

## Subjects, components and dependencies

`qualification.yaml` says what a qualification covers. It is written for a
**subject**, the thing being qualified, and the subject's version is the plan's
version. This file is for `nvke`, which stands for NVKE as a whole, so its version
is a beta tag on `main`. A component qualified on its own would have a file of
the same shape next to its code.

The file lists two kinds of thing.

`components` are the parts of NVKE tested together: `controller`, `cli`,
`os-image` and `capi-provisioned`. Each carries `selectors` that say which
version to take, and today the only selector is `latest`, the highest version of
any kind ordered by semver, so `v0.1.1` beats `v0.1.1-rc.8` and `v0.1.1-rc.1`
beats `v0.1.0`. One choice of versions for all four is a
**combination**, which is what a plan is organised by.

`dependencies` are the other versions a run needs: Kubernetes, the OVN CNI, the
tools, the nvkectl artifact for each architecture, and the node image and its
bootloader for every architecture, whatever the stream's own. Each image or
bootloader is named by the key the e2e suite reads.
They have no selectors, because the components determine them or a pin file does.
A dependency says what to resolve with four fields, each with one meaning:

| Field | Meaning |
|---|---|
| `constraint` | A literal version constraint, such as `"1.4"`. Naming every part pins one version, so `"1.4.11"` matches that and nothing else |
| `constraintFrom` | The constraint is the stream's value of a capability, `{capability: kube}` |
| `releases` | The versions to choose from, for a strategy that cannot list them itself. An image's Ubuntu point releases are one. The highest the constraint matches is used, so series `24.04` picks `24.04.5` |
| `value` | A declared answer that nothing derives, such as a bootloader's path |

A dependency with no `constraintFrom` is resolved once for the combination. One
with a `constraintFrom` is resolved once per stream, with that stream's value.
Which of the two it is comes from that field and nothing else. So the images are
per stream and the bootloaders are per combination.

A resolved entry in the plan has a `version`, a `url`, or both, and the strategy
that answered each. A `version` is always a version and a `url` is always a
location. An image carries the `os-image` release it was built from and the URL
of the qcow, a bootloader carries only its URL, and a component published to a
registry carries the reference to pull.

`versionStrategy` is the registry's name for what resolved the version, or which
arm answered where one entry can resolve two ways. An entry that takes another
entry's version has none, because no strategy answered. `locationStrategy` is
the registry's name for what built the URL.

Quote a version in the file. YAML reads an unquoted `1.40` as a number and the
constraint becomes `1.4`.

## The two questions an entry answers

`registry.yaml` gives each entry a `resolve` with two keys, because what a
version is and where it is published are separate questions.

```yaml
- name: controller
  resolve:
    version:  {strategy: gitTagOrCommitDescribe, with: {tagPrefix: "controller/"}}
    location: {strategy: ociArtifact, with: {source: artifact-registry, repository: nvke-controller}}
```

A `version` block answers with a `strategy` of its own or takes one `from`
another entry, and the two are mutually exclusive. A `location` block is
optional and always names a strategy. An entry may declare either half alone: a
bootloader is a location with no version, and a tool pin is a version with no
location this names.

A version can be taken only from a **component**. Components resolve first and
each is offered to every dependency, which a dependency's own result is not.

An override names what a strategy would have produced, so it goes to the half
that has one, and to the version half when both do. `--overrides
controller=v0.1.1` names a version and `--overrides os-image-amd64=<url>` names
a location, without either declaring which it is.

| Component | Version | Location |
|---|---|---|
| `nvke`, `cli`, `os-image` | Git tags, under each component's tag prefix | none this names |
| `controller`, `capi-provisioned` | the same | `<artifact-registry>/<repository>:<version>` |
| `kube`, `cni-ovn` | The lock files the image's packages commit, read at the `os-image` tag the combination resolved. A node installs these from the image's own apt repository, so the lock files are the versions it can offer | none |
| `os-image-amd64`, `os-image-arm64` | taken from `os-image` | Built from that release, the architecture and the Ubuntu point release the stream's series picks from `releases` |
| `cli-amd64`, `cli-arm64` | taken from `cli` | `<artifact-registry>/nvkectl:<version>-linux-<arch>`. nvkectl is published as one tag per platform with no index above them, so the `cli` version alone names none of them |
| `os-image-bootloader-amd64`, `os-image-bootloader-arm64` | none, a declared path has no version behind it | A path declared in `qualification.yaml` under the `artifact-cdn` source, because nothing derives it |
| `kind`, `clusterctl`, `nerdctl` | `make/tool-versions.mk`, which owns every tool pin | none |

The resolvers know none of this vocabulary. They receive plain values and return
plain values, and the planner is the only code that knows where those came from.

## Streams, scopes and combinations

Three words carry the whole matrix, and they are worth keeping straight.

A **stream** is one configuration, named by the capabilities it provides --
`{provider: forge, gpu: gb200, arch: amd64, ubuntu: "24.04", kube: "1.36"}` -- and never by
the infrastructure that satisfies them. Which runner, site or subnet a
capability resolves to is an environment knob belonging to the e2e suite's own
config. That boundary is what keeps the suite runnable by hand and by a second
organisation, and keeps this tool from becoming a config translator. `arch` is
the architecture of the site and control-plane node, so it does not say which
node images a run needs.

A **scope** is a named selection over the streams, matched on capabilities.
`full` selects every Forge stream, `golden-path` narrows to Ubuntu 24.04 on
GB200, `l40s` to Ubuntu 24.04 on L40S at site-1, and `dsx` to the DSX datahall
stream; [`policy.yaml`](policy.yaml) says why each is drawn where it is. The
set of supported Kubernetes minors is not declared anywhere,
it is whichever minors have streams, so adding one means adding streams and
nothing else.

A **combination** is one resolved set of components, qualified against every
stream in the scope. Its id is the components' `<name>-<version>` pairs sorted
by name and joined with `_`, with characters unsafe in a path replaced by `-`,
so the id in a plan and the id in a published record are the same string without
a lookup. Today every component carries the single selector `latest`, so there
is exactly one combination.

Each stream in a combination carries its capabilities rather than just the
name, so the run that executes it is configured from the plan. Reading them
back out of the policy at run time would let an edit landing between the plan
job and the last stream change what that stream runs, which is what resolving
once is for.
