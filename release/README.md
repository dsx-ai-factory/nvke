# release/

This directory holds release and qualification tooling: the orchestration that
takes a candidate set of already-built artifacts and resolves what a
qualification run would cover.

> **Plan only.** This directory defines the shape of NKE release
> qualification, and `qualify plan` resolves a real one from the repository's
> tags and writes it as `plan.json`, which the e2e qualification workflow will
> consume to run the matrix of jobs.

## Usage

`qualify plan` resolves what to qualify and writes it as JSON. Run it from the
repository root:

```console
# The latest release candidate of nke, over every stream
$ go run ./release/cmd/qualify plan --subject nke

# A specific tag, narrowed to the active scope
$ go run ./release/cmd/qualify plan --subject nke --version v0.22.0-beta.1 --scope active

# Use a version you choose in place of what resolves
$ go run ./release/cmd/qualify plan --subject nke --version v0.22.0-beta.1 --version-overrides controller=v0.1.1-rc.11,kube=1.36.3

# Write the plan to a file instead of stdout
$ go run ./release/cmd/qualify plan --subject nke --output plan.json
```

| Flag | Default | Meaning |
|---|---|---|
| `--subject` | required | What to qualify. `nke` is NKE as a whole |
| `--version` | the latest release candidate | The subject's version, which must be an existing tag |
| `--version-overrides` | none | Versions to use in place of what resolves, as `name=version` pairs separated by commas |
| `--scope` | `full` | How much of the matrix to cover, a scope from `policy.yaml` |
| `--output` | `-` | Where to write the plan. `-` is stdout |
| `--root`, `--policy`, `--registry` | the enclosing checkout and the files under `release/` | Where to read the repository and the two files from |

`nke` has no release candidate until the first beta tag is cut, so without
`--version` `plan` stops and says so. Pass `--version` to name a tag that
exists.

A version given with `--version-overrides` replaces what that component or dependency
would have resolved to, and is checked the way a resolved one is: a tag has to
exist, and a Kubernetes or OVN version has to be one the image locks. The names
that were overridden are listed under `overrides` in the plan. The plan looks
like this:

```console
$ go run ./release/cmd/qualify plan --subject nke --scope full
{
  "subject": "nke",
  "version": "v0.22.0-beta.1",
  "scope": "full",
  "combinations": [
    {
      "id": "capi-provisioned-v0.1.0_cli-v0.1.0_controller-v0.1.1_os-image-v0.1.0",
      "components": [
        { "name": "controller",       "version": "v0.1.1" },
        { "name": "cli",              "version": "v0.1.0" },
        { "name": "os-image",         "version": "v0.1.0" },
        { "name": "capi-provisioned", "version": "v0.1.0" }
      ],
      "dependencies": [
        { "name": "nke-cni-ovn", "version": "1.4.0" },
        { "name": "kind",        "version": "v0.32.0" },
        { "name": "clusterctl",  "version": "v1.12.3" },
        { "name": "nerdctl",     "version": "2.3.5" }
      ],
      "streams": [
        {
          "name": "forge-amd64-ubuntu-24.04-kube-1.36",
          "capabilities": { "arch": "amd64", "gpu": "gb200", "kube": "1.36", "provider": "forge", "ubuntu": "24.04" },
          "dependencies": [{ "name": "kube", "version": "1.36.3" }]
        },
        {
          "name": "forge-arm64-ubuntu-24.04-kube-1.36",
          "capabilities": { "arch": "arm64", "gpu": "gb200", "kube": "1.36", "provider": "forge", "ubuntu": "24.04" },
          "dependencies": [{ "name": "kube", "version": "1.36.3" }]
        }
      ]
    }
  ]
}
```

## How the plan is used

`plan.json` is the input to a qualification run. The e2e qualification workflow
will consume it to run the matrix of qualification jobs: each stream in each
combination is one job, run at the versions the plan resolved. The plan carries
what a job needs, the versions to test and the capabilities of the stream to run
on.

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
| [`registry.yaml`](registry.yaml) | How each component resolves to a version. It holds the rule that finds a version, never a version |
| [`policy.yaml`](policy.yaml) | The streams that exist and the scopes over them |
| [`qualification.yaml`](qualification.yaml) | What is qualified when NKE is qualified as a whole: the components tested together, and the other versions a run needs |
| [`cmd/qualify/`](cmd/qualify/) | The binary. `plan` is the only verb |
| [`internal/qualification/`](internal/qualification/) | Reading and checking the three files, and turning them into a plan |
| [`internal/resolve/`](internal/resolve/) | The resolution strategies. It imports nothing from this repository |

Each file has one concern. A component that is qualified on its own would carry
its own `<component>/qualification.yaml` next to its code, using the same
schema, and share the registry and the policy.

## Subjects, components and dependencies

`qualification.yaml` says what a qualification covers. It is written for a
**subject**, the thing being qualified, and the subject's version is the plan's
version. This file is for `nke`, which stands for NKE as a whole, so its version
is a beta tag on `main`. A component qualified on its own would have a file of
the same shape next to its code.

The file lists two kinds of thing.

`components` are the parts of NKE tested together: `controller`, `cli`,
`os-image` and `capi-provisioned`. Each carries `selectors` that say which
version to take, and today the only selector is `latest`, the highest release,
else the highest candidate. One choice of versions for all four is a
**combination**, which is what a plan is organised by.

`dependencies` are the other versions a run needs: Kubernetes, the OVN CNI and
the tools. They have no selectors, because the components determine them or a
pin file does. A dependency with no constraint, or a literal one such as
`"1.4"`, is resolved once for the combination. One whose constraint names a
stream capability, `{capability: kube}`, is resolved once per stream with that
stream's value. Which of the two it is comes from that constraint and nothing
else.

| Component | Where its version comes from |
|---|---|
| `nke`, `controller`, `cli`, `capi-provisioned`, `os-image` | Git tags, under each component's tag prefix |
| `kube`, `nke-cni-ovn` | The lock files the image's packages commit, read at the `os-image` tag the combination resolved. A node installs these from the image's own apt repository, so the lock files are the versions it can offer |
| `kind`, `clusterctl`, `nerdctl` | `make/tool-versions.mk`, which owns every tool pin |

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
`full` carries no selector so it matches everything, and `active` narrows to the
current minor. The set of supported Kubernetes minors is not declared anywhere,
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
