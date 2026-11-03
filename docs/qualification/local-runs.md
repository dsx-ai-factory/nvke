<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Running the hardware lane from a workstation

`qualify local` runs the hardware qualification lane from your own checkout.
One command takes one stream through the same steps as the `qualify-e2e`
workflow, on the same Forge hardware: it provisions a site, bootstraps it,
runs the e2e suite against it, tears it down, and records the result.

You need it when your change comes from a fork. The workflow does not run for
a fork's pull request, because a fork cannot reach the Forge API key or the
Teleport join token. With `qualify local` you run the lane yourself, with your
own credentials. It runs the workflow's own steps rather than a copy of them,
so a pass from a clean checkout, with nothing built that CI would pull, is the
same evidence a CI run would give.

It is also how the lane itself is developed. A change to the workflow's steps
can be run and debugged from a workstation, with every file the run wrote kept
locally, instead of pushing and waiting on CI for each try. `--keep-site` and
`--keep-tenant` leave the machines up to inspect afterwards.

With `--from-source` it builds every image the site needs from the checkout
and pulls none, so it needs no registry credential at all.

[hardware-lane.md](hardware-lane.md) is the operator's guide, and
[hardware-lane-design.md](hardware-lane-design.md) says what each step of the
lane does and why. This page is the reference for `qualify local`: every flag,
every variable, and where it differs from CI.

## How it stays the same run as CI

The workflow's per-stream steps live in
`release/hack/qualify-e2e.sh`. Each workflow step is one call to
that script, and `qualify local` calls the same steps in the same order, so no
run logic exists twice.

| Stage | GitHub CI | `qualify local` |
|---|---|---|
| Plan | `qualify plan` | `qualify plan`, in-process, after fetching upstream's tags |
| Stream values | `qualify-e2e.sh matrix`, `stream-env` | `qualify-e2e.sh stream-env` |
| Check artifacts | `qualify verify` | the same checker for registry images; the CDN is asked through the jump host, because it refuses workstations |
| Resolve | `qualify-e2e.sh resolve`, `controller-image` | the same |
| Site inputs | `qualify-e2e.sh site-inputs` | the same |
| Teleport | a `tbot` bot identity | your `tsh login` |
| Config, suite | `qualify-e2e.sh config`, `run` | the same |
| Scrub | `qualify-e2e.sh withhold` | the same |
| Record | `qualify result` | `qualify result`, in-process |
| Publish, upload | TestGrid if trusted; workflow artifacts | nothing is published; the files are kept in the run directory |

The script also has an `artifacts` step that only `qualify local` calls. It
lists what the suite wrote in `controller/test/e2e/`, which `qualify local`
then moves into the run directory.

## Run

From the repository root, with a current Teleport login:

```bash
tsh login --proxy=<teleport proxy> --ttl=720
```

```bash
go run ./release/cmd/qualify local --stream forge-l40s-amd64-ubuntu-24.04-kube-1.36 --stage full --from-source
```

`--stream` and `--stage` (`provision`, `bootstrap`, `deployment` or `full`) are required.
`go run ./release/cmd/qualify local --help` lists every flag.

| Flag | Default | What it does |
|---|---|---|
| `--build` | pull all | builds the named components from the checkout instead of pulling them: `controller`, `cli`, `capi-provisioned`. Verify does not ask a registry for them |
| `--from-source` | off | builds all three and names the images under `localhost/nvke` (`--registry local`), so no registry and no registry credential is involved |
| `--dry-run` | off | plans, verifies, resolves and stages the site inputs, then prints the suite's arguments without taking a machine |
| `--keep-site` | off | keeps the site node after the run, pass or fail, and a tenant whose specs failed |
| `--keep-tenant` | off | keeps every tenant, passed or not, and the site node they hang off. The spec that deletes a tenant is skipped, so tenant deletion is not tested |
| `--plan FILE` | plan `--scope` | reads that plan instead of planning |
| `--scope`, `--subject` | `full`, `nvke` | what to plan when `--plan` is not given |
| `--version` | the highest version | the version of the subject to plan, as `qualify plan --version` takes it |
| `--overrides` | none | values to plan in place of what resolves, as `qualify plan --overrides` takes them: `name=value` pairs separated by commas. An OS image's value is a URL, which is how a run uses a published image while the one its version names is not published yet. Refused for a component `--build` builds, and with `--plan` |
| `--combination` | the only one | needed only when the plan has more than one |
| `--registry` | `nvcr`, or `local` with `--from-source` | where the site images are pulled from: `nvcr`, `ghcr`, or `local` when every image is built |
| `--ngc-org` | `$QUALIFY_E2E_NGC_ORG` | the NGC org the NVCR images are published under |
| `--node-user` | `$QUALIFY_E2E_NODE_USER` | the account the OS image bakes; empty for the OSS images |
| `--wait` | `0` | how long to keep asking for an artifact that is not published yet |
| `--run-dir DIR` | under the user cache | where the run's files are kept |

The OS images are never built here. Even with `--from-source`, a stream names
where the provider reads them from, and verify checks that they are there.

Whatever `--keep-site` or `--keep-tenant` keeps must be deleted by hand. Neither
kept node can be logged into over SSH once the run ends. The site node holds the
run's public key, but the private half never leaves the suite's process. A
tenant node holds no key at all, and the image locks the account. Each one's
serial console, which carries cloud-init's output, is the way in.

## What it needs from your shell

A run reads the same names the workflow's job puts in its environment, from
the shell that starts it. Most of them are the same for everyone who runs the
lane, and the lane owners can give them to you. Only the jump hosts and your
Teleport login on each are your own, plus the Forge API key, which is secret.

### The simplest case

A `--from-source` run needs nothing from a registry, so this is all of it.
Keep it in a file only you can read, outside the repository:

```bash
# ~/nvke-forge.env (chmod 600)

# The same for everyone; ask the lane owners.
export FORGE_TELEPORT_PROXY=<teleport proxy>
export QUALIFY_E2E_NGC_ORG=<ngc org>
export FORGE_ORG_ID=<org id>
export FORGE_SITE_ID=<site-1 id>
export FORGE_VPC_ID=<site-1 vpc id>
export FORGE_SUBNET_ID=<site-1 subnet id>
export FORGE_SSH_KEY_GROUP_ID=<site-1 key group id>
export FORGE_INSTANCE_TYPE_CPU_ID=<site-1 cpu type id>
export FORGE_INSTANCE_TYPE_GPU_ID=<site-1 gpu type id>

# Yours: the site-1 jump host, and one of the Logins `tsh status` lists.
export FORGE_JUMP_HOST=<jump host>
export FORGE_TELEPORT_LOGIN=<login>

# Secret.
export FORGE_API_KEY=<forge api key>
```

Then, from the repository root:

```bash
source ~/nvke-forge.env
tsh login --proxy="$FORGE_TELEPORT_PROXY" --ttl=720
go run ./release/cmd/qualify local --stream forge-l40s-amd64-ubuntu-24.04-kube-1.36 --stage full --from-source
```

For a GB200 stream, add site-2. The ids are shared; the jump host and login
are yours:

```bash
export FORGE_SITE_2_SITE_ID=<site-2 id>
export FORGE_SITE_2_VPC_ID=<site-2 vpc id>
export FORGE_SITE_2_VPC_PREFIX_ID=<site-2 vpc prefix id>
export FORGE_SITE_2_SSH_KEY_GROUP_ID=<site-2 key group id>
export FORGE_SITE_2_INSTANCE_TYPE_CPU_AMD64_ID=<site-2 amd64 cpu type id>
export FORGE_SITE_2_INSTANCE_TYPE_CPU_ARM64_ID=<site-2 arm64 cpu type id>
export FORGE_SITE_2_INSTANCE_TYPE_GPU_GB200_ID=<site-2 gb200 type id>
export FORGE_SITE_2_JUMP_HOST=<jump host>
export FORGE_SITE_2_TELEPORT_LOGIN=<login>
```

Pulling images instead of building them adds a registry credential; see
[Registry](#registry). The tables below are the reference for every name.

### Tools

| Tool | Used for | Checked up front |
|---|---|---|
| `go` | Running `qualify local` itself, and building nvkectl from the checkout | Needed to start |
| `bash`, `jq` | The script, `release/hack/qualify-e2e.sh` | Yes |
| `git` | Planning from upstream's tags, and describing what was built | Yes |
| `make` | Building nvkectl and the site inputs | Yes |
| `docker` | Building the site images, and pulling them unless every one is built. The controller image cross-compiles for the site's architecture, which needs BuildKit (Docker Desktop's default). Building capi-provisioned for an architecture other than the host's also runs that architecture's binaries, which needs QEMU emulation: Docker Desktop has it, and on Linux install it with `docker run --privileged --rm tonistiigi/binfmt --install arm64,amd64` | Yes |
| `tsh` | Reaching the site's jump host through Teleport | Yes |
| `oras` | Pulling nvkectl when it is not built | No; only that path needs it |

### Teleport

| Name | Required | What it is |
|---|---|---|
| `FORGE_TELEPORT_PROXY` | Yes, unless `TELEPORT_PROXY` is set | The Teleport proxy. It is set as `TELEPORT_PROXY` unless that is already set, before the `tsh status` check, so the check, verify and the suite all use this proxy's login even when you hold logins for other clusters. |
| A current `tsh login` | Yes | Checked with `tsh status` after planning and before verify, which asks the CDN through the jump host. A dry run is checked too. The login must last the whole run: the suite opens relays until the end, teardown included, and a `full` run is about an hour plus builds. Ask for a longer one with `tsh login --ttl=<minutes>` if yours is short. |

### Forge

`environments/forge.yaml` reads these. An unset or empty variable is an
error, so every site-1 id is required on every run, a GB200 run included. The
jump hosts and Teleport logins default to empty, and a site run stops on an
empty jump host before a machine exists. Site-2 is skipped unless all of its
ids are set, and a GB200 stream needs it.

| Name | Site | What it is |
|---|---|---|
| `FORGE_API_KEY` | All | The Forge API key. Secret; read by the suite. |
| `FORGE_ORG_ID` | All | The Forge org the machines are created in |
| `FORGE_SITE_ID`, `FORGE_VPC_ID`, `FORGE_SUBNET_ID` | site-1 | The site, and the VPC and subnet its machines attach to |
| `FORGE_SSH_KEY_GROUP_ID` | site-1 | The key group for serial console access. It grants no shell on a node. |
| `FORGE_INSTANCE_TYPE_CPU_ID`, `FORGE_INSTANCE_TYPE_GPU_ID` | site-1 | The CPU and GPU (L40S) machine types |
| `FORGE_JUMP_HOST`, `FORGE_TELEPORT_LOGIN` | site-1 | The jump host in front of the machines, and your Teleport login on it (one of the Logins `tsh status` lists) |
| `FORGE_SITE_2_SITE_ID`, `FORGE_SITE_2_VPC_ID`, `FORGE_SITE_2_VPC_PREFIX_ID` | site-2 | The site, and the VPC and VPC prefix its machines attach to |
| `FORGE_SITE_2_SSH_KEY_GROUP_ID` | site-2 | As for site-1 |
| `FORGE_SITE_2_INSTANCE_TYPE_CPU_AMD64_ID`, `FORGE_SITE_2_INSTANCE_TYPE_CPU_ARM64_ID`, `FORGE_SITE_2_INSTANCE_TYPE_GPU_GB200_ID` | site-2 | The machine types: a site controller of either architecture, and the arm64 GB200 tenant |
| `FORGE_SITE_2_JUMP_HOST`, `FORGE_SITE_2_TELEPORT_LOGIN` | site-2 | As for site-1 |

### Registry

Where the site images come from is set by `--registry`.

| `--registry` | Needs |
|---|---|
| `nvcr`, the default | `NGC_API_KEY`, `QUALIFY_E2E_NGC_ORG` (or `--ngc-org`), and `docker login nvcr.io` |
| `ghcr` | `GITHUB_ACTOR`, `GITHUB_TOKEN`, and `docker login ghcr.io`, and the NVCR access above as well: verify checks the plan's locations, which are on NVCR, and an nvkectl that is not built is pulled from NVCR. GHCR carries released tags only, so this suits a release `--version` or a `--plan` resolved by tag, not the default plan, which names unreleased commits |
| `local`, the default with `--from-source` | Nothing; every image is built |

Planning needs `QUALIFY_E2E_NGC_ORG` whatever the registry, because
`release/registry.yaml` names the artifacts by it. A run given `--plan` and
`--from-source` does not plan, and needs none of these.

### Optional

| Name | Default | What it changes |
|---|---|---|
| `QUALIFY_E2E_NODE_USER` (or `--node-user`) | The OSS images' account | The account the OS image bakes. Set it for an image from another pipeline. |
| `QUALIFY_E2E_SITE_CONTROLLER_VERSION` | The plan's controller | The site controller version, in place of the one the plan names |
| `SITE_DEPLOY_PATH_REF` | The controller's commit | The commit the deploy path is exported from |
| `NVKE_CONTROLLER_NAMESPACE`, `NVKE_CONTROLLER_SERVICE` | `nvke-controller` | The name the controller is installed under. A release cut before the rename uses `nke-controller`. |

## Run directory

By default, a new directory under the user cache:
`~/Library/Caches/nvke/qualify-local/<time>-<stream>/` on macOS,
`~/.cache/nvke/qualify-local/<time>-<stream>/` on Linux. It holds `plan.json`
(unless `--plan` named one), `steps.env` (what each step resolved),
`e2e-config.yaml`, `site-inputs/`, `qualify-e2e.log`, the JUnit and JSON
reports, any diagnostics, and `qualify-result.json`. A dry run writes no
`e2e-config.yaml` and no result. The jump hosts and the token issuer are
scrubbed from the text files (the log, the JUnit and JSON reports,
`tenant-diagnostics.json` and `site-cluster-state.txt`) before they are moved
there. Nothing else is scrubbed: those files still carry node addresses, the
Forge ids, the NGC org and the Teleport login, and `nvkectl-diagnostics.tar.gz`
and the `site-bundle*` files are moved as the suite wrote them. Only
`qualify-result.json` is fit to attach to a pull request.

When anything was built, the result is written from `plan-as-run.json`: the
plan with each built component naming the commit it was built at, so the
result records what ran rather than what the plan would have pulled. A build
takes the working tree, uncommitted changes included, and is named for
`HEAD`. The resolve step warns when a tracked file differs from `HEAD`; it
does not look at untracked files, which a build can still pick up.

## Interrupts

The suite runs in its own process group. The first Ctrl-C is passed to it
once, and Ginkgo tears the node down; wait for it. Further Ctrl-Cs are not
passed on, because Ginkgo skips its cleanup on a second interrupt and the
instance would leak.

## Planning from a fork

- **Which remote is upstream.** The one whose URL names
  `dsx-ai-factory/nvke`, whatever it is called. It is needed even with
  `--plan`, because the resolve step fetches the commits it stages from it.
  With none, `qualify local` stops before it creates the run directory and
  says to `git remote add upstream https://github.com/dsx-ai-factory/nvke.git`.
- **Tags.** `qualify local` fetches upstream's tags before planning, and not
  at all with `--plan`. A tag that exists only locally, or only on your fork,
  can still change the plan; delete it locally if it does.
- **Hash length.** Publishers describe commits in a fresh clone of upstream,
  so the planner sizes the abbreviation from the objects reachable from the
  upstream remote's branches and tags. A clone that has also fetched forks
  would otherwise describe a digit longer and name artifacts nobody published.
  The fetch before planning prunes the upstream branches that have since been
  deleted, which would otherwise still count. Every local tag counts too,
  including one only on your fork, so delete one that makes the plan name a
  version nobody published.

## Compared with running the suite directly

`qualify local` is a wrapper around running the e2e suite directly: its last
step is the same `go test` invocation, with CI's flags. The difference is
everything around that command. Use `qualify local` when you want a result
that is comparable to CI's, and run the suite directly when you need control
over what it runs, to debug a spec or develop the suite.

| Step | `qualify local` | `go test` directly |
|---|---|---|
| Choose versions | `qualify plan`, or `--plan`, `--version` or `--overrides` | you write the `-e2e.versions` string |
| Choose the machine | the stream's capabilities, from the plan | you write `-e2e.capabilities` |
| Checks before a machine is taken | the tools are on `PATH`, the plan's inputs are set, `tsh status`, and every artifact the plan names is published, the OS image included (asked through the jump host) | none; a missing artifact or a lapsed login fails partway through, with a machine already taken |
| Build what the node needs | the lane's `resolve`, `controller-image` and `site-inputs` steps: the controller and capi-provisioned archives, the profiles, and nvkectl for the node's architecture | you build each archive and nvkectl, and put them where the config points |
| E2E config | written by the lane's `config` step | you write it: the nvkectl binary, the image archives and repositories, `deployPathRef`, the lifecycle |
| Environment and tenants | from the stream's provider, with the other provider's tenant entries excluded | you pick `-e2e.environment`, and a label filter that excludes the other provider's tenant |
| Suite flags | CI's: label `site-controller`, `-timeout 220m`, `-ginkgo.timeout=150m`, the JUnit and JSON reports | whatever you pass |
| Ctrl-C | the first one reaches Ginkgo once, so its teardown runs | depends on how you start it; a second Ctrl-C reaching Ginkgo skips its cleanup and leaks the instance |
| After the run | withhold (the jump hosts and the token issuer are scrubbed from the text outputs), collect into the run directory, `plan-as-run.json`, `qualify-result.json` | the raw outputs in `controller/test/e2e/`, nothing scrubbed, nothing recorded |

Running the suite directly can do what `qualify local` does not:

- **Any label filter,** so one spec or a few rather than the lane's whole
  `site-controller` set.
- **Any flags and stage,** including the tenant specs against a site
  controller that already exists, which never dials the jump host.
- **Fast iteration:** keep a site with `-e2e.keep-site`, change the test code,
  and run again against it, without a new plan or new archives.
- **A debugger:** the test binary under `dlv`, or with more Ginkgo verbosity.

`qualify local` gives what running the suite directly does not:

- **A result comparable to CI's:** the same steps, in the same order, from the
  same script. A pass from running the suite directly says only that the specs
  you chose passed at the versions you picked.
- **Failing before a machine is taken,** rather than twenty to forty minutes
  in.
- **A record:** `plan-as-run.json` names what was built from your checkout,
  and `qualify-result.json` holds the result, so the run can be attached to a
  pull request or published.
- **A file you can share:** `qualify-result.json` names no host or address.
  The withhold step scrubs the jump hosts and the token issuer from the text
  outputs, but they still carry the site's addresses and ids, so keep them to
  yourself.

Both need a Forge API key, a Teleport login and, unless every image is built,
registry access, and both run against Forge or DSX only. One way to use them
together: develop and debug against a kept site by running the suite directly,
then confirm with `qualify local` before asking for review.

## What a run executes

An L40S `full` run built from source, given a plan it does not have to make:

```bash
go run ./release/cmd/qualify local --plan <plan.json> --stream forge-l40s-amd64-ubuntu-24.04-kube-1.36 --stage full --from-source
```

runs the equivalent of the commands below. `$SCRIPT` is
`release/hack/qualify-e2e.sh` and `$RUN` the new run directory.
The versions are an example; `<commit>` is `HEAD`, abbreviated to 12.

```bash
# 1. Preflight, in-process.
#    bash, jq, git, make, docker and tsh are on PATH.
#    The upstream remote is the one whose URL names dsx-ai-factory/nvke.
#    With --plan, nothing is fetched or planned. Without it, this runs first:
#      git fetch --quiet --tags <upstream remote>
#      qualify plan --subject nvke --scope full --version <version> --root . --output $RUN/plan.json
mkdir -m 700 $RUN

# 2. The Teleport login must be current before anything reaches the jump host.
tsh status

# 3. Verify, in-process, with qualify verify's checker. Built components are
#    not pulled, so only the OS images are checked, and the CDN is asked
#    through the jump host because it refuses workstations.

# 4. The script's steps. Each is `bash $SCRIPT <step>`, with the shell's
#    environment plus what earlier steps wrote to $RUN/steps.env.
bash $SCRIPT stream-env <plan.json> <combination> forge-l40s-amd64-ubuntu-24.04-kube-1.36
#    -> PROVIDER=forge ARCH=amd64 GPU=l40s KUBERNETES_VERSION=1.36.3
#       CNI_OVN_VERSION=2.4.2, and the plan's component versions

#    --from-source empties the three built versions; empty is what the
#    resolve step reads as "build it":
#    SITE_NVKE_CONTROLLER_VERSION=  CLI_VERSION=  CAPI_PROVISIONED_VERSION=

bash $SCRIPT resolve
#    -> SITE_NVKE_CONTROLLER_BUILD=1 SITE_PROVISIONED_BUILD=1 SITE_CLI_BUILD=1,
#       each named <commit>; SITE_IMAGES=localhost/nvke SITE_ENVIRONMENT=forge
#       SITE_TENANT_EXCLUDE=nico-dsx CAPABILITIES=arch=amd64,gpu=l40s

bash $SCRIPT controller-image
#    -> SITE_NVKE_CONTROLLER_REPOSITORY=nvke-controller

bash $SCRIPT site-inputs
#    which runs
make -C controller/test/e2e site-inputs SITE_NVKE_CONTROLLER_VERSION=<commit> SITE_NVKE_CONTROLLER_BUILD=1 SITE_NVKE_CONTROLLER_REPOSITORY=nvke-controller SITE_ARCH=amd64 SITE_CLI_BUILD=1 SITE_CLI_VERSION=<commit> SITE_IMAGE_REGISTRY=localhost/nvke SITE_PROVISIONED_VERSION=<commit> SITE_PROVISIONED_BUILD=1 SITE_INPUTS=$RUN/site-inputs
#    -> $RUN/site-inputs/controller.tar, capi-provisioned.tar, profiles.tar,
#       and cli/dist/nvkectl-linux-amd64

bash $SCRIPT config
#    -> $RUN/e2e-config.yaml

bash $SCRIPT run
#    in its own process group, so one Ctrl-C reaches Ginkgo; it runs
go test -v -timeout 220m . -args -ginkgo.label-filter="site-controller && !nico-dsx" -ginkgo.timeout=150m -ginkgo.v -ginkgo.fail-on-empty -ginkgo.no-color -ginkgo.junit-report=qualify-e2e.junit.xml -ginkgo.json-report=qualify-e2e.report.json -e2e.config=$RUN/e2e-config.yaml -e2e.environment=forge -e2e.site-stage=full -e2e.versions="controller=<commit>,cli=<commit>,capi-provisioned=<commit>,os-image-bootloader-amd64=<url>,os-image-amd64=<url>,...,kube=1.36.3,cni-ovn=2.4.2,..." -e2e.capabilities=arch=amd64,gpu=l40s

bash $SCRIPT withhold
#    scrubs the jump hosts and the token issuer from the log, the reports,
#    tenant-diagnostics.json and site-cluster-state.txt
bash $SCRIPT artifacts
#    lists what the suite wrote in controller/test/e2e/, which is moved to $RUN

# 5. The record, in-process.
#    $RUN/plan-as-run.json: the plan, with the built components named <commit>
#    qualify result, from plan-as-run.json and the JUnit report, writes
#    $RUN/qualify-result.json
```

The config step writes:

```yaml
node:
  user: ""
nvkectl:
  binary: cli/dist/nvkectl-linux-amd64
site:
  deployPathRef: ""
  images:
    controller:
      repository: localhost/nvke/nvke-controller
      archive: <run dir>/site-inputs/controller.tar
    capi-provisioned:
      repository: localhost/nvke/cluster-api-provider-provisioned
      archive: <run dir>/site-inputs/capi-provisioned.tar
    profiles:
      repository: nke-e2e-profiles
      tag: local
      archive: <run dir>/site-inputs/profiles.tar
```

Without `--from-source`, the resolve step keeps the plan's published versions,
and `make site-inputs` pulls those images, with `SITE_*_BUILD=0` and the
registry `--registry` names, instead of building them. The Forge API key and
the site ids never appear in these commands: the suite reads them from the
environment, through `environments/forge.yaml`. The workflow runs the same
`$SCRIPT` steps; it differs in using a `tbot` identity instead of `tsh status`,
and in running `qualify verify` and `qualify result` as steps of their own.

## Known limits

- Two runs from one checkout at once collide: the suite writes its log and
  reports in `controller/test/e2e/` before they are moved.
- Do not rebase or switch branches in the checkout while a run is going. The
  suite is compiled from it when the `run` step starts, and the site inputs
  are built from it.
- `qualify verify` reports a 401 ahead of a missing artifact, so a credential
  problem can hide what is missing.
- A `--keep-tenant` run skips the tenant-delete spec, which Ginkgo does not
  count as a failure, so its `qualify-result.json` can say `passed` without
  tenant deletion having been tested. Do not take it as a qualification.
